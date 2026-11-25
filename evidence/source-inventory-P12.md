# Source Inventory — P12 (cluster thứ hai: placement tự động, reconcile giữa cluster, bootstrap trả đúng cluster)

- **Trạng thái:** `Planned` (chỉ inventory + ticket nháp). **Không đổi code.** Chưa `Reviewed`.
- **Nhánh:** `feat/P12-inventory` từ `master` @ `b2b5d8d` (docs PLAN sau merge P11b `ee4c637`).
- **Ticket:** `tickets/P12.md` (nháp). PLAN: `PLAN.md:287` (P12), `:288` (P12b), `:47` và `:323–328` (N2).
- **Quy ước:** mọi trích dẫn dạng `path:dòng` tại `b2b5d8d`. "Không tìm thấy" = đã grep và không có, không phải suy đoán. Mục đánh dấu **[Đề xuất]** chờ chủ dự án duyệt. Không chạy Go/test, không SSH lab trong inventory này.
- **Phụ thuộc chưa merge:** P08b (`feat/P08b-control-plane`) sửa `hscontrol/cluster.go` (+36), `internal/placement/placementapi/api.go` (+79) và thêm `internal/placement/apikeys.go` (`git diff --stat b2b5d8d...feat/P08b-control-plane`). Chỉ ghi để tránh xung đột; P12 không dựa vào nội dung P08b.

## 1. Tài liệu đã đọc

| Tài liệu | Phần liên quan |
|---|---|
| `AGENTS.md` | rule 1 (nhánh, không tự merge), 3–4 (lab: chỉ create/update trong pool `meshcontrol-lab`, VMID 7700–7799), 5 (inventory), 6 (scope fail closed), 10 (OpenAPI qua generator), 11 (migration chỉ thêm cuối), 12 (Noise: một key mỗi cluster), 13 (isolation = BLOCK MERGE) |
| `PLAN.md:47` | N2: 2 cluster × (active + standby), 3+ tailnet, bootstrap trả đúng cluster, không owner kép, F01–F04 |
| `PLAN.md:63, 65, 69–71` | Registry, Endpoint (bootstrap), Sharding (shared/dedicated), Runtime HA, Migration (move = CAS owner/epoch, client đăng nhập lại) |
| `PLAN.md:194–210` | SQL minh họa: `clusters.state IN ('READY','DRAINING','MAINTENANCE','UNAVAILABLE')`, `cluster_leases … REFERENCES clusters(id)`, `owner_cluster_id … REFERENCES clusters(id)` — **khác code** (§3.4) |
| `PLAN.md:287–288, 305, 323–328, 356–375` | dòng P12/P12b; lab hiện có (7701–7706); gate N2; bảng VM lab (`mc-c2-a`, `mc-c2-b`) |
| `05-data-api-contract.md:74–95` | `clusters.noise_public_key TEXT NOT NULL -- chỉ public key, để đối chiếu`; mẫu fencing lease |
| `06-tests-benchmarks.md:25, 31–42` | S17 (split brain, bootstrap dữ liệu cũ), F01–F08 |
| `03-isolation-threat-model.md:39, 41, 83` | I12 (không hai owner), I14, `MoveTenant` (P12b) |
| ADR 0003 (`design/adr/0003-…md:3, 22, 34, 41`) | lớp shard; "Trong MVP, move do operator khởi động và được duyệt; không tự động" |
| ADR 0005 (`design/adr/0005-…md:20, 39–40, 49, 55, 64–68, 73–80`) | một Noise key mỗi cluster; bootstrap trả URL owner ở epoch hiện tại; cluster tự kiểm owner/epoch/lease; failover không đổi `owner_epoch` |
| ADR 0007 (`design/adr/0007-…md:24, 37, 52`) | runtime chỉ nạp cho tailnet `owner_cluster_id = cluster này`, `ACTIVE`, đúng epoch; `OwnedTailnets` |
| `tickets/P10.md:12–13, 34`; `tickets/P11.md:38`; `tickets/P11b.md:20, 45`; `tickets/P12b.md` | P10 để "cluster thứ hai trong lab" cho P12; move có kế hoạch là P12b |
| `evidence/E2E-two-cluster.md` | 9/9 ×2 trên `15dd9f3` (trước P11b); hạn chế: move bằng `reassign`, chưa placement tự động |
| `evidence/source-inventory-P10.md:35, 55, 96`; `evidence/source-inventory-P10-placement.md:42, 114, 117` | không FK `owner_cluster_id → clusters`; đếm thiết bị là ước lượng mềm; `mesh-placement` chưa chạy trên lab |
| `evidence/source-inventory-P11.md:24, 163–165, 267`, §8, §10.5 | bootstrap: DISABLED/chưa đăng ký → 503; không cache (Q5); `cluster register` không ghi `noise_public_key`; `moving_to_cluster_id` thay `MOVING` |
| `evidence/source-inventory-P11b.md`, `evidence/P11b-cluster-failover.md:140–157` | lease theo cluster; harness hai cluster vẫn chờ `/health`, nên đổi sang `/ready`; chỉ c1 có standby |
| Mẫu định dạng | `feat/P08b-control-plane:evidence/source-inventory-P08b.md`, `:tickets/P08b.md` |

## 2. Path và symbol đã đọc

### 2.1 Registry — `internal/placement`

