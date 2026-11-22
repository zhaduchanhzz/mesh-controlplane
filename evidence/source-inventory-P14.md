# Source Inventory — P14 (lần đầu: B2/B3 trên PostgreSQL dùng chung, PgBouncer)

- **Trạng thái:** harness `Implemented`; B2/B3 lần đầu `Verified` trên `mc-bench` (`evidence/P14-db-benchmarks.md`); B1 `Planned` (`tickets/P05.md`). Chưa `Reviewed`.
- **Nhánh:** `agent/bench-db-pgbouncer`, tạo từ `feat/P03-tenant-schema` @ `3a4cb6c` (schema v0.1), rồi merge `docs/plan-v2-cluster` @ `4c1ac6e` (PLAN 2.0) để sửa tài liệu trên bản 2.0. Merge không xung đột; thay đổi `AGENTS.md` trong merge là của commit chủ dự án `4c1ac6e`, không phải của nhánh này.
- **Không đổi code non-test trong `hscontrol/`.** Chỉ thêm `cmd/bench-db/` (build tag `benchdb`, ngoài `go build ./...` và `go test ./...`).

## 1. Path/symbol đã đọc

| Path | Symbol | Dùng cho |
|---|---|---|
| `hscontrol/db/db.go` | `NewHeadscaleDatabase`, `openDB` (pool: `SetMaxOpenConns/SetMaxIdleConns/SetConnMaxIdleTime`, không `PrepareStmt`), `HSDatabase.Read/Write`, `Read[T]/Write[T]`, `Close` | Đường mở DB thật trong mỗi process; transaction ngắn |
| `hscontrol/db/tenant.go` | `createSchema`, `checkV01Database`, `bindRuntimeTailnet`, `resolveRuntimeTailnet`, `ScopedContext` | Mỗi process bind tenant đầu tiên + kiểm owner; tenant còn lại dùng cùng pool qua scope mới |
| `hscontrol/db/tenantscope/tenantscope.go` | `Register`, `scopeWhere`, `scopeMutation`, `stampCreate` | Mọi câu trên bảng tenant đi qua plugin; `Raw` không qua plugin |
| `hscontrol/db/tenant_test.go` | `backends`, `postgresConfig`, `addTailnet` | Cách hai runtime chia một DB; tạo tailnet qua handle nền tảng |
| `hscontrol/db/schema_postgres.sql` | toàn bộ | Bảng/index đo kích thước; `tailnets.status` CHECK **không có** `MOVING` |
| `hscontrol/db/node.go` | `ListNodes`, `GetNodeByNodeKey`, `SetLastSeen`, `preloadNode` | Thao tác đọc/ghi B2 |
| `hscontrol/db/preauth_keys.go` | `CreatePreAuthKey`, `GetPreAuthKey`, `UsePreAuthKey` | Đăng ký node |
| `hscontrol/db/users.go` | `CreateUser`, `ListUsers` | Seed + thao tác B2 |
| `hscontrol/state/state.go:102-123` | `nodeUpdateColumns` (unexported, chép lại trong harness) | `map_update` |
| `hscontrol/state/state.go:855-917` | `persistNode/persistNodeWith` | Hình dạng câu `Select(cols).Updates` |
| `hscontrol/state/state.go:~2942-2965` | đăng ký node qua pre-auth key | `register` |
| `hscontrol/types/config.go:531-620` | `PostgresConfig`, `DSN()` | DSN chỉ có `sslmode`: **không cấu hình được** `default_query_exec_mode` của pgx |
| `internal/tailnet/tailnet.go` | `ParseID`, `NewScope`, `WithScope`, `SourceInternalService` | Scope cho các tenant của process |
| `go.mod` | `gorm.io/driver/postgres v1.6.3`, `gorm.io/gorm v1.31.2`, `github.com/jackc/pgx/v5 v5.11.0` | Hành vi prepared statement |
| `PLAN.md` 2.0, ADR 0001/0005, `tickets/P12b.md`, `06-tests-benchmarks.md`, `04-proxmox-lab.md`, `evidence/P00b-capacity-baseline.md`, `evidence/source-inventory-P03b.md` | — | Bối cảnh, định dạng evidence |

## 2. Ràng buộc hiện có

- Một process hiện tại (`NewHeadscaleDatabase`) = **một pool cho một tenant**. Process nhiều tenant (P05) chưa có; harness dùng `HSDatabase{DB: base.DB.WithContext(scope)}` để chia một pool, như ADR 0001 dự kiến.
- Code không dùng `SET`, advisory lock, `LISTEN/NOTIFY` (grep `hscontrol/ internal/ cmd/`).
- Fencing owner+epoch (P10), `cluster_leases` (P11b), câu CAS move (P12b) chưa có code → harness **mô phỏng** bằng SQL thô, có nhãn.

## 3. Giả định

- Mix thao tác B2 là tổng hợp (tỉ lệ ghi/đọc chọn tay), không phải mô hình client đã đo.
- Harness và PostgreSQL chạy cùng VM `mc-bench` (ghim CPU khác nhau); một benchmark P00c của người khác chạy song song (ghi trong evidence).
- Thiết kế "cluster_id trên mọi dòng" của bài blog được mô phỏng bằng bản sao bảng + cột + index; bài blog không có DDL.

## 4. SQL thô mới (chỉ trong harness, không vào code chạy thật)

`fenceCheck` (`SELECT … FROM tailnets … FOR SHARE`), standby poll, câu CAS owner/epoch, `UPDATE blogsim.*`, câu thống kê `pg_stat_*`, `CREATE SCHEMA blogsim …`. Không áp dụng allowlist SQL thô của P03b vì không nằm trong `hscontrol/`.
