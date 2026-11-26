# PR P13d + P13c (code) Evidence — migration là bước riêng + tương thích schema; epoch fence

- **Trạng thái:** P13d code `Verified` (test trong process); P13c code `Verified` (test trong process). Drill lab (canary c2, F08, pgBackRest) **chưa làm** (`Not started`): ngoài phạm vi nhánh này. Chờ người duyệt.
- **Nhánh:** `feat/P13dc-migrate-fence` (từ `feat/P13-plan` `c4533aa`, chứa P12c). Chỉ push `mcci`; không push `origin`, không merge.
- **Môi trường test:** mc-ci, Go 1.27.4, `GOFLAGS=-buildvcs=false`; PostgreSQL 16 tạm (`initdb`, unix socket, `trust`, port 55437, chỉ cho nhánh này) qua `MESH_TEST_POSTGRES_DSN` + `MESH_TEST_REQUIRE_POSTGRES=1`; SQLite chạy song song trong mọi test `backends(t)` của `hscontrol/db`; `internal/lease` chạy một lần không DSN (SQLite) và một lần có DSN (PG). servertest `-tags p05cluster` chỉ chạy tập con (không chạy cả suite).

## Repo commit before / after
- Before: `c4533aa`. Inventory: `802cad4` (`evidence/source-inventory-P13dc.md`).
- After: xem `git log c4533aa..feat/P13dc-migrate-fence` (bảng dưới).

## Verified source paths & symbols
Xem `evidence/source-inventory-P13dc.md` §2. Thay đổi chính:
- `hscontrol/db/migrate.go` (mới): `Migrate`, `MigrateAndOpenPlatform`, `migrateConn` (một transaction, `pg_advisory_xact_lock(hashtextextended('mesh/schema-migrate',0))`, SQLite `BEGIN IMMEDIATE`), `verifySchema`, `chain`.
- `hscontrol/db/schema_compat.go` (mới): migration `202611271000-schema-compat`, `migrationCompat`, `checkSchemaCompat`, `recordSchemaCompat`.
- `hscontrol/db/epoch_fence.go` (mới): migration `202611271100-epoch-fence`, `EpochFloor`, `Platform.EpochFence`.
- `hscontrol/db/platform.go` `OpenPlatform`/`openPlatformChain` (chỉ kiểm, không DDL), `readonly.go` `OpenReadOnly` (cùng quy tắc), `tenant.go` `checkV01Database` (trả `fresh`, bỏ kiểm migration lạ), `versioncheck.go` `compareStoredVersion`, `db.go` `NewHeadscaleDatabase` (dùng `MigrateAndOpenPlatform`), schema files.
- `internal/lease/lease.go` `AcquireAbove`, `get`, `PostgresTakeoverLock`; `internal/lease/epoch.go` `ErrEpochRegression`, `HeldError`; `internal/lease/manager.go` HWM.
- `hscontrol/cluster.go`: `ownerEpochs`, `ownerEpochRegressed`, kiểm trong `load`, `reconcile`, ghi HWM ở `handOver`, `takeOverOrphans`.
- CLI: `headscale cluster migrate`, `headscale cluster epoch-fence`, `mesh-placement migrate`, `mesh-placement epoch-fence`.

## Design choice / ADR
- ADR 0013 (P13d) và ADR 0012 (P13c), không đổi ngữ nghĩa. Chi tiết chọn lựa: inventory §4.
- `schema_compat` của chính migration `202611271000` là **contract** (binary trước P13d từ chối mọi migration lạ, không sửa được); `202611271100-epoch-fence` là **expand** (`min_reader` = `202611271000`).
- `OpenReadOnly` (mesh-bootstrap) dùng cùng quy tắc tương thích, không áp versioncheck (như trước).
- Version (`database_versions`) chỉ do bước migrate ghi; `OpenPlatform` chỉ đọc và so.

## Tests + exact commands + exit codes

Biến chung trên mc-ci: `. ~/p13dc-env.sh` (PATH, `GOFLAGS`, `MESH_TEST_POSTGRES_DSN=<pg tạm>`, `MESH_TEST_REQUIRE_POSTGRES=1`, checkout nhánh).