| Path:dòng | Symbol | Ghi chú cho P12 |
|---|---|---|
| `internal/placement/placement.go:29–39` | `Class`, `ClassShared`, `ClassDedicated` | lớp shard (ADR 0003) |
| `internal/placement/placement.go:42–56` | `ClusterState`: `ACTIVE` (nhận tailnet mới), `DRAINING` (giữ tailnet, không nhận mới), `DISABLED` (không nhận gì; cluster mới mặc định) | chỉ placement và bootstrap đọc state; process cluster **không** đọc (§3.2) |
| `internal/placement/placement.go:60–70` | `Cluster{ID, EndpointURL, Class, State, NoisePublicKey, MaxDevices, Revision}` | `NoisePublicKey` tùy chọn |
| `internal/placement/placement.go:139, 142` | `DefaultSharedTenantLimit = 5000`, `DefaultWarnPercent = 80` | |
| `internal/placement/placement.go:161–163, 184–186` | `ErrNoCapacity`, `ErrUnknownCluster` | |
| `internal/placement/clusters.go:50–79` | `CreateCluster` | state rỗng → `DISABLED` (`:56–58`) |
| `internal/placement/clusters.go:83–132` | `UpdateCluster` (endpoint, noise key, max_devices) | revision +1 |
| `internal/placement/clusters.go:137–170` | `UpdateClusterState` | CAS theo revision; "never touches tailnets" (`:135–136`) |
| `internal/placement/clusters.go:174–208` | `DeleteCluster` | từ chối khi còn tailnet tham chiếu (kể cả đã xóa mềm) |
| `internal/placement/clusters.go:259–265` | `PlaceTailnet` | xem trước, không khóa |
| `internal/placement/clusters.go:271–330` | `place(db, class, lock)` | ứng viên = `class` khớp **và** `state = ACTIVE` (`:278–280`); dedicated đã có tailnet → bỏ (`:296–298`); `Devices >= MaxDevices` → bỏ (`:300–302`); chọn **ít tải nhất theo devices/max_devices**, hòa thì **id nhỏ nhất** (`:313–327`). Không xét số tailnet (shared), không xét lease/process sống |
| `internal/placement/clusters.go:334–351` | `lockClusters` | `FOR UPDATE` trên PostgreSQL theo thứ tự id |
| `internal/placement/clusters.go:378–395` | `clusterUsage` | SQL thô: đếm tailnet `deleted_at IS NULL` (mọi status) và node sống theo `owner_cluster_id` |
| `internal/placement/tailnets.go:31–98` | `CreateTailnet` | một transaction: `place(tx, class, true)` (`:48`) → tailnet `PENDING`, `OwnerEpoch: 1` (`:59–69`) → policy `{"grants":[]}` (`:88–91`) → audit |
| `internal/placement/tailnets.go:100–107` | `ActivateTailnet` | comment "picks the change up on restart (live reload is the runtime's part of P10)" **đã cũ**: reconcile nạp live (`TestClusterLoadsNewTailnetLive`) |
| `internal/placement/tailnets.go:116–165` | `setStatus` | `LockOwnership` (`:127`), CAS `owner_epoch` + status nguồn; ACTIVE cần policy (`:134–139`) |
| `internal/placement/tailnets.go:194–249` | `MoveOwner` | đích phải đã đăng ký và `ACTIVE` (`:207–214`); dedicated đích phải trống (`:216–228`); CAS `owner_cluster_id/owner_epoch+1` (`:230–232`). **Không** ghi `moving_to_cluster_id`, không đổi status |
| `internal/placement/quota.go:56–122, 131–157` | `EnrollmentAllowedTx`, `clusterBudget` | owner chưa đăng ký → `ErrUnknownCluster` (`:81–84`); advisory lock tailnet rồi cluster |
| `internal/placement/audit.go:93–183` | `mutate` | audit + idempotency trong cùng transaction |
| `internal/placement/placementapi/api.go:186–217` | `register` | 14 operation: clusters CRUD/state, `GET /v1/placement`, `POST /v1/tailnets`, activate/suspend/owner, quota, audit. **Không** có endpoint reconcile/báo cáo bất thường |
| `internal/placement/placementapi/api.go:249–268` | `apiError` | `ErrNoCapacity` → 503 |
| `cmd/mesh-placement/main.go:45–145` | `run` | chỉ phục vụ API mTLS; **không** có vòng nền nào (không reconcile, không job) |

### 2.2 Bootstrap — `internal/bootstrap`, `cmd/mesh-bootstrap`

| Path:dòng | Symbol | Ghi chú |
|---|---|---|
| `internal/bootstrap/bootstrap.go:59–74` | `Record{…, MovingTo, ClusterEndpoint, ClusterState}` | không có trường Noise key, không có lease |
| `internal/bootstrap/bootstrap.go:152–197` | `Service.Resolve` | key/tenant không hợp lệ → 404 đồng nhất (`:173–181`); `MovingTo != nil` → MOVING (`:186–187`); cluster chưa đăng ký/không endpoint/state ∉ {ACTIVE, DRAINING} → 503 (`:188–190`); còn lại → `ControlURL = endpoint_url` (`:191–193`) |
| `internal/bootstrap/store.go:20–27` | `lookupSQL` | `credentials ⋈ tailnets ⟕ clusters`; **không** đọc `cluster_leases` |
| `internal/bootstrap/store.go:60–102` | `GormStore.LookupAuthKey` | mỗi request một transaction `READ ONLY` (`:69`); >1 hàng → lỗi (`:83`) |
| `internal/bootstrap/http.go:60–66` | `DefaultHandlerConfig` | global 200/s burst 400; tenant 20/s burst 100; IP tắt |
| `internal/bootstrap/http.go:125–189` | `Handler.bootstrap` | bucket tenant chỉ trừ sau xác thực (`:164`); Retry-After: MOVING 30 s, unavailable 10 s, limited 1 s (`:183–185`, `:130`) |
| `internal/bootstrap/http.go:208–221, 278` | `write` (`Cache-Control: no-store`), `maxKeys = 100_000` | |
| `cmd/mesh-bootstrap/main.go:65, 74–79` | `db.OpenReadOnly`, `NewGormStore` | **không cache**; mỗi request đọc DB (quyết định P11 Q5) |

### 2.3 Cluster process — `hscontrol/cluster.go`, `hscontrol/db`

