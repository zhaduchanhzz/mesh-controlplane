# Source Inventory — P08b (giới hạn theo tailnet, API vận hành và API v2 theo tailnet ở cluster mode, DERP verify, metrics theo tenant)

- **Trạng thái:** `Planned` (chỉ inventory + ticket nháp). Không đổi code. Chưa `Reviewed`.
- **Nhánh:** `feat/P08b-inventory` từ `master` @ `fcce29a` (merge `feat/E2E-postgres`).
- **Ticket:** `tickets/P08b.md` (nháp). PLAN chưa có dòng P08b riêng (PLAN.md:277 chỉ có P08); xem Q1.
- **Quy ước:** mọi trích dẫn dạng `path:dòng` tại `fcce29a`. "Không tìm thấy" = đã grep và không có, không phải suy đoán. Mục đánh dấu **[Đề xuất]** chờ chủ dự án duyệt.

## 1. Tài liệu đã đọc

| Tài liệu | Phần liên quan |
|---|---|
| `AGENTS.md` | rule 6 (fail closed khi thiếu scope), 7, 9 (debug/metrics không ra listener public), 13; chu trình ticket; mẫu evidence |
| `PLAN.md:277–278` | P08 "API/admin/metrics/debug/queue limits/DERP"; P09 (Gate N1) cần S01–S18. `PLAN.md:325`: queue/rate limit, giới hạn kết nối PG theo cluster |
| `03-isolation-threat-model.md:36–42` | I09 (debug/metrics/API không lộ), I10 (rate limit pending enrollment/map streams/quota/bootstrap theo tailnet), I11 (DERP verify tenant-aware), I15 (không leak qua error/timing/log) |
| `06-tests-benchmarks.md:21–23` | S13 (flood A, B giữ SLO), S14 (endpoint public), S15 (DERP verify unknown/revoked, A/B, sau failover) |
| `evidence/source-inventory-P08.md:34, 74–76` | F4 (API admin v2 theo tailnet) → P08b; DERP verify cho DERP ngoài ở cluster mode → P08b; rate/queue limit → P14 |
| `evidence/HANDOFF-P08-wip.md:44–47` | "Để P08b": F4 (`TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` SKIP ≠ PASS), S15 cần lab giai đoạn 2; I10/S13 → **P14** |
| `evidence/source-inventory-P00d.md:213, 302, 420–421`, `:326` | M06 phải chạy một tailnet vì cluster không có API xóa node/user khi đang chạy; metric divergence cộng dồn mọi tenant (R10); `DestroyUser` từ chối user còn node |
| `evidence/source-inventory-P07.md:26–29, 107` | metric toàn process, không label tenant; `Cluster.HTTPHandler` không có route debug/admin/metrics |
| `evidence/source-inventory-P08a.md` §3 | 404 (không phải 403) cho tailnet khác: đề xuất, chưa duyệt |
| ADR 0007 §4 (`design/adr/0007-…md:43–45`) | API admin theo tailnet, metrics, debug, DERP verify theo tenant để ticket sau |
| ADR 0008 §4 (`design/adr/0008-per-tailnet-dns.md:45–47, 93`) | ghi DNS từ phía tenant qua API v2 để ticket sau (P08b hoặc P12), cần review authZ + giới hạn tần suất |
| ADR 0006 `:52–53`; `infra/lab/db/setup-db.sh:50–53` | PgBouncer `pool_mode=transaction`, `default_pool_size=20`, `max_client_conn=2000` (lab); đề xuất pool riêng cho lease |

## 2. Path và symbol đã đọc

