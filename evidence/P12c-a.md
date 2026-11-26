# PR P12c nhóm A Evidence — cô lập dữ liệu và policy

- **Repo commit before / after:** trước `1ffffc1` (`feat/P12c-review-hardening`); sau: HEAD của `feat/P12c-a` (bảng commit ở §1; commit cuối là commit thêm file này).
- **Verified source paths & symbols:** `evidence/source-inventory-P12c-a.md` (viết trước khi sửa code, commit `473c51f`).
- **Design choice / ADR:** không đổi ADR 0005/0010, không đổi mô hình Noise. Chi tiết thiết kế từng ID ở §2.
- **Môi trường:** mc-ci, Go 1.27.4, `GOFLAGS=-buildvcs=false`; PostgreSQL 16.15 tạm (`initdb`, unix socket, trust, không mật khẩu) qua `MESH_TEST_POSTGRES_DSN=<pg tạm>`; `postgrestest` tự tìm `/usr/lib/postgresql/16/bin`. "SQLite + PostgreSQL" = test chạy cả hai backend (`backends(t)` trong `hscontrol/db`; trong `tenantscope` chạy một lần không DSN và một lần có DSN).
- **Trạng thái:** `Verified` (test RED → GREEN, lint, race bên dưới). Chưa `Reviewed`. Nhánh chỉ push lên `mcci`, chưa lên `origin`, chưa gộp.

## 1. RED → GREEN theo ID

| ID | Test RED | Commit RED | Lệnh | Exit RED | Commit GREEN | Exit GREEN |
|---|---|---|---|---|---|---|
| A7 | `TestScopedContextRefusesOtherTailnet` | `a58f856` | `go test -count=1 -run TestScopedContext ./hscontrol/db/` | 1 (handle A đọc được user `bob` của B, không lỗi) | `63c2f90` | 0 |
| A1 | `TestOrCannotEscapeScopeOnRead/OnUpdate/OnDelete`, `TestScopeWrapsCallerWhere` | `9bbf7ff` | `MESH_TEST_POSTGRES_DSN=$dsn go test -count=1 -run 'TestOrCannotEscape\|TestScopeWraps' ./hscontrol/db/tenantscope/` (dsn rỗng và PG) | 1 cả hai DB: `Find` trả `i0-7e9d` của B; `Update` đổi tên item của B; `Delete` xóa item của B; SQL `WHERE name = ? OR name = ? AND items.tailnet_id = ?` | `9cb2190` | 0 cả hai |
| A2 | `TestTenantTableWithoutSchemaIsRefused` (SQLite + PG) | `ec2e0f4` | `go test -count=1 -run TestTenantTableWithoutSchemaIsRefused ./hscontrol/db/` | 1: mọi dạng đọc được 2 dòng (cả hai tenant), `Table("credentials").Create(map)` chèn vào B, `Table("nodes").Updates`/`Delete` sửa/xóa node của B | `4145ba0` | 0 |
| A3 | `TestUpdateCannotMoveRowToAnotherTailnet` (SQLite + PG) | `790fcb3` | `go test -count=1 -run TestUpdateCannotMoveRowToAnotherTailnet ./hscontrol/db/` | 1: cả 7 dạng (`Update`, `Updates(map tailnet_id)`, `Updates(map TailnetID)`, `Updates(struct)`, `Update(Expr)`, `UpdateColumn`, `Save`) chuyển dòng sang B | `08ff454` | 0 |
| L10 | `TestDeleteSliceWithoutKeysRefused`, `TestCreateFromMapsStamped` | `a3f9cbb` | `MESH_TEST_POSTGRES_DSN=$dsn go test -count=1 -run 'TestDeleteSliceWithoutKeysRefused\|TestCreateFromMapsStamped' ./hscontrol/db/tenantscope/` | 1 cả hai DB: `Delete(&[]item{{},{}})` xóa mọi item của A; batch `[]map` panic `reflect.Value.Field on map Value` | `ca20791` | 0 cả hai |
| A8 | `TestTailnetIDImmutableInDatabase` (SQLite + PG) | `287a3a5` | `go test -count=1 -run TestTailnetIDImmutableInDatabase ./hscontrol/db/` | 1: `UPDATE <bảng> SET tailnet_id = B` thô thành công trên cả 5 bảng, cả hai DB | `9caa7c1` | 0 (kèm `TestTenantIDImmutableDDLInSchemaFiles`, `TestTenantIDImmutableMigrationUpgrades`) |
| A5 | `TestSchemaTenantColumns`, `TestSchemaSQLiteMatchesPostgres` | snapshot `8c049a5` (không commit): `schema.sql` bỏ `NOT NULL` của `policies.tailnet_id` | `MESH_TEST_POSTGRES_DSN=<pg> go test -count=1 -run 'TestSchemaTenantColumns\|TestSchemaSQLiteMatchesPostgres' -v ./hscontrol/db/` | 1: `policies.tailnet_id must be NOT NULL` (sqlite); `policies.tailnet_id: NOT NULL` khác nhau giữa hai DB | `34bd4ec` | 0 |
| A4 | `TestRawSQLGuard` (+ tự kiểm `TestRawSQLGuardCatches`) | snapshot `5fc4c29` (không commit): thêm tạm `hscontrol/db/zz_p12c_a4_offending.go` với `Raw("SELECT * FROM nodes WHERE hostname = ?")`, `Exec(q)` (q là tham số), `Exec("INSERT OR REPLACE INTO nodes …")` | `go test -count=1 -run 'TestRawSQLGuard$' -v ./hscontrol/db/` | 1, báo đủ 3 câu. Guard cũ `TestNoRawTenantWritesOutsideFence` trên cùng snapshot: **0** (bỏ sót cả 3) | `d2d53b3` | 0 |
| A6 | `TestCLITailnetPolicyValidates`; servertest `TestClusterEmptyPolicyReloadDenies` (tag `p05cluster`) | `3903de7` | `go test -count=1 -run TestCLITailnetPolicyValidates ./cmd/headscale/cli/`; `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -tags p05cluster -count=1 -run '^TestClusterEmptyPolicyReloadDenies$' ./hscontrol/servertest/` | 1 (policy không parse được vẫn được lưu); 1 (`WaitForCondition(revoke-a1, "deny-all after the policy was emptied"): timeout after 20s`: client vẫn thấy peer) | `e4aa386` | 0 và 0 (servertest PASS 6.6 s) |