| Path:dòng | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/cluster.go:69–74` | `reconcileInterval = 2 * time.Second` | comment: giới hạn thời gian owner cũ còn trả map từ bộ nhớ |
| `hscontrol/cluster.go:89–126` | `Cluster{…, registry, lease, leaseMu, ready}` | |
| `hscontrol/cluster.go:151–214` | `NewCluster` | Noise key từ file (`:165`); `placement.New` (`:175`); `RequireLease` (`:189`); `lease.NewManager(…, platform.ClusterID(), leaseHolderName(), …)` (`:193`). **Không** nạp tailnet; **không** kiểm cluster đã đăng ký, `clusters.state`, `endpoint_url` hay `noise_public_key` |
| `hscontrol/cluster.go:246–314` | `load` | `TenantDB` (`:254`) đọc lại owner/ACTIVE/epoch; `SetEnrollGate` quota (`:283–296`); `leaseGate` (`:298`) |
| `hscontrol/cluster.go:344–451` | `reconcile` | chỉ khi `ready && leaseHeld` (`:348–350`) → **chỉ process active**; `OwnedTailnets(ClusterID())` (`:352`); epoch khác/không còn → retire (`:371`, `:424–426`); policy/DNS revision (`:374–422`); nạp tailnet mới (`:428–450`) |
| `hscontrol/cluster.go:500–528` | `retire` | gỡ khỏi routing trước, dừng nền |
| `hscontrol/cluster.go:584–586` | `NoisePublicKey` | không ghi vào registry |
| `hscontrol/cluster.go:588–681` | `runtimeFor`, `runtimeForRegister`, `knownNode`, `checkQuota`, `runtimeForNode` | tailnet không phục vụ ở đây → `db.ErrUnknownTenant` (F07) |
| `hscontrol/cluster.go:755–786` | `HealthHandler` (DB ping), `ReadyHandler` (200 chỉ khi giữ lease + đã nạp) | |
| `hscontrol/cluster.go:805–851` | `onLeaseAcquired` | `OwnedTailnets` (`:814`), nạp + start, `ready = true` (`:841`) |
| `hscontrol/cluster.go:856–872` | `onLeaseLost` | retire tất cả |
| `hscontrol/cluster.go:1003–1113` | `Serve` | `lease.Run` (`:1070`); ticker reconcile (`:1075–1087`) chạy trên mọi process nhưng standby thoát sớm |
| `hscontrol/db/platform.go:46–61` | `v01Migrations` | chuỗi hiện tại: init, nodeKeyPair, registry, moving, nodeIP, tailnetSettings, clusterLeases. Migration mới chỉ thêm cuối |
| `hscontrol/db/platform.go:88–117` | `RequireLease`, `SetLease`, `ClearLease`, `currentLease` | |
| `hscontrol/db/platform.go:190`; `hscontrol/db/tenant.go:164–170` | `clusterIDOf` | `ClusterID` rỗng → `defaultClusterID` |
| `hscontrol/db/platform.go:302–321` | `OwnedTailnets` | `owner_cluster_id = ? AND status = ACTIVE AND deleted_at IS NULL`; **không** join `clusters` |
| `hscontrol/db/platform.go:327–373` | `TenantDB` | owner = cluster này (`:348`), ACTIVE (`:352`), lease (`:356`) |
| `hscontrol/db/platform_admin.go:28–78` | `Platform.CreateTailnet` | owner = **cluster của CLI** (`:44`); không placement, không class, không kiểm capacity/đăng ký |
| `hscontrol/db/platform_admin.go:110–157` | `SetTailnetStatus` | chỉ trên tailnet do cluster của CLI sở hữu (`:129`) |
| `hscontrol/db/platform_admin.go:164–200` | `ReassignTailnet` | CAS owner/epoch; đích chỉ cần khác rỗng (`:165–167`): **không** kiểm đã đăng ký/ACTIVE/dedicated (khác `MoveOwner`) |
| `hscontrol/db/schema_postgres.sql:7–21` | `tailnets` | status CHECK không có `MOVING` (`:10`); `moving_to_cluster_id` (`:18`); `idx_tailnets_owner(owner_cluster_id, status)` (`:21`); **không** FK tới `clusters` |
| `hscontrol/db/schema_postgres.sql:154–175` | `clusters` | `state` mặc định `DISABLED`; `noise_public_key` nullable (`:159`); `endpoint_url` unique (`:165`); revision monotonic |
| `hscontrol/db/schema_postgres.sql:176–186` | trigger `owner_epoch` monotonic | |
| `hscontrol/db/schema_postgres.sql:218–234` | `cluster_leases` | PK `cluster_id`, **không** FK tới `clusters`; trigger epoch monotonic |
| `hscontrol/db/tenantscope/tenantscope.go:329–354` | `checkFence` | đọc `tailnets` owner/epoch/status (+ lease, P11b); không đọc `clusters.state` |
| `hscontrol/db/moving.go:8–26`; `hscontrol/types/tailnet.go:30–32` | `movingMigration`, `MovingToClusterID` | **không có đường ghi** trong code sản phẩm (chỉ test `moving_test.go:53`, `bootstrap_test.go:76`) |

### 2.4 Lease — `internal/lease`

| Path:dòng | Symbol | Ghi chú |
|---|---|---|
| `internal/lease/lease.go:37` | `MigrationID = "202611251000-cluster-leases"` | |
| `internal/lease/lease.go:42–43` | comment | "cluster_id has no foreign key to clusters … a cluster that is not registered yet still elects one process". Inventory P11b dự định FK (`source-inventory-P11b.md` §Lựa chọn 1), code chọn không FK |
| `internal/lease/lease.go:133–161, 164, 181, 193` | `Store.Acquire/Renew/Release/Get` | một hàng mỗi cluster |
| `internal/lease/manager.go:29–34, 137` | `DefaultConfig` (TTL 10 s, renew 3 s, acquire 1 s, margin 2 s), `Manager.Run` | |

### 2.5 CLI — `cmd/headscale/cli/cluster.go`

| Path:dòng | Symbol | Ghi chú |
|---|---|---|
| `:45–67` | `init` | `cluster serve/register`, `tailnet create/status/policy/dns/authkey/authkey-revoke/nodes/reassign` |
| `:88–112` | `clusterServeCmd` | |
| `:114–117, 120–155` | `tailnetCmd`, `withPlatform`, `withTenant` | mở platform DB trực tiếp với `--cluster-id` của người gọi |
| `:164–177` | `tailnetCreateCmd` → `Platform.CreateTailnet` | bỏ qua placement |
| `:389–412` | `tailnetReassignCmd` → `Platform.ReassignTailnet` | bỏ qua kiểm đích |
| `:414–448` | `clusterRegisterCmd` | `State: ACTIVE`, endpoint mặc định `server_url`, **không** ghi `NoisePublicKey` (`:437–440`) |

### 2.6 Test và lab

| Path:dòng | Symbol | Ghi chú |
|---|---|---|
| `internal/placement/placement_test.go:117–178` | `TestPlacement` | hòa → id nhỏ nhất (`"tie: lowest id"`); least loaded; DRAINING/DISABLED không được chọn; dedicated một tailnet |
| `internal/placement/placement_test.go:181, 231, 273, 333` | `TestPlacementDedicatedConcurrent`, `TestMoveOwnerStaleEpochRejected`, `TestMoveOwnerConcurrentExactlyOneWins`, `TestEpochNeverDecreases` | SQLite + PostgreSQL |
| `internal/placement/placementapi/api_test.go:245` | `TestOperatorFlow` | hai cluster đăng ký qua API, tạo tailnet |
| `internal/bootstrap/store_test.go:105` | `TestBootstrapStoreFollowsOwner` | sau `MoveOwner` c1→c2 trả URL c2; MOVING; SUSPENDED |
| `internal/bootstrap/bootstrap_test.go:124–304` | 8 test handler | |
| `internal/lease/lease_test.go:73–432` | 12 test lease/manager | một cluster mỗi test |
| `hscontrol/servertest/cluster_p05.go:56–70, 101–117` | `TailnetSeed{Owner}`, `TestCluster{ClusterID}` | **một** cluster ID mỗi `TestCluster`; cluster "khác" chỉ là chuỗi owner, không có process |
| `hscontrol/servertest/cluster_p10.go:19, 57, 139, 167` | `SetTailnetOwner`, `CreateTailnetLive`, `registerCluster`, `UnregisterCluster` | ghi DB trực tiếp, không qua `Registry` |
| `hscontrol/servertest/cluster_p10_test.go:30, 58, 75, 113, 280` | `TestClusterOwnershipMoveRetiresRuntime`, `…SuspendRetiresRuntime`, `…EpochBumpReloadsRuntime`, `…LoadsNewTailnetLive`, `…UnregisteredClusterRefusesEnrollment` | |
| `hscontrol/servertest/cluster_p11b.go:127–272`; `cluster_p11b_test.go:50–248` | `StartProcess`, front door `/ready`; F02–F04 trong servertest | nhiều process **cùng** cluster ID trên một file SQLite |
| `infra/lab/e2e/twocluster/run.sh:9–24, 229–235, 245–247, 360–374, 382` | harness N01–N09 | tailnet tạo bằng CLI (`tailnet create` trên host owner), không qua placement/`mesh-placement`; N06 = stop/start c2 (không phải failover); N07 = `reassign`; chờ `/health` |
| `infra/lab/e2e/p11b/run.sh:1–28` | H01, F01–F04, I01, L01 | chỉ cluster c1 (mc-cp + mc-c1b, HAProxy trên mc-cp) |
| `infra/lab/c2/setup-c2.sh:1–30` | mc-c2 (7705, .244), cluster id `mc-c2`, endpoint `https://c2.cp.gta5web.online:8445`, Noise key riêng | không có standby |
| `infra/lab/{vm,cp,bench,c2,c1b}.tf`, `infra/lab/db/main.tf` | `mc-ci` 7701, `mc-cp` 7702, `mc-bench` 7703, `mc-db` 7704, `mc-c2` 7705, `mc-c1b` 7706 | `stage2-*.tf` (7710–7718) mặc định tắt, topology cũ (`stage2-variables.tf:5–8, 122–130`) |

