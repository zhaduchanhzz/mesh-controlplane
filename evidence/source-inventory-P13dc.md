# Source inventory P13d + P13c (code): migration là bước riêng, tương thích schema, epoch fence

- **Commit đọc:** `c4533aa` (nhánh `feat/P13dc-migrate-fence`, tách từ `feat/P13-plan`; chứa P12c). Baseline v0.1 của repo vẫn là commit ghi trong `evidence/source-inventory-P00.md`; nhánh này không đụng tới nó.
- **Ngày:** 2026-11-26. **Phạm vi:** chỉ code + test trong process (SQLite, PostgreSQL tạm trên mc-ci, servertest `-tags p05cluster`). **Không** drill, **không** đổi lab, **không** chạy E2E.
- **ADR:** 0013 (P13d), 0012 (P13c), cả hai `Accepted` 2026-11-26. Liên quan: 0005 Bổ sung 2 §2, 0006 §4, 0010.
- Nhãn: [R] đọc code, [I] suy luận.

## 1. Tài liệu đã đọc

| Tài liệu | Dùng cho |
|---|---|
| `AGENTS.md` rule 5, 7, 11, 13 | inventory trước code; migration nối cuối, trong transaction, không AutoMigrate trong migration, không tắt FK; lỗi isolation = BLOCK |
| `hscontrol/db/AGENTS.md` | ID `YYYYMMDDHHMM-…`, `tx.Transaction`, test SQLite + PostgreSQL |
| `tickets/P13.md` §P13c, §P13d, thứ tự migration (P13d trước, P13c sau, khai báo `compat`) | phạm vi, test RED, điều kiện dừng |
| `design/adr/0013-*.md` | §Quyết định 1–6, §Kiểm chứng |
| `design/adr/0012-*.md` | §1 (fence), §2 (chặn DELETE), §3 (HWM trong process), §4, §Kiểm chứng |
| `design/adr/0005-*.md`, `0010-*.md` | lease đo bằng đồng hồ DB; epoch tăng đúng một lần khi flip |
| `evidence/source-inventory-P13.md` §2.1, §2.3 | điểm xuất phát (xem §2 dưới, đã đọc lại code) |

## 2. Symbol đã đọc (trước khi sửa)

### 2.1 Mở DB, migration (P13d)
- [R] `hscontrol/db/platform.go:45-63` `v01Migrations()`: 9 migration, cuối là `202611261200-tenant-id-immutable` (`tenant_immutable.go`).
- [R] `platform.go:127` `OpenPlatform(cfg)`: `openDB` → `checkV01Database` → `checkVersionUpgradePath` (AutoMigrate `database_versions`) → gormigrate `InitSchema(createSchema)` + `runMigrations` (**luôn migrate**, không khóa) → `setDatabaseVersion` (bản không phải dev) → `validateSQLiteSchema` (squibble, chỉ SQLite) → `tenantscope.Register`.
- [R] Người gọi `OpenPlatform` ở production: `hscontrol/cluster.go:204` (`NewCluster`), `cmd/mesh-placement/main.go:110`, `cmd/headscale/cli/cluster.go:141` (`withPlatform`: mọi lệnh admin `cluster tailnet …`, `cluster register`), `cmd/bench-db/b3.go:74` (tag `benchdb`). `NewHeadscaleDatabase` (`db.go:68`) = `OpenPlatform` + `bindRuntimeTailnet`: đường single-tenant (`state.NewState`, `headscale serve`, `headscale policy`), servertest `seed` (`cluster_p05.go:313`) và ~70 test.
- [R] `tenant.go:118` `checkV01Database`: DB có `users`/`nodes` mà không có `migrations` → Headscale; không có `tailnets` → không phải v0.1; **migration lạ → `ErrNotV01Database` "unknown migration"**.
- [R] `readonly.go:31` `OpenReadOnly` (mesh-bootstrap): không tạo/migrate; đòi `LatestMigrationID()` có trong `migrations`, chấp nhận mọi migration lạ.
- [R] `db.go:208` `runMigrations`: SQLite chạy `PRAGMA foreign_key_check` sau migrate.
- [R] gormigrate v2.1.7 (`gormigrate.go` trên mc-ci): `UseTransaction=false` (mặc định) thì `g.tx = g.db`; `createMigrationTableIfNotExists` dùng `AutoMigrate` của bảng `migrations` (đã có từ trước, không đổi); `InitSchema` chỉ khi chưa có migration nào. Truyền một `*gorm.DB` đang trong transaction vào `gormigrate.New` là hợp lệ; `db.Transaction` lồng bên trong thành SAVEPOINT (gorm).
- [R] `sqliteconfig.Default`: `_txlock=immediate`, `busy_timeout=10000`: transaction SQLite lấy khóa ghi ngay khi `BEGIN`, process thứ hai chờ.
- [R] `versioncheck.go:186` `checkVersionUpgradePath`: AutoMigrate `database_versions` rồi so major/minor (dev build bỏ qua). Chặn hạ minor.
- [R] Harness E2E (`infra/lab/e2e/{n1,p05,p08b,p11b,p12,p12b,twocluster}/run.sh`): `createdb` (PG) hoặc xóa file SQLite rồi lệnh đầu tiên chạm DB (`cluster register`, `tailnet create` hoặc `cluster serve`) **dựa vào tự migrate** để tạo schema.