Ghi chú quy trình: A1 và L10 có test RED được sửa (amend) trước khi có commit GREEN vì test ban đầu sai (A1: điều kiện đếm hàng `tailnet_id <> A` bị chính OR của caller nuốt; L10: GORM tự nó không scan được khóa trả về vào `[]map`, nay kiểm bằng `DryRun`). Hash RED trong bảng là bản đã amend và đã chạy lại RED. A6 RED gồm trích hàm `setTailnetPolicyCLI` không đổi hành vi để test gọi được.

### 1b. Chạy rộng trước mỗi commit GREEN (mc-ci, `MESH_TEST_POSTGRES_DSN=<pg tạm>`)

| Sau | Lệnh | Exit |
|---|---|---|
| A1 | `go test -count=1 -p 4 ./hscontrol/db/... ./hscontrol/state/` | 0 |
| A2 | `go test -count=1 -p 4 ./hscontrol/ ./hscontrol/db/... ./hscontrol/mapper/ ./internal/...` | 0 (lần đầu 1: báo nhầm hai mẫu, xem §2 A2) |
| A3 | `go test -count=1 -p 4 ./hscontrol/ ./hscontrol/db/... ./hscontrol/state/ ./hscontrol/mapper/ ./hscontrol/api/... ./internal/... ./cmd/...` | 0 (21 package ok) |
| L10 | `go test -count=1 -p 4 ./hscontrol/db/ ./hscontrol/state/` | 0 |
| A8 | như A3 | lần đầu 1 (chỉ `TestTailnetMovesDDLInSchemaFilesAndLast`, xem §4); sau sửa: `./hscontrol/db/... ./hscontrol/state/ ./internal/... ./cmd/...` 0 |
| A5 | `MESH_TEST_REQUIRE_POSTGRES=1 go test -count=1 ./hscontrol/db/...` (chế độ DSN) | 0, không test nào skip |
| A6 | `go test -count=1 -p 4 ./hscontrol/ ./hscontrol/state/ ./hscontrol/policy/... ./cmd/...` | 0; servertest p05cluster `TestClusterEmptyPolicyReloadDenies\|TestClusterLoadsNewTailnetLive\|TestClusterP06WildcardStaysInTailnet\|TestClusterEpochBumpReloadsRuntime` 0; servertest `TestTenantDefaultDeny\|TestTenantDefaultDenyOffKeepsUpstream` 0 |

