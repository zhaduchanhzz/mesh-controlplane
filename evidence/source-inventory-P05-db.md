# Source Inventory — P05 (phần DB): `db.Platform`, unique `(machine_key, node_key)`, lỗi kết nối, chế độ query PostgreSQL

- **Trạng thái:** `Implemented`, `Verified` (unit + race, SQLite và PostgreSQL thật qua `postgrestest`). Chưa `Reviewed`.
- **Nhánh:** `agent/p05-db`, tách từ `feat/P05-multitenant-runtime` @ `ce75c1b`.
- **Hợp đồng:** ADR 0007, mục "Hợp đồng code". Liên quan: ADR 0005, ADR 0006, P03b, P14 §3.5.

## 1. Path/symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `design/adr/0007-…md` | Hợp đồng code, §1, §2 | `db.Platform`, `ErrUnknownTenant`, unique `(machine_key, node_key)` |
| `design/adr/0006-pgbouncer-connection-pooling.md` | §3.5 | transaction mode, `max_prepared_statements` |
| `evidence/P14-db-benchmarks.md` §3.5, §5 | | `max_prepared_statements=0` báo sai "not a v0.1 database"; mỗi `NewHeadscaleDatabase` mở một pool |
| `evidence/source-inventory-P03b.md` | §2.2, §5 | plugin `tenantscope`, `Raw`/`Exec` không qua plugin; cùng machine key nhiều user trong một tailnet |
| `hscontrol/db/db.go` | `NewHeadscaleDatabase`, `openDB`, `runMigrations`, `validateSQLiteSchema`, `HSDatabase`, `Close` | |
| `hscontrol/db/tenant.go` | `createSchema`, `checkV01Database`, `bindRuntimeTailnet`, `resolveRuntimeTailnet`, `createDefaultTailnet` | |
| `hscontrol/db/tenantscope/tenantscope.go` | `Register`, `tenantField`, `scopeWhere`, `stampCreate` | callback Query/Row/Update/Delete/Create; không có callback Raw |
| `hscontrol/db/preauth_keys.go` | `findAuthKey`, `parsePrefixedKey`, `legacyAuthKeyIdentifier` | |
| `hscontrol/db/secret.go` | `authenticateCredential`, `verifySecret` | identifier lạ trả về sớm, không tính hash (giống upstream) |
| `hscontrol/db/text_serialiser.go` | `TextSerialiser.Value` | key 0 được lưu là `mkey:000…`/`nodekey:000…`, không phải NULL |
| `hscontrol/db/schema.sql`, `schema_postgres.sql` | `nodes`, index | |
| `hscontrol/db/AGENTS.md` | quy tắc migration | |
| `hscontrol/db/versioncheck.go` | `checkVersionUpgradePath` | |
| `internal/tailnet/tailnet.go` | `ID`, `Scope`, `NewScope`, `WithScope` | |
| `hscontrol/types/config.go`, `config_values.go`, `hscontrol/config/default.go` | `PostgresConfig.DSN`, `setEnum`, `Allowed()` | mẫu enum của config |
| `tools/configgen` | `go generate ./hscontrol/types/` | sinh `config_docs_generated.go`, `docs/ref/*`, `nix/options.generated.nix` |

## 2. Thiết kế

