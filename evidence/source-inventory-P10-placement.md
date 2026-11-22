# Source Inventory — P10 (phần placement): registry, `internal/placement`, `cmd/mesh-placement`

- **Trạng thái:** `Implemented`, `Verified` (unit + race, SQLite và PostgreSQL 16 thật qua `postgrestest` trên `mc-ci`). Chưa `Reviewed`.
- **Nhánh:** `agent/p10-placement`, tách từ `master` @ `a316f78` (merge P05). Không push, không merge.
- **Commit:** RED `051bf40` → GREEN `1ca2a2f` → evidence (commit chứa file này).
- **Phạm vi phần này:** schema registry, `internal/placement`, API operator `cmd/mesh-placement`. **Ngoài phần này** (kỹ sư chính làm song song): helper fencing owner/epoch trên mọi mutation tenant, gắn quota vào đường đăng ký, runtime tự dừng khi mất quyền sở hữu, lint đường ghi tenant.
- **Hợp đồng:** `tickets/P10.md`, PLAN §4 (Registry, mục 4 "Quyền sở hữu và fencing", mục 6 API admin), PLAN §5 (luồng `CreateTailnet`), ADR 0005 (Bổ sung 4, Bổ sung 2 §2–§3), ADR 0003 (lớp shard), ADR 0007.

## 1. Path/symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `AGENTS.md`, `hscontrol/db/AGENTS.md` | quy tắc 2, 6, 8, 10, 11, 13; quy tắc migration | migration chỉ nối cuối, trong transaction, không AutoMigrate |
| `tickets/P10.md` | mục tiêu, RED tests | |
| `PLAN.md` | §1 mục 4–6, §2 bảng (PgBouncer, Registry, Sharding), §4 mục 4/6, ví dụ SQL, §5 | `clusters(id, endpoint_url, class, state, noise_public_key)`; `pg_advisory_xact_lock` được phép với PgBouncer |
| `design/adr/0005-…md` | Bổ sung 4, Bổ sung 2 §1–§3 | owner+epoch trên `tailnets`; epoch đơn điệu |
| `design/adr/0003-…md` | lớp shared/dedicated | |
| `hscontrol/db/platform.go` | `Platform`, `OpenPlatform`, `v01Migrations`, `TenantDB`, `OwnedTailnets` | thêm `registryMigration()` vào cuối chuỗi |
| `hscontrol/db/platform_admin.go` | `CreateTailnet`, `denyAllPolicy`, `SetTailnetStatus`, `TailnetBySlug` | mẫu INSERT policy thô cùng transaction |
| `hscontrol/db/nodekeys.go`, `migration_test.go` | `nodeKeyPairMigration`, `TestNodeKeyPairDDLInSchemaFiles`, `TestMigrationFromV01Init` | mẫu DDL byte-identical với file schema |
| `hscontrol/db/tenant.go` | `createSchema`, `tableExists`, `clusterIDOf` | |
| `hscontrol/db/db.go` | `openDB` (`TranslateError: true`, SQLite `MaxOpenConns=1`) | lỗi unique → `gorm.ErrDuplicatedKey` |
| `hscontrol/db/tenantscope/tenantscope.go` | `Register` | bảng không có `tailnet_id` không bị đụng; `Raw`/`Exec` không qua plugin |
| `hscontrol/db/schema.sql`, `schema_postgres.sql` | `tailnets`, `nodes`, `policies` | `nodes.deleted_at` |
| `hscontrol/db/suite_test.go`, `tenant_test.go`, `platform_test.go` | `newPostgresDBForTest`, `postgresConfig`, `backends` | mẫu `postgrestest` |
| `hscontrol/types/tailnet.go`, `internal/tailnet/tailnet.go` | `Tailnet`, `TailnetStatus`, `ID`, `ParseID` | |
| `hscontrol/api/v2/api.go`, `cmd/gen-openapi/main.go` | `Config`, `NewAPI`, `Spec`, `Spec30`, bảng `specs` | mẫu Huma + xuất OpenAPI |
| `hscontrol/config/load.go` | `config.Load` | `mesh-placement` đọc phần `database.*` của config Headscale |
| `hscontrol/servertest/cluster_p05.go` | seed tailnet (tag `p05cluster`) | đổi owner không tăng epoch → sửa (§2.1) |
| `cmd/bench-db/b3.go` | CAS `owner_epoch + 1` | đã tăng epoch, không ảnh hưởng |