**Không tìm thấy** (grep trong `internal/`, `hscontrol/`, `cmd/`, bỏ `gen/`): `cluster_leases`/`lease` trong `internal/placement`, `internal/bootstrap`, `cmd/mesh-placement`, `cmd/mesh-bootstrap`; bất kỳ chỗ nào process cluster đọc `clusters.state`, `ClusterDraining`, `ClusterDisabled` hay `EndpointURL`; so sánh Noise key thật với `clusters.noise_public_key`; `rebalance`, `orphan` (ngoài test upstream), `BeginMove`, `AbortMove`, `DrainCluster`; đường ghi `moving_to_cluster_id` ngoài test; servertest có hai cluster ID chạy cùng DB.

## 3. Trả lời phạm vi

### 3.1 Tailnet mới được đặt như thế nào; cluster đang chạy có nhận mà không cần restart không

**Hai đường tạo tailnet, hành vi khác nhau:**

| Đường | Chọn cluster | Kiểm |
|---|---|---|
| `mesh-placement` `POST /v1/tailnets` → `Registry.CreateTailnet` (`tailnets.go:31`) | `place(tx, class, lock=true)` (`clusters.go:271`): cluster cùng `class`, `state = ACTIVE`, còn chỗ (`devices < max_devices`; dedicated: chưa có tailnet nào); ít tải nhất theo `devices/max_devices`, hòa → id nhỏ nhất | `FOR UPDATE` trên các hàng cluster ứng viên (PostgreSQL), tạo `PENDING` + deny-all + audit trong một transaction |
| CLI `cluster tailnet create` → `Platform.CreateTailnet` (`platform_admin.go:28`) | luôn là `--cluster-id` của người gọi (`:44`) | không class, không capacity, không kiểm cluster đã đăng ký/ACTIVE |

Hệ quả đo được từ code (không cần chạy):
- Tải tính **chỉ theo số thiết bị**. Tailnet mới có 0 thiết bị nên không đổi tải: với hai cluster shared rỗng, **mọi** tailnet tạo liên tiếp đều về cluster id nhỏ nhất cho tới khi có thiết bị đăng ký (`clusters.go:313–327`; `TestPlacement` khẳng định "tie: lowest id"). Kịch bản "3 tailnet / 2 cluster" tạo bằng placement trên DB trống sẽ cho 3/0, không phải 2/1. Số tailnet mỗi cluster không được xét với lớp shared (mỗi tailnet là một runtime với batcher/worker riêng, `source-inventory-P08b.md` §3.1).
- Cluster `ACTIVE` trong registry nhưng **không có process nào giữ lease** (chưa từng chạy, chết lâu) vẫn được chọn: `place` không đọc `cluster_leases` (không tìm thấy).
- `PlaceTailnet` (`clusters.go:259`) chỉ xem trước, không khóa; `CreateTailnet` chọn lại trong transaction.
- Đếm thiết bị không khóa bảng `nodes`, là ước lượng mềm (`source-inventory-P10-placement.md:114`).

