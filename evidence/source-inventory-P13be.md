# Source inventory P13b (HA PgBouncer, phía client) + P13e (xoay Noise key cluster)

- **Commit đọc:** `c4533aa` (nhánh `feat/P13be-db-noise`, tách từ `feat/P13-plan`; chứa toàn bộ P12c). `master` `dece058` là tổ tiên.
- **Ngày:** 2026-11-26. **Phạm vi:** chỉ code + test trong process (Go race, servertest `-tags p05cluster`, PostgreSQL/PgBouncer tạm trên mc-ci). **Không** drill, **không** đổi lab.
- **Thiết kế đã duyệt:** ADR 0011 §D (DSN nhiều host + `target_session_attrs=read-write`, VIP dự phòng; pool lease `mesh_lease`, ADR 0006 §3.6; self-check pooler, ADR 0006 §3.5), ADR 0014 §3 (CLI xoay có kiểm định dạng, active kiểm lại registry mỗi chu kỳ reconcile, không tự tạo key). Chủ dự án duyệt 2026-11-26 (D3, D8).
- [R] = đọc code; [I] = suy luận; [M] = đo/thử trên mc-ci.

## 1. Tài liệu đã đọc

`AGENTS.md` (rule 1, 2, 5, 6, 9, 10, 11, 12, 13), `tickets/P13.md` (P13b, P13e, phân vùng file chung với P13c/P13f, D3, D8, D12), ADR 0005 (Bổ sung quyết định §1/§5, Bổ sung 2: một Noise key mỗi cluster), ADR 0006 §3 (ràng buộc transaction mode: chỉ `SET LOCAL`, advisory xact, không `LISTEN`, §3.4–3.7), ADR 0011 §D, ADR 0014, `evidence/source-inventory-P13.md` §2.1, §2.2, §2.4, §5 (giả định 2, 3), `evidence/P12c-b.md` (B1 khóa advisory, B2 timeout `SET LOCAL` 4 s, B3 renew song song onAcquire), `evidence/P02-noise-client-research.md` Q1 (qua inventory P13), `.github/workflows/ci.yml` (PostgreSQL tạm: initdb, unix socket, trust).

## 2. Symbol (đọc)