## 2. Thiết kế đã làm

- **A1** (`tenantscope.confineWhere`): WHERE thành `"<bảng>"."tailnet_id" = ? AND (<WHERE của caller>)`. Biểu thức của caller được GORM dựng như cũ (`clause.Where.Build`, giữ quy tắc đảo vị trí `Or`) rồi luôn bọc một cặp ngoặc; vị từ tenant đứng trước, nên một `--` cuối chuỗi caller chỉ làm hỏng câu lệnh, không nuốt được vị từ. Áp cho Query, Row, Update, Delete. WHERE không phải `clause.Where` → `ErrUnscopable`. SQL sinh ra được kiểm bằng `DryRun` trên SQLite và PostgreSQL (`TestScopeWrapsCallerWhere`).
- **A2** (`tenantscope/tables.go`): plugin biết danh sách bảng tenant (`TenantTables()`: `users`, `nodes`, `credentials`, `policies`, `tailnet_settings`; test thêm bảng của mình qua `Register(db, extra…)`). Câu GORM (không phải SQL thô) bị từ chối với `ErrUnscopable` khi nêu bảng tenant qua `Table()`, table expression (alias, schema-qualified, subquery), join chuỗi, mệnh đề `FROM`, join association từ model không có scope, hoặc đích không có `TailnetID`; trên **handle tenant** (context có scope) còn kiểm chuỗi trong điều kiện, select, order, group (subquery mà vị từ không với tới). Nhận diện bảng là so khớp từ vựng: định danh (trần, có ngoặc kép, backtick, `[]`, có schema) trùng bảng tenant và không phải tiền tố cột (`nodes.id`); bỏ literal và chú thích. Giữ nguyên `Table("migrations")`, `Joins("AuthKey")`, cột có tiền tố bảng.
  - Lần chạy rộng đầu tiên phát hiện hai mẫu hợp lệ bị chặn: (1) placement (handle platform, không scope) dùng `EXISTS (SELECT 1 FROM policies p WHERE p.tailnet_id = tailnets.id …)` — vì vậy kiểm chuỗi điều kiện chỉ áp cho handle tenant (file placement thuộc nhóm C, không sửa); (2) `DestroyPreAuthKey`/`DestroyRevokedPreAuthKeysBefore` (handle tenant) dùng `id IN (SELECT auth_key_id FROM nodes …)` đọc node của mọi tenant (không lộ dữ liệu nhờ FK kép, nhưng là đọc không scope) — đã viết lại bằng subquery GORM `keysInUse(tx)` để plugin scope.