### 2.1 `db.Platform` (`hscontrol/db/platform.go`)
- `OpenPlatform(cfg)`: mở DB, `checkV01Database`, `checkVersionUpgradePath`, migration v0.1, lưu version, squibble (SQLite), rồi **đăng ký `tenantscope` một lần** trên pool. **Không tạo tailnet.**
- `NewHeadscaleDatabase(cfg)` = `OpenPlatform` + `bindRuntimeTailnet` (giữ hành vi một tenant: DB mới có một tailnet mặc định). Handle này sở hữu pool.
- `TailnetForAuthKey(key)`: `parseAuthKey` (tách từ `findAuthKey`, dùng chung) → `SELECT tailnet_id, hash FROM credentials WHERE kind='authkey' AND identifier=?` → `verifySecret`. Key không parse được, identifier lạ, secret sai, hash rỗng (đã revoke) đều trả **`ErrUnknownTenant`**. Lỗi DB trả lỗi khác (không phải thông tin về tenant). Thời gian: identifier lạ trả về sớm, giống `authenticateCredential` upstream. Không nâng cấp hash legacy (runtime sẽ làm khi xác thực lại). Hết hạn / đã dùng / revoke (còn hash) để runtime báo như upstream.
- `TailnetForNode(mk, nk)`: key 0 → `ErrUnknownTenant`; `SELECT tailnet_id FROM nodes WHERE machine_key=? AND node_key=? LIMIT 2`; khác đúng 1 dòng → `ErrUnknownTenant`.
- `OwnedTailnets(clusterID)`: `ACTIVE`, `deleted_at IS NULL`, đúng owner, sắp theo `created_at, id`. `clusterID` rỗng → lỗi.
- `TenantDB(id)`: ID 0 / không có / đã xóa → `ErrUnknownTenant`; cluster khác → `ErrTailnetNotOwned`; không ACTIVE → `ErrTailnetNotActive` (mới). Handle dùng **cùng `*gorm.DB`/`*sql.DB`** (`WithContext(scope)`, scope mang `owner_epoch`), `Close()` của handle này là no-op.
- `Ping(ctx)` (coordinator yêu cầu, cho `/health` của cluster), `Close()`, `ClusterID()`.
- **Review tay SQL thô (P03b §4):** các câu `Raw` trên bảng tenant chỉ nằm trong `platform.go`, chỉ đọc `tailnet_id` (và `hash` để xác minh), không trả dữ liệu tenant. Migration dùng `Raw`/`Exec` chỉ cho DDL và đếm trùng.

### 2.2 Migration `202611230000-nodes-key-pair-one-tailnet` (lệch ADR, xem §4.4)
- Nối sau `202611220000-v0.1-init` (không sửa entry init). Bản đầu là **unique index toàn cục** `(machine_key, node_key)` (commit `adb4712`). Full suite trên mc-ci làm hỏng 3 test upstream: `TestAuthenticationFlows/reauth_existing_node_different_user_auth_key`, `…/interactive_reauth_existing_node_different_user_creates_new_node`, `TestPreAuthKeyLogoutAndReloginDifferentUser` (`saving node: duplicated key not allowed`): upstream, khi node đăng nhập lại thành **user khác trong cùng tailnet** với cùng machine key + node key, tạo node mới và **giữ node cũ**.
- Thiết kế cuối (commit `3a916a4`; migration đổi tên vì chưa rời nhánh này, không DB nào đã chạy nó): index thường `idx_nodes_machine_key_node_key` để lookup, cộng **trigger** từ chối một cặp đã có ở **tailnet khác** (INSERT và UPDATE `machine_key`/`node_key`/`tailnet_id`). PostgreSQL: hàm `nodes_key_pair_tailnet_check()` lấy `pg_advisory_xact_lock(hashtextextended(mk||'/'||nk,0))` rồi kiểm, nên hai transaction đồng thời không cùng lọt (READ COMMITTED: mỗi câu trong hàm plpgsql volatile lấy snapshot mới); lỗi `ERRCODE unique_violation`. SQLite: hai trigger `RAISE(ABORT)` (SQLite một writer).
- Migration trong `tx.Transaction`: đếm cặp đã nằm ở > 1 tailnet; có → `errDuplicateNodeKeys`, DB không đổi (thử lại được). Rollback xóa trigger/hàm/index.
- Key 0 (`mkey:000…`/`nodekey:000…`, cách `TextSerialiser` lưu node chưa có key) được miễn: nhiều test tạo node không key; Platform từ chối key 0 nên các dòng này không bao giờ dùng để định tuyến.
- `TailnetForNode` dùng `SELECT DISTINCT tailnet_id … LIMIT 2`, khác đúng 1 → `ErrUnknownTenant`.
- `schema.sql`/`schema_postgres.sql` chứa đúng DDL đó (`TestNodeKeyPairDDLInSchemaFiles`); squibble xác nhận DB SQLite đã migrate khớp DB mới.
- `checkV01Database` lấy danh sách ID biết được từ `v01Migrations()`.