| Hành vi | Test | RED commit → kết quả | GREEN commit → kết quả |
|---|---|---|---|
| D1 `OpenPlatform` không tạo/migrate schema; DB thiếu migration → `ErrSchemaNeedsMigration`, DB giữ nguyên | `TestOpenPlatformDoesNotMigrate` (sqlite+pg) | `6f8809a`: `go test -count=1 -run '…' ./hscontrol/db/` **exit 1** — "Expected error … but got nil" (cả hai DB) | `6da638e`: cùng lệnh **exit 0** |
| D2 cluster process không migrate khi khởi động | `TestP13dClusterDoesNotMigrateAtStart` (`-tags p05cluster`) | `6f8809a`: `go test -tags p05cluster -count=1 -run TestP13dClusterDoesNotMigrateAtStart ./hscontrol/servertest/` **exit 1** — NewCluster trả nil trên DB trống | `6da638e`: **exit 0** |
| D3 L7: hai `migrate` đồng thời (hai process) → cả hai exit 0, mỗi migration ghi một lần | `TestMigrateTwoProcessesAtOnce` (sqlite+pg; process con = test binary) | `6f8809a`: **exit 1** trên PG — "fresh run A: … duplicate key value violates unique constraint "pg_type_typname_nsp_index""; SQLite **đã pass** ở RED (driver đã `BEGIN IMMEDIATE`, ghi nhận trung thực) | `6da638e`: **exit 0** cả hai |
| D4 canary: binary N đang chạy tiếp và khởi động lại được sau migration expand của N+1 (process khác); sau contract thì N bị từ chối (`ErrSchemaTooNew`), N không migrate DB mới hơn | `TestCanaryOldBinaryAcrossExpandMigration` (sqlite+pg) | `6f8809a`: **exit 1** — `unknown migration "209912310000-p13d-test-expand"` | `6da638e`: **exit 0** |
| D5 migration lạ không có dòng `schema_compat` → `ErrSchemaTooNew` (không còn báo nhầm là DB Headscale) | `TestUnmarkedUnknownMigrationRefused`, `TestRejectsUnknownMigration` | `6f8809a`: **exit 1** — `ErrNotV01Database` | `6da638e`: **exit 0** |
| D6 khai báo expand/contract bắt buộc; DDL trong file schema; nâng từ trước `schema_compat` | `TestSchemaCompatDeclarations`, `TestSchemaCompatMigrationUpgrades` | `6f8809a`: **exit 1** — migration chưa có trong chuỗi | `6da638e`: **exit 0** |
| C1 `DELETE`/`TRUNCATE` `cluster_leases` bị từ chối | `TestClusterLeaseRowsCannotBeDeleted` (sqlite+pg) | `ce89d85`: **exit 1** — "An error is expected but got nil" | `f9d28e7`: **exit 0** |
| C2 epoch floor = giây Unix × 2^20 < 2^53 | `TestEpochFloor` | `ce89d85`: **exit 1** (stub) | `f9d28e7`: **exit 0** |
| C3 fence: mọi epoch ≥ F và > trước; lease hết hạn, holder cũ `ErrLost`, ghi của handle cũ `ErrFenced`; move chờ bị hủy; ghi `platform_epoch_fences`; chạy lại vẫn tăng; HWM ngoài thắng | `TestEpochFenceRaisesEveryEpoch`, `TestEpochFenceAfterSimulatedRestore` (sqlite+pg) | `ce89d85`: **exit 1** (stub "not implemented") | `f9d28e7`: **exit 0** |
| C4 migration fence: nối cuối, expand, DDL trong file schema, nâng từ DB trước nó | `TestEpochFenceMigrationDeclared`, `TestEpochFenceMigrationUpgrades` | `ce89d85`: **exit 1** | `f9d28e7`: **exit 0** |
| C5 lease HWM: không bao giờ nhận epoch ≤ đã thấy (restore mô phỏng), standby chờ tới khi fence; `AcquireAbove` không đổi dòng khi từ chối; standby học epoch của lease đang sống | `TestManagerRefusesEpochRegression`, `TestAcquireAboveRefusesRegression`, `TestManagerStandbySeesHeldEpoch` | `289a6f6`: `env -u MESH_TEST_POSTGRES_DSN go test -count=1 -run '…' ./internal/lease/` **exit 1** và có DSN **exit 1** — "took lease {Cluster:c1 Holder:b Epoch:2} after the restore: epoch 2 granted twice" | `f9d28e7`: cả hai **exit 0** |
| C6 reconcile không nạp lại tailnet có `owner_epoch` lùi; tailnet khác không ảnh hưởng; nạp lại sau fence | `TestP13cReconcileRefusesOwnerEpochRegression` (`-tags p05cluster`) | `ce89d85`: **exit 1** — "runtime of "a" present=false not reached within 10s" (retire rồi nạp lại ngay ở epoch 2) | `f9d28e7`: **exit 0**; log `CRITICAL epoch regression … owner_epoch 2 … high_water_mark 3` |