- **A3** (`tenantscope/assign.go`): trước UPDATE, giá trị gán cho `tailnet_id` (map của `Update`/`UpdateColumn`/`Updates`, struct cùng hoặc khác kiểu của `Updates`/`Save`/`Select("*")`, `clause.Set` tường minh; khóa so theo tên field hoặc cột, không phân biệt hoa thường, bỏ ngoặc và tiền tố bảng) phải đúng bằng ID của scope dạng chuỗi; khác (kể cả `gorm.Expr`) → `ErrForeignTenant`. `TailnetID` rỗng mà sẽ được ghi (Save) thì được đóng dấu scope như Create.
- **L10**: `hasPrimaryKey` coi slice là có khóa chỉ khi **mọi** phần tử có khóa chính; `stampCreate` đóng dấu từng map trong batch qua `stampMap` (từ chối tailnet khác) thay vì panic.
- **A8**: migration `202611261200-tenant-id-immutable` (§4).
- **A4**: guard mới `hscontrol/db/raw_sql_guard_test.go` + `raw_sql_allowlist_test.go`, xóa `raw_write_guard_test.go` (§5).
- **A5**: `hscontrol/db/schema_test.go` + CI (§6).
- **A6**: `State.reloadPolicyLocked` gọi `SetPolicy(nil)` khi không có policy và `PolicyEmptyDenies` bật (deny-all ngay, như lúc khởi động); CLI `cluster tailnet policy`: `--file` được validate bằng `policy.ValidatePolicy` với users/nodes của tailnet và `WithEmptyPolicyDenies(true)`; nội dung rỗng chỉ được lưu với `--empty` (và `--empty` không nhận nội dung).
- **A7**: `HSDatabase.ScopedContext` trả `(context.Context, error)`: `ErrScopeConflict` khi context đã gắn tailnet khác, `ErrNoScope` khi handle không có scope; không bao giờ trả lại context của caller.

## 3. Negative tenant isolation tests (mới)

`TestOrCannotEscapeScopeOnRead/OnUpdate/OnDelete`, `TestScopeWrapsCallerWhere`, `TestUnconfinedTenantTableRefused`, `TestRefersTo` (tenantscope); `TestTenantTableWithoutSchemaIsRefused`, `TestUpdateCannotMoveRowToAnotherTailnet`, `TestTailnetIDImmutableInDatabase`, `TestScopedContextRefusesOtherTailnet` (hscontrol/db, SQLite + PostgreSQL); `TestDeleteSliceWithoutKeysRefused`, `TestCreateFromMapsStamped`; `TestClusterEmptyPolicyReloadDenies` (servertest, p05cluster).

## 4. DB migration and downgrade constraints

- **ID:** `202611261200-tenant-id-immutable`, nối **cuối** chuỗi v0.1 (`v01Migrations`), một migration duy nhất của P12c. `LatestMigrationID()` = ID này.
- Trong `tx.Transaction`, DDL viết tay, không AutoMigrate, không tắt FK. SQLite: 5 trigger `trg_<bảng>_tailnet_id_immutable BEFORE UPDATE OF tailnet_id … WHEN NEW.tailnet_id IS NOT OLD.tailnet_id … RAISE(ABORT)`. PostgreSQL: hàm `tenant_tailnet_id_immutable()` (`IS DISTINCT FROM`, `ERRCODE = 'check_violation'`) + 5 trigger `BEFORE UPDATE OF tailnet_id … FOR EACH ROW`. Ghi lại cùng giá trị (GORM `Save`) không bị chặn.
- DDL nối nguyên văn vào `schema.sql`/`schema_postgres.sql` (`TestTenantIDImmutableDDLInSchemaFiles`); DB SQLite đã migrate khớp `schema.sql` theo squibble (kiểm trong `OpenPlatform`).
- Nâng DB cũ: `TestTenantIDImmutableMigrationUpgrades` (SQLite + PG) quay DB về trước migration bằng chính `Rollback`, mở lại → migration chạy, số dòng 5 bảng giữ nguyên, trigger hoạt động.
- `TestTailnetMovesDDLInSchemaFilesAndLast` khẳng định tailnet-moves là migration cuối; đổi thành `TestTailnetMovesDDLInSchemaFilesAndInChain` (vẫn trong chuỗi, đứng trước migration mới).
- **Hệ quả triển khai:** mesh-bootstrap (`OpenReadOnly`) đòi `LatestMigrationID`, nên từ chối DB lab cho tới khi một process cluster hoặc CLI đã chạy migration này. Downgrade: binary cũ gặp ID migration lạ → `checkV01Database` từ chối (như mọi migration v0.1); `Rollback` có sẵn.

## 5. Guard SQL thô (A4)