### 2.1 Kết nối DB (P13b)
- [R] `types.PostgresConfig` (`hscontrol/types/config.go:538-579`): `Host`, `Port`, `Name`, `User`, `Pass`, `Ssl`, `MaxOpenConnections`, `MaxIdleConnections`, `ConnMaxIdleTimeSecs`, `QueryExecMode` (enum, `config_values.go:234-246`, `setEnum`). Mặc định `hscontrol/config/default.go:50-56` (max_open 10, idle 10, 3600 s, `cache_statement`). Tài liệu sinh bằng `go generate` (`config.go:3-4`: `tools/configgen table` → `config_docs_generated.go`; `tools/configgen/files` → `nix/options.generated.nix`, `docs/ref/configuration-options.md`, `docs/ref/config-example.yaml`). `TestExamples` đòi example khác default (P12c B4).
- [R] `PostgresConfig.DSN()` (`config.go:614-661`): URL `postgres://`, một host; Host bắt đầu bằng `/` = socket qua query `host`. Không có `target_session_attrs`, `connect_timeout`, `statement_timeout`, danh sách host.
- [R] `openDB` (`hscontrol/db/db.go:162-198`): log DSN không mật khẩu, `ResolveSecret`, `gorm.Open(postgres.Open(dsn))`, đặt pool Go.
- [R] `OpenPlatform` (`db/platform.go:127-195`): `openDB` → `checkV01Database` → `checkVersionUpgradePath` → migrate → `tenantscope.Register`. `Platform.LeaseStore()` (`:121`) = `lease.NewStore(p.db)`: **cùng pool** với dữ liệu.
- [R] `tableExists` (`db/tenant.go:86-112`) đã bọc lỗi truy vấn thành `ErrDatabaseUnavailable` (P12c), nhưng **không** nói gì về pooler; với một client duy nhất, PgBouncer `max_prepared_statements = 0` thường **không** lỗi ngay (cùng một server connection giữ statement đã prepare) [I], nên lỗi xuất hiện muộn và ngẫu nhiên khi nhiều kết nối (B2: `prepared statement "stmtcache_…" already exists`).
- [R] `db.OpenReadOnly` (`db/readonly.go:31-70`, mesh-bootstrap) dùng `openDB`; `mesh-placement` dùng `OpenPlatform` (`cmd/mesh-placement/main.go:110`).
- [R] Người dùng `OpenPlatform`: `NewCluster` (`hscontrol/cluster.go:204`), CLI `withPlatform` (`cmd/headscale/cli/cluster.go`), `mesh-placement`.
- [R] `lease.Store` (`internal/lease/lease.go`): `NewStore(*gorm.DB)`, Acquire/Renew/Release/Get; Manager (`manager.go`) chỉ dùng `store` interface. Ghi có fence kiểm lease **trong transaction dữ liệu** (`tenantscope.checkLease`) → phải ở pool dữ liệu; chỉ Acquire/Renew/Release (Manager) và `RecordNoiseKey` (đọc registry) là thao tác lease.
- [R] pgx v5.11.0 (`go.mod:175`, gián tiếp qua `gorm.io/driver/postgres v1.6.3`):
  - URL nhiều host `postgres://u@a:1,b:2/db` được tách thành nhiều `connectOneConfig` (`pgconn/config.go`); thử lần lượt; lỗi `28P01`, `3D000`, `42501` dừng sớm (`pgconn.go:273-287`).
  - `connect_timeout` (giây, số nguyên) áp **cho từng host** (`pgconn.go:254-261`).
  - `target_session_attrs`: `read-write` → `ValidateConnect` chạy `show transaction_read_only`, `on` → thử host tiếp (`config.go:620-635`, `:1124-1135`); giá trị lạ → lỗi parse.
  - Tham số DSN không thuộc `notRuntimeParams` (`config.go:498-533`) được gửi làm **startup parameter** (ví dụ `statement_timeout`).
  - Dialer mặc định `&net.Dialer{}` (`config.go:1084-1088`): keepalive TCP mặc định của Go (15 s). pgx **không** đọc `keepalives*` của libpq: chúng sẽ thành startup parameter và PgBouncer từ chối [I, kiểm ở §4].

### 2.2 Noise key (P13e)
- [R] `readOrCreatePrivateKey` (`hscontrol/app.go:1384-1419`): file thiếu → tạo key mới (0600) và log "creating". Dùng bởi `NewCluster` (`cluster.go:199`) và `headscale serve`.
- [R] `Platform.RecordNoiseKey` (`db/noisekey.go`): rỗng → ghi dưới lease; giống → nil; khác → `ErrNoiseKeyMismatch`. Chỉ gọi ở `onLeaseAcquired` (`cluster.go:1179-1187`).
- [R] `Cluster.reconcile` (`cluster.go:428-…`) mỗi `reconcileInterval` = 2 s (`:77`, ticker `:1466`), dưới `leaseMu`, chỉ khi `ready && leaseHeld`. **Không** đọc registry key.
- [R] `lease.Manager.Run/hold` (`internal/lease/manager.go`): không có đường "tự nhả lease" từ bên ngoài; `onAcquire` lỗi → release + `AcquireInterval`; lease kết thúc (lost/self-fence/shutdown) → `onLose`; chỉ shutdown mới release.
- [R] `placement.Registry.UpdateCluster` (`internal/placement/clusters.go:84-135`) ghi `noise_public_key` không kiểm (`""` xóa); `CreateCluster` cũng không kiểm `ClusterSpec.NoisePublicKey`. Audit: `mutate` (`audit.go:93-…`) ghi `platform_audit` trong cùng transaction. API `PATCH /v1/clusters/{id}` (`placementapi/api.go:111-115, 444-452`) chỉ `maxLength:128`; `ErrInvalid` → 400 (`api.go:368`).
- [R] CLI `headscale cluster …` (`cmd/headscale/cli/cluster.go`): `--cluster-id`, `clusterConfig()`, `withPlatform`, `cliOp(cluster)` = actor `cli:<cluster>`. Không có lệnh xoay key. `headscale generate private-key` có (`cli/generate.go:40`).
- [R] servertest: `startProcess` (`servertest/cluster_p11b.go:131`) gọi `hscontrol.NewCluster(&cfg)`; `AddCluster` (`cluster_p12.go`) và cluster đầu dùng đường dẫn key **chưa tồn tại** trong `TempDir` → dựa vào tự tạo key. Helper `RegisteredNoiseKey`, `SetRegisteredNoiseKey`, `NoiseKey`.
- [R] Harness lab (`infra/lab/e2e/{n1,p11b,p12,p12b}/run.sh`) kiểm `test -s noise_private.key` trên mc-cp trước khi chạy, chép key sang mc-c1b qua pipe: **không** dựa vào tự tạo. c2 lâu dài: key đã có.