### 2.2 Epoch (P13c)
- [R] `internal/lease/lease.go:59-88` DDL `cluster_leases` + trigger `trg_cluster_leases_epoch_monotonic` (chỉ `BEFORE UPDATE OF epoch, holder`). `Store.Acquire` (`:163`): PG đọc `expires_at > clock_timestamp()` không khóa, rồi `pg_advisory_xact_lock` theo cluster, rồi upsert `acquire` (`:210`): chưa có dòng → epoch 1; hết hạn → `epoch + 1`.
- [R] `internal/lease/manager.go` `Manager`: chỉ giữ `cur`; `acquire` (`:199`) nhận mọi epoch store trả về. Không HWM.
- [R] Test xóa dòng lease: `internal/placement/p12_placement_test.go:30, :84`, `p12_explicit_test.go:30`, `p12_reconcile_test.go:119`. Production: không có `DELETE FROM cluster_leases` (grep).
- [R] `hscontrol/cluster.go:428` `reconcile`: `tn.OwnerEpoch != rt.epoch` (`:464`) → retire rồi **nạp lại** ở epoch mới, kể cả nhỏ hơn. `load` (`:335`) lấy epoch từ `TenantDB` (`platform.go`), không so với gì. `handOver` (`:789`) ghi chủ mới ở `epoch+1`; `takeOverOrphans` (`:822`) ở `OwnerEpoch+1`. Mọi load/reconcile/hand-over chạy dưới `leaseMu`.
- [R] Thứ tự khóa của ghi có fence (`tenantscope.go` `checkFence`/`checkLease`): advisory chia sẻ theo tailnet → `tailnets` `FOR SHARE` → advisory chia sẻ theo cluster (`lease.PostgresSharedLock`). `LockOwnership` (`:438`): advisory độc quyền theo tailnet. `Store.Acquire`: advisory độc quyền theo cluster → dòng lease.
- [R] `tailnet_moves` (`tailnet_moves.go`): trạng thái mở `SCHEDULED`/`REQUESTED`, CAS theo `from_epoch`; `tailnets.moving_to_cluster_id` (`moving.go`).
- [R] Raw SQL guard (`raw_sql_guard_test.go`): SQL nhắc bảng tenant hoặc không resolve tĩnh (vd. `fmt.Sprintf`) trong package chạm DB phải có trong `raw_sql_allowlist_test.go`.

## 3. Ràng buộc