## 2. Thiết kế

### 2.1 Schema — migration `202611231200-registry-clusters-audit` (`hscontrol/db/registry.go`)
- Nối **sau** `202611230000-nodes-key-pair-one-tailnet`; không sửa migration cũ. `Migrate`/`Rollback` trong `tx.Transaction`, DDL tường minh cho SQLite và PostgreSQL, không AutoMigrate, không tắt FK.
- DDL **byte-identical** với phần cuối `schema.sql` / `schema_postgres.sql` (`TestRegistryDDLInSchemaFiles`); squibble kiểm DB SQLite đã migrate khớp DB mới (`TestRegistryMigrationFromPreviousChain`).
- `clusters(id PK, endpoint_url NOT NULL UNIQUE (index), class CHECK shared|dedicated, state CHECK ACTIVE|DRAINING|DISABLED DEFAULT 'DISABLED', noise_public_key, max_devices NOT NULL CHECK > 0, revision NOT NULL DEFAULT 1 CHECK >= 1, created_at, updated_at)`. Cluster mới mặc định `DISABLED` (không nhận tailnet cho tới khi operator bật).
- Trigger `trg_clusters_revision_monotonic`: `revision` không bao giờ giảm.
- Trigger `trg_tailnets_owner_epoch_monotonic` trên `tailnets` (BEFORE UPDATE OF `owner_epoch`, `owner_cluster_id`): từ chối `owner_epoch` giảm **và** đổi `owner_cluster_id` mà `owner_epoch` không tăng (chặn A→B→A cùng epoch). Áp cho **mọi** đường ghi, kể cả SQL thô. PostgreSQL: hàm plpgsql, `ERRCODE check_violation`; SQLite: `RAISE(ABORT)`.
- `platform_audit(id, at, actor, action, target, outcome CHECK ok|replay|error, detail, idempotency_key)`, index `at`.
- `platform_idempotency(actor, idempotency_key, operation, request_hash, response, created_at, PK(actor, idempotency_key))`: không gian key theo từng operator.
- **Không có FK** `tailnets.owner_cluster_id → clusters(id)`: DB hiện có mang cluster id chưa đăng ký (ví dụ `local`, mặc định của `clusterIDOf`). Thay vào đó: placement/`MoveOwner` chỉ chọn cluster đã đăng ký; `EnrollmentAllowed` từ chối tailnet có owner chưa đăng ký (`ErrUnknownCluster`, fail closed). Thêm FK là việc sau khi mọi cluster đã được đăng ký (cần quyết định, §6).
- `hscontrol/servertest/cluster_p05.go` (tag `p05cluster`): seed đổi owner của tailnet mặc định nay tăng `owner_epoch` (`gorm.Expr("owner_epoch + 1")`), nếu không trigger từ chối.
- `Platform.RegistryDB()`: trả pool của platform cho placement (đã ghi chú: không bao giờ đưa cho code phục vụ request tenant).

