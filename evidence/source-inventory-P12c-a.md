# Source Inventory — P12c nhóm A (cô lập dữ liệu và policy)

- **Ticket:** `tickets/P12c.md` nhóm A (A1–A8), phát hiện trong `evidence/REVIEW-2026-11-26-tickets.md` §"Nhóm A" (+ L10 phần `hasPrimaryKey`/`stampOne`).
- **Nhánh:** `feat/P12c-a`, tạo từ `feat/P12c-review-hardening` tại commit `1ffffc1ec8da17c9e2f81da3af45d33ce871f3b1` (= `master` `dece058` + review + ticket).
- **Trạng thái:** `Planned` → đang làm. Inventory này viết **trước** khi sửa code (AGENTS rule 5).
- **Môi trường test:** mc-ci (Go 1.27.4, PostgreSQL 16.15 tạm trên unix socket qua `initdb`, `MESH_TEST_POSTGRES_DSN`; `postgrestest` tự tìm `/usr/lib/postgresql/16/bin`). Lint: `golangci-lint` từ `flake.lock`.

## 1. Path/symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `AGENTS.md` | rule 5, 6, 7, 8, 10, 11, 13 | |
| `hscontrol/db/AGENTS.md` | migration rules 1–5 | nối cuối, không tắt FK, không AutoMigrate, `tx.Transaction` |
| `design/adr/0005-…md` | "Bổ sung 2 §3" | cô lập chỉ ở tầng ứng dụng, không RLS; SQL thô review tay + test/lint allowlist có lý do |
| `hscontrol/db/tenantscope/tenantscope.go` | `Register`, `tenantField`, `scopeOf`, `whereTailnet`, `scopeWhere` (:113-129), `scopeMutation` (:135-152), `hasPrimaryKey` (:154-175), `stampCreate`/`stampOne`/`stampMap`, `confineUpsert`, `LockOwnership`, `checkFence`, `checkLease` | `scopeWhere` dùng `AddClause(clause.Where{…})` → GORM nối vào WHERE có sẵn không bọc ngoặc (A1); thoát sớm khi `Statement.Schema` nil hoặc không có `TailnetID` (A2); mutation chỉ thêm WHERE, không xét giá trị gán (A3); slice có phần tử khóa 0 được coi là có khóa (L10) |
| `hscontrol/db/tenantscope/tenantscope_test.go`, `fence_test.go`, `lease_fence_test.go`, `fence_bench_test.go` | `openDB`, `dialector` (`MESH_TEST_POSTGRES_DSN`, schema riêng mỗi test), model `owner`/`item`/`global`/`tailnetRow` | Harness cho test RED A1–A3 |
| `gorm.io/gorm@v1.31.2` | `chainable_api.go` `Table` (:64-86, `tableRegexp`), `joins` (:258-277), `statement.go` `join`, `callbacks/query.go` `BuildQuerySQL` (join theo association vs `NamedExpr`), `clause/where.go` `Where.Build`, `buildExprs` | Xác nhận: `Table("nodes n")` đặt `Statement.Table = "n"` và `TableExpr`; join chuỗi không phải association thành `clause.NamedExpr` nguyên văn; `buildExprs` chỉ bọc ngoặc khi chuỗi chứa `" AND "`/`" OR "` |
| `hscontrol/db/tenant.go` | `createSchema` (:46), `tableExists`, `migrationIDs` (`Table("migrations")` :144), `createDefaultTailnet`, `ScopedContext` (:302-325) | `ScopedContext` trả `parent` khi `WithScope` lỗi (A7); chỉ dùng trong `db_test.go:54`, `tenant_test.go:264` |
| `internal/tailnet/tailnet.go`, `fence.go` | `WithScope` (`ErrScopeConflict`), `FromContext`, `WithFence`, `FenceFromContext` | |
| `hscontrol/db/platform.go` | `v01Migrations` (:46-62), `OpenPlatform`, `Platform` | Chuỗi v0.1: init, node key pair, registry, moving, node IP, tailnet settings, cluster leases, tailnet moves |
| `hscontrol/db/readonly.go` | `LatestMigrationID` (phần tử cuối của `v01Migrations`), `OpenReadOnly` | mesh-bootstrap đòi migration mới nhất: thêm migration thì bootstrap chỉ mở được DB sau khi một cluster/CLI đã migrate |
| `hscontrol/db/tailnet_moves.go`, `cluster_leases.go`, `nodekeys.go`, `nodeips.go`, `tailnet_settings.go` | migration `Migrate`/`Rollback`, DDL hằng nối nguyên văn vào schema | Mẫu cho migration A8 |
| `hscontrol/db/schema.sql`, `schema_postgres.sql` | toàn bộ | Bảng có `tailnet_id`: `users`, `credentials`, `nodes`, `policies`, `tailnet_settings` (tenant), `tailnet_moves` (bảng platform). Trigger tenant hiện có: chỉ `credentials`→`users` và cặp khóa node. `schema_postgres.sql:2-3` nhắc `schema_test.go` không tồn tại |
| `hscontrol/db/db.go` | `runMigrations`, `validateSQLiteSchema` (squibble so `schema.sql`) | DB SQLite sau migrate phải khớp `schema.sql` từng digest |
| `hscontrol/db/migration_test.go` | `v01InitState`, `TestMigrationFromV01Init`, `TestNodeKeyPairDDLInSchemaFiles` | Mẫu test nâng DB cũ và DDL-trong-schema |
| `hscontrol/db/tenant_test.go`, `platform_test.go`, `suite_test.go` | `backends`, `postgresConfig`, `newPostgresDBForTest` (`postgrestest`, skip khi không có binary), `openPlatform`, `platformTailnet`, `tenantDB`, `addNode` | |
| `hscontrol/db/raw_write_guard_test.go` | `tenantWriteRE`, `rawTenantWriteAllowlist` (theo file), `TestNoRawTenantWritesOutsideFence` | Chỉ `Exec`/`Raw`/`ExecContext`, chỉ literal, chỉ ghi (A4) |
| `hscontrol/db/policy.go` | `SetPolicy`, `GetPolicy`, `PolicyBytes` (:66-99: chuỗi rỗng → `nil`) | |
| `hscontrol/state/state.go` | `ReloadPolicy` (:650), `reloadPolicyLocked` (:672-684 `if pol != nil`), `policyOptions`, `ValidatePolicy` (:2013) | A6 |
| `hscontrol/policy/v2/policy.go` | `SetPolicy` (:761: rỗng + `emptyDenies` → `{}`), `WithEmptyPolicyDenies`; `test.go` `ValidatePolicy` (rỗng → `ErrPolicyEmpty`) | |
| `hscontrol/cluster.go` | reconcile (:446-456, `ReloadPolicy` theo `policy_revision`), `PolicyEmptyDenies = true` (:176) | **Không sửa** (file của nhóm B/C); sửa ở `state.go` |
| `cmd/headscale/cli/cluster.go` | `tailnetPolicyCmd` (:207-239), `clusterConfig` | Lưu file policy không validate (A6) |
| `cmd/headscale/cli/policy.go` | `validateLocalPolicy` | Mẫu validate với users/nodes của DB |
| `hscontrol/servertest/cluster_p10.go`, `cluster_p10_test.go`, `cluster_p06_test.go` | `SetPolicyViaDB`, `NewTestCluster`, `enroll`, `waitPeersExactly`, `allowAll` (tag `p05cluster`) | Harness cho servertest A6 |
| `.github/workflows/ci.yml` | job `test-race` | Không đặt `MESH_TEST_POSTGRES_DSN`: test PG của `tenantscope`, `lease` bị skip trên CI (A5) |
| `zombiezen.com/go/postgrestest@v1.0.1` | `Start`, `command`/`findPostgresBin` | Tìm `pg_ctl`/`initdb` trên PATH rồi `/usr/lib/postgresql/<max>/bin` |

