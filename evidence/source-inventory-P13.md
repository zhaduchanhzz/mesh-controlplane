# Source inventory P13 (Gate N3, Operational) — kế hoạch

- **Commit đọc:** `c9ba662` (nhánh `feat/P13-plan`, tách từ nhánh tích hợp `feat/P12c-review-hardening`; `master` `dece058` là tổ tiên, 62 commit phía sau). P12c đã merge vào nhánh tích hợp, **chưa** merge `master`, chưa `Reviewed`.
- **Ngày:** 2026-11-26. **Người viết:** agent lập kế hoạch (chỉ tài liệu; không sửa Go, harness hay lab).
- **Trạng thái:** `Planned`. Không có dòng code nào của P13 tồn tại. Mọi khẳng định về code dưới đây là **đọc code** (đánh dấu [R]) hoặc **suy luận** ([I]).

## 1. Tài liệu đã đọc

| Tài liệu | Phần dùng cho P13 |
|---|---|
| `AGENTS.md` | rule 2 (secret), 3 (tofu: chỉ create/update trong pool + VMID 7700–7799), 4 (root@pam chỉ trong pool lab), 11 (migration nối cuối, transaction, không AutoMigrate), 12 (Noise: một key mỗi cluster, không tự đổi), 13 (lỗi isolation = BLOCK MERGE) |
| `PLAN.md` | §0 (N3 DoD), §1 hàng Storage / Connection pooling / Runtime HA / Noise identity, §4 (owner_epoch không giảm kể cả sau restore, giữ high-water mark ngoài backup), §6 hàng P13, §7 N3, §8, §9 (mc-pg-01/02, PgBouncer trên mỗi VM PG), §11 rủi ro |
| `04-proxmox-lab.md` | §3 địa chỉ (10.77.40.x đề xuất), §4 ma trận VM, §5–6 pipeline IaC, §8 nghiệm thu (F05, restart relay) |
| `design/adr/0005-*.md` | Bổ sung 2 §2 (lease trong PG, đồng hồ DB, failover không đổi owner_epoch; PG sập = control plane mọi cluster dừng), §3 (không RLS, một role ứng dụng), Bổ sung quyết định §5 (xoay key cluster ảnh hưởng mọi tenant) |
| `design/adr/0006-*.md` | §3.4 (`query_wait_timeout` < hạn self-fence), §3.5 (self-check pooler), §3.6 (pool lease riêng `mesh_lease`), §3.7 (kích thước pool), §5 (HA PgBouncer) |
| `design/adr/0009-*.md` | một region DERP mỗi cluster trên `mc-relay`, verify theo cluster fail-closed, secret trong argv của `derper` (ghi nhận cho P13), STUN chỉ ở tiến trình region 901 |
| `design/adr/0010-*.md` | trao tay: epoch tăng đúng một lần khi flip |
| `evidence/REVIEW-2026-11-26-tickets.md` | B7, L1–L10 |
| `tickets/P12c.md`, `evidence/P12c-a.md`, `evidence/P12c-b.md` | L1, L2, L7, một phần L10 đã làm trong P12c; B2 timeouts; B1 thiết kế khóa lease |
| `tickets/P14.md`, `evidence/P14-db-benchmarks.md` | B2 (PgBouncer +0,3–0,6 ms p50; standby poll p99 tới 651 ms qua pool dữ liệu bão hòa) |
| `evidence/E2E-postgres-pgbouncer.md` | `ssl: disable` cluster ↔ PgBouncer trong lab; một PG, một PgBouncer |
| `evidence/LAB-stage2-n1.md`, `LAB-stage2a.md`, `LAB-stage2b-c2.md`, `LAB-resize-2026-11-23.md` | VM, IP, cách apply, sự cố đĩa 7705 |
| `evidence/P02-noise-client-research.md` Q1 | client chỉ lấy Noise key mới khi `Direct` được tạo lại; TLS của `/ts2021` không được client enforce (dòng 112) |
| `evidence/P09-N1.md`, `evidence/N2-gate.md` | S15 (DERP verify) `Verified` trong E2E N1; phương án (a) cho client 1.84.3 |
| `06-tests-benchmarks.md` | F05, F08 (`:38`, `:41`) |
| `03-isolation-threat-model.md` | I14 (restore), `RotateClusterKey` (`:84`), rủi ro key cluster dùng chung (`:103`), PG dùng chung (`:104`) |
| `05-data-api-contract.md:110` | epoch không giảm sau restore, high-water mark ngoài backup |
| `infra/lab/*.tf`, `infra/lab/db/main.tf`, `infra/lab/db/setup-db.sh`, `infra/lab/README.md`, `infra/lab/scripts/{tofu.sh,check-plan.py,pve_api.py}` | xem §3 |