**Cluster nhận tailnet mới không cần restart, nhưng chỉ ở process active:**
- `ActivateTailnet` (`tailnets.go:104`) đặt `ACTIVE`. `reconcile` (`cluster.go:344`) chạy mỗi `reconcileInterval = 2 s` (`:74`, ticker `:1075–1087`), gọi `OwnedTailnets(ClusterID())` (`:352`) rồi `load` + `start` tailnet chưa có (`:428–450`). Servertest `TestClusterLoadsNewTailnetLive` (`cluster_p10_test.go:113`) chứng minh trong một process.
- Sau P11b: `reconcile` thoát ngay nếu `!ready || !leaseHeld` (`:348–350`) → **standby không nạp gì**. Khi standby lên active, `onLeaseAcquired` (`:805–851`) nạp toàn bộ `OwnedTailnets` tại thời điểm đó. Vậy độ trễ: ≤ 2 s + thời gian `load` trên active; nếu cluster đang failover thì ≤ TTL + acquire (~11 s) rồi nạp.
- Comment `tailnets.go:100–103` ("picks the change up on restart") đã cũ so với code; nên sửa khi làm P12.

### 3.2 "Reconcile giữa cluster" có thể nghĩa là gì; cái gì có, cái gì thiếu

`reconcile` hiện tại là **trong một cluster**: so `OwnedTailnets` với runtime trong bộ nhớ. Không có thành phần nào nhìn toàn bộ registry. Các bất thường có thể xảy ra theo code, và hiện trạng phát hiện/xử lý:

| # | Bất thường | Có thể xảy ra vì | Hiện trạng |
|---|---|---|---|
| R1 | Tailnet `ACTIVE` do cluster **chưa đăng ký** sở hữu | không FK (`schema_postgres.sql:7–21`); `ReassignTailnet` nhận đích bất kỳ (`platform_admin.go:165`); CLI `tailnet create` với `--cluster-id` chưa đăng ký | process cluster đó (nếu chạy) vẫn nạp và phục vụ map; đăng ký thiết bị mới bị từ chối `ErrUnknownCluster` (`quota.go:81–84`, `TestClusterUnregisteredClusterRefusesEnrollment`); bootstrap trả 503 (`bootstrap.go:188–190`). **Không có báo cáo** |
| R2 | Tailnet do cluster `DISABLED` sở hữu | `UpdateClusterState` không đụng tailnet (`clusters.go:135–136`) | process vẫn phục vụ (`OwnedTailnets` không join `clusters`, `platform.go:312–315`); bootstrap 503 → client mới không tới được, client cũ vẫn chạy. Ý nghĩa chưa định (Q3) |
| R3 | Tailnet do cluster `DRAINING` sở hữu | trạng thái hợp lệ | phục vụ bình thường, bootstrap trả URL; không có cơ chế "rút" tailnet (đó là P12b, operator khởi động) |
| R4 | Tailnet `ACTIVE` mà cluster owner **không có process giữ lease** (chưa từng start, chết hẳn, cả active lẫn standby đều chết) | lease độc lập với registry (`lease.go:42–43`) | bootstrap vẫn trả URL (không đọc lease); placement vẫn chọn cluster đó. Client nhận lỗi kết nối. **Không phát hiện** |
| R5 | Cluster đăng ký `ACTIVE` nhưng `endpoint_url` không trỏ tới process thật | `clusterRegisterCmd` mặc định `server_url` (`cli/cluster.go:423–426`), có thể đổi tay | không kiểm ở runtime (không tìm thấy so sánh `EndpointURL` ↔ `server_url`) |
| R6 | `clusters.noise_public_key` rỗng hoặc khác key thật | `cluster register` không ghi (`:437–440`); `UpdateCluster` ghi tay (`clusters.go:95–96`) | không ai so; bootstrap không trả key (client tự lấy `/key` từ cluster). Hợp đồng `05-data-api-contract.md:81` muốn `NOT NULL` "để đối chiếu" |
| R7 | Cluster vượt `max_devices` | `max_devices` hạ sau khi đã có thiết bị, hoặc tailnet chuyển tới bằng `reassign`/`MoveOwner` (MoveOwner không kiểm capacity shared, `tailnets.go:207–228`) | đăng ký mới bị chặn (`clusterBudget`, `quota.go:131–157`); placement bỏ qua cluster đầy; không báo cáo tổng |
| R8 | Dedicated cluster có > 1 tailnet sống | `ReassignTailnet` hoặc CLI `tailnet create` bỏ qua luật dedicated | `MoveOwner`/`place` chặn; đường CLI không chặn. Không phát hiện |
| R9 | Tailnet `PENDING` lâu (tạo nhưng chưa activate) | quy trình operator | không có báo cáo/timeout |
| R10 | `moving_to_cluster_id` bị đặt | chỉ test ghi | bootstrap trả 503 MOVING mãi mãi; không ai dọn. Hiện **không có đường ghi** → rủi ro thấp; P12b sở hữu |
| R11 | Hai cluster cùng phục vụ một tailnet **trong bộ nhớ** | sau `MoveOwner`, nguồn retire và đích load ở hai vòng reconcile độc lập (≤ 2 s mỗi bên) | ghi bị fence ở DB (`checkFence`, `tenantscope.go:329–354`), nên không có owner kép ở **dữ liệu**; nhưng nguồn có thể còn trả MapResponse từ bộ nhớ ≤ ~2 s trong khi đích đã nạp (`cluster.go:69–73` thừa nhận). E2E N07/N08 chưa đo cửa sổ này (`E2E-two-cluster.md` §Hạn chế). Định nghĩa "owner kép" cho P12 cần chốt (Q7) |