### 2.2 `internal/placement`
- `Registry` trên `*gorm.DB` của platform (`placement.New(db, Config{SharedTenantLimit, WarnPercent})`, mặc định 5000 / 80%).
- **Mọi mutation** qua `mutate()` (`audit.go`): `Op{Actor, IdempotencyKey, RequestHash}`; thiếu actor → `ErrInvalid`, không ghi gì. Một transaction gồm: thao tác + dòng audit `ok` + (nếu có key) kết quả JSON trong `platform_idempotency`. Key đã dùng: cùng operation + hash → trả kết quả cũ (audit `replay`, **không có hiệu lực lần hai**); khác → `ErrIdempotencyConflict` (audit `error`). Lỗi → rollback, rồi audit `error` ngoài transaction (best effort). Hai request cùng key đồng thời: bên thua bị PK/unique/CAS chặn, đọc lại key và trả kết quả của bên thắng.
- **Cluster CRUD:** `CreateCluster` (id `[a-z0-9._-]`, endpoint chỉ `https`, class/state hợp lệ, `max_devices > 0`), `UpdateCluster` (endpoint/noise key/max_devices, `revision+1`), `UpdateClusterState(id, state, expectedRevision)` (CAS trên `revision` khi `expectedRevision > 0`, `revision+1`, **không đụng `tailnets`**), `DeleteCluster` (khóa dòng `FOR UPDATE`, từ chối nếu bất kỳ dòng `tailnets` nào còn trỏ tới), `GetCluster`, `ListClusters` (kèm số tailnet và số thiết bị).
- **Placement:** `PlaceTailnet(class)` = cluster `ACTIVE` cùng class, `devices < max_devices`; dedicated chỉ khi chưa có tailnet sống; chọn tải thấp nhất (`devices/max_devices`, so sánh nhân chéo số nguyên), hòa thì theo id. Không có → `ErrNoCapacity`.
- **`CreateTailnet(slug, class)`**: MỘT transaction: khóa các cluster ứng viên `FOR UPDATE` (PostgreSQL, theo id) → chọn → `INSERT tailnets` (`PENDING`, owner, epoch 1) → `INSERT policies {"grants":[]}` (SQL thô, nêu rõ `tailnet_id`, như `Platform.CreateTailnet`) → audit → kết quả idempotent. Lỗi ở bất kỳ bước nào → không còn gì. Slug `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, trùng → `ErrConflict`.
- **`ActivateTailnet(id, expectedEpoch)`**: `PENDING|SUSPENDED → ACTIVE` với `owner_epoch = expectedEpoch` và **phải có dòng policy** (không bao giờ fallback allow-all). **`SuspendTailnet`**: `PENDING|ACTIVE → SUSPENDED`. Sai epoch → `ErrStaleEpoch`; sai trạng thái/thiếu policy → `ErrInvalidTransition`. Process cluster đang chạy chỉ thấy đổi trạng thái khi khởi động lại (reload là phần runtime).
- **`MoveOwner(id, from, fromEpoch, to)`**: khóa cluster đích (`FOR UPDATE`, phải `ACTIVE`; dedicated phải trống), rồi CAS
  `UPDATE tailnets SET owner_cluster_id=?, owner_epoch=owner_epoch+1, updated_at=? WHERE id=? AND owner_cluster_id=? AND owner_epoch=? AND deleted_at IS NULL`. `RowsAffected != 1` → `ErrStaleEpoch`. Không đổi `status` (thủ tục move cho client là P12b).
- **Quota** (`quota.go`): `EnrollmentAllowed(ctx, tailnet.ID) (Quota, error)` và `EnrollmentAllowedTx(tx *gorm.DB, tailnet.ID) (Quota, error)`. Fail closed: chỉ cho phép khi `err == nil`. Shared: giới hạn `SharedTenantLimit` **theo tailnet** (tailnet khác cùng cluster không tính); dedicated: `max_devices` của cluster; `Warn` khi `(devices+1)*100 >= limit*WarnPercent`. Tailnet không có/đã xóa → `ErrNotFound`; không `ACTIVE` → `ErrTailnetNotActive`; owner chưa đăng ký → `ErrUnknownCluster`; đầy → `ErrQuotaExceeded`. Bản `Tx` lấy `pg_advisory_xact_lock(hashtextextended('placement/quota/'||id,0))` để hai lần đăng ký đồng thời của **cùng** tailnet nối tiếp nhau tới khi transaction của caller commit (tailnet khác không bị chặn; được phép với PgBouncer transaction mode).
- **SQL thô trên bảng tenant (review tay, tenantscope không parse `Raw`/`Exec`):**

  | Vị trí | Câu | Trả về |
  |---|---|---|
  | `tailnets.go` `CreateTailnet` | `INSERT INTO policies (tailnet_id, data, …) VALUES (?, …)` | ghi policy deny của tailnet đang tạo |
  | `tailnets.go` `deviceCount` | `SELECT count(*) FROM nodes WHERE tailnet_id = ? AND deleted_at IS NULL` | một số |
  | `clusters.go` `clusterUsage` | `SELECT owner_cluster_id, count(DISTINCT t.id), count(n.id) FROM tailnets t LEFT JOIN nodes n … GROUP BY owner_cluster_id` | chỉ số đếm (đọc chéo tenant duy nhất) |
  | `tailnets.go` `setStatus` | `EXISTS (SELECT 1 FROM policies p WHERE p.tailnet_id = tailnets.id …)` | điều kiện, không trả dòng |
  | `quota.go` | `pg_advisory_xact_lock(…)` | khóa |

### 2.3 API operator — `internal/placement/placementapi`, `cmd/mesh-placement`
- Huma v2 (`humachi`), OpenAPI 3.1 tại `/v1/openapi` (cũng sau kiểm tra operator), không docs UI. `go run ./cmd/gen-openapi -api placement` (thêm vào bảng `specs`) hoặc `mesh-placement -openapi`. Không có client sinh trong `gen/` (chưa ai gọi bằng code); không sửa `gen/`.
- Operation: `GET/POST /v1/clusters`, `GET/PATCH/DELETE /v1/clusters/{id}`, `PUT /v1/clusters/{id}/state`, `GET /v1/placement?class=`, `POST /v1/tailnets`, `GET /v1/tailnets/{id}`, `POST /v1/tailnets/{id}/activate|suspend|owner`, `GET /v1/tailnets/{id}/quota`, `GET /v1/audit`.
- **mTLS:** `TLSConfig`: TLS 1.3, `ClientCAs` = CA operator được pin (không dùng system roots), `VerifyClientCertIfGiven` (cert không verify được → handshake lỗi; không cert → middleware trả 401 thay vì lỗi handshake trơn). Cert phải có EKU `clientAuth` (Go kiểm).
- **`Authorizer`:** (1) có `Authorization`, `Proxy-Authorization`, `X-Api-Key`, `X-Headscale-Api-Key` hoặc `Cookie` → **403** dù cert hợp lệ (API operator không bao giờ nhận credential tenant); (2) không có chain đã verify → 401; (3) URI SAN hoặc DNS SAN phải nằm **chính xác** trong danh sách `-operators` (CN bị bỏ qua) → nếu không 403. Identity khớp là `actor` trong audit. Danh sách rỗng → không khởi động.
- Mọi mutation **bắt buộc** header `Idempotency-Key` (1–200 ký tự; thiếu → 422 do Huma). `RequestHash` = SHA-256 của tên operation + path param + body JSON.
- Map lỗi: `ErrNotFound` 404, `ErrInvalid` 400, `ErrIdempotencyConflict` 422, stale epoch/revision, trùng, cluster đang dùng, chuyển trạng thái sai 409, `ErrNoCapacity` 503, lỗi khác 500 không chi tiết.
- `cmd/mesh-placement`: `-config` (chỉ dùng `database.*`, mật khẩu PostgreSQL qua `file:` như cluster), `-tls-cert/-tls-key/-client-ca/-operators` bắt buộc, `-listen` mặc định `127.0.0.1:9443`, timeout HTTP, `-shared-tenant-limit`. Không log key/mật khẩu.

## 3. Test (RED → GREEN)

Môi trường: `mc-ci` (Linux), Go 1.27.4, PostgreSQL 16 (`/usr/lib/postgresql/16/bin` trong `PATH`). Mã nguồn chuyển bằng `git bundle` → `git clone` vào `~/src/p10-placement` (không copy cây Windows). Log: `~/src/p10-placement-logs/` trên mc-ci.

| Bước | Lệnh | Exit | Kết quả |
|---|---|---|---|
| RED (`051bf40`, chỉ có chữ ký, thân trả `not implemented`) | `go test -race -count=1 ./internal/placement/` | 1 | 13/13 test FAIL, cả subtest `sqlite` và `postgres` (24 lỗi `not implemented`) |
| RED cùng commit, schema | `go test -race -count=1 -run Registry ./hscontrol/db/` | 0 | DDL trong file schema + migration từ chuỗi trước: PASS (sqlite, postgres) |
| GREEN build | `go build ./...` | 0 | |
| GREEN vet | `go vet ./...` | 0 | |
| GREEN placement + API | `go test -race -count=1 -v ./internal/placement/...` | 0 | 16 test PASS; 11 subtest `postgres` PASS, không SKIP |
| Hồi quy DB | `go test -race -count=1 ./hscontrol/db/...` | 0 | `hscontrol/db` 163.6 s, `sqliteconfig`, `tenantscope` ok |
| Hồi quy servertest cluster (tag) | `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | 0 | `ok` 78.1 s (seed đổi owner nay tăng epoch) |
| Lặp lại đồng thời | `go test -race -count=10 -run "Concurrent|Idempotency" ./internal/placement/` | 0 | 10 lần × (SQLite + PG) ổn định, 53.8 s |