## 2. Symbol trong code (đọc, không sửa)

### 2.1 Lease, epoch
- [R] `cluster_leases(cluster_id PK, holder, epoch CHECK >= 1, expires_at, updated_at)`: `hscontrol/db/schema_postgres.sql:218-234`, DDL `internal/lease/lease.go:59-88`. Trigger `trg_cluster_leases_epoch_monotonic` (`BEFORE UPDATE OF epoch, holder`): chặn epoch giảm, chặn đổi holder mà epoch không tăng. **Chỉ UPDATE**: `DELETE` + chèn lại đặt epoch về 1 (test làm vậy: `internal/placement/p12_placement_test.go:30`). Không có đường code production nào xóa dòng lease (grep `DELETE FROM cluster_leases` ngoài test: không có).
- [R] `Store.acquire` (`internal/lease/lease.go:208-237`): `INSERT … VALUES (…, 1, …) ON CONFLICT DO UPDATE SET epoch = epoch + 1 … WHERE expires_at <= now`. Dòng chưa có → epoch 1.
- [R] `tailnets.owner_epoch` `bigint NOT NULL DEFAULT 1 CHECK >= 1`, trigger `trg_tailnets_owner_epoch_monotonic` (`schema_postgres.sql:12`, `:176-186`); `clusters.revision` có trigger tương tự (`:166-175`).
- [R] Tiến trình **không** nhớ epoch cao nhất từng thấy: `lease.Manager` chỉ giữ lease hiện tại (`internal/lease/manager.go:69-79, 116-120`); reconcile của cluster retire runtime khi `tn.OwnerEpoch != rt.epoch` (`hscontrol/cluster.go:464`) rồi nạp lại ở epoch mới, **kể cả khi nhỏ hơn**. Không có epoch floor, high-water mark ngoài DB hay restore fence (grep: không có).
- [R] Fence: `tenantscope.checkFence`/`checkLease` (`hscontrol/db/tenantscope/tenantscope.go:478-515`) so **bằng** epoch, rồi kiểm lease (holder + epoch + `expires_at > clock_timestamp()`).
- [R] Thời gian lease mặc định: TTL 10 s, renew 3 s, acquire 1 s, safety margin 2 s (`hscontrol/config/default.go:98-101`) ⇒ hạn self-fence 8 s.
- [R] `ClearLease` (`hscontrol/db/platform.go:105`) chỉ quên lease trong bộ nhớ, không xóa dòng.

### 2.2 Kết nối DB, pooler
- [R] `PostgresConfig` (`hscontrol/types/config.go:538-579`): Host, Port, Name, User, Pass, Ssl, MaxOpen/MaxIdle, ConnMaxIdleTimeSecs, QueryExecMode. `DSN()` (`:614-661`) chỉ đặt host/port/`default_query_exec_mode`/`sslmode`. **Không có** danh sách host, `target_session_attrs`, `connect_timeout`, keepalive, DSN lease riêng.
- [R] `openDB` (`hscontrol/db/db.go:162-198`); `Platform.LeaseStore()` dùng **cùng pool** (`db/platform.go:121`): pool `mesh_lease` của ADR 0006 §3.6 chưa có.
- [R] Self-check pooler (ADR 0006 §3.5) **chưa có**; chỉ có `tableExists` bọc lỗi thành `ErrDatabaseUnavailable` (`db/tenant.go:86-112`). Chỗ đặt hợp lý [I]: `OpenPlatform` (`db/platform.go:127`) ngay sau `openDB`, trước `checkV01Database` (`:144`).