## 3. Quyết định thiết kế (trong phạm vi ADR đã duyệt)

### P13b
1. **Danh sách host:** khóa mới `database.postgres.hosts: ["a:6432", "b:6432"]` (mỗi phần tử `host` hoặc `host:port`, IPv6 trong `[]`; thiếu port thì dùng `database.postgres.port`). `host` có dấu phẩy (dạng libpq) cũng được hiểu như danh sách; đặt cả `host` và `hosts` → lỗi cấu hình. Socket unix không dùng được trong danh sách. `DSN()` sinh URL nhiều host của pgx.
2. **`target_session_attrs`:** enum (`any`, `read-write`, `read-only`, `primary`, `standby`, `prefer-standby`). Rỗng = mặc định của driver (`any`), **trừ** `OpenPlatform` (mọi process ghi: cluster, CLI admin, `mesh-placement`) dùng `read-write` khi rỗng. `mesh-bootstrap` (`OpenReadOnly`) giữ nguyên.
3. **`connect_timeout_secs`** (giây, pgx áp cho từng host; 0 = không giới hạn ngoài context). Mặc định **2** (ADR 0011 §D).
4. **`statement_timeout`** (duration, 0 = không đặt): gửi là startup parameter `statement_timeout` (ms). PgBouncer chỉ chuyển nếu có trong `track_extra_parameters` (≥ 1.20); nếu không, PgBouncer từ chối kết nối → self-check báo rõ cách sửa. Khuyến nghị lab vẫn là `ALTER ROLE … SET` (như `idle_in_transaction_session_timeout`, P12c). Keepalive: dùng mặc định Go (15 s), không thêm khóa (pgx không hỗ trợ `keepalives*`).
5. **Pool lease riêng:** khóa `lease_name` (tên DB/alias PgBouncer cho pool lease, rỗng = `name`) và `lease_max_open_conns` (mặc định 2). Trên PostgreSQL `OpenPlatform` luôn mở pool lease **riêng** (pool Go riêng, cùng DSN trừ dbname), đăng ký tenantscope trên nó (bảng tenant vẫn bị chặn), dùng cho `LeaseStore()`. SQLite: cùng pool (một kết nối). Ghi có fence vẫn kiểm lease trong transaction của pool dữ liệu (không đổi).
6. **Self-check pooler** (ngay sau `openDB`, trước `checkV01Database`; cả pool lease): hai kết nối pgx riêng (không qua pool Go) chạy xen kẽ cùng một câu có tham số (A, B, A, B: mỗi câu là một transaction riêng trong transaction mode) với `default_query_exec_mode` của cấu hình; lỗi `prepared statement … already exists / does not exist` (SQLSTATE `42P05`/`26000`) → `ErrPoolerMisconfigured` "PgBouncer transaction mode without prepared statement support (max_prepared_statements = 0?)"; lỗi PgBouncer `unsupported startup parameter` → `ErrPoolerMisconfigured` nêu tham số và `track_extra_parameters`. `SHOW transaction_read_only = on` → `ErrReadOnlyDatabase` (OpenPlatform). Fail closed: không khởi động. OpenReadOnly chạy phần prepared statement, bỏ kiểm read-only.