"Restore" mô phỏng: viết lại cột epoch với trigger đơn điệu bị bỏ qua (PG: `SET LOCAL session_replication_role = replica`, user test là superuser; SQLite: drop/tạo lại trigger) — cùng hiệu ứng lên dòng như restore. Restore thật (pgBackRest, template) là drill F08.

Chạy rộng (không race) trên `d3dd6c5`:
- `go test -count=1 -p 4 ./hscontrol/db/` → **0** (85 s; gồm raw SQL guard, `TestSchemaSQLiteMatchesPostgres`).
- `env -u MESH_TEST_POSTGRES_DSN go test -count=1 ./internal/lease/` → **0**; có DSN → **0**.
- `go test -count=1 -p 4 ./internal/... ./cmd/...` → **0**.
- `go test -tags p05cluster -count=1 -run 'TestP13c|TestP13d' ./hscontrol/servertest/` → **0**; trước đó `-run 'TestP13d|TestP11b|TestP12'` → **0**.
- Lint `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run ./...` trên `c8a4824` → **0 issues**. Với `--build-tags p05cluster` còn lỗi lint có sẵn ở các file servertest cũ (CI không lint tag này); file P13 sạch sau `487bda2`.
- Race: xem mục dưới.

Smoke CLI (binary build trên mc-ci, SQLite tạm, `487bda2`): `cluster tailnet create` trên DB trống → exit 1 "database schema is behind this binary; run `mesh-placement migrate` …"; `mesh-placement migrate` → exit 0 `fresh:true`, 11 migration; `headscale cluster migrate` lần hai → exit 0, không áp gì; `register` → 0; `epoch-fence` thiếu `-reason` → từ chối; `mesh-placement epoch-fence` → exit 0 `floor 1882927543615488` (< 2^53); `headscale cluster epoch-fence` → exit 0.

## E2E artifacts / protocol map redacted
Không chạy E2E (ngoài phạm vi). Harness đã sửa (chưa chạy): n1, p05, p08b, p11b, p12, p12b, twocluster gọi `headscale cluster migrate` ngay sau khi tạo DB và cài config (kết quả JSON vào `logs/migrate.json`, chỉ có ID migration). p00d, p02 chạy `headscale serve` single-tenant (vẫn tự migrate, tùy chọn tường minh) — không đổi.

## Negative tenant isolation tests
Không đổi đường dữ liệu tenant. Raw SQL guard xanh: SQL mới ở `hscontrol/db` resolve tĩnh, không nhắc bảng tenant; hai mục allowlist mới ở `internal/lease/lease.go` (đọc/cấu hình `cluster_leases`, bảng nền tảng), ba mục cũ bị gỡ vì câu lệnh đổi. Fence ghi `tailnets`, `cluster_leases`, `tailnet_moves`, `platform_epoch_fences` (bảng nền tảng). Không có API unscoped mới (CLI operator chạy trên kết nối trực tiếp).

## DB migrations and downgrade constraints
- `202611271000-schema-compat` — **contract** (binary trước P13d không khởi động được trên DB này: chúng từ chối mọi migration lạ). Bảng `schema_compat(migration_id PK, min_reader_id)`; hàng được ghi trong transaction migrate.
- `202611271100-epoch-fence` — **expand** (`min_reader_id = 202611271000-schema-compat`). Bảng `platform_epoch_fences`, trigger `trg_cluster_leases_no_delete` (SQLite, PG) + `trg_cluster_leases_no_truncate` (PG).
- Cả hai nối cuối, trong `tx.Transaction`, SQLite + PostgreSQL, đã thêm vào `schema.sql`/`schema_postgres.sql`; có `Rollback` (chỉ dùng trong test rewind).
- Downgrade: sau khi DB có `202611271000`, chỉ binary biết `schema_compat` chạy được. Từ nay về binary N được khi DB chỉ thêm migration expand.

