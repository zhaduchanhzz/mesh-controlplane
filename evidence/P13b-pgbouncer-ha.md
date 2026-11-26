# PR P13b Evidence — HA PgBouncer, phía client (code)

- **Repo commit before / after:** `c4533aa` (`feat/P13-plan`) → tip `feat/P13be-db-noise` (bảng §1; tip trong báo cáo cuối).
- **Trạng thái:** hạ tầng lab §7 **PAUSED** (đã áp, chưa có cluster dùng); phần code `Verified` (test trong process, PostgreSQL 16 + PgBouncer thật trên mc-ci). Drill lab (kill PgBouncer khi tải, sau F05) **`Not started`** (cần P13a). Chưa `Reviewed`.
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
- **Ghi chú tích hợp P13 (2026-11-26):** P13f (D9b) dùng alias theo cluster; cấu hình thống nhất là `name: mesh_cN` + `lease_name: mesh_cN_lease` cho từng cluster (`infra/lab/pgbouncer/`), thay cho một alias chung `mesh_lease` ở hai dòng trên/dưới.
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

## 7. Lab: PgBouncer + HAProxy on mc-pg-01/02, pgBackRest (2026-11-26, nhánh `feat/P13b-infra`)

**Trạng thái: PAUSED** (chủ dự án đổi hướng 2026-11-26 23:04; dừng mọi thay đổi mới). Từng mục bên dưới ghi rõ đã áp hay chưa. Chưa `Reviewed`.

### 7.1 Đã áp trên lab

| Máy | Đã áp | Trạng thái lúc dừng |
|---|---|---|
| mc-pg-01 (.247), mc-pg-02 (.248) | PgBouncer 1.26.1-1.pgdg24.04+1 (`apt-mark hold`), listen `<IP node>:6432`, `infra/lab/pgbouncer/pgbouncer.ini.tmpl` + `pgbouncer-clusters.ini` (alias `mesh_c1`, `mesh_c2`, `*_lease`, `mesh_ops`) + catch-all `*` (DB `e2e_*` và drill); auth `hba` (headscale SCRAM từ 192.168.1.0/24; admin console chỉ cho user `pgbouncer` qua unix socket, cùng uid với process); unit drop-in `Restart=on-failure`, `RestartSec=1`, `LimitNOFILE=8192` | active, enabled |
| mc-pg-01, mc-pg-02 | HAProxy 2.8.30-0ubuntu0.24.04.2 (hold), `127.0.0.1:5000` → node có Patroni `GET :8008/primary` = 200; `inter 1s fall 2 rise 2`, `on-marked-down shutdown-sessions`, `retries 3`, `option redispatch 1`, `timeout connect 1s`; socat (để đọc stats socket) | active, enabled |
| mc-pg-01, mc-pg-02 | pgBackRest 2.60.1 (gói có từ P13a): `/etc/pgbackrest/pgbackrest.conf` (= `infra/lab/pg/pgbackrest.conf`), `/etc/pgbackrest/conf.d/cipher.conf` (root:postgres 0640, tạo từ `/etc/mc-pg/pgbackrest.pass`), key SFTP của user postgres `/var/lib/postgresql/.ssh/pgbackrest_ed25519`, known_hosts của mc-db; file mẫu cũ `/etc/pgbackrest.conf` đổi tên thành `.package-sample` | stanza `mc-pg`: status ok |
| Patroni DCS | `postgresql.parameters.archive_command = pgbackrest --stanza=mc-pg archive-push %p` (`patronictl edit-config`, chỉ reload; `archive_mode` đã `on` từ P13a, **không restart**) | có hiệu lực trên cả hai node |
| mc-db (.243) | user hệ thống `pgbackrest` (khóa mật khẩu, chỉ dùng key); repo `/var/lib/pgbackrest-mc-pg/repo` (6,8 MB); `authorized_keys` `from="192.168.1.247,192.168.1.248",restrict`; `/etc/ssh/sshd_config.d/70-pgbackrest-repo.conf` (`Match User pgbackrest` → `ForceCommand internal-sftp`, không TTY, không forward); `sshd -t` rồi reload | Docker `mc-pg`/`mc-pgb` và etcd witness không đụng tới (Up 2 days) |
| Operator | `%USERPROFILE%\.mesh-lab\mc-pg.env` thêm `MC_PG_PGBACKREST_CIPHER_PASS` (không in ra) | — |

- **nftables: không đổi.**
  - `mc-pg.nft` của P13a đã mở 6432 cho .240, .241, .242 (mc-bench, cho capload P14 ở cluster mode), .244–.246, và giữ 5432 đóng với các VM cluster.
  - Lệch so với brief (brief chỉ có .240, .241, .244–.246): vẫn giữ .242 vì P13a và P14 đã ghi nhận. Nếu cần bỏ thì sửa một dòng.
  - Rule nft tạm `p13b-blackhole` (ca P4 của test) đã gỡ: `nft list ruleset | grep -c p13b` = 0 trên cả hai node.