### 2.3 Migration, version
- [R] Chuỗi v0.1 (`hscontrol/db/platform.go:45-63`): `202611220000-v0.1-init`, `202611230000-nodes-key-pair-one-tailnet`, `202611231200-registry-clusters-audit`, `202611232000-tailnets-moving-to`, `202611241200-nodes-ip-unique-per-tailnet`, `202611241300-tailnet-settings`, `202611251000-cluster-leases`, `202611261000-tailnet-moves`, `202611261200-tenant-id-immutable`.
- [R] **Mọi** người gọi `OpenPlatform` tự migrate, không có cờ tắt (`platform.go:160`): `headscale cluster serve` (`cluster.go:204`), `mesh-placement` (`cmd/mesh-placement/main.go:110`), mọi lệnh CLI admin.
- [R] `checkV01Database` (`db/tenant.go:118-161`) từ chối migration lạ (`unknown migration %q`), với thông báo "not created by mesh-controlplane v0.1" gây hiểu nhầm khi đó chỉ là DB mới hơn binary.
- [R] `mesh-bootstrap` mở `db.OpenReadOnly` (`hscontrol/db/readonly.go:31-70`): từ chối DB thiếu `LatestMigrationID()` (`ErrSchemaNotCurrent`), **chấp nhận** DB có migration mới hơn.
- [R] `versioncheck` (`db/versioncheck.go:186-279`): cùng minor hoặc +1 minor; dev build bỏ qua.
- [I] L7: không có khóa quanh `gormigrate.Migrate`; hai process mở cùng DB chưa migrate: một bên lỗi và thoát.

### 2.4 Noise key của cluster
- [R] Nạp một lần từ `noise.private_key_path` trong `NewCluster` (`cluster.go:199`) qua `readOrCreatePrivateKey` (`app.go:1384-1419`): **tự tạo key mới nếu file thiếu**.
- [R] Registry: `clusters.noise_public_key` (nullable) (`schema_postgres.sql:159`). `Platform.RecordNoiseKey` (`db/noisekey.go:22-65`): rỗng → ghi (dưới lease sống); giống → OK; khác → `ErrNoiseKeyMismatch`. Chỉ gọi trong `onLeaseAcquired` (`cluster.go:1179-1187`): lệch → `ClearLease`, nhả lease, process ở lại standby và thử lại (không thoát, không ghi đè).
- [R] Đổi key trong registry: `PATCH /v1/clusters/{id}` trường `noise_public_key` (`internal/placement/placementapi/api.go:111-115, 253-256, 444-452`), chỉ kiểm `maxLength:128`, không kiểm định dạng; `""` xóa. Không có lệnh xoay, không có key kế tiếp, không có thời gian chồng lấn.
- [R] `/key` trả đúng một key (`hscontrol/handlers.go:268-270`); `/ts2021` dùng `*c.noiseKey` (`cluster.go:1042`). Không có gì đóng phiên khi key đổi; key bất biến trong bộ nhớ.
- [I] Registry chỉ được đối chiếu lúc lấy lease: sửa registry khi active đang giữ lease không bị phát hiện tới lần takeover sau.
- [R] `headscale generate private-key` có (`cmd/headscale/cli/generate.go:40`).

### 2.5 Rate limit, IP client
- [R] Bootstrap (`internal/bootstrap/http.go`): global 200/s burst 400, tenant 20/s burst 100 (sau xác thực), per-IP chỉ khi `-ip-rate > 0` (mặc định 0, `cmd/mesh-bootstrap/main.go:43-48`). IP = `r.RemoteAddr` (`http.go:231-238`); không XFF, không PROXY protocol (`go-proxyproto` chỉ `// indirect` trong `go.mod:213`).
- [R] Cluster listener (`cluster.go:1339-1372`) không gắn middleware real-IP; `/ts2021`, `/key`, `/verify` không có rate limit; `/api/v2` có bucket global, tiền xác thực, theo key, theo tailnet (P12c B4).
- [R] Lab: VPS `systemd-socket-proxyd` (chỉ TCP) → wg-mesh → mc-cp `systemd-socket-proxyd` → cluster/bootstrap: nguồn luôn là 10.99.77.1 hoặc mc-cp (`infra/lab/README.md`, "Public test endpoint").

### 2.6 Giám sát
- [R] Cluster: `/metrics` trên `metrics_listen_addr`, chỉ `metrics_operator_cidrs`/loopback (`cluster.go:1324-1333`, `1414-1434`). Có metric theo tailnet (trần nhãn), map, nodestore, `cluster_api_refused_total`.
- [R] **Không có**: metric lease (epoch, holder, độ trễ renew, lỗi acquire), metric fence (`ErrFenced`), thống kê pool DB, metric của `mesh-bootstrap`/`mesh-placement`; `mesh-placement` không có health endpoint.
- [R] `/health` (ping DB, cache 1 s), `/ready` (200 khi giữ lease, không lộ epoch) sau P12c.