Ánh xạ RED test của ticket:

| Test ticket | Test | Backend |
|---|---|---|
| CAS `owner_epoch` cũ → không đổi gì | `TestMoveOwnerStaleEpochRejected` (4 tổ hợp from/epoch sai, owner/epoch giữ nguyên, audit 1 ok + 6 error) | SQLite, PG |
| Hai (N) operator CAS đồng thời → đúng một thắng | `TestMoveOwnerConcurrentExactlyOneWins` (24 goroutine, đích c2/c3 xen kẽ; đúng 1 thắng, còn lại `ErrStaleEpoch`, epoch = 2) | SQLite, PG |
| `epoch` không giảm qua API, kể cả `UpdateClusterState` | `TestEpochNeverDecreases` (revision tăng qua 4 lần đổi state, CAS revision cũ bị từ chối, `owner_epoch` không đổi; `UPDATE` thô hạ epoch / đổi owner không tăng epoch / hạ revision đều lỗi) | SQLite, PG |
| `CreateTailnet` lỗi giữa chừng → không tailnet thiếu policy/owner | `TestCreateTailnetFailureLeavesNothing` (tiêm lỗi ở `place`, `tailnet`, `policy`, `audit`, `idempotency`, `commit`: 0 tailnet, 0 policy mồ côi, 0 kết quả idempotent, 0 audit `ok`, 6 audit `error`; key đó dùng lại được) | SQLite, PG |
| Hard quota 5.000 trên shared; tenant khác không ảnh hưởng | `TestQuotaSharedHardLimit` (5000 thiết bị → `ErrQuotaExceeded`; hàng xóm 10 thiết bị cùng cluster được phép; 3999 → cảnh báo; 3998 → không; thiết bị đã xóa không tính; dedicated theo `max_devices`; PENDING/owner chưa đăng ký/không tồn tại → từ chối) | SQLite, PG |
| mTLS sai identity → từ chối; token tenant → 403 | `TestOperatorAuth` (bearer tenant không cert → 403; không cert → 401; cùng CA nhưng SAN không được phép → 403 (CN = alice bị bỏ qua); CA khác → handshake lỗi; cert EKU serverAuth → handshake lỗi; operator hợp lệ + bearer → 403; không ghi audit nào) | SQLite |