| Path:dòng | Symbol | Ghi chú cho P08b |
|---|---|---|
| `hscontrol/cluster.go:37–39, 209–210` | `errClusterEmbeddedDERP`, `validateClusterConfig` | Cluster từ chối DERP nhúng; thông điệp nói rõ "per-tailnet DERP verify in cluster mode is P08b" |
| `hscontrol/cluster.go:64–69` | `reconcileInterval = 2s` | chu kỳ áp thay đổi từ DB vào runtime |
| `hscontrol/cluster.go:78–112, 114–131` | `Cluster`, `tenantRuntime{policyRev, configRev}` | một listener, một pool DB, một key; mỗi tailnet một `*Headscale` (State, batcher, GC riêng) |
| `hscontrol/cluster.go:233–299` | `Cluster.load` | `NewStateWithDB` trên handle tenant dùng chung pool; `SetEnrollGate` (quota trong transaction insert node) |
| `hscontrol/cluster.go:329–430` | `Cluster.reconcile` | so `owner_epoch`, `policy_revision`, `config_revision`; policy → `ReloadPolicy`, DNS → `ReplaceTenantDNS`; **không** có revision cho node/user |
| `hscontrol/cluster.go:582–611, 619–631, 635–655, 657–659` | `runtimeForRegister`, `knownNode`, `checkQuota`, `runtimeForNode` | tra platform DB trước khi bind; `checkQuota` timeout 5 s |
| `hscontrol/cluster.go:701–721` | `Cluster.PingResponseHandler` | duyệt mọi runtime |
| `hscontrol/cluster.go:724–737` | `Cluster.HealthHandler` | `/health` public → `platform.Ping` mỗi request, không giới hạn |
| `hscontrol/cluster.go:741–744` | `clusterRegistrationDisabled` | `/register/*`, `/auth/*` trả 404 |
| `hscontrol/cluster.go:795–800, 806–830` | `MetricsHandler`, `HTTPHandler` | listener public chỉ có `/ts2021`, `/key`, `/health`, `/version`, `/robots.txt`, ping-response; **không** `/api/*`, **không** `/verify` |
| `hscontrol/cluster.go:878–899, 908–920` | `Serve` | listener metrics riêng (operator-only); vòng reconcile |
| `cmd/headscale/cli/cluster.go:45–67` | `init` | lệnh: `cluster serve/register`, `tailnet create/status/policy/dns/authkey/authkey-revoke/nodes/reassign` |
| `cmd/headscale/cli/cluster.go:114–155` | `tailnetCmd` ("a running cluster follows within seconds"), `withPlatform`, `withTenant` | CLI mở platform DB trực tiếp, không nói chuyện với process đang chạy |
| `cmd/headscale/cli/cluster.go:199–236` | `tailnetPolicyCmd` | `hs.SetPolicy` + `p.BumpPolicyRevision` → reconcile nạp lại |
| `cmd/headscale/cli/cluster.go:310–354` | `tailnetAuthKeyCmd`, `tailnetAuthKeyRevokeCmd` | tạo user/khóa trực tiếp trong DB; thu hồi khóa không ảnh hưởng node đã đăng ký |
| `cmd/headscale/cli/cluster.go:356–387` | `tailnetNodesCmd` | chỉ đọc |
| `hscontrol/db/platform.go:75–141, 175–219, 222–249, 276–317` | `OpenPlatform`, `TailnetForAuthKey`, `TailnetForNode`, `TenantDB` | `TenantDB` trả handle scoped **trên cùng pool** (`sharedPool: true`) |
| `hscontrol/db/platform_admin.go:28, 81, 110, 164, 205` | `CreateTailnet`, `TailnetBySlug`, `SetTailnetStatus`, `ReassignTailnet`, `BumpPolicyRevision` | đường DB của operator |
| `hscontrol/db/db.go:39, 149–150, 185–186`; `hscontrol/config/default.go:52`; `hscontrol/types/config.go:566` | `contextTimeout = 10s`, `SetMaxOpenConns` | PG: `max_open_conns` mặc định 10, **một pool cho mọi runtime + platform** |
| `hscontrol/db/secret.go:32–40, 56–64` | `hashPrefixSHA256`, `legacyHashLimiter` | xác minh secret là SHA-256 (rẻ); bcrypt/argon2 cũ có limiter toàn process |
| `hscontrol/db/schema_postgres.sql:43–69, 119` | `credentials(tailnet_id NOT NULL, kind IN ('api','authkey','oauth_client','oauth_token'))`, `idx_credentials_identifier(kind, identifier)` UNIQUE, `idx_nodes_machine_key_node_key` | **API key đã gắn tailnet ở schema**; identifier duy nhất toàn cục theo kind → tra tailnet theo khóa như `TailnetForAuthKey` được |
| `hscontrol/db/api_key.go:136–148, 198–223` | `AuthenticateAPIKey`, `validateAPIKey` | qua GORM trên handle tenant → bị `tenantscope` lọc theo tailnet |
| `hscontrol/db/tenantscope/tenantscope.go:23–24, 58, 109, 131` | `Register`, `scopeWhere`, `scopeMutation` | SQL thô không được lọc, phải review tay |
| `hscontrol/db/users.go:53–64` | `DestroyUser` | từ chối khi user còn node |
| `hscontrol/api/v2/api.go:37–45, 63–67, 102–125, 174–240, 292–296` | `Backend{State,Change,Cfg,Connected}`, `Register`, `NewAPI`, `authMiddleware`, `requireDefaultTailnet` | API key admin = all-access trong phạm vi `State` được truyền vào; `{tailnet}` chỉ nhận `-` |
| `hscontrol/api/v2/*.go` (`Path:`) | 18 path: `/api/v2/tailnet/{tailnet}/{acl,acl/validate,devices,keys,keys/{keyId},settings,users}`, `/api/v2/device/{id}[/attributes,/authorized,/expire,/key,/name,/routes,/tags,/update-client,/update-status]`, `/api/v2/users/{id}` | `requireDefaultTailnet` gọi ở 12 chỗ |
| `hscontrol/app.go:604–662, 764–776, 1428–1443` | `createRouter`, socket v2, `Headscale.HTTPHandler` | single-tailnet mount v1/v2 trên router public (`app.go:662`), unix socket local trust (`app.go:776`) |
| `hscontrol/servertest/p08a_apiv2_tailnet_scope_test.go:29–33, 72–119` | `p08aTwoTailnets` (luôn `ok=false`), `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` | **SKIP**; hợp đồng: 404 cho tailnet/device khác, `-` = tailnet của khóa, v1 không phục vụ khóa tenant |
| `hscontrol/noise.go:76–80, 131–239, 242–254` | `noiseBodyLimit` 1 MiB, `serveNoiseSession`, `newNoiseHTTPServer` | mọi machine key được tunnel; timeout 30 s (`types/const.go:6`); **không** giới hạn số phiên theo IP/tổng |
| `hscontrol/noise.go:1081–1156` | `RegistrationHandler` | bind tenant trước mọi side effect; lỗi tenant → 200 + `Error` |
| `hscontrol/noise.go:853–877` | `PollNetMapHandler` | `bindForNode` → `enter` |
| `hscontrol/session_route.go:100–151` | `bindForRegister`, `bindForNode`, `enter`, `requireBound` | `inflight` gate theo runtime (P10 B1) |
| `hscontrol/auth.go:156–177, 408–430, 545–575` | `handleRegister`, `reqToNewRegisterResponse`, `handleRegisterInteractive` | node đã biết gửi register không kèm auth key → `SetAuthCacheEntry` trong **runtime của nó** |
| `hscontrol/state/state.go:53–55, 246–254, 368–377, 2539–2590` | `authCacheSSHReserve=64`, `authCache` (expirable LRU), `NewStateWithDB`, `SetAuthCacheEntry` (`ErrAuthCacheFull`, không evict) | **mỗi runtime một cache**; mặc định 1024 mục / 15 phút (`config/default.go:88–89`) |
| `hscontrol/state/state.go:710, 836, 1027, 1359, 1379, 1512, 1599, 1659, 2459, 2464` | `CreateUser`, `DeleteUser`, `DeleteNode`, `SetNodeExpiry`, `ExpireNodeAt`, `SetNodeTags`, `SetApprovedRoutes`, `RenameNode`, `ExpirePreAuthKey`, `DeletePreAuthKey` | mutation có trả `change.Change` → muốn hiệu lực live phải gọi trên State **của runtime đang chạy** |
| `hscontrol/state/state.go:644–661, 3644, 3822, 4229–4255` | `ReloadPolicy`, `HandleNodeFromPreAuthKey` → `updatePolicyManagerUsers` | user tạo bằng CLI được PolicyManager thấy ở lần đăng ký kế tiếp |
| `hscontrol/state/node_store.go:51–104, 120, 157, 325–336, 525, 756` | metric NodeStore, `writeQueue` (unbuffered), `nodeStoreNodesCount.Set`, `nodeStoreQueueDepth` | một writer goroutine mỗi runtime; gauge `nodestore_nodes` là **package-level, mỗi runtime `Set` đè** |
| `hscontrol/state/node_store_reconcile.go:22–40`; `state.go:2258` | `nodestore_netmap_divergence_total{kind}`, `ha_health_updates_total{result}` | không label tenant |
| `hscontrol/mapper/node_conn.go:26, 58, 256–273` | `maxConcurrentMapSessionsPerNode = 4`, `maxPendingSize = 256`, `addConnectionWithLimit` | giới hạn theo **node**; quá 4 thì thay phiên cũ nhất |
| `hscontrol/mapper/batcher.go:35–47, 49–58, 65, 353–356` | `mapResponseGenerated`, `mapperChangesDropped`, `NewBatcher` (`workCh` = workers×200) | **mỗi runtime một batcher**, `workers` = 3/4 số CPU (`types/common.go:342–356`) → N tailnet = N×workers goroutine trên cùng CPU |
| `hscontrol/metrics.go:11–39` | `mapresponse_sent_total{status,type}`, `…_endpoint_updates_total`, `…_ended_total{reason}`, `mapresponse_last_sent_seconds{type,id}` (chỉ khi high-cardinality) | |
| `hscontrol/handlers.go:161–193, 195–241` | `handleVerifyRequest`, `VerifyHandler`, `verifyCallerAuthenticated` | admit khi node key thuộc node còn hạn **của runtime này** |
| `hscontrol/derp/server/derp_server.go:65–80, 407–440` | `NewDERPServer(verifyURL)`, `RegisterVerifyHandler`, `verifyDispatcher` | chỉ dùng cho DERP nhúng (bị cấm ở cluster) |
| `hscontrol/servertest/cluster_p06_test.go:461` | `TestClusterP06PolicyDERPMapStaysInTailnet` | derpMap theo policy của A không tới B |
| `internal/bootstrap/http.go:41–65, 125–170, 223–230, 269–300` | `HandlerConfig{Global,Tenant,IP}Rate`, `bucket`, `buckets` (`maxKeys=100_000`) | **mô hình** rate limit: global → parse → tenant (chỉ tính sau khi credential đã xác minh) |
| `internal/placement/placement.go:139`; `quota.go:18` | `DefaultSharedTenantLimit = 5000` | quota thiết bị theo tailnet (P10) |