- **Lúc dừng:**
  - Leader `mc-pg-01`, sync standby `mc-pg-02` đang streaming, timeline 13 (nhảy nhiều lần vì test này switchover nhiều lần).
  - Cụm không có database ứng dụng nào: DB tạm `p13b_rt` đã xóa.
  - Trên mc-ci không còn writer nào chạy và không còn file tmpfs nào.
  - `check-pg.sh` → **ALL CHECKS PASSED** (gồm cả PgBouncer, HAProxy, pgBackRest).
- **Chưa làm:**
  - chưa trỏ cluster nào (c1/c2, placement, bootstrap) sang mc-pg: tất cả vẫn dùng mc-db; `meshlab` chưa chuyển;
  - chưa chạy drill F05, F08, kill PgBouncer với process cluster thật;
  - chưa cấu hình `restore_command` cho replica;
  - chưa có monitoring cho pgBackRest.

### 7.2 Lệnh và exit code

| Lệnh | Exit |
|---|---|
| `bash infra/lab/pg/deploy.sh pgbouncer` lần 1 và lần 2 (lần 2 idempotent: PID của pgbouncer và haproxy không đổi) | 0, 0 |
| `bash infra/lab/pg/deploy.sh pgbouncer` sau khi sửa HAProxy/PgBouncer (reload) | 0 |
| `bash infra/lab/pg/pgbouncer-failover-test.sh <art>` run 1 và run 2 (P1–P4) | 0, 0 |
| `PHASES=P1 … pgbouncer-failover-test.sh`: run 3, 4 (trước sửa); run 5–8 (sau sửa) | 0 ×6 |
| run 9 (P1–P4) | 1. `systemctl kill -s SIGKILL` báo lỗi giả ("auxiliary processes") nên script dừng; sửa bằng `--kill-whom=main` |
| run 10 | 3. DB `p13b_rt` sót lại từ run 9 vẫn còn pool trong PgBouncer; sửa bằng `DROP … WITH (FORCE)` |
| run 11 (P1–P4, sau mọi sửa) | 0 |
| `bash infra/lab/pg/deploy.sh pgbackrest` lần 1 | 37: `stanza-create command requires option: repo1-sftp-host-key-hash-type`; sửa bằng thêm `sha256` |
| `bash infra/lab/pg/deploy.sh pgbackrest` lần 2 (stanza-create, archive_command, check, info) | 0 |
| `pgbackrest --stanza=mc-pg --type=full backup` trên leader mc-pg-02 | 0. Mất 6,1 s; 22,5 MB nén còn 2,8 MB trong repo; label `20261126-160157F` |
| `patronictl switchover --leader mc-pg-02 --candidate mc-pg-01`, rồi `pgbackrest check` trên primary mới | 0. check mất 819 ms; `pg_stat_archiver`: failed 0, WAL cuối `0000000D0000000000000015` (timeline mới) |
| `pgbackrest info` + `archive-get` trên replica mc-pg-02 | 0. Replica đọc và giải mã được repo qua SFTP (một WAL 16 MiB) |
| `bash infra/lab/pg/check-pg.sh` | 0 |

### 7.3 Số đo

Cách đo: mỗi writer ghi một dòng mỗi 200 ms, mỗi lần ghi mở một kết nối mới, qua PgBouncer.

- `w247`: chỉ dùng PgBouncer của mc-pg-01.
- `w248`: chỉ dùng PgBouncer của mc-pg-02.
- `wmulti`: DSN `host=.247,.248 port=6432 target_session_attrs=read-write connect_timeout=2`, tức DSN mà process cluster dùng.

Kết quả run 11 (sau khi sửa):

| Pha | w247 | w248 | wmulti |
|---|---|---|---|
| P1 switchover mc-pg-01 → 02 | 0 lỗi, gap 2,4 s | 0 lỗi, gap 3,7 s | 8 lỗi `connection to server was lost` trong 2,2 s, gap 2,5 s |
| P2 kill -9 PgBouncer mc-pg-01 (systemd bật lại sau 1 s) | 4 lỗi trong 1,0 s | 0 | **0**, gap 0,28 s |
| P3 dừng PgBouncer mc-pg-01 trong 15 s | lỗi suốt 16,6 s (đúng như dự kiến: writer này chỉ có một host) | 0 | **0**, gap 0,29 s |
| P4 chặn tcp/6432 tới mc-pg-01 trong 15 s (không trả RST) | 7 lỗi (mỗi lần chờ 2 s) | 0 | **0**, gap 2,3 s (bằng `connect_timeout`) |
| Commit đã ack nhưng bị mất | 0 | 0 | 0 |