**Có:** CAS owner/epoch, fencing ghi, trigger epoch monotonic, retire/load trong mỗi cluster, quota cluster khi đăng ký, placement theo class/state/capacity.
**Thiếu:** (a) bất kỳ chế độ xem/báo cáo xuyên cluster nào (R1–R9); (b) placement biết lease/process sống (R4); (c) đối chiếu endpoint/Noise key thật với registry (R5, R6); (d) đường CLI đi qua cùng luật với `Registry` (R1, R8); (e) phân tán tailnet mới khi tải thiết bị bằng nhau (§3.1); (f) servertest hai cluster ID cùng DB (§3.5).

### 3.3 Bootstrap: tenant → cluster

- **Phân giải:** key_id → một hàng `credentials` (kind pre-auth) ⋈ `tailnets` ⟕ `clusters` (`store.go:20–27`). Tenant lấy **chỉ** từ credential đã xác minh (`bootstrap.go:157–181`). Trả `endpoint_url` của `owner_cluster_id` hiện tại nếu tailnet `ACTIVE`, không xóa, không `moving_to`, cluster đã đăng ký và `ACTIVE`/`DRAINING`; ngược lại 404 đồng nhất hoặc 503 (chỉ cho người có key hợp lệ).
- **Noise key:** **không** trả (`Record` không có trường, body chỉ `control_url`, `issued_at`: `http.go:175–178`). Client lấy key qua `/key` của cluster. Ghi nhận ở `source-inventory-P11.md` §10.5.
- **Cache:** **không có** ở server (mỗi request một transaction READ ONLY, `store.go:69`); phản hồi `Cache-Control: no-store` (`http.go:211`). Quyết định P11 Q5.
- **Ngay sau `MoveOwner`:** request kế tiếp đã đọc owner mới → trả URL cluster đích ngay, **trước** khi đích kịp `reconcile` (≤ 2 s). Client đăng ký trong cửa sổ đó nhận `ErrUnknownTenant` ở đích (`cluster.go:593–597`) và phải thử lại. `MoveOwner` không đặt `moving_to_cluster_id`, nên nhánh 503 MOVING không bao giờ xảy ra trong thực tế hiện nay (P12b sẽ dùng). Nếu đích không có process giữ lease → bootstrap vẫn trả URL (R4).
- **Rate limit:** global 200/s burst 400; tenant 20/s burst 100 (sau xác thực); IP tắt mặc định vì forwarder giấu địa chỉ (`http.go:41–66`, `cmd/mesh-bootstrap/main.go:41–46`). Map bucket giới hạn 100.000 khóa (`http.go:278`).
- **Test:** `TestBootstrapStoreFollowsOwner` (`store_test.go:105`) đã chứng minh trả URL đích sau move trên SQLite + PostgreSQL; E2E N05/N07 với hai cluster thật.

### 3.4 Tương tác với lease P11b

- Mỗi cluster có **một hàng** `cluster_leases` theo `cluster_id` (`schema_postgres.sql:218–224`); process lấy lease bằng `platform.ClusterID()` (`cluster.go:193`), tức `--cluster-id` (`cli/cluster.go:76–81`). Hai cluster không tranh lease của nhau.
- **Không FK** giữa `cluster_leases.cluster_id`, `tailnets.owner_cluster_id` và `clusters.id` (`lease.go:42–43`; `source-inventory-P10-placement.md:42`). PLAN `:194–210` và `05-data-api-contract.md:74–86` minh họa có FK và `state IN ('READY','DRAINING','MAINTENANCE','UNAVAILABLE')`: tài liệu lệch code (`ACTIVE/DRAINING/DISABLED`). P12 không tự thêm FK (đụng DB lab đang có cluster chưa đăng ký, ví dụ `local`/`mc-cp-c2` trong harness P05) mà ghi vào câu hỏi.
- Hệ quả: process với `--cluster-id` gõ sai vẫn lấy được lease riêng và phục vụ những tailnet (nếu có) mang owner đó; không ảnh hưởng cluster khác vì `TenantDB`/`checkFence` dùng đúng ID đó (`platform.go:348`). Không phải lỗi isolation, nhưng là nguồn của R1/R4.
- Failover không đổi `owner_epoch` (ADR 0005 Bổ sung 2 §2; `tickets/P11b.md:20`); P12 không được trộn hai epoch.
- Placement và bootstrap hiện **không** biết lease. Nếu P12 cho placement/báo cáo đọc `cluster_leases` thì chỉ đọc (`Store.Get`, `lease.go:193`), không bao giờ ghi.

### 3.5 Test hiện có và khoảng trống so với nghiệm thu "3 tailnet / 2 cluster, restart/failover trên cả hai"

| Lớp | Có | Thiếu |
|---|---|---|
| Unit placement | `TestPlacement` (class/state/capacity/hòa), concurrent dedicated, MoveOwner CAS/stale/concurrent, epoch monotonic, API flow hai cluster | phân tán tailnet mới khi tải bằng nhau; bỏ cluster không có lease; báo cáo R1–R9 |
| Unit bootstrap | follow owner sau MoveOwner, MOVING, DISABLED/chưa đăng ký → 503 | hành vi khi owner không có lease (nếu Q2 chọn) |
| Servertest | một process/nhiều process **cùng** cluster ID; move tới "another-cluster" chỉ là chuỗi (`cluster_p10_test.go:30–55`) | **hai cluster ID chạy cùng DB**: đích nạp tailnet được placement đặt, nguồn retire, không lúc nào cả hai nhận ghi; cửa sổ R11 |
| E2E hai cluster (`twocluster/run.sh`) | 3 tailnet/2 cluster (a, c ở c1; b ở c2) **tạo bằng CLI**; bootstrap theo owner; F07; restart c2 (N06); move bằng `reassign` (N07) | tạo qua `mesh-placement` (chưa chạy trên lab: `source-inventory-P10.md:96`); restart **c1**; **failover** trên c1 và c2 cùng lúc có hai cluster; chưa chạy lại sau P11b (harness chờ `/health`, nên chờ `/ready`: `P11b-cluster-failover.md:155`) |
| E2E P11b (`p11b/run.sh`) | F01–F04, I01, L01 trên **c1** (mc-cp + mc-c1b + HAProxy) | c2 **không có standby** (không có VM, không có HAProxy cho `:8445`) → "failover trên cả hai" chưa thể chạy |

