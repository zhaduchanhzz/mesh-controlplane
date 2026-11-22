# ADR 0006 — PgBouncer bắt buộc trước PostgreSQL dùng chung

- **Trạng thái:** **Accepted** (PgBouncer là thành phần bắt buộc, chủ dự án quyết định 2026-11-22). Chế độ pool đề xuất ở §3 là **Proposed**, chờ reviewer chốt sau khi đọc số đo `evidence/P14-db-benchmarks.md`.
- **Liên quan:** [ADR 0005](0005-cluster-architecture-shared-postgres-bootstrap-incremental-netmap.md) (PostgreSQL dùng chung), PLAN §1/§3/§7/§9, `04-proxmox-lab.md`, tickets P11b (lease), P13 (HA), P14 (B2).

## Bối cảnh

Mọi cluster (active + standby) dùng chung một PostgreSQL (ADR 0005). Mỗi process Go có pool riêng (`database.postgres.max_open_conns`, `hscontrol/db/db.go` `openDB`). Số kết nối tới PostgreSQL vì vậy tăng theo **số process × kích thước pool**, không theo tải thật. Ví dụ N cluster, mỗi active pool 8 và standby pool 2: 10·N kết nối; 16 cluster là 160 kết nối, phần lớn rảnh. Mỗi backend PostgreSQL là một process OS có bộ nhớ riêng; `max_connections` không tăng vô hạn được.

Code hiện tại (P03b, commit `3a4cb6c`):
- Driver: `gorm.io/driver/postgres` v1.6.3 trên `pgx/v5` v5.11.0, mở bằng `postgres.Open(pg.DSN())`.
- `PostgresConfig.DSN()` (`hscontrol/types/config.go:579`) chỉ đặt `sslmode`. **Không có cách cấu hình** tham số pgx như `default_query_exec_mode` hay `statement_cache_capacity`. Vì vậy pgx chạy ở chế độ mặc định `cache_statement`: mỗi kết nối tự tạo **prepared statement có tên** (`stmtcache_<hash>`) ở mức giao thức và cache tối đa 512 câu.
- GORM `PrepareStmt` không bật (`db.go:195`).
- Không có `SET`, advisory lock, `LISTEN/NOTIFY` hay bảng tạm trong code non-test (đã grep `hscontrol/`, `internal/`, `cmd/`).
- Trigger `credentials_user_tailnet_check` là plpgsql chạy trong server, không phụ thuộc phiên.

## Quyết định

1. **Mọi process cluster (active và standby) kết nối PostgreSQL qua PgBouncer.** Không process nào của cluster được nối thẳng tới primary, trừ hai ngoại lệ ở mục 4.
2. **Standby giữ pool tối thiểu:** `max_open_conns` 1–2, `max_idle_conns` 1. Standby chỉ đọc lease. Khi lấy được lease và lên active, nó tăng pool lên mức active (cấu hình, không tự đoán).
3. **Phiên bản:** PgBouncer **≥ 1.21** (hỗ trợ prepared statement giao thức trong chế độ transaction). Lab dùng `edoburu/pgbouncer:v1.24.1-p1` (PgBouncer 1.24.1).
4. **Ngoại lệ nối thẳng:**
   - tạo schema / migration v0.1 và thao tác quản trị nền tảng (operator, `mesh-placement`) — chạy qua kết nối trực tiếp hoặc một pool PgBouncer `session` riêng, vì DDL và script nhiều câu không cần pooling và dễ chẩn đoán hơn;
   - giám sát (`pg_stat_*`) từ listener quản trị riêng.
5. **HA PgBouncer** là yêu cầu của P13/N3 (§5).

## 3. Chế độ pool: đề xuất **transaction** + `max_prepared_statements`

| Chế độ | Kết nối server | Phù hợp với code hiện tại | Kết luận |
|---|---|---|---|
| `session` | 1 server cho mỗi client **suốt phiên**. Pool Go giữ kết nối rảnh lâu (`conn_max_idle_time_secs` 600) nên không giảm số kết nối server | Đúng mọi tính năng phiên | Không đạt mục tiêu giảm kết nối. Số đo: B2 `N4-pgbsession20-sat` |
| `transaction` | Server chỉ gắn với client **trong một transaction** | Cần PgBouncer ≥ 1.21 và `max_prepared_statements > 0`, vì pgx dùng prepared statement có tên. Với `max_prepared_statements = 0` lỗi ngay lúc mở DB (đã đo, §4) | **Đề xuất** |
| `statement` | Mỗi câu một server, cấm transaction nhiều câu | Không dùng được: mọi `Write` là transaction nhiều câu, fencing owner+epoch phải cùng transaction | Loại |

**Lý do chọn transaction:**
- Giảm kết nối server từ 10·N xuống kích thước pool PgBouncer (đo: B2).
- Mọi thao tác DB của code đều là transaction ngắn (`HSDatabase.Write/Read`, `db.go:356-400`) hoặc một câu autocommit.
- Thiết kế lease/fencing (PLAN §4) dùng `UPDATE … WHERE … RETURNING` và đọc `FOR SHARE` **trong cùng transaction**, không dựa vào trạng thái phiên.