Thêm: `TestCreateTailnetPendingDenyOwner`, `TestPlacement`, `TestPlacementDedicatedConcurrent` (8 request đồng thời trên 3 cluster dedicated → đúng 3 thành công, không cluster nào có 2 tailnet), `TestIdempotency` (replay không tăng epoch lần hai; key khác request → conflict; key theo operator; 8 retry đồng thời → 1 tailnet), `TestMutationsNeedActor`, `TestTailnetLifecycle`, `TestDeleteCluster`, `TestOperatorFlow` (idempotency/CAS/quota/audit qua HTTP, actor = SAN của cert), `TestSpec`.

Chứng chỉ test sinh trong bộ nhớ (`crypto/x509`, ECDSA P-256) mỗi lần chạy; không key nào được ghi ra đĩa hay commit.

## 4. Migration và ràng buộc downgrade
- Migration mới là entry cuối chuỗi; DB tạo mới có sẵn bảng qua file schema và được ghi nhận bởi `InitSchema`.
- `Rollback` xóa trigger, hàm, 3 bảng (mất dữ liệu registry/audit) — chỉ dùng trong test. Binary cũ (trước P10) mở DB đã migrate sẽ bị `checkV01Database` từ chối vì ID migration lạ (hành vi có sẵn).
- Trigger epoch áp dụng cho mọi code cũ: đường nào đổi `owner_cluster_id` mà không tăng epoch sẽ lỗi (đã sửa seed servertest; `cmd/bench-db` b3 đã tăng epoch).