### 2.7 DERP/STUN
- [R] `/verify` cluster mode: `hscontrol/cluster_verify.go:21-75`; DERP nhúng bị cấm trong cluster mode (`cluster.go:45-48`), nên STUN chỉ đến từ `derper` ngoài.
- [R] Theo ADR 0009 §2 và P09: `derper` chạy với secret **theo lượt chạy E2E**; config lâu dài (`meshlab`) vẫn dùng DERP công cộng.

## 3. Lab: số liệu đo trực tiếp (chỉ đọc, 2026-11-26)

### 3.1 Host PVE (API `pve_api.py inventory` và `GET /nodes/hanh/qemu`, TLS pin)
- PVE 9.2.27, node `hanh`: 2× Xeon E5-2696 v3, **36 thread**; RAM **125,7 GiB**, dùng 96,6 GiB, **trống 26,8 GiB** (KSM đang chia sẻ 14,4 GiB); swap 8 GiB gần như không dùng; loadavg 0,55 / 1,03 / 2,13.
- RAM cấp cho VM đang chạy: **116 GiB** (non-lab 48 GiB: 100 OPNsense 8, 101 pandownloader 32, 102 VPN 8; lab 68 GiB); mc-bench (24 GiB) tắt. vCPU đang chạy: 70 trên 36 thread.
- Storage: `nvme01` lvmthin trống **1629,5 / 1876 GiB**; `local` 74,4 GiB trống; `local-lvm` 347,9 GiB.
- VMID lab đang dùng: 7701–7707, 7714–7718. **7708, 7709 trống.**
- Địa chỉ vmbr0: .240–.246 đã dùng; **.247, .248, .249 không trả lời ping, ARP `INCOMPLETE`** (kiểm từ mc-ci). Có thể nằm trong dải DHCP của router (README đã ghi): cần đặt reservation.

### 3.2 VM
| VM | vCPU / RAM | Dùng (free -m) | Dịch vụ thấy được |
|---|---|---|---|
| mc-db 7704 (.243) | 4 / 8 GiB | 649 MiB dùng + 2,8 GiB cache; đĩa `/` 58 G, trống 54 G | Ubuntu 24.04.5, Docker 29.1.3; `mc-pg` = **PostgreSQL 16.16** (image `postgres:16`), `mc-pgb` = **PgBouncer 1.24.1** (`edoburu/pgbouncer:v1.24.1-p1`), cả hai "Up 46 hours"; NTP đồng bộ |
| mc-cp 7702 (.241) | 4 / 8 GiB | 531 MiB | `headscale-tailnets` (POC), cluster c1, HAProxy `mesh-c2-ha` |
| mc-c2 7705 (.244) | 4 / 6 GiB | 523 MiB | `headscale-cluster` |
| mc-c1b 7706 (.245) | 4 / 6 GiB | 503 MiB | không có unit headscale đang chạy lúc đọc |
| mc-c2b 7707 (.246) | 4 / 6 GiB | 506 MiB | `headscale-cluster` |

### 3.3 PostgreSQL/PgBouncer trên mc-db (chỉ `SHOW`/`SELECT`, không đổi gì)
- `wal_level = replica`, `max_wal_senders = 10`, `archive_mode = off`, `synchronous_commit = on` (không có standby đồng bộ nên tương đương cục bộ), `max_connections = 200`, `shared_buffers = 128MB`, **`data_checksums = off`**, `wal_keep_size = 0`, `pg_is_in_recovery() = f`.
- Kích thước: `meshlab` 8,4 MB, `postgres` 7,6 MB. 7 backend lúc đọc.
- Role `headscale`: `idle_in_transaction_session_timeout=5s` (P12c B2 đã áp).
- `/opt/mc-db/pgbouncer.ini`: `pool_mode = transaction`, `max_prepared_statements = 200`, `default_pool_size = 20`, `query_wait_timeout = 5`, `idle_transaction_timeout = 5`. **Chưa kiểm** process PgBouncer đang chạy đã nạp file này hay chưa (container "Up 46 hours" có từ trước P12c; cần `SHOW CONFIG` trên console admin, cần credential; không làm vì đang có E2E).
- Không có replica, không có backup, không có WAL archive.