## Quy trình upgrade operator (old → new) từ bản này
1. **Lần đầu lên bản có P13d (bản này): dừng toàn bộ** cluster process, mesh-placement, mesh-bootstrap (binary cũ tự migrate khi khởi động và từ chối migration lạ; không canary được ở bước này). Backup + kiểm backup.
2. `mesh-placement migrate -config <config kết nối trực tiếp tới primary>` (hoặc `headscale cluster migrate`). Hai lần chạy đồng thời an toàn (một bên chờ rồi không làm gì).
3. Khởi động binary mới: mesh-bootstrap, mesh-placement, rồi các cluster (standby trước). Process nào gặp DB chưa migrate sẽ thoát với thông báo chỉ lệnh migrate.
4. Các bản sau (theo ADR 0013 §5): backup → `migrate` (chỉ expand) → bootstrap/placement → mỗi cluster: standby → switchover → active; canary = c2; contract chỉ ở bản phát hành sau khi mọi binary đã mới.
   - **Quy ước phát hành (chủ dự án quyết 2026-11-26, ADR 0013 §6):** upgrade canary ở bước 4 chỉ áp cho bản **PATCH** trong cùng minor (thay đổi schema cần canary chỉ phát hành ở PATCH). Bản **tăng minor** đi theo bước 1–3: dừng toàn bộ → backup + kiểm → `migrate` (trực tiếp tới primary) → khởi động bootstrap → placement → các cluster (standby trước, rồi active).
5. Sau restore hoặc promote cưỡng bức (ADR 0012 §4): PAUSE/DISABLE PgBouncer → restore → `mesh-placement epoch-fence -config <trực tiếp> -reason "<điểm restore>" -operator <tên> [-external-hwm N]` → kiểm `SELECT max(epoch) FROM cluster_leases` > HWM ghi trước restore → RESUME. Fence hủy mọi move đang chờ: operator lên lịch lại.

## Benchmark before/after
Không đo. Đường nóng bị chạm: standby poll `AcquireAbove` vẫn **một** câu đọc không khóa (đọc thêm `holder, epoch`); reconcile thêm một tra map trong bộ nhớ mỗi tailnet chưa nạp. Không có truy vấn mới trên đường ghi tenant.

## Known limitations / security blockers
1. **versioncheck chặn hạ minor** (giữ nguyên theo ADR 0013 §6): bước migrate của bản N+1 (không phải dev) ghi version N+1; process bản N **khác minor** khởi động lại sẽ bị từ chối → canary giữa hai minor bị chặn. Cùng minor hoặc build dev không bị. Cần chủ dự án quyết (đổi ADR 0013 §6 hoặc quy ước phát hành) — **không tự đổi**. **Đã quyết 2026-11-26:** giữ versioncheck; schema cần canary chỉ phát hành ở PATCH cùng minor, tăng minor = upgrade dừng toàn bộ (ADR 0013 §6, quy trình bước 4 ở trên).
2. HWM chỉ trong bộ nhớ; file `cluster_lease.epoch_hwm_path` (ADR 0012 §3, tùy chọn) chưa làm vì cần option config ở vùng P13b/P13f sửa chung. Process khởi động lại sau restore không có HWM: fence bắt buộc là lớp bảo vệ chính (như ADR).
3. HWM của owner_epoch cho phép **bằng** (tailnet SUSPENDED → ACTIVE cùng epoch), chỉ từ chối **nhỏ hơn**. Restore về đúng epoch process đã phục vụ (không thấy epoch sau) không bị phát hiện trong process; fence xử lý.
4. Metric cho regression chưa có (P13f); hiện là log `CRITICAL` (một lần mỗi epoch DB với tailnet; lần đầu và mỗi 30 lần với lease) + `Manager.Regressions()`.
5. Fence trong lúc mesh-placement scheduler đang chạy có thể deadlock với `runDueMove` (`internal/placement/moves.go`: dòng `tailnet_moves` rồi `tailnets`; fence: `tailnets` rồi `tailnet_moves`; đảo thứ tự thì lại ngược với hand-over `recordHandedOver`) → PG phát hiện và hủy một transaction, không ghi dở; runbook chạy fence khi PgBouncer đã PAUSE nên không xảy ra; nếu xảy ra, chạy lại fence.
6. SQLite: lần migrate thứ hai chờ tối đa `busy_timeout` (10 s); chỉ dev.
7. Harness chạy migrate qua PgBouncer (config của lab); ADR 0006 §4 khuyến nghị kết nối trực tiếp cho bước operator. Đúng vì toàn bộ là một transaction.
8. Deployment lâu dài trên lab (`meshlab`, c1/c2 systemd) đang ở schema P12c: khi triển khai binary này phải chạy bước 1–3 ở trên (chưa làm, không đổi lab).

## Reviewer approval
Chưa.