## 2. Ràng buộc đang có

- AGENTS rule 6/13: thiếu scope thì fail closed; lỗi isolation chặn merge.
- Rule 7: thêm ràng buộc `tailnet_id` phải xét schema, unique, FK, trigger, cả hai DB.
- Rule 8: tenant tailnet policy rỗng = DENY; không fallback allow-all.
- Rule 10: không sửa `gen/` (A không đụng API).
- Rule 11 + `hscontrol/db/AGENTS.md`: **một** migration mới nối cuối chuỗi v0.1, trong `tx.Transaction`, DDL viết tay, có Rollback, không AutoMigrate, không tắt FK; DB mới nhận hiệu ứng từ `schema.sql`/`schema_postgres.sql` (InitSchema ghi mọi ID), nên DDL phải có trong file schema; SQLite được squibble so khớp.
- Ticket P12c: chỉ nhóm A thêm migration. Không đụng file nhóm B (`checkLease`, timeout fence, `internal/lease/`, cluster.go load/start/onLeaseAcquired/health/ready, `app.go servable`, `noise.go`, `session_route.go`, `cluster_apiv2.go`, `infra/lab/db/`) và nhóm C (`internal/placement/`, `hscontrol/db/handover.go`, hand-over trong cluster.go).
- Trigger `tailnets_owner_epoch_monotonic` cho thấy mẫu trigger từ chối trên cả hai DB (SQLite `RAISE(ABORT)`, PostgreSQL hàm plpgsql + `ERRCODE`).

## 3. Thiết kế dự kiến