Không tìm thấy: `rate.NewLimiter`, `x/time/rate`, `semaphore`, `netutil.LimitListener` trong `hscontrol/`, `internal/`, `cmd/` (ngoài bucket tự viết của bootstrap); route `/verify` hoặc `/api` trong `Cluster.HTTPHandler`; CLI cluster cho xóa/hết hạn node, xóa user, route, API key; giới hạn số tailnet mỗi cluster.

## 3. Trả lời phạm vi

### 3.1 Giới hạn theo tailnet / DoS (I10, S13)

| Tài nguyên | Giới hạn hiện có | Phạm vi | Bằng chứng |
|---|---|---|---|
| Phiên Noise (handshake, tunnel) | Không giới hạn số phiên; chỉ timeout 30 s + body 1 MiB | toàn process, **trước** khi biết tenant | `noise.go:131–254`; không tìm thấy limiter |
| Kết nối TCP/TLS listener | Không `MaxConns`; `ReadTimeout`/`WriteTimeout` 30 s | toàn process | `cluster.go:856–862` |
| Map stream | 4 phiên/node (thay phiên cũ); số node ≤ quota 5000/tailnet (shared) | theo node; gián tiếp theo tailnet | `node_conn.go:26`, `batcher.go:353–356`, `placement.go:139` |
| Pending registration (auth cache) | 1024 mục, 15 phút, không evict, +64 cho SSH | **theo runtime** (mỗi State một cache). Chỉ node đã biết của tailnet đó tạo được mục (web/OIDC tắt ở cluster) | `state.go:53–55, 246–254, 2539–2590`; `auth.go:567`; `cluster.go:741` |
| Batcher | `workCh` = workers×200; pending/node 256 → gộp thành full; buffer phiên 30 | theo runtime; CPU dùng chung | `batcher.go:49–58`, `node_conn.go:58`, `config/default.go:85` |
| NodeStore write queue | channel không buffer, một writer; caller chặn tới khi batch áp dụng (batch 100 / 500 ms) | theo runtime | `node_store.go:120, 325–336, 525`; `config/default.go:86–87` |
| Peer entries | `max_peer_entries` 1 000 000 | theo tailnet | `types/config.go:1028–1032`, `config/default.go:90` |
| DNS tenant | 1000 record, 64 NS, 64 search, 256 KiB | theo tailnet | ADR 0008 §3 |
| Pool DB | `max_open_conns` 10 (PG) cho **mọi runtime + platform lookup + `/health` + reconcile**; timeout 10 s; PgBouncer lab pool 20 | **toàn process / toàn cluster** | `db.go:39, 185–186`, `default.go:52`, `platform.go:276–317`, `setup-db.sh:52` |
| Lookup platform trước bind | mỗi register: `TailnetForAuthKey` (1 query) + `knownNode` (1–2) + `checkQuota`; mỗi map/register bằng node key: `TailnetForNode` | toàn process, **chưa xác thực** với node key ngẫu nhiên | `cluster.go:582–659`, `platform.go:175–249` |
| `/health` | `platform.Ping` mỗi request, không giới hạn | public, toàn process | `cluster.go:724–737` |
| Bootstrap | global 200/s, tenant 20/s, IP tùy chọn | đã có (P11) | `internal/bootstrap/http.go:59–65` |