## 4. Constraints hiện có

- AGENTS 1: code ở `feat/P12-<tên>` từ `master`, không tự merge. AGENTS 5: inventory này là file đầu của PR.
- AGENTS 3–4: agent tự `tofu apply` plan **chỉ create/update** VM trong pool `meshcontrol-lab`, VMID 7700–7799; destroy/replace, firewall, mạng host, VPS → người duyệt. Credential Proxmox chỉ ở `%USERPROFILE%\.mesh-lab\proxmox.env`.
- AGENTS 6: placement/báo cáo là thao tác **quản trị nền tảng** (operator mTLS), không phải API tenant; không trả dữ liệu tenant, chỉ ID/đếm (như `clusterUsage`, `clusters.go:378`). Mọi SQL thô mới phải review tay và vào allowlist `TestNoRawTenantWritesOutsideFence` (`source-inventory-P10.md:73`).
- AGENTS 10: đổi `placementapi` → cập nhật spec qua generator đã pin (`go run ./cmd/gen-openapi -api placement` / `mesh-placement -openapi`, `source-inventory-P10-placement.md:67`); không sửa `gen/` tay.
- AGENTS 11: nếu cần cột/FK/CHECK mới (ví dụ FK `owner_cluster_id`), chỉ thêm migration **cuối** `v01Migrations` (`platform.go:46–61`), trong transaction, DDL SQLite + PostgreSQL, schema file khớp.
- AGENTS 12 / ADR 0005: **một Noise key mỗi cluster**, dùng chung active/standby. P12 không đổi; chỉ có thể đối chiếu public key với registry.
- ADR 0005 Bổ sung 2 §1: bootstrap chỉ trả URL owner ở epoch hiện tại; cluster luôn tự kiểm owner/epoch/lease, không tin bootstrap.
- I12: một owner (`owner_cluster_id`, `owner_epoch`) và một holder lease mỗi cluster; mọi mutation kiểm cả hai trong cùng transaction; epoch không lùi (trigger `schema_postgres.sql:176–186, 225–234`).
- ADR 0003:41 + `tickets/P12b.md` §Phạm vi: move giữa cluster do **operator** khởi động, có duyệt; **không tự động** trong MVP. Thủ tục move (MOVING, thông báo, drain) là P12b.
- AGENTS 13: lỗi isolation hoặc owner kép = BLOCK MERGE. Go race/servertest không chứng minh Noise/client; nghiệm thu cần client thật.

## 5. Giả định còn mở

- **G1:** harness `twocluster/run.sh` vẫn chạy được sau P11b (mỗi cluster một process tự lấy lease ≤ 1 s) — chưa chạy lại ở `b2b5d8d`; harness chờ `/health` thay vì `/ready`.
- **G2:** servertest chạy được **hai** `Cluster` với hai cluster ID trên một file SQLite (P11b đã chạy nhiều process cùng ID trên một file, `cluster_p11b.go:127`, `busy_timeout(10000)` ở `cluster_p05.go:305`). Chưa thử với ID khác nhau; nếu khóa SQLite gây flaky thì phần đó phải chạy trên PostgreSQL ở mc-ci.
- **G3:** "reconcile giữa cluster" trong PLAN nghĩa là **phát hiện + làm cho registry nhất quán** (báo cáo R1–R9, placement đúng), không phải tự động chuyển tailnet. Dựa trên ADR 0003:41 và `tickets/P12b.md` ("Tự động chọn thời điểm move hoặc placement tối ưu" ngoài phạm vi). Cần xác nhận (Q4).
- **G4:** `endpoint_url` mỗi cluster ổn định (không có hostname theo tenant, PLAN `:37`); active và standby cùng URL qua front door (P11b).
- **G5:** đếm thiết bị không khóa là đủ cho placement (ước lượng mềm); giới hạn cứng nằm ở `EnrollmentAllowedTx`.
- **G6:** lab cho "failover trên cả hai" cần VM standby cho c2 và một front door `/ready` cho `:8445`; giả định đặt HAProxy thứ hai trên mc-cp như P11b (mc-cp đã relay `:8445` → mc-c2). Chưa kiểm cấu hình relay hiện tại trên mc-cp.
- **G7:** P08b merge trước P12 sẽ gây xung đột nhỏ ở `placementapi/api.go` và `cluster.go`; giả định rebase được, không xung đột ngữ nghĩa.

## 6. Kế hoạch [Đề xuất] (RED trước, PR nhỏ)

1. **RED placement** (`internal/placement`, SQLite + PostgreSQL):
   - `TestPlacementSpreadsNewTailnets`: hai cluster shared rỗng, tạo 3 tailnet → không phải 3/0 (luật hòa theo Q1). Hiện FAIL (3/0).
   - `TestPlacementSkipsClusterWithoutLiveLease` (nếu Q2 = có): cluster ACTIVE không có lease sống không được chọn; không có cluster nào → `ErrNoCapacity`.
   - `TestReassignRequiresRegisteredActiveTarget` / `TestCLICreateUsesPlacement` (nếu Q5 = qua Registry).
2. **RED báo cáo reconcile** [tên tạm `Registry.Reconcile(ctx) (Report, error)`, `GET /v1/reconcile` operator-only, chỉ đọc]: mỗi R1–R9 một ca dữ liệu → đúng một mục báo cáo; dữ liệu sạch → báo cáo rỗng; không trả slug/tên tenant ngoài ID (I15). Spec OpenAPI qua generator.
3. **RED servertest hai cluster** (helper mới trong `hscontrol/servertest`, ví dụ `StartCluster(tb, clusterID)` trên cùng DB):
   - tailnet tạo bằng `Registry.CreateTailnet` + `ActivateTailnet` được **đúng** owner nạp trong ≤ 2 interval, cluster kia không bao giờ có runtime;
   - `MoveOwner` c1 → c2: c1 retire, c2 nạp; ghi từ c1 sau move bị `ErrFenced`; ghi lại thời điểm hai bên (đo cửa sổ R11);
   - 3 tailnet / 2 cluster: không peer/IP/node chéo (I02, I03, S05);
   - failover trong c2 (hai process c2) không đổi owner_epoch và không làm c1 nạp tailnet của c2.