### 3.4 OpenTofu
- State thật của `infra/lab` và `infra/lab/db` nằm ở **checkout chính** (`mesh-controlplane/infra/lab/terraform.tfstate`, gitignored). Không plan `infra/lab` từ worktree (sẽ đòi tạo lại mọi VM).
- Bản nháp `infra/lab/drafts/p13-pg/main.tf` là **root riêng** (state rỗng), không được root `infra/lab` đọc. Kết quả (WSL Ubuntu-24.04, OpenTofu 1.13.9, `bpg/proxmox` 0.123.1, lock copy từ `infra/lab/db`):
  - `tofu fmt -check -recursive`: exit 0; `tofu validate`: "Success! The configuration is valid." exit 0.
  - `tofu plan -out=p13-pg.tfplan` (file trong worktree, gitignored, **không apply**): **`Plan: 4 to add, 0 to change, 0 to destroy.`** — 2 snippet `mc-pg-0{1,2}-user-data.yaml` và 2 VM: `mc-pg-01` VMID 7708, `mc-pg-02` VMID 7709, `pool_id = meshcontrol-lab`, 4 vCPU, 4096 MiB, 60 GB trên `nvme01`, `192.168.1.247/24`, `.248/24`, MAC `BC:24:11:77:02:47/48`.
  - `tofu show -json p13-pg.tfplan | python3 infra/lab/scripts/check-plan.py`: "In lab scope: agent may apply this exact plan file." **exit 0** (chỉ create, trong pool + dải VMID; AGENTS rule 3).

## 4. Ràng buộc hiện có

- PG dùng chung và PgBouncer là điểm lỗi chung của mọi cluster (ADR 0005, 0006). Lease đánh giá bằng `now()` của **primary hiện tại**: sau failover PG, đồng hồ đổi sang máy khác.
- Không RLS, một role ứng dụng (ADR 0005 Bổ sung 2 §3): giới hạn kết nối theo cluster không làm bằng role DB được.
- Chỉ `SET LOCAL`, `pg_advisory_xact_lock*`, không `LISTEN/NOTIFY` qua PgBouncer (ADR 0006 §3.2). Migration và công cụ vận hành đi đường trực tiếp (ADR 0006 §4).
- Migration: nối cuối, trong transaction, không AutoMigrate, không tắt FK (AGENTS 11). Chỉ một nhánh thêm migration tại một thời điểm (quy ước P12c).
- Noise: một key mỗi cluster (ADR 0005, AGENTS 12). Agent không được tự đưa vào trial-decrypt nhiều key hay key theo tenant (PLAN §1 chú ý Noise).
- Lab: mọi VM trên một host vật lý; HA PG trên lab chỉ kiểm phần mềm (process, mạng), không kiểm lỗi phần cứng host. Một harness E2E tại một thời điểm.

## 5. Giả định còn lại (chưa kiểm chứng)

1. Client Tailscale (1.104.7, 1.84.3) **không** tự tạo lại `Direct` khi handshake Noise thất bại sau khi key server đổi (P02 Q1 chỉ đọc code; hành vi retry thật chưa đo). P13e đo.
2. pgx v5 xử lý chuỗi host nhiều phần tử với `target_session_attrs=read-write` qua PgBouncer transaction mode như libpq (câu kiểm `SHOW transaction_read_only` đi qua pooler). P13b có RED test thật.
3. PgBouncer 1.24.1 không có `target_session_attrs` phía server; vì vậy đề xuất HAProxy cục bộ theo `/primary` của Patroni. Chưa kiểm trên lab.
4. Patroni (bản trong PGDG/apt Ubuntu 24.04) và etcd 3.5 chạy được trên 4 GiB cùng PostgreSQL + PgBouncer với DB lab vài chục MB. Chưa đo.
5. pg_auto_failover: tình trạng bảo trì upstream chưa kiểm (không tra cứu web trong lượt này).
6. Không có đường code production nào `DELETE` dòng `cluster_leases` (grep), nhưng không có gì ở DB chặn.
7. Địa chỉ .247–.249 không nằm trong lease DHCP đang cấp (chỉ kiểm ping/ARP).