**Kết luận 3.1:** các giới hạn hiện có hoặc theo node, hoặc theo runtime (auth cache, batcher, NodeStore) — tức là cô lập về **bộ nhớ/hàng đợi**. Nhưng A vẫn có thể làm B chậm qua bốn tài nguyên chung: (1) **pool DB** 10 kết nối (A có reusable key tạo/đăng nhập lại node liên tục, hoặc nhiều node A gửi MapRequest đổi endpoint → write) → lookup của B chờ tới 10 s rồi lỗi; (2) **CPU/goroutine** (mỗi runtime 3/4 số CPU worker, không có fairness); (3) **listener/phiên Noise** không giới hạn, kẻ tấn công không cần credential; (4) `/health` public đánh DB. Chưa có test flood nào (S13 chưa có). HANDOFF-P08 (`:47`) đã chuyển I10/S13 sang P14 — mâu thuẫn với phạm vi giao cho P08b (Q1).

### 3.2 API vận hành cho cluster mode

**Có:** `tailnet create/status/policy/dns/authkey/authkey-revoke/nodes/reassign`, `cluster register/serve` (`cli/cluster.go:45–67`). Tất cả mở platform DB trực tiếp (`cli/cluster.go:120–155`); không có kênh tới process đang chạy (không gRPC, không unix socket ở cluster: `cluster.go:806–830`).