- Duyệt mọi lời gọi `Raw`/`Exec`/`ExecContext`/`Query`/`QueryContext`/`QueryRow`/`QueryRowContext` trong code không phải test của module (trừ harness `hscontrol/servertest`, như guard cũ). Tham số SQL: thứ hai với `*Context` hoặc khi tham số đầu là context, thứ nhất nếu không.
- Phân giải tĩnh: literal, const của package và của package khác trong module, ghép `+`, biến cục bộ qua mọi phép gán (kể cả gán trong closure); tham số hàm và biến package coi là không phân giải được.
- SQL phân giải được mà nhắc bảng tenant ở **bất kỳ** đâu (đọc, ghi, DDL; `INSERT OR REPLACE`, `REPLACE`, `UPDATE ONLY`, `MERGE`, `TRUNCATE`, tên có schema hoặc ngoặc) → phải có trong allowlist. SQL không phân giải được trong package import `gorm.io/gorm`, `database/sql` hoặc pgx → cũng phải có.
- Allowlist theo **từng câu**: file + 16 hex đầu SHA-256 của SQL chuẩn hóa khoảng trắng (câu không phân giải: `expr:<hàm>: <mã nguồn>`) + lý do. Mục không khớp câu nào → test fail (không để allowlist cũ đi). 50 mục; 5 file của allowlist cũ được tách thành từng câu ghi của chúng (không nới), các câu đọc/không phân giải được liệt kê lần đầu.
- **Lưu ý tích hợp:** allowlist có mục cho file của nhóm B (`internal/lease/lease.go`, `tenantscope.LockOwnership`) và nhóm C (`hscontrol/db/handover.go`, `internal/placement/*`). Nhóm B/C sửa các câu đó thì hash đổi và guard fail khi gộp: coordinator chạy `MESH_RAW_SQL_GUARD_PRINT=1 go test -run TestRawSQLGuard -v ./hscontrol/db/`, review câu mới và cập nhật mục.

## 6. A5 và CI

- `schema_test.go` đọc lại schema từ DB (SQLite: `pragma_table_info`, `pragma_index_list`, `pragma_index_info`, `pragma_foreign_key_list`; PostgreSQL: `information_schema`, `pg_index`, `pg_constraint`), không so chuỗi. Kiểm: mọi bảng có `tailnet_id` thì `NOT NULL` + FK `tailnet_id->tailnets(id)`; `tenantscope.TenantTables()` = các bảng đó trừ `tailnet_moves`; hai DB cùng bảng, cột, cột NOT NULL, khóa chính, unique index (kèm cờ partial), FK. Kiểu cột không so (cố ý khác).
- `newPostgresDBForTest` dùng `MESH_TEST_POSTGRES_DSN` khi có (mỗi test một database, xóa sau); `MESH_TEST_REQUIRE_POSTGRES` biến skip thành fail.
- `.github/workflows/ci.yml` job `test-race`: bước mới dựng PostgreSQL tạm từ binary có sẵn trên runner (`initdb`/`pg_ctl`, unix socket, `trust`, không mật khẩu, không secret), đặt hai biến trên. Đã chạy mô phỏng bước này trên mc-ci (đọc `GITHUB_ENV` theo dòng như GitHub) và `actionlint` = 0. **Chưa chạy trên GitHub** (không push `origin`); giả định: runner `ubuntu-latest` có `/usr/lib/postgresql/<ver>/bin` (postgrestest vốn dựa vào điều này). Nếu thiếu, bước fail rõ ràng, không skip.

## 7. Lint, vet, race (commit `2407657`, mã nguồn = HEAD trừ file evidence này)