- Migration đã commit (9) không sửa, không đổi thứ tự (AGENTS 11); migration mới nối cuối, `tx.Transaction`, DDL tường minh, SQLite + PostgreSQL, thêm vào `schema.sql`/`schema_postgres.sql` (DB mới tạo từ file schema rồi ghi mọi ID).
- ID cố định theo yêu cầu: `202611271000-schema-compat` (P13d), `202611271100-epoch-fence` (P13c).
- Chỉ `pg_advisory_xact_lock*` (transaction-scoped) để đi được qua PgBouncer transaction mode (ADR 0006 §3.2).
- Epoch không bao giờ lùi; không tạo owner kép; không đổi ngữ nghĩa ADR 0005/0010/0012/0013.

## 4. Thiết kế chọn (trong khuôn ADR, không đổi ngữ nghĩa)

P13d:
- `OpenPlatform` **không** migrate, không tạo schema, không DDL: chỉ kiểm (Headscale/không phải v0.1, version, tương thích). DB thiếu migration binary biết → `ErrSchemaNeedsMigration` ("run `mesh-placement migrate` or `headscale cluster migrate`"); DB có migration lạ mà `schema_compat` không cho phép → `ErrSchemaTooNew`.
- `db.Migrate(ctx, cfg)`: một transaction ngoài, `pg_advisory_xact_lock` cố định (SQLite: `BEGIN IMMEDIATE`), trong đó gormigrate + ghi `schema_compat` + version. Hai lần chạy đồng thời: một bên chờ, rồi thấy không còn gì (L7).
- `db.MigrateAndOpenPlatform(cfg)`: tùy chọn tường minh cho dev/test và đường single-tenant (`NewHeadscaleDatabase`); không có đường gọi nào ở cluster mode, `mesh-placement serve`, hay lệnh admin `cluster …`.
- `schema_compat(migration_id PK, min_reader_id)`: migration `202611271000` tự khai báo `contract` (binary trước P13d không hiểu bảng này, vẫn từ chối mọi migration lạ); các migration sau khai báo trong `migrationCompat` (test tĩnh bắt buộc).
- Version chỉ được ghi bởi bước migrate; `OpenPlatform` chỉ đọc và so.

P13c:
- Migration `202611271100-epoch-fence` (`expand`, `min_reader` = `202611271000`): bảng `platform_epoch_fences`, trigger chặn `DELETE` (và `TRUNCATE` trên PG) `cluster_leases`.
- `db.EpochFloor(t) = unix_s(t) << 20` (< 2^53, kiểm). `Platform.EpochFence`: một transaction; thứ tự khóa theo đúng thứ tự của ghi có fence (dòng `tailnets` trước, advisory theo cluster sau) để không deadlock.
- `lease.Store.AcquireAbove(floor)`: không lấy lease nếu epoch mới ≤ floor (rollback, `ErrEpochRegression`); đọc epoch người khác đang giữ (`HeldError`). `Manager` giữ HWM trong bộ nhớ.
- `Cluster`: HWM owner_epoch theo tailnet (load, hand-over, take-over); `owner_epoch` < HWM → retire, không nạp lại.

## 5. Giả định còn lại

1. [I] DDL của mọi migration hiện có chạy được trong **một** transaction PostgreSQL (không có `CREATE INDEX CONCURRENTLY`, `ALTER TYPE … ADD VALUE`): đọc 9 migration, không thấy.
2. [I] Bước migrate qua PgBouncer transaction mode vẫn đúng vì toàn bộ nằm trong một transaction; ADR 0006 §4 vẫn khuyến nghị kết nối trực tiếp (harness lab hiện đi qua PgBouncer).
3. File HWM (`cluster_lease.epoch_hwm_path`, ADR 0012 §3 "tùy chọn") **không** làm trong nhánh này: cần option config ở `hscontrol/types/config.go` + `tools/configgen` (vùng P13b/P13f sửa chung). HWM chỉ trong bộ nhớ.
4. `versioncheck` chặn hạ minor: canary giữa hai bản khác **minor** (không phải dev) sẽ bị chặn khi bản mới đã migrate và ghi version. Ghi là rủi ro cần chủ dự án quyết (ADR 0013 §6 giữ versioncheck nguyên vẹn); build dev/cùng minor không bị ảnh hưởng.