## 5. Giới hạn đã biết
- Đây **không** phải fencing ở đường ghi tenant (helper `BEGIN → kiểm owner/epoch FOR SHARE → mutation`): phần đó của kỹ sư chính. Registry chỉ bảo đảm CAS/epoch đơn điệu ở `tailnets`.
- `EnrollmentAllowed` (không `Tx`) có thể cho 2 lần đăng ký đồng thời ở `limit-1` cùng lọt; muốn chính xác, gọi `EnrollmentAllowedTx` trong transaction chèn node. Chưa nối vào đường đăng ký (kỹ sư chính).
- Đếm thiết bị bằng `count(*)` mỗi lần (không counter). Với 5.000 dòng/tailnet và index hiện có là chấp nhận được; chưa benchmark trên hot path đăng ký.
- Dung lượng cluster shared (`max_devices`) chỉ dùng cho placement, không chặn enrollment (để tenant này không ảnh hưởng tenant khác); placement đọc số đếm không khóa node nên là ước lượng mềm.
- Audit của lời gọi lỗi/replay ghi ngoài transaction, best effort; audit của mutation thành công nằm trong transaction (không mất).
- Từ chối ở tầng mTLS/Authorizer (không cert, sai identity) không ghi audit (chỉ log HTTP của Go); tránh khuếch đại ghi DB từ client lạ.
- Chưa có integration "hai process cùng DB, khác cluster ID" và chưa chạy `mesh-placement` thật trong lab (không đụng mc-cp/VPS/Proxmox theo phạm vi).
- Không có FK owner → clusters (§2.1). Restore backup làm lùi epoch (F08) là P13.
- Nếu kỹ sư chính cũng nối migration vào `v01Migrations()`/cuối file schema, merge sẽ xung đột ở đó: giữ thứ tự theo ID thời gian, không sửa entry đã commit.

## 6. Câu hỏi mở cho reviewer
1. Thêm FK `tailnets.owner_cluster_id → clusters(id)` khi nào (cần đăng ký `local` và cluster lab trước)?
2. Tailnet có owner chưa đăng ký hiện bị **từ chối enrollment**; giữ fail closed hay coi như shared 5.000 trong giai đoạn chuyển tiếp?
3. Identity operator: URI SAN dạng `spiffe://mesh/operator/<tên>` có phù hợp CA nội bộ dự kiến không?

## 7. Reviewer approval
- Chưa có.