4. **Cluster side** theo Q3/Q6: (a) xử lý `clusters.state`/đăng ký khi nạp (nếu chủ dự án chọn); (b) đối chiếu `noise_public_key` khi lấy lease: rỗng → ghi (nếu được phép), khác → từ chối phục vụ (fail closed) và log. Mỗi thay đổi có RED.
5. **CLI** theo Q5: `tailnet create --class` và `reassign` đi qua `placement.Registry` (cùng luật, có audit).
6. **Lab** (sau duyệt Q8): VM standby c2 (ví dụ `mc-c2b`, VMID còn trống trong 7700–7799, create-only → agent tự apply được theo rule 3), front door `/ready` cho c2, chạy `mesh-placement` loopback mTLS trên mc-cp. Diff hạ tầng đụng mc-cp/VPS trình người duyệt.
7. **E2E** mở rộng `twocluster/run.sh` (chờ `/ready`): tạo 3 tailnet qua `mesh-placement`; bootstrap trả đúng cluster cho cả 3; restart c1 và c2; F01 (và F02 nếu Q9) trên c1 và trên c2 khi cluster kia đang phục vụ; báo cáo reconcile sạch trước/sau; audit `owner_epoch` + `cluster_leases` không có owner/holder kép; artifact không có secret. Chạy ×2 với client v1.104.7 và v1.84.3.
8. Sửa comment `tailnets.go:100–103`; cập nhật PLAN/ADR nếu chủ dự án đổi ý nghĩa `DISABLED` hoặc thêm FK.

## 7. Câu hỏi cho chủ dự án

- **Q1 — Luật placement cho tailnet mới.** Hiện chỉ theo `devices/max_devices`, hòa → id nhỏ nhất, nên tailnet mới (0 thiết bị) dồn hết vào một cluster. Chọn: (a) giữ nguyên; (b) **[Đề xuất]** hòa theo số tailnet sống của cluster, rồi id; (c) tính mỗi tailnet như một lượng thiết bị dự kiến tối thiểu (cần con số); (d) trọng số khác.
- **Q2 — Placement có bỏ qua cluster không có process giữ lease không?** **[Đề xuất]** có (placement đọc `cluster_leases`, chỉ đọc). Bootstrap có nên trả 503 khi owner không có lease sống không, hay giữ nguyên (client thử lại, cluster vẫn fail closed)? **[Đề xuất]** giữ nguyên bootstrap.
- **Q3 — `DISABLED`/`DRAINING`/chưa đăng ký đối với process đang chạy.** Hiện process vẫn phục vụ mọi tailnet nó sở hữu bất kể `clusters.state`; bootstrap trả 503 cho `DISABLED`. Có giữ nguyên (state chỉ là tín hiệu cho placement/bootstrap) không, hay cluster `DISABLED`/chưa đăng ký phải ngừng nạp tailnet? **[Đề xuất]** giữ nguyên và chỉ báo cáo (R1, R2), vì ngừng phục vụ là cắt dịch vụ của tenant.
- **Q4 — Phạm vi "reconcile giữa cluster".** **[Đề xuất]** chỉ phát hiện + báo cáo (R1–R9) qua API operator, operator tự xử lý; **không** tự động chuyển hay cân bằng lại (ADR 0003:41). Auto-rebalance có thuộc P12 không?
- **Q5 — Đường CLI `tailnet create` và `tailnet reassign`.** Hai lệnh này bỏ qua luật của `Registry` (class, capacity, đích đã đăng ký/ACTIVE, dedicated). Cho chúng đi qua `placement.Registry` (**[Đề xuất]**), hay giữ làm công cụ lab và chỉ dùng `mesh-placement` cho N2?
- **Q6 — Noise public key trong registry.** Cluster có ghi/đối chiếu `clusters.noise_public_key` khi lấy lease không (khác → từ chối phục vụ, fail closed)? Có thêm migration `NOT NULL` như `05-data-api-contract.md:81` không? Bootstrap vẫn không trả key? **[Đề xuất]** đối chiếu khi lấy lease, ghi lần đầu nếu rỗng, không đổi schema, bootstrap không trả key.
- **Q7 — Định nghĩa "không owner kép" để nghiệm thu.** Ghi đã bị fence ở DB, nhưng sau `MoveOwner` cluster nguồn có thể còn trả MapResponse từ bộ nhớ ≤ ~2 s trong khi đích đã nạp (R11). Chấp nhận cửa sổ này (đo và ghi lại) trong P12, hay P12 phải bảo đảm đích chỉ nạp sau khi nguồn đã retire (cần phối hợp giữa cluster, có thể đẩy sang P12b)?
- **Q8 — Lab.** Duyệt: (a) VM standby cho c2 (`mc-c2b`, 4 vCPU/8 GB như `mc-c1b`, VMID trống trong dải lab); (b) front door `/ready` cho c2 trên mc-cp thay relay `:8445` hiện tại; (c) chạy `mesh-placement` (mTLS, loopback) trên mc-cp cho E2E. (b) và (c) đổi dịch vụ trên mc-cp nên cần diff được duyệt.
- **Q9 — Mức nghiệm thu "failover trên cả hai".** F01 (kill -9 active) trên mỗi cluster có đủ không, hay cần F01–F04 trên cả c1 và c2 khi hai cluster cùng chạy?
- **Q10 — FK registry.** Thêm FK `tailnets.owner_cluster_id → clusters(id)` (và/hoặc `cluster_leases.cluster_id`) bằng migration cuối, hay giữ không FK và chỉ báo cáo R1? **[Đề xuất]** giữ không FK trong P12 (DB lab có cluster chưa đăng ký), báo cáo R1; cập nhật PLAN/`05-data-api-contract.md` cho khớp `ACTIVE/DRAINING/DISABLED`.
- **Q11 — Thứ tự với P08b.** P12 bắt đầu code sau khi P08b merge (tránh xung đột `placementapi/api.go`, `cluster.go`), hay song song rồi rebase?

> **2026-11-25:** chủ dự án trả lời "theo đề xuất hết", Q7 đo trong P12. Bản ghi quyết định: `tickets/P12.md` §Quyết định.