**Ràng buộc bắt buộc khi dùng transaction mode (áp cho mọi PR sau):**

1. **Prepared statement:** PgBouncer `max_prepared_statements` ≥ số câu khác nhau mà một process dùng thường xuyên. Đề xuất bắt đầu ở **200** (lab đã chạy) và theo dõi `SHOW STATS`/log; vượt giới hạn thì PgBouncer prepare lại (tốn CPU), không lỗi. Không giảm về 0.
   - Phương án thay thế (cần đổi code, ticket riêng): cho phép cấu hình `default_query_exec_mode` của pgx (`exec`, `describe_exec` hoặc `simple_protocol`) hoặc `statement_cache_capacity=0`. Khi đó không cần `max_prepared_statements`, đổi lại mỗi câu có thêm round-trip Parse/Describe. Chưa đo.
2. **Không dùng trạng thái phiên:**
   - `SET` chỉ được dùng dạng `SET LOCAL` trong transaction (ví dụ `statement_timeout`, `lock_timeout`);
   - advisory lock chỉ dạng `pg_advisory_xact_lock`; **cấm** `pg_advisory_lock` (lock phiên sẽ dính vào kết nối server và bị client khác "thừa kế"). Lease cluster phải giữ đúng thiết kế CAS trên bảng `cluster_leases`, không dùng advisory lock phiên;
   - không `LISTEN/NOTIFY` qua PgBouncer. Nếu sau này cần thông báo giữa process, dùng kết nối trực tiếp riêng (ngoại lệ phải ghi ADR);
   - không bảng tạm, không cursor `WITH HOLD`, không `PREPARE` SQL thủ công.
3. **Trigger plpgsql** (`credentials_user_tailnet_check`) không bị ảnh hưởng: chạy trong server, trong transaction của câu gọi.
4. **Timeout:** đặt `query_wait_timeout` của PgBouncer nhỏ hơn deadline self-fence của lease (P11b), để process đang chờ kết nối không tưởng mình còn giữ lease. Giá trị chốt trong P11b.
5. **Kiểm tra khi khởi động:** khi pooler cấu hình sai (`max_prepared_statements = 0`), `NewHeadscaleDatabase` hiện báo lỗi **gây hiểu nhầm** "database was not created by mesh-controlplane v0.1 … tables exist but no migration record", vì `Migrator().HasTable` nuốt lỗi prepared statement (đã đo, §4). Hành vi vẫn fail closed (không khởi động), nhưng P13 phải thêm self-check rõ ràng (ví dụ chạy hai lần cùng một câu có tham số và báo lỗi cấu hình pooler).

## 4. Bằng chứng đã có

`evidence/P14-db-benchmarks.md` (B2):
- Transaction mode, `max_prepared_statements=200`: chạy được toàn bộ đường đọc/ghi thật (`NewHeadscaleDatabase`, `CreateUser`, `CreatePreAuthKey`, `GetPreAuthKey`, đăng ký node, cập nhật node, `ListNodes`).
- Transaction mode, `max_prepared_statements=0`: PostgreSQL log `ERROR: prepared statement "stmtcache_…" already exists`; process không mở được DB.
- Số kết nối, độ trễ và thông lượng có và không có PgBouncer: xem evidence.

## 5. HA PgBouncer (P13, gate N3)

PgBouncer là điểm lỗi chung mới cho mọi cluster. Yêu cầu:
- ít nhất 2 instance; client nối qua VIP (keepalived) hoặc danh sách host của libpq/pgx (`host=a,b`); chọn phương án trong P13;
- khi failover PostgreSQL (F05), PgBouncer phải trỏ sang primary mới (DNS/VIP của PG, hoặc cập nhật `[databases]` + `RELOAD`), không gửi ghi tới replica;
- test: kill một PgBouncer khi đang tải → các process cluster reconnect, không owner kép, lease không lùi; đo thời gian gián đoạn;
- giám sát: `SHOW POOLS` (`cl_waiting`, `maxwait`), `SHOW STATS`, cảnh báo khi `cl_waiting > 0` kéo dài.

## Phương án thay thế đã cân nhắc

1. **Không pooler, chỉ giới hạn pool Go.** Đơn giản nhất. Bị loại vì số kết nối vẫn tăng tuyến tính theo số process và standby, và không có điểm hàng đợi chung.
2. **Pooler phía ứng dụng** (pgxpool dùng chung) — không áp dụng được vì các process là OS process riêng.
3. **Pgpool-II / Odyssey / pgcat.** Không có số đo; PgBouncer là lựa chọn phổ biến và đủ cho yêu cầu. Có thể xem lại nếu P14 cho thấy PgBouncer (single-threaded) là nút thắt.