### P13e
7. **Định dạng key:** `placement.ValidateNoisePublicKey` (`mkey:` + 64 hex, qua `key.MachinePublic.UnmarshalText`); `CreateCluster`/`UpdateCluster` trả `ErrInvalid` (→ 400); `""` trong PATCH vẫn là "xóa" (hành vi P12, ghi rõ). Không đổi OpenAPI (kiểm ở tầng registry) → không cần `make openapi/generate`.
8. **Registry:** `Registry.RotateNoiseKey(ctx, op, id, newKey)`: kiểm định dạng, cluster phải tồn tại và key mới ≠ key hiện tại, ghi `noise_public_key` + `revision + 1` + audit `RotateNoiseKey` (detail: public key cũ/mới, không bao giờ private key) trong một transaction.
9. **CLI** `headscale cluster noise-key {show,install,rotate}`:
   - `show`: public key của file `noise.private_key_path` và public key trong registry, khớp hay không (không in private key).
   - `install --from <file> | --generate`: kiểm định dạng private key, ghi **nguyên tử** vào `noise.private_key_path` (file tạm cùng thư mục, 0600, fsync, rename, fsync thư mục); không đụng DB; in public key.
   - `rotate [--public-key mkey:…]`: mặc định lấy public key từ file key cục bộ; gọi `RotateNoiseKey` (actor `cli:<cluster>`).
10. **Active kiểm lại registry mỗi reconcile:** `reconcile` gọi `RecordNoiseKey` (đọc, ghi chỉ khi rỗng như P12); `ErrNoiseKeyMismatch` → `ready=false`, `ClearLease`, `lease.Manager.StepDown(reason)` → Manager kết thúc hold, `onLose` (retire mọi runtime), **release** lease, chờ `TTL` trước khi thử lấy lại (để standby đúng key lấy trước). Lỗi DB khác: chỉ log (fence ở DB vẫn chặn ghi).
11. **Không tự tạo key ở cluster mode:** `NewCluster(cfg, opts...)` với option `WithNoiseKeyInit()` (CLI `cluster serve --init-noise-key`). File thiếu, không có option → `ErrNoiseKeyMissing`, không tạo file. Có option: chỉ tạo khi registry **chưa** có key cho cluster; registry đã có key → từ chối (key mới chắc chắn lệch). `headscale serve` (không phải cluster) giữ hành vi upstream.

## 4. Ràng buộc

- Không migration (mọi thay đổi nằm trong code/config). Không đổi schema, không đổi `gen/`/OpenAPI.
- Không đổi mô hình Noise (AGENTS 12, ADR 0005, ADR 0014): một key mỗi cluster, không chồng lấn hai key.
- Transaction mode (ADR 0006 §3.2): self-check không dùng `SET`/`PREPARE` SQL/lock phiên; kết nối tạm của self-check đóng ngay.
- File chung với P13c/P13f (`hscontrol/types/config.go`, `tools/configgen` output, `internal/lease`): P13b/P13e chỉ thêm khóa trong `PostgresConfig` và `Manager.StepDown`; merge P13b trước (ticket).
- Raw SQL guard: câu mới là hằng số, không chạm bảng tenant.
- Secret: test dùng PostgreSQL trust trên socket/127.0.0.1; không mật khẩu, không key thật; private key test sinh trong `TempDir`.

## 5. Giả định còn lại

1. PgBouncer trên mc-ci là **1.26.1** (nixpkgs), lab là 1.24.1: hành vi `max_prepared_statements = 0`, `unsupported startup parameter`, chuyển `show transaction_read_only` được coi là giống nhau [I]; drill lab kiểm lại trên 1.24.1.
2. Danh sách host qua PgBouncer → HAProxy → `/primary` (ADR 0011 §D) chưa kiểm (lab P13a/P13b drill). Test ở đây: hai PostgreSQL thật (một `default_transaction_read_only = on`), trực tiếp và qua PgBouncer.
3. Renew bị treo trên kết nối "chặn mạng không RST" giữ tới hạn self-fence (Manager không bỏ một renew đang bay): ngoài phạm vi code P13b (không có RED), ghi rủi ro cho drill.
4. Client sau xoay key: chưa đo (drill P13e).