**Cách một thay đổi DB tới runtime đang chạy:** chỉ qua cột revision trên `tailnets` mà `reconcile` so mỗi 2 s: `owner_epoch` (retire/load), `policy_revision` (`ReloadPolicy` + `Change`), `config_revision` (DNS) (`cluster.go:329–400`). Trạng thái (SUSPENDED/DELETING) đi qua `OwnedTailnets` → retire cả runtime.

**Thiếu (không tìm thấy):** xóa node, hết hạn node, xóa user, đổi tên, tag, liệt kê/duyệt route, tạo/thu hồi API key cho tailnet. Lý do không thể "chỉ ghi DB": NodeStore và PolicyManager là bộ nhớ của runtime; `State.DeleteNode` (`state.go:1027`) vừa ghi DB vừa cập nhật NodeStore và trả `change.NodeRemoved(...)` cho batcher. Xóa dòng DB từ CLI sẽ không làm peer biến mất (và còn lệch NodeStore↔DB tới lần nạp lại). Đây là nguyên nhân M06 phải chạy một tailnet (`source-inventory-P00d.md:420`).

**Hai hướng [Đề xuất]:**
- **A (ưu tiên):** endpoint quản trị nội bộ trên **listener operator-only riêng** (cùng gate `operatorOnly` với metrics, `cluster.go:795–800`, hoặc unix socket chỉ root), route `…/tailnet/{id}/…`, handler lấy runtime bằng `Cluster.Runtime(id)` (`cluster.go:313`) rồi gọi `State.DeleteNode/SetNodeExpiry/DeleteUser/SetApprovedRoutes` + `h.Change(...)`. Tailnet không do process này phục vụ → 404. CLI `cluster tailnet node delete …` gọi endpoint đó. Hiệu lực ngay, cùng đường delta như M06.
- **B:** bảng lệnh/`admin_revision` trong DB + reconcile thực thi. Chạy được cả khi CLI không chạm tới process, nhưng thêm migration, độ trễ ≤ 2 s, cần idempotency và fencing epoch.