### 2.3 Lỗi kết nối trong kiểm tra v0.1
- `tableExists(db, table)` truy vấn `information_schema.tables` (PostgreSQL, `current_schema()`, `BASE TABLE`) hoặc `sqlite_master`, trả lỗi bọc `ErrDatabaseUnavailable` (mới, exported). `checkV01Database` và `createSchema` dùng hàm này thay cho `Migrator().HasTable` (nuốt lỗi).

### 2.4 `database.postgres.query_exec_mode`
- Kiểu enum `types.PostgresQueryExecMode`: `cache_statement` (mặc định trong `default.go`) | `cache_describe` | `describe_exec` | `exec` | `simple_protocol`, `UnmarshalText` qua `setEnum` (giá trị lạ bị từ chối khi load). `DSN()` thêm `default_query_exec_mode=<mode>` khi khác rỗng (pgx v5 đọc tham số này).
- Sinh lại bằng `go generate ./hscontrol/types/`: `config_docs_generated.go`, `docs/ref/config-example.yaml`, `docs/ref/configuration-options.md`, `nix/options.generated.nix`. Không sửa tay file sinh. Ví dụ (`example`) là `simple_protocol` vì `TestExamples` cấm ví dụ trùng mặc định.

## 3. Test (RED → GREEN)

| Bước | Lệnh | Exit | Kết quả |
|---|---|---|---|
| RED Platform/migration (Windows) | `go test ./hscontrol/db/ -run 'Platform\|Migration\|NodesMachine\|CheckV01' -count=1` | 1 | build fail: `undefined: Platform`, `OpenPlatform`, … |
| RED unique (Windows, đã có Platform + migration, chưa sửa schema) | như trên | 1 | `TestNodesMachineNodeKeyGloballyUnique` (tên cũ): "An error is expected but got nil" (cùng cặp ở 2 tailnet được chấp nhận); `TestMigrationFromV01Init` fail |
| RED thiết kế unique index (full suite mc-ci, commit `adb4712`) | `go test -count=1 -timeout 120m ./...` | (dừng sớm) | `hscontrol` FAIL: 3 test re-login user khác, `duplicated key not allowed` → đổi sang trigger |
| GREEN 3 test trên (commit `3a916a4`) | `go test -count=1 -run "TestAuthenticationFlows\|TestPreAuthKeyLogoutAndReloginDifferentUser" ./hscontrol/` | 0 | ok 56 s |
| RED checkV01 (worktree tạm @ `ce75c1b`, chỉ thêm test) | `go test ./hscontrol/db/ -run CheckV01 -count=1` | 1 | "An error is expected but got nil": kết nối đã đóng bị coi là **DB mới** |
| RED config (worktree tạm @ `ce75c1b` + `load_test.go` mới) | `go test ./hscontrol/config/ -run TestPostgresQueryExecMode` | 1 | build fail: `QueryExecMode undefined` |
| GREEN (Windows, SQLite; PostgreSQL skip vì không có `pg_ctl`) | `go test ./hscontrol/db/ -run '…' -count=1 -v` | 0 | mọi subtest sqlite PASS |
| GREEN mc-ci, race | `CGO_ENABLED=1 go test -count=1 -race -v ./hscontrol/db/... ./hscontrol/types/... ./hscontrol/config/...` | 0 | 7 package ok. **Subtest PostgreSQL chạy thật** (PostgreSQL 16, `postgrestest`): `TestPlatform*/postgres`, `TestNodeKeyPairOneTailnet/postgres`, `TestMigrationFromV01Init/postgres`, `TestCheckV01DatabaseSurfacesConnectionErrors/postgres`, `TestPlatformPostgresSimpleProtocol` đều PASS. Skip duy nhất có chữ postgres là fixture config `TestRoundTrip/hsic-postgres` (không liên quan) |
| Full suite mc-ci | `go test -count=1 -timeout 120m ./...` | xem §3.1 | |

