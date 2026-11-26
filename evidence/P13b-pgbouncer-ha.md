# PR P13b Evidence — HA PgBouncer, phía client (code)

- **Repo commit before / after:** `c4533aa` (`feat/P13-plan`) → tip `feat/P13be-db-noise` (bảng §1; tip trong báo cáo cuối).
- **Trạng thái:** phần code `Verified` (test trong process, PostgreSQL 16 + PgBouncer thật trên mc-ci). Drill lab (kill PgBouncer khi tải, sau F05) **`Not started`** (cần P13a). Chưa `Reviewed`.
- **Verified source paths & symbols:** `evidence/source-inventory-P13be.md` §2.1, quyết định §3.1–3.6.
- **Design choice / ADR:** ADR 0011 §D (D3: danh sách host + `target_session_attrs=read-write`), ADR 0006 §3.5 (self-check), §3.6 (pool lease). Không migration, không đổi schema, không đổi `gen/`/OpenAPI, không thêm module (pgx vẫn gián tiếp: self-check dùng `database/sql` với driver `pgx` gorm đã đăng ký).
- **Môi trường test:** mc-ci, Go 1.27.4, `-race` với `CGO_ENABLED=1`, `-p 4`. Server tạm `tools/pgtest/throwaway.sh` (`/tmp/p13b-pg.*`, 127.0.0.1 + unix socket, trust, không mật khẩu, xóa sau): PostgreSQL 16 `rw` và `ro` (`default_transaction_read_only = on`, đóng vai replica), PgBouncer **1.26.1** (nixpkgs; lab là 1.24.1): `pgb` (transaction, `max_prepared_statements = 200`) → rw, `pgb0` (`= 0`) → rw, `pgbro` → ro, `pgbtrack` (`track_extra_parameters = IntervalStyle, statement_timeout`) → rw.

## 1. Commit RED → GREEN

| Hành vi | RED commit | Lệnh RED (exit) và kết quả | GREEN commit | Lệnh GREEN (exit) |
|---|---|---|---|---|
| Danh sách host, bỏ qua host read-only / chết | `12ebdaf` (+ sửa helper test `fbd6ea6`, `f85a123`, `7f96ab4`, `eca1e76`, `111aa5d`; chạy ở `111aa5d`) | `set -a; . <dir>/env; go test -count=1 -v -run 'TestOpenPlatformMultiHost\|TestOpenPlatformRefusesPooler\|TestLeaseStoreHasOwnPool' ./hscontrol/db/` → **1**. `host: "ro,rw"`: `hostname resolving error: lookup 127.0.0.1:<port>: no such host` (cả chuỗi bị hiểu là một IPv6 literal: không có cách nào khai báo nhiều host) | `d2186f1` | cùng lệnh: `MultiHostSkipsReadOnly/direct` 0,17 s, `/pgbouncer` 0,38 s, `SkipsDeadHost`, mới `TestOpenPlatformHostsKey` (khóa `hosts`, `[::1]:<đóng>` đứng đầu) → PASS; `go test ./hscontrol/types/ ./hscontrol/config/ ./tools/configgen/...` → **0** |
| Self-check pooler (ADR 0006 §3.5) | như trên | `TestOpenPlatformRefusesPoolerWithoutPreparedStatements`: **`An error is expected but got nil`** — `OpenPlatform` qua PgBouncer `max_prepared_statements = 0` thành công (một client giữ một server connection), lỗi chỉ xuất hiện khi có tải | `0772366` | → **0**; mới `TestOpenReadOnlyRefusesPoolerWithoutPreparedStatements`, `TestOpenPlatformStatementTimeout`, `TestOpenPlatformRefusesReadOnlyServer` |
| Pool lease riêng (ADR 0006 §3.6) | như trên | `TestLeaseStoreHasOwnPool` (pool dữ liệu 1 kết nối, bị một transaction mở giữ): `Acquire` **`context deadline exceeded` sau 2,0 s** | `4556c82` | → **0**: acquire + renew 0,13 s; mới `TestLeaseDatabase` |
| Lint | — | `golangci-lint run ./...` ở `47e5544` → **1**: `noctx` (`net.Listen` trong helper test), `unparam` (`p12Platform` luôn nhận `"c1"`, có từ trước) | `1886808` | xem §4 |

Thông báo lỗi self-check (log test, cổng/tên DB đã thay):

```
database connection pooler is misconfigured for mesh-controlplane: database "mesh_p13b_N" on 127.0.0.1:PORT does not keep prepared statements across transactions (PgBouncer in transaction mode with max_prepared_statements = 0?): set max_prepared_statements (ADR 0006: 200), or database.postgres.query_exec_mode: simple_protocol: ERROR: prepared statement "stmtcache_…" already exists (SQLSTATE 42P05)
database connection pooler is misconfigured for mesh-controlplane: database "mesh_p13b_N" on 127.0.0.1:PORT refuses a startup parameter (PgBouncer: list it in track_extra_parameters, or set it on the role with ALTER ROLE ... SET and remove it from database.postgres): … FATAL: unsupported startup parameter: statement_timeout (SQLSTATE 08P01)
database server is read-only: every server of database "mesh_p13b_N" on 127.0.0.1:PORT answers read-only (target_session_attrs read-write): is the primary or its pooler listed and up?: … ValidateConnect failed: read only connection
database server is read-only: database "mesh_p13b_N" on 127.0.0.1:PORT (transaction_read_only = on): a replica, or a pooler in front of one; list the primary's pooler first, or let database.postgres.target_session_attrs default to read-write
```