### 3.3 API v2 theo tailnet (F4)

- Cluster mode **không mount** `/api/v1`, `/api/v2` (`cluster.go:806–830`; E2E C02 trong `source-inventory-P08.md` §4). Fail closed, nhưng không có API nào cho tenant.
- **"Khóa admin scoped của P08a" chưa tồn tại dưới dạng code.** P08a chỉ để lại hợp đồng trong test SKIP: `p08aTwoTailnets` luôn trả `ok=false` (`p08a_apiv2_tailnet_scope_test.go:29–33`), test bị SKIP (`:72–78`). Hợp đồng: 404 cho `/tailnet/{B}` và `/device/{B}` (không phân biệt "không tồn tại"), `-` = tailnet của khóa, `/api/v1` không nhận khóa tenant (`:57–69, 84–118`).
- Nền tảng đã có: `credentials.tailnet_id NOT NULL` cho `kind='api'` (`schema_postgres.sql:43–46`); `identifier` duy nhất theo kind toàn cục (`:67`) → tra tailnet từ khóa giống `TailnetForAuthKey` (`platform.go:175–219`); `apiv2.Backend` nhận đúng một `State` (`api.go:37–45`) và `AuthenticateAPIKey` đi qua handle tenant (bị `tenantscope` lọc). Node ID toàn cục (ADR 0002) nên `/device/{id}` của B tra trong NodeStore của A sẽ không thấy.
- Thiếu: router cluster cho `/api/v2/*`: xác thực khóa → tailnet → runtime; `requireDefaultTailnet` (`api.go:292–296`) phải chấp nhận ID của tailnet khóa và `-`; cách mint API key tenant (không có CLI); rate limit API; quyết định có mở trên listener public hay không.
- Rủi ro đã thấy: OAuth client/token (`kind oauth_*`) và `/api/v2/tailnet/-/acl` POST ghi policy qua `State.SetPolicy` — ở cluster mode policy chuẩn là DB + `policy_revision`; ghi qua API phải bump revision hoặc ít nhất không lệch với reconcile (`cluster.go:384–399`). ADR 0008 §4 hoãn ghi DNS từ tenant.

### 3.4 DERP verify cho DERP ngoài ở cluster mode (I11, S15)