- **A1:** `scopeWhere` thay WHERE có sẵn bằng `tenant = ? AND (<toàn bộ WHERE cũ>)` (biểu thức nhóm tự bọc ngoặc, vị từ tenant đứng trước nên một chú thích `--` cuối chuỗi không nuốt được nó mà chỉ gây lỗi cú pháp). Kiểm SQL sinh ra bằng `DryRun` trên SQLite và PostgreSQL.
- **A2:** danh sách bảng tenant cố định trong plugin (`users`, `nodes`, `credentials`, `policies`, `tailnet_settings`; test thêm bảng của mình qua `Register(db, extra…)`). Câu lệnh không phải SQL thô mà `Table()`/`TableExpr`/join chuỗi/`clause.From`/chuỗi điều kiện nêu bảng tenant ở vị trí bảng, hoặc đích không có trường `TailnetID` mà bảng chính là bảng tenant → `ErrUnscopedTable`. Join theo association (`Joins("AuthKey")`) giữ nguyên khi bảng chính có scope. SQL thô (`Raw`/`Exec`) vẫn thuộc guard A4.
- **A3:** trước UPDATE: giá trị gán `tailnet_id` (map, struct, `Select("*")`/`Save`) phải bằng scope; khác → `ErrForeignTenant`; `Save` với `TailnetID` rỗng được đóng dấu scope như Create. **L10:** slice chỉ được coi là có khóa khi mọi phần tử có khóa chính khác 0; `[]map` khi Create đi qua `stampMap`.
- **A8:** migration `202611261200-tenant-id-immutable`: trigger `BEFORE UPDATE OF tailnet_id` trên 5 bảng tenant từ chối `NEW.tailnet_id` khác `OLD.tailnet_id` (SQLite `RAISE(ABORT)`; PostgreSQL một hàm plpgsql `ERRCODE = 'check_violation'`). DDL hằng, nối nguyên văn vào hai file schema; Rollback xóa trigger/hàm.
- **A4:** guard mới duyệt AST mọi `Raw`/`Exec`/`ExecContext`/`Query`/`QueryContext`/`QueryRow`/`QueryRowContext`; phân giải hằng (literal, const package/hàm, const package khác trong module, ghép `+`, biến cục bộ mà mọi phép gán đều phân giải được); bất kỳ nhắc tới bảng tenant ở vị trí định danh (đọc hay ghi, mọi cú pháp) → phải có trong allowlist theo câu (file + SHA-256 của SQL chuẩn hóa + lý do); SQL không phân giải được trong package import `gorm.io/gorm` hoặc `database/sql` → cũng phải có trong allowlist.
- **A5:** `schema_test.go` introspect (SQLite `pragma_table_info`/`pragma_foreign_key_list`/`pragma_index_list`; PostgreSQL `information_schema`/`pg_catalog`), so tập bảng, cột, NOT NULL, unique, FK giữa hai DB; mọi bảng có `tailnet_id` thì NOT NULL + FK tới `tailnets(id)`; danh sách bảng tenant của plugin = bảng có `tailnet_id` trừ `tailnet_moves`. CI: service PostgreSQL (mật khẩu ném đi), `MESH_TEST_POSTGRES_DSN`, cờ bắt buộc PG để test PG không skip âm thầm.
- **A6:** `reloadPolicyLocked` gọi `SetPolicy(nil)` khi không có policy và `PolicyEmptyDenies` bật (deny ngay). CLI `tailnet policy` validate (`policy.ValidatePolicy` với users/nodes của tailnet, `WithEmptyPolicyDenies`), từ chối nội dung rỗng trừ khi có `--empty`.
- **A7:** `ScopedContext` trả `(context.Context, error)`; context đã gắn tailnet khác → `ErrScopeConflict`.

## 4. Giả định còn mở

- GORM v1.31.2 là bản trong `go.mod`; hành vi `Table`/`Joins`/`Where.Build` đã đọc từ mã nguồn module cache trên mc-ci, sẽ xác nhận bằng `DryRun` trong test.
- Phát hiện bảng tenant trong chuỗi SQL ở runtime (A2) là so khớp từ vựng: tên bảng tenant đứng sau `FROM`/`JOIN`/`UPDATE`/`INTO`/dấu phẩy… Một chuỗi dùng bí danh cột trùng tên bảng tenant có thể bị từ chối nhầm (fail closed). Sẽ chạy toàn bộ test package chạm DB để chắc đường hợp lệ không gãy.
- GitHub runner `ubuntu-latest` có PostgreSQL 16 cài sẵn ở `/usr/lib/postgresql/16/bin` (postgrestest dùng được); job CI mới không chạy được từ máy này (không push `origin`) — chỉ kiểm cú pháp workflow và chạy lệnh tương đương trên mc-ci.
- Không có lời gọi `.Or(`/`.Not(` hay ghi `tailnet_id` trong code hiện tại (review); nếu thấy thì là BLOCKER và dừng.