Windows: các package `types`/`config`/`db` còn fail do đường dẫn `\`, CRLF và khóa file tạm của Windows (`TestBadConfigs`, `TestConfigCompat`, `TestResolveSecret`, "The process cannot access the file…"), không liên quan thay đổi này; trên Linux tất cả xanh. `TestConfigFields` (danh sách key) và `TestExamples` đã được cập nhật/sửa.

### Test mới
- `platform_test.go`: `TestPlatformTailnetForAuthKey` (2 tailnet; secret sai, identifier lạ, rỗng, rác, key legacy lạ → `ErrUnknownTenant`), `TestPlatformTailnetForNode` (cùng mk, 2 nk ở A/B; mk khác, nk lạ, key 0 → `ErrUnknownTenant`), `TestPlatformOwnedTailnets` (PENDING/SUSPENDED/đã xóa/cluster khác bị loại; DB mới không có tailnet), `TestPlatformTenantDB` (cùng `*sql.DB`; cô lập A/B; `Close` handle không đóng pool; `Ping`; PENDING/SUSPENDED → `ErrTailnetNotActive`, đã xóa/lạ/0 → `ErrUnknownTenant`, cluster khác → `ErrTailnetNotOwned`), `TestPlatformSeesSingleTenantDatabase`, `TestPlatformPostgresSimpleProtocol`.
- `migration_test.go`: `TestNodeKeyPairOneTailnet` (cùng cặp ở tailnet khác: INSERT và UPDATE bị từ chối; cùng cặp cho user khác trong cùng tailnet: cho phép; cùng mk khác nk: cho phép; key 0 miễn), `TestMigrationFromV01Init` (đưa DB về trạng thái sau `v0.1-init` bằng Rollback của migration + xóa bản ghi, mở lại → index + trigger có, squibble khớp; cặp đã ở 2 tailnet → `errDuplicateNodeKeys`, không bản ghi, không index, dữ liệu giữ nguyên), `TestCheckV01DatabaseSurfacesConnectionErrors`, `TestNodeKeyPairDDLInSchemaFiles`.
- `hscontrol/config/load_test.go`: `TestPostgresQueryExecMode`; `hscontrol/types/config_test.go`: ca DSN mới.

### 3.1 Full suite mc-ci
- Lần 1 (commit `adb4712`): `hscontrol` FAIL 3 test re-login (§2.2); bị dừng sau khi đã thấy lỗi. Các package xong trước đó đều ok.
- Lần 2 (commit `3a916a4`): đang chạy nền trên mc-ci (`~/src/p05db-logs/full2.log`, mã thoát ở `full2.exit`). **Chưa có kết quả lúc bàn giao.**

## 4. Lệch so với ADR 0007 / giả định cần reviewer chốt
1. **Lỗi mới `ErrTailnetNotActive`** (ADR chỉ nêu `ErrUnknownTenant`). Tailnet đã xóa được gộp vào `ErrUnknownTenant`.
2. **Lookup không lọc owner/ACTIVE:** `TailnetForAuthKey`/`TailnetForNode` trả ID tailnet bất kể cluster; kiểm tra owner/ACTIVE nằm ở `TenantDB` (và bảng runtime của Cluster). Cluster phải coi `ErrUnknownTenant`/`ErrTailnetNotOwned`/`ErrTailnetNotActive` là 401 như nhau để không lộ tenant của cluster khác.
3. Key 0 được miễn khỏi ràng buộc (lý do §2.2).
4. **Không dùng unique index toàn cục như ADR viết.** ADR §2 ghi "Unique toàn hệ thống trên `(machine_key, node_key)`"; điều đó phá hành vi upstream (re-login user khác trong một tailnet giữ cặp key, §2.2). Ràng buộc thực thi là "**một cặp chỉ thuộc một tailnet**" bằng trigger + advisory lock, đủ cho định tuyến không mơ hồ. **Cần reviewer chốt và sửa câu trong ADR 0007.** Ghi chú an ninh: machine key do Noise xác thực, nên chỉ chính thiết bị mới tạo được cặp của nó; ràng buộc chủ yếu chặn lỗi logic.
5. Không nâng cấp hash legacy trong `TailnetForAuthKey` (runtime làm).
6. `ErrDatabaseUnavailable` mới, exported.
7. Pool một tenant: handle từ `NewHeadscaleDatabase` vẫn sở hữu pool riêng (đường `headscale serve`), đúng ADR §3.