- DERP nhúng bị từ chối (`cluster.go:37–39, 209–210`).
- `Cluster.HTTPHandler` **không có `/verify`** (`cluster.go:806–830`); `VerifyHandler` chỉ mount ở single-tailnet (`app.go:647`). Một `derper --verify-client-url` trỏ vào cluster sẽ nhận 404 (sau guard Host 421 nếu sai host).
- Hành vi của derper khi nhận non-200 (fail-open hay từ chối) phụ thuộc cờ `--verify-client-url-fail-open` của bản tailscale đã pin (`go.mod:90`) — **chưa kiểm** trong inventory này (giả định G3).
- Không có cách xác minh theo tenant: `DERPAdmitClientRequest` chỉ có node key + IP nguồn; `handleVerifyRequest` tra theo node key trong một runtime (`handlers.go:181–190`). Ở cluster cần tra qua mọi runtime hoặc platform (`nodes.node_key` không unique toàn cục: chỉ `idx_nodes_tailnet_node_key` và `idx_nodes_machine_key_node_key`, `schema_postgres.sql:114, 119`).
- Bảo đảm "relay không vượt tailnet" không đến từ DERP: derper chuyển gói theo node key; client B không có node key của A trừ khi netmap lộ (đã chặn bởi P07). DERP verify chỉ chặn client **không hợp lệ/đã thu hồi** dùng relay.
- Tenant có thể đặt `derpMap` riêng qua policy (P06, `cluster_p06_test.go:461`); đó là lựa chọn của tenant, verify cho DERP do tenant tự vận hành nằm ngoài phạm vi.
- S15 cần lab giai đoạn 2 (derper riêng): `HANDOFF-P08-wip.md:46`, `PLAN.md:295`.

### 3.5 Metrics theo tenant

- Mọi metric là biến `promauto` package-level, không label tenant: `metrics.go:25–39`, `batcher.go:35–47`, `node_store.go:51–104`, `node_store_reconcile.go:22–40`, `state.go:2258`.
- **Lỗi đúng-sai, không chỉ thiếu label:** gauge `headscale_nodestore_nodes` được mỗi runtime `Set` (`node_store.go:157, 756`) → ở cluster giá trị là của runtime ghi sau cùng, không phải tổng. `nodestore_queue_depth` dùng Inc/Dec nên vẫn là tổng.
- Cần label tenant có giới hạn (ứng viên): `nodestore_nodes`, `nodestore_netmap_divergence_total{kind}` (R10), `mapper_changes_dropped_total{reason}`, `mapresponse_ended_total{reason}`, số map stream, độ đầy auth cache, đăng ký bị từ chối theo lý do (quota/limit/rate), số request bị rate limit.
- **Rủi ro cardinality:** không có giới hạn số tailnet mỗi cluster (chỉ `max_devices`, `placement/clusters.go:42, 104`); tailnet 0 node vẫn là một runtime. Label `tailnet_id` × label hiện có có thể bùng. Label phải là tailnet ID (không slug: slug do người đặt, I15), chỉ trên listener metrics operator-only.

## 4. Constraints hiện có

- AGENTS 6: API dữ liệu node phải nhận `TailnetID` đã xác minh; thiếu → fail closed. AGENTS 9: endpoint lộ node/IP/key không lên listener public. AGENTS 10: đổi API v2 phải `make openapi` + `make generate`, commit `gen/`. AGENTS 11: migration chỉ thêm cuối. AGENTS 13: lỗi isolation = BLOCK MERGE.
- 404 thay 403 cho tailnet khác (hợp đồng test P08a, chưa duyệt).
- Một Noise key cho cluster (ADR 0005); phiên Noise không chứng minh tenant.
- Cluster: policy chỉ từ DB (`errClusterPolicyFile`), web/OIDC tắt, tailsql tắt (`cluster.go:203–231`).
- Go race/servertest không chứng minh Noise/DERP security; S13/S15 cần client thật và derper thật.

## 5. Giả định còn mở