| Lệnh | Exit / kết quả |
|---|---|
| `go vet ./...`; `go vet -tags p05cluster ./hscontrol/servertest/` | 0; 0 |
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --max-issues-per-linter 0 --max-same-issues 0 ./hscontrol/db/... ./hscontrol/state/... ./cmd/headscale/...` | 0, `0 issues.` |
| như trên với `./hscontrol/servertest/...` (không tag, như CI) | 0, `0 issues.` |
| như trên với `--build-tags p05cluster ./hscontrol/servertest/...` | 64 issue, **0** trong file của nhóm A (`cluster_p12c_policy_test.go`); 64 issue có sẵn trong các file p05cluster khác (CI không lint với tag này) |
| `gofumpt -l` trên các file đã sửa | rỗng |
| `actionlint .github/workflows/ci.yml` | 0 |
| `CGO_ENABLED=1 MESH_TEST_POSTGRES_DSN=<pg> MESH_TEST_REQUIRE_POSTGRES=1 go test -race -count=1 -p 4 ./hscontrol/db/... ./hscontrol/state/ ./cmd/headscale/...` | 0 (db 171 s, tenantscope, sqliteconfig, state 340 s, cmd/headscale, cli) |
| `nix shell … tscli opentofu -c go test -race -tags p05cluster -count=1 -run '^(TestClusterEmptyPolicyReloadDenies\|TestClusterLoadsNewTailnetLive\|TestClusterP06.*\|TestClusterEpochBumpReloadsRuntime\|TestClusterOwnershipMoveRetiresRuntime\|TestClusterSuspendRetiresRuntime\|TestClusterQuota.*)$' ./hscontrol/servertest/` | 0, 18 PASS, không DATA RACE |
| `nix shell … -c go test -count=1 -timeout 55m ./hscontrol/servertest/` (toàn bộ servertest không tag, như CI) | 0 (`ok … hscontrol/servertest 1963.5s`) |

Không chạy toàn bộ `p05cluster` (coordinator chạy khi tích hợp).

## 8. Benchmark (hot path: plugin chạy trên mọi câu lệnh)

`BenchmarkScopedStatements` (`hscontrol/db/tenantscope/scope_bench_test.go`), SQLite, mc-ci (16 vCPU, load ~1), `go test -run XXX -bench BenchmarkScopedStatements -benchtime=2s -count=3`, hai vòng xen kẽ. "Trước" = `1ffffc1` cộng đúng file benchmark (snapshot `3915a5f`, không commit vào nhánh); "sau" = `2407657`. Trung bình 6 lần:

| Câu lệnh | Trước ns/op | Sau ns/op | Δ | allocs/op trước → sau |
|---|---|---|---|---|
| `Where("id = ?").First` | 76 393 | 78 987 | +3.4 % | 73 → 84 |
| `Where.Where.Find` | 84 364 | 87 216 | +3.4 % | 94 → 104 |
| `Model.Where.Update("name")` | 134 893 | 142 327 | +5.5 % | 106 → 110 |

Chi phí đến từ việc dựng văn bản các mệnh đề để kiểm bảng tenant (A2) và nhóm WHERE (A1); vài µs mỗi câu so với độ trễ một round-trip PostgreSQL thật (hàng trăm µs đến ms). Chưa đo trên PostgreSQL/PgBouncer của lab.

## 9. Known limitations / security blockers

- **Không có BLOCKER**: không tìm thấy đường xuyên tenant khai thác được với code hiện tại. Các đường RED ở §1 đều là "tiềm ẩn" (code hiện tại không gọi các dạng đó), trừ hai subquery `FROM nodes` trong `preauth_keys.go` (đọc không scope nhưng không lộ dữ liệu nhờ FK kép `(tailnet_id, auth_key_id)`), đã sửa trong A2.
- A2 kiểm chuỗi điều kiện/select/order/group chỉ trên handle tenant; handle platform (operator, placement) vẫn được dùng subquery tương quan đã review. Nhận diện bảng là từ vựng: alias hoặc cột trùng tên bảng tenant bị từ chối nhầm (fail closed).
- Subquery thô truyền làm biến (`Where("x IN (?)", db.Raw("SELECT … FROM nodes"))`) bị A2 phát hiện ở runtime trên handle tenant; `db.Raw` đó cũng bị guard A4 bắt tĩnh.
- Hook `BeforeUpdate` của model đổi `TailnetID` sau khi plugin kiểm sẽ không bị A3 thấy; trigger A8 vẫn chặn ở DB. Không có hook nào như vậy.
- L10 phần còn lại (`.After` chuỗi chỉ giữ cái cuối; `createDefaultTailnet` không qua PENDING) không làm trong nhóm A.
- Bước CI PostgreSQL chưa chạy trên GitHub (xem §6).

## 10. Reviewer approval

Chưa.