- **D12 (kill PgBouncer → lỗi ghi ≤ 10 s): đạt.** Với DSN nhiều host, thời gian lỗi là 0 s ở run 1, 2 và 11.
- Sau khi sửa: 0 lỗi read-only qua 5 lần switchover, cả hai chiều (run 5–8 và 11).

### 7.4 Lỗi tìm thấy và đã sửa (RED → GREEN)

**RED (run 2, switchover mc-pg-01 → 02).**
- Qua PgBouncer mc-pg-01 có 46 lỗi `cannot execute INSERT in a read-only transaction`, kéo dài **27 s**. Lỗi chỉ hết khi pha P2 giết PgBouncer; nếu không có P2, lỗi sẽ kéo dài tới `server_lifetime`.
- `wmulti` cũng bị (43 lỗi). Lý do: qua PgBouncer, `target_session_attrs=read-write` chỉ kiểm câu trả lời của pooler lúc kết nối, không kiểm server connection mà mỗi transaction thực sự nhận.

**Nguyên nhân** (đối chiếu log PgBouncer, HAProxy và PostgreSQL):
1. 15:41:02.861: PgBouncer mở một server connection. Lúc đó HAProxy vẫn đánh dấu mc-pg-01 là UP, dù mc-pg-01 đang khởi động lại sau demote và từ chối kết nối.
2. HAProxy **thử lại trên chính server đó** (không redispatch).
3. 15:41:02.930: HAProxy đánh mc-pg-01 DOWN ("0 sessions active").
4. 15:41:04.887: mc-pg-01 lên lại dưới dạng **replica**, và lần thử lại của HAProxy kết nối thành công vào đó.
5. `shutdown-sessions` chỉ cắt các phiên đang mở tại lúc đánh DOWN, nên phiên này sống tiếp và PgBouncer giữ nó trong pool.

**Sửa.**
- HAProxy: `option redispatch 1`, `retries 3`, `timeout connect 1s`. Mỗi lần thử lại sẽ chọn lại server trong số đang UP.
- Phòng thủ thêm ở PgBouncer: `server_check_query = SELECT 1 / (1 - pg_is_in_recovery()::int)`. Trên replica câu này lỗi chia cho 0, nên một server connection rảnh từ `server_check_delay` (5 s) trở lên sẽ bị bỏ trước khi được dùng lại.

**GREEN:** 5 lần switchover (run 5–8 và 11) → 0 lỗi read-only.

**Còn lại (ghi nhận, không sửa):**
- **Đợt lỗi ngắn ≤ 3,3 s khi PgBouncer còn bám vào primary cũ đang tắt.** Lỗi có dạng `connection to server was lost` hoặc `server login has been failing, cached error: the database system is shutting down`. PgBouncer 1.26 trả lỗi ngay cho client đang chờ thay vì xếp hàng.
  - Lease renew chịu được vì hạn self-fence là 8 s; ghi của process sẽ lỗi rồi thử lại.
  - Drill F05 với process thật sẽ đo lại.
- **Cửa sổ ~250 ms cả hai server cùng UP trong HAProxy** (primary mới được đánh UP trước khi primary cũ bị đánh DOWN). Phiên tới server cũ bị cắt khi nó bị đánh DOWN; không thấy lỗi read-only nào từ cửa sổ này.

### 7.5 Điều kiện F08 nhìn thấy lúc dừng

Đây là bước P00 của `infra/lab/e2e/p13/f08.sh` trên nhánh `feat/P13-drill-ops`.

| Điều kiện | Trạng thái |
|---|---|
| Patroni có một primary và một replica | đạt (mc-pg-01 primary, mc-pg-02 replica) |
| `pgbackrest --stanza=mc-pg --output=json info` trên primary trả `status.code = 0` | đạt (0, có một full backup) |
| `SHOW archive_command` chứa `pgbackrest` | đạt |
| PgBouncer `.247:6432` và `.248:6432` truy cập được từ mc-cp | đạt (check-pg) |
| Replica: cổng 5439 trống, có `/usr/lib/postgresql/16/bin/pg_ctl` và `pg_dump`, không có `/var/lib/p13-f08` | đạt (`ss`, `test -x`, `command -v`, `test -e` trên mc-pg-02) |
| Replica đọc được repo để restore | đạt (`info` + `archive-get` chạy từ mc-pg-02) |
| `MC_PG_HEADSCALE_PASSWORD` có trong `mc-pg.env`; `CI_BINDIR` có headscale trên mc-ci | mật khẩu có; `CI_BINDIR` do người chạy drill cung cấp |

## 6. Reviewer approval

Chưa có.
