# Source Inventory — P03b (schema v0.1: một SQLite cho mỗi shard, `tailnet_id`, scope fail-closed)

- **Trạng thái:** `Planned` → đang làm. Quyết định kiến trúc: **phương án A** (ADR 0004 bị từ chối, ADR 0002).
- **Nhánh:** `feat/P03-tenant-schema`
- **Ticket:** `tickets/P03.md`. P03a (policy mặc định deny) làm song song trên nhánh riêng.

## 1. Path/symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/db/db.go:45-75` | `NewHeadscaleDatabase` | `checkMinimumMigration`, `checkVersionUpgradePath`, rồi `gormigrate.New` với chuỗi upstream |
| `hscontrol/db/db.go:~411-465` | `migrations.InitSchema` | DB mới: `AutoMigrate(User)`, `ensureCredentialsTable`, `AutoMigrate(Node, Policy)`, rồi tạo lại index khớp `schema.sql` |
| `hscontrol/db/db.go:~488-520` | squibble | Chỉ chạy với SQLite: `schema.sql` là nguồn sự thật, DB phải khớp digest |
| `hscontrol/db/schema.sql` | toàn bộ | `users` (unique toàn cục), `credentials`, `nodes` (không có unique `machine_key`), `policies` |
| `hscontrol/types/users.go:65` | `User` (`gorm.Model`) | |
| `hscontrol/types/node.go:132` | `Node` | `UserID *uint` FK cascade; `AuthKeyID *uint64` → credentials; `GivenName unique_index` (tag, không có trong schema.sql) |
| `hscontrol/types/credential.go:29` | `Credential` | `UserID` FK `ON DELETE SET NULL`; unique `(kind, identifier)` |
| `hscontrol/types/policy.go:15` | `Policy` (`gorm.Model`, `Data`) | |
| Các file `hscontrol/db/*.go` không phải test | ~80 lời gọi GORM | node 11, users 6, preauth 12, oauth 12, api_key 6, policy 2, ip 3, secret 4, migrate_credentials 17, db 17 |
| `hscontrol/state/state.go` | truy cập DB ngoài package db | |

## 2. Thiết kế

### 2.1 Schema v0.1 (`schema.sql` mới, DDL viết tay, không AutoMigrate)
- `tailnets(id TEXT PK` (UUID canonical, giống `internal/tailnet.ID`)`, slug UNIQUE, status CHECK(PENDING|ACTIVE|SUSPENDED|DELETING), policy_revision, config_revision, created_at, updated_at, deleted_at)`
- `tailnet_policies(tailnet_id PK → tailnets, data TEXT NOT NULL, revision, updated_at)`: thay bảng `policies` toàn cục.
- `tailnet_keys(tailnet_id → tailnets, kind CHECK('noise'), public_key, created_at, retired_at, PK(tailnet_id, kind, public_key))`: key inventory. Chỉ lưu public key; private key nằm ở file hoặc secret store (P12b).
- `tailnet_events(tailnet_id → tailnets, revision, kind, created_at, PK(tailnet_id, revision))`: log revision theo tailnet.
- `users`, `credentials`, `nodes`: thêm `tailnet_id TEXT NOT NULL REFERENCES tailnets(id)`.
  - `users`: `UNIQUE(tailnet_id, id)` làm đích cho composite FK. Unique `name`/`provider_identifier` chuyển sang theo `(tailnet_id, …)`.
  - `nodes`: composite FK `(tailnet_id, user_id) → users(tailnet_id, id) ON DELETE CASCADE`; `(tailnet_id, auth_key_id) → credentials(tailnet_id, id)`; index `(tailnet_id, machine_key)`, `(tailnet_id, node_key)`.
  - `credentials`: FK `user_id → users(id) ON DELETE SET NULL` (composite FK + SET NULL sẽ đặt `tailnet_id` thành NULL nên không dùng được), cộng **trigger** bảo đảm `user.tailnet_id = credential.tailnet_id` khi INSERT/UPDATE. Unique `(kind, identifier)` giữ toàn cục: identifier ngẫu nhiên, và lookup luôn kiểm tailnet.

### 2.2 Scope fail-closed ở tầng ORM
- Plugin GORM (`hscontrol/db/tenantscope`): với model có cột `tailnet_id`:
  - query, update, delete: thêm `WHERE tailnet_id = ?`, lấy giá trị từ `tailnet.FromContext(stmt.Context)`;
  - create: gán `tailnet_id`;
  - **không có scope trong context thì báo lỗi `ErrNoScope`**, không chạy query.
- `HSDatabase` của một runtime giữ `DB = raw.WithContext(ctx đã gắn scope)`, nên mọi lời gọi hiện có tự động có scope mà không phải sửa ~80 chỗ.
- `Raw`/`Exec` không qua callback: **review tay** từng chỗ, ghi vào §4.

### 2.3 ID toàn cục
- Mỗi shard có `shard_index` (0…8191). Khi tạo DB, đặt `sqlite_sequence` của `nodes` và `users` bắt đầu từ `shard_index × 2^40`. Như vậy dải ID của các shard không chồng nhau và giữ dưới 2^53 (an toàn với JSON/JS, vừa `tailcfg.NodeID` int64). `AUTOINCREMENT` không tái sử dụng ID.
- P10: registry cấp `shard_index`. Lab trước P10: cấu hình tĩnh.

### 2.4 Phiên bản DB
- Chuỗi migration mới bắt đầu bằng `202611220000-v0.1-init`.
- DB có migration ID của upstream, hoặc có bảng `policies`/`users` mà không có `tailnets`, → **từ chối**, kèm lỗi rõ ràng.
- Cấu hình `database.type: postgres` → từ chối khi khởi động (shard chỉ dùng SQLite).

### 2.5 Tailnet của runtime
- Chế độ một tailnet (upstream, test): DB mới tự tạo đúng một tailnet ACTIVE; runtime dùng tailnet đó. DB có nhiều hơn một tailnet mà config không chỉ rõ → **từ chối khởi động**.
- `serve-tailnets`: truyền ID tailnet vào runtime (P05 sẽ gom các runtime về một file DB).

## 3. Test (RED trước)
- Schema: DB mới khớp `schema.sql` (squibble).
- FK âm tính: node của A trỏ tới user của B → lỗi. Credential của A gán user của B → trigger chặn.
- Unique: cùng `name` user ở A và B → được; cùng `name` hai lần trong A → lỗi.
- Plugin: query, create, update, delete khi thiếu scope → `ErrNoScope`; scope A không thấy dòng của B; create tự gán `tailnet_id`.
- Versioncheck: DB upstream (testdata) bị từ chối; Postgres bị từ chối.
- ID: shard_index = 3 → node đầu tiên có id = 3·2^40 + 1.
- Bộ test hiện có của `hscontrol/db`, `state`, `servertest` vẫn xanh ở chế độ một tailnet. Các test migration từ dump 0.29 sẽ bị gỡ, kèm lý do (chuỗi upstream đã bỏ).

## 4. Review tay các câu SQL thô
(điền khi làm)

## 5. Giả định
- Upstream cho phép cùng machine key trên nhiều user **trong một** tailnet (`nodesByMachineKey map[mk]map[UserID]`, `node_store.go:209`). Vì vậy chỉ thêm index `(tailnet_id, machine_key)`, không unique, để không phá ngữ nghĩa upstream. Ticket viết "UNIQUE(tailnet_id, machine_key)". Lệch này cần reviewer chốt.