Không có mật khẩu trong thông báo (DSN trong log `Opening database` vẫn bỏ mật khẩu như trước).

## 2. Thiết kế

- **Cấu hình mới** (`database.postgres.*`; tài liệu sinh lại bằng `go generate ./hscontrol/types/`: `docs/ref/configuration-options.md`, `docs/ref/config-example.yaml`, `nix/options.generated.nix`, `config_docs_generated.go`):

| Khóa | Mặc định | Ý nghĩa |
|---|---|---|
| `hosts` | `[]` | danh sách `host[:port]` (IPv6 trong `[]`); thiếu port → `port` (0 → 5432). Loại trừ với `host`; `host` có dấu phẩy cũng là danh sách. Không socket unix trong danh sách |
| `target_session_attrs` | `""` | enum libpq. Rỗng = `read-write` trong `OpenPlatform` (cluster, CLI admin, `mesh-placement`), mặc định driver (`any`) trong `OpenReadOnly` (`mesh-bootstrap`) |
| `connect_timeout_secs` | **2** | pgx áp cho từng host (đọc `pgconn.connectPreferred`); host chết tốn 2 s |
| `statement_timeout` | `0` | startup parameter (ms). PgBouncer cần `track_extra_parameters`; khuyến nghị lab: `ALTER ROLE … SET` |
| `lease_name` | `""` | alias DB của pool lease (lab: `mesh_lease`); rỗng = `name` |
| `lease_max_open_conns` | **2** | trần pool lease |

- **DSN:** danh sách host đi dưới dạng tham số `host=a,b&port=p1,p2` (dạng danh sách của pgx), vì `net/url` (và pgx) từ chối IPv6 có ngoặc trong host URL có dấu phẩy (đo: `invalid IP-literal`).
- **Keepalive:** không thêm khóa. pgx không đọc `keepalives*` của libpq (sẽ thành startup parameter, PgBouncer từ chối); dialer mặc định của pgx là `net.Dialer{}` → keepalive TCP mặc định của Go (15 s).
- **Pool lease:** `OpenPlatform` (PostgreSQL) mở pool thứ hai cùng DSN (dbname = `lease_name`), `tenantscope` đăng ký trên nó; `Platform.LeaseStore()` (Manager: acquire/renew/release) dùng pool này. Ghi có fence vẫn kiểm lease trong transaction của pool dữ liệu (không đổi B1/B2 của P12c). SQLite: một pool.
- **Self-check:** trước `checkV01Database`; hai kết nối riêng (`database/sql`, tối đa 2), câu `SELECT $1::int + 1` xen kẽ A,B ×3 vòng (mỗi lần một transaction), rồi `SHOW transaction_read_only` (writer). Pool lease được kiểm khi `lease_name` khác `name`. `OpenReadOnly` kiểm prepared statement, không kiểm read-only.

## 3. Cấu hình lab cần cho drill (chưa áp; P13a/P13b drill)

- Mọi process cluster: `hosts: ["192.168.1.247:6432", "192.168.1.248:6432"]` (bỏ `host`), `lease_name: mesh_lease`; giữ `ssl: disable` như lab hiện tại.
- PgBouncer trên mc-pg-01/02: `[databases]` có `mesh_lease = host=127.0.0.1 port=5000 dbname=meshlab pool_size=4 reserve_pool_size=2` (+ alias tương ứng cho DB `e2e_*` của harness, hoặc harness đặt `lease_name` rỗng); `max_prepared_statements = 200`; không cần `track_extra_parameters` nếu không đặt `statement_timeout` trong cấu hình.

## 4. Lint, vet, race

Xem `evidence/P13e-noise-rotation.md` §4 (chạy chung một lượt, cùng commit; gồm lượt chạy lại sạch của mọi bước RED/GREEN sau khi worktree mc-ci bị agent khác reset).

## 5. Known limitations / security blockers

- Không có blocker isolation. Không đổi fencing/lease semantics (ADR 0005/0006/0010).
- **Renew treo trên kết nối "mất mạng không RST":** `Manager.hold` không bỏ một renew đang bay; renew đó giữ tới hạn self-fence (8 s) nên ca "chặn 6432 không RST" có thể làm active tự fence dù host kia còn sống. Không sửa ở đây (không có RED trong process, `internal/lease` dùng chung với P13c/P13f); drill sẽ đo, nếu RED thì đề xuất giới hạn mỗi lần renew ≤ `renew_interval` để lần sau mở kết nối mới (đi qua host kia trong `connect_timeout_secs`).
- PgBouncer trong test là 1.26.1 (lab 1.24.1). Chưa thử qua HAProxy → `/primary` của Patroni.
- Pool lease mở một kết nối (gorm ping) cả trong process không dùng lease (CLI admin, `mesh-placement`); qua PgBouncer chỉ là một client connection rảnh.
- Mỗi lần mở platform thêm hai kết nối ngắn cho self-check và mỗi kết nối mới thêm một round trip `show transaction_read_only` (`read-write`).
- Benchmark hot path: không đổi đường ghi/đọc nóng (chỉ lúc mở kết nối và lúc khởi động); không chạy benchmark.

## 6. Reviewer approval

Chưa có.