- G1: `/health` dùng làm probe cho front door; giới hạn nó không phá monitoring. Chưa kiểm cấu hình VPS/L4.
- G2: client đứng sau L4 forwarder (VPS → WireGuard) nên `RemoteAddr` là `10.99.77.1` (`source-inventory-P08.md` §5): giới hạn theo IP ở mc-cp **vô dụng** cho tới khi có PROXY protocol.
- G3: hành vi derper với verify trả non-200/timeout ở bản tailscale đã pin chưa đọc.
- G4: số tailnet mỗi cluster trong mục tiêu N1 nhỏ (≤ vài trăm) — chưa có con số trong PLAN.
- G5: OAuth token của API v2 (`kind='oauth_token'`) đã có `tailnet_id`; chưa đọc `oauth.go` chi tiết cho đường mint ở cluster.

## 6. Kế hoạch nhỏ theo thứ tự [Đề xuất]

1. **RED** `TestP08bClusterNodeStoreGaugeIsNotLastWriter` (hai runtime, `nodestore_nodes` phải là tổng hoặc theo tailnet) — lỗi đã thấy, nhỏ.
2. **RED** quản trị live: `TestP08bOperatorDeleteNodeLive` / `…ExpireNodeLive` / `…DeleteUserLive` / `…ApproveRouteLive` trên servertest hai tailnet: thao tác ở A → peer A biến mất ≤ M06 bound, B **không frame**; ID của B qua runtime A → 404, B không đổi; listener public không có route này.
3. Hiện thực hướng A (§3.2) + CLI `cluster tailnet node delete|expire`, `user delete`, `routes list|approve`. Chạy lại M06 ở **cluster mode** với client thật (đóng hạn chế P00d).
4. **RED** S13 servertest: A spam register bằng reusable key và MapRequest; đo p99 register/map của B; **RED** phiên Noise không xác thực không làm cạn pool DB. Sau đó giới hạn: (a) semaphore pool DB theo tailnet (tối đa k kết nối/tailnet, k < `max_open_conns`); (b) token bucket theo tailnet cho register/mutation sau khi bind (mô hình `bootstrap/http.go`); (c) giới hạn tổng phiên Noise chưa bind + rate cho lookup platform; (d) cache/`/health` không đánh DB mỗi request.
5. **RED** bật `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` (thay `p08aTwoTailnets` bằng `NewTestCluster`). Hiện thực router v2 cluster: tra tailnet theo identifier khóa → `Runtime(id)` → mux v2 của runtime; `{tailnet}` ∈ {`-`, ID khóa}; khóa sai/khác tailnet/không phục vụ → cùng 404/401; v1 không mount. `make openapi && make generate`.
6. Metrics: label `tailnet` (ID) cho tập metric §3.5 sau khi chủ dự án chốt giới hạn cardinality.
7. DERP verify (S15) khi có lab giai đoạn 2: `/verify` trên listener riêng (không public) với secret, tra node key qua mọi runtime; E2E derper thật: unknown/expired/deleted bị từ chối, A/B hợp lệ được admit, sau failover.

## 7. Câu hỏi cho chủ dự án

- **Q1:** I10/S13 thuộc P08b hay P14 (HANDOFF-P08 `:47` ghi P14)? Có thêm dòng P08b vào PLAN không?
- **Q2:** Kênh quản trị live: hướng A (listener operator-only/unix socket) hay B (lệnh qua DB + reconcile)?
- **Q3:** API v2 cho tenant có mở trên **listener public** ở N1 không, hay chỉ listener nội bộ cho tới P12? Ai mint API key tenant (CLI operator)? Có cho tenant ghi policy/DNS qua API không (ADR 0008 Q-b)?
- **Q4:** Ngân sách mặc định: kết nối DB tối đa mỗi tailnet, rate register/mutation mỗi tailnet, tổng phiên Noise chưa bind, và SLO của B trong S13 (p99 register/map).
- **Q5:** Label `tailnet` trên metric: chấp nhận với trần số tailnet/cluster (bao nhiêu?) hay chỉ xuất top-N/endpoint riêng?
- **Q6:** DERP verify: hoãn S15 tới lab giai đoạn 2 (giữ cluster cấm DERP nhúng) hay làm phần unit/servertest trước?
