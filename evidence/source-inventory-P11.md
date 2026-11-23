# PR P11 Evidence — source inventory: bootstrap service (tenant → URL trực tiếp của cluster)

- **Trạng thái:** `Planned`. File này chỉ là inventory và hợp đồng thiết kế: chưa đổi code, chưa chạy test, chưa chạm lab/mạng. Mục đánh dấu **[Đề xuất]** là đề xuất chờ chủ dự án chốt, **chưa** phải quyết định.
- **Nhánh:** `feat/P11-bootstrap` (worktree `wt-p11`), từ `master`.
- **Ticket / ADR:** `tickets/P11.md` (hợp đồng); ADR 0005 (quyết định 2, "Biện pháp bắt buộc / Bootstrap", **Bổ sung 2 §1**), ADR 0006 (PgBouncer), ADR 0007 (§2 phân giải tenant cho phiên Noise, hợp đồng `db.Platform`); PLAN §1 dòng "Endpoint", §2 (`cmd/mesh-bootstrap`, `internal/bootstrap`), §3 "Quy tắc định tuyến và xác thực" mục 1–4, bảng ticket (P11 phụ thuộc P10), gate N2; `03-isolation-threat-model.md` (attacker model, I08, I09, I10, I12, I15, §3 "Bootstrap", §4, §7); `05-data-api-contract.md` §4 (dòng `Bootstrap`), §5, §8; `06-tests-benchmarks.md` S13, S14, S17, F06, F07, fixture E2E và `bootstrap-responses.json`. Ranh giới: `tickets/P11b.md` (lease, VIP/L4 — ngoài P11), `tickets/P12b.md` (re-bootstrap sau move — ngoài P11). Không có `tickets/P12.md` trong repo.

## 1. Repo commit before / after

| | Commit |
|---|---|
| Before (inventory đọc trên) | `3269688` (= `master`: P00c đã merge, P10/P10b/P10c đã merge) |
| After | chưa có (chưa có commit code) |

Đọc bằng `git`/`grep`/`sed` trên working tree sạch tại `3269688`. Không chạy `go test`, không SSH, không gọi DNS/HTTP.
Lưu ý: nhánh `feat/P10d-capacity-limits` (`dcb8afc`, budget thiết bị dùng chung của cluster) **chưa** vào `master`; `internal/placement/quota.go` có thể đổi khi nó merge. Bootstrap không phụ thuộc quota.

## 2. Path & symbol đã đọc (file:line tại `3269688`)

### 2.1 Registry — `internal/placement`

| Path:dòng | Symbol | Ghi chú cho bootstrap |
|---|---|---|
| `internal/placement/placement.go:60-73` | `Cluster{ID, EndpointURL, Class, State, NoisePublicKey, MaxDevices, Revision}`, `TableName()="clusters"` | `EndpointURL` chính là giá trị bootstrap sẽ trả |
| `placement.go:42-56` | `ClusterState` `ACTIVE`/`DRAINING`/`DISABLED` | DRAINING vẫn giữ tailnet; DISABLED **có thể** vẫn sở hữu tailnet (`UpdateClusterState` không đụng `tailnets`, `clusters.go:137`) |
| `placement.go:141-174` | `ErrNotFound`, `ErrTailnetNotActive`, `ErrUnknownCluster`, … | |
| `placement.go:177-207` | `Registry`, `New(db, Config)` | chạy trên pool platform; **toàn bộ API là operator**, không có hàm "tenant X ở đâu" theo credential |
| `internal/placement/clusters.go:14-21` | `validateEndpoint` | endpoint chỉ chấp nhận `https` (tốt cho bootstrap: không bao giờ trả `http://`) |
| `clusters.go:211`, `:230`, `:354` | `GetCluster`, `ListClusters`, `loadCluster` | đọc theo id, không theo tenant |
| `internal/placement/tailnets.go:168-189` | `GetTailnet(id)` | tra **theo tailnet id**, kèm đếm thiết bị — nặng hơn cần thiết, và là API operator |
| `tailnets.go:194-249` | `MoveOwner` (CAS `owner_epoch+1`, `tenantscope.LockOwnership`) | không đổi `status`; trạng thái `MOVING` là P12b |
| `tailnets.go:251-266` | `loadTailnet` (`deleted_at IS NULL`) | |
| `internal/placement/quota.go:30-113` | `EnrollmentAllowed(Tx)` | mẫu "fail closed: chỉ cho phép khi `err == nil`"; tra tailnet → `loadCluster` → class |
| `internal/placement/audit.go:93`, `:262` | `mutate`, `AuditLog` | idempotency/audit chỉ cho mutation; bootstrap không ghi nên không dùng |
| `internal/placement/placementapi/api.go:28-67` | `Config()` (Huma, `/v1/openapi`, không docs UI), `NewAPI`, `Handler`, `Spec`, `Spec30` | mẫu Huma + OpenAPI để chép cho bootstrap |
| `placementapi/api.go:249-268` | `apiError` | **trả `err.Error()` trong body** → không dùng lại cho bootstrap (lộ chi tiết) |
| `placementapi/auth.go:26-123` | `Authorizer` (mTLS, SAN allowlist, từ chối header credential tenant), `TLSConfig` (TLS 1.3, CA pin) | API operator; bootstrap là **public** nên không dùng mTLS này |
| `cmd/mesh-placement/main.go:45-145` | `run`: flag `-config/-listen(127.0.0.1:9443)/-tls-*/-client-ca/-operators/-openapi`; `db.OpenPlatform`; `http.Server` có `ReadHeaderTimeout 10s`, `ReadTimeout/WriteTimeout 30s`, `MaxHeaderBytes 64KiB` | mẫu binary; **lưu ý** `OpenPlatform` chạy migration (§2.3) |
| `cmd/gen-openapi/main.go:36-41` | bảng `specs` (`v1`, `v2`, `placement` → `openapi/placement/mesh-placement.yaml`) | chỗ thêm `-api bootstrap` |
| `cmd/headscale/cli/cluster.go:334-368` | `clusterRegisterCmd` | `EndpointURL` mặc định = `server_url` của cluster; **không bao giờ đặt `NoisePublicKey`** → cột này rỗng trên mọi cluster lab |

**Kết luận 2.1:** chưa có hàm "tenant X đang ở cluster nào" cho caller không phải operator. Có đủ dữ liệu (`tailnets.owner_cluster_id`, `status`, `deleted_at` + `clusters.endpoint_url`, `state`) để viết một câu đọc duy nhất.

### 2.2 Schema — `hscontrol/db/schema_postgres.sql` (SQLite `schema.sql` tương đương)

| Dòng | Đối tượng | Ghi chú |
|---|---|---|
| 7-18 | `tailnets(id, slug, status CHECK IN ('PENDING','ACTIVE','SUSPENDED','DELETING'), owner_cluster_id, owner_epoch, …, deleted_at)` | **Không có `MOVING`** trong CHECK; `hscontrol/types/tailnet.go:8-13` cũng không có hằng `MOVING` |
| 19-20 | `idx_tailnets_slug` UNIQUE (không partial), `idx_tailnets_owner(owner_cluster_id, status)` | slug của tenant đã xóa vẫn bị giữ (S18) |
| 42-69 | `credentials(id, tailnet_id NOT NULL, kind CHECK('api','authkey','oauth_client','oauth_token'), identifier, hash bytea, reusable, used, expiration, revoked, …)`, `idx_credentials_identifier UNIQUE(kind, identifier)`, FK `tailnet_id → tailnets` | identifier duy nhất **toàn hệ thống** theo kind → một pre-auth key xác định đúng một tailnet |
| 150-161 | `clusters(…, endpoint_url NOT NULL, state CHECK, noise_public_key, …)`, `idx_clusters_endpoint_url` UNIQUE | không có FK `tailnets.owner_cluster_id → clusters` (P10-placement §2.1) |
| 172-182 | trigger `trg_tailnets_owner_epoch_monotonic` | epoch không giảm, đổi owner bắt buộc tăng epoch |

### 2.3 Platform (handle không scope) — `hscontrol/db`

| Path:dòng | Symbol | Ghi chú |
|---|---|---|
| `platform.go:18-27` | `ErrUnknownTenant` ("không phân biệt các trường hợp"), `ErrTailnetNotActive` | |
| `platform.go:62-66` | `Platform{db, cfg, cluster}` | |
| `platform.go:72-137` | `OpenPlatform` | **tạo schema trên DB mới, chạy migration v0.1, ghi version**, rồi `tenantscope.Register`. Không dùng được cho dịch vụ chỉ đọc (cần quyền DDL; trái ADR 0005 Bổ sung 2 §3 "role chạy migration tách riêng") |
| `platform.go:162-165` | `lookupContext` (`contextTimeout` 10 s, `db.go:39`) | |
| `platform.go:172-211` | **`TailnetForAuthKey(authKey)`**: `parseAuthKey` → `SELECT tailnet_id, hash FROM credentials WHERE kind=? AND identifier=?` (SQL thô, không qua tenantscope) → `verifySecret` → `ErrUnknownTenant` đồng nhất | **Đã có sẵn phân giải tenant từ pre-auth key.** Nhưng **không** kiểm `revoked`, `expiration`, `used` (comment: để runtime kiểm) — bootstrap phải tự kiểm |
| `platform.go:219-243` | `TailnetForNode(mk, nk)` | cần machine key do Noise xác thực → bootstrap (HTTP thường) **không** dùng được |
| `platform.go:248-268` | `OwnedTailnets(clusterID)` (`ACTIVE`, chưa xóa) | dùng bởi `reconcile` |
| `platform.go:273-313` | `TenantDB(id)` | từ chối owner khác (`ErrTailnetNotOwned`, `tenant.go:23`) hoặc không ACTIVE |
| `platform_admin.go:75-93` | `TailnetBySlug(slug)` | tra slug **bất kể owner/status**; dùng cho CLI operator. Nếu đưa ra public sẽ là oracle tồn tại tenant |
| `platform_admin.go:97-104` | `ErrTailnetDeleting`, `SetTailnetStatus` | DELETING là terminal |
| `registry.go:181` | `RegistryDB()` | trả pool platform cho placement |
| `tenant.go:164-170` | `clusterIDOf` (mặc định `local`) | |
| `preauth_keys.go:86` | `authKeyPrefix = "hskey-auth-"` | |
| `preauth_keys.go:245-262` | `parseAuthKey` → `(identifier, secret)`; key không prefix = legacy | |
| `secret.go:22-30` | định dạng `<prefix><identifier 12 hex>-<secret 64 hex>` | |
| `secret.go:77-82` | `hashSecret` = `"$sha256$" + hex(sha256(secret))` | 256 bit entropy, không cần stretching |
| `secret.go:159-196` | `verifySecret`: SHA-256 + `subtle.ConstantTimeCompare`; nhánh legacy bcrypt/Argon2id có limiter | DB v0.1 mới không có hash legacy |
| `hscontrol/types/preauth_key.go:109-132` | `PreAuthKey.ValidAt`: revoked / expired / used (single-use) | logic cần lặp lại (hoặc dùng chung) ở bootstrap |
| `types/preauth_key.go:186-193` | `maskedPrefix` = `hskey-auth-<identifier>-***` được **ghi log** | identifier **không phải bí mật** → không được dùng làm khóa rate limit "theo tenant" trước khi xác thực (§5.5) |
| `hscontrol/db/raw_write_guard_test.go:18-34` | `TestNoRawTenantWritesOutsideFence`, `rawTenantWriteAllowlist` | chỉ chặn **ghi** thô; chưa có lint cho **đọc** thô (ADR 0005 Bổ sung 2 §3.4 yêu cầu cả hai) → câu đọc thô của bootstrap phải review tay trong file này |

### 2.4 Cluster side — `hscontrol/cluster.go`, `session_route.go`, `frontdoor`

| Path:dòng | Symbol | Hành vi hiện tại |
|---|---|---|
| `cluster.go:58` | `reconcileInterval = 2s` | cận trên thời gian cluster cũ còn phục vụ map **từ bộ nhớ** sau khi mất ownership; ghi thì bị fence ngay ở DB |
| `cluster.go:73-103` | `Cluster{cfg, platform, noiseKey, registry, host, front, runtimes, …}` | |
| `cluster.go:123-186` | `NewCluster`: `OpenPlatform`, `clusterHost(cfg.ServerURL)`, nạp `OwnedTailnets` | |
| `cluster.go:291-364` | `reconcile` | nạp tailnet ACTIVE của mình; retire runtime đổi owner/epoch/không ACTIVE |
| `cluster.go:469-482` | `runtimeFor` | tailnet biết nhưng không phục vụ ở đây → **`db.ErrUnknownTenant`**, giống tenant lạ |
| `cluster.go:485-516` | `runtimeForRegister` | auth key → `TailnetForAuthKey` → `runtimeFor` (cluster tự kiểm, **không** tin URL/claim) → quota nếu thiết bị mới; không auth key → `TailnetForNode` |
| `cluster.go:560-562` | `runtimeForNode` | map/poll |
| `cluster.go:644-646` | `clusterRegistrationDisabled` | web/OIDC tắt trong chế độ cluster (P04b) |
| `cluster.go:650-662` | `clusterHost` | `server_url` phải là DNS hostname |
| `cluster.go:669-687` | `hostGuard` | Host (chuẩn hóa) == host của `server_url`; TLS: SNI == Host; sai → **421** "misdirected request" |
| `cluster.go:698-703` | `MetricsHandler` (listener riêng, `operatorOnly`) | mẫu rule 9 |
| `cluster.go:709-735` | `HTTPHandler`: chỉ `/ts2021`, `/key`, `/health`, `/version`, `/robots.txt`, `/machine/ping-response`, `/register/*` + `/auth/*` (tắt), `/favicon.ico`, `/` | không admin/debug/metrics |
| `session_route.go:171-205` | `refuseRoute` | runtime retired → 503 + `Retry-After: 1` + đóng phiên; đổi tenant trên phiên → 403; từ chối tenant → **401 "unauthorized"**; lỗi tra cứu → 503 + `Retry-After: 10` |
| `session_route.go:211-233` | `registerRefusal` | key của tenant lạ/khác cluster/không ACTIVE → `RegisterResponse.Error = "invalid pre auth key"` (giống key sai) |
| `session_route.go:235-246` | `isTenantRefusal` | |
| `hscontrol/frontdoor/frontdoor.go:155` | `NormalizeHost` | dùng lại được cho bootstrap (Host/SNI guard) |
| `hscontrol/servertest/cluster_p10_test.go:30-56` | `TestClusterOwnershipMoveRetiresRuntime` (tag `p05cluster`) | **đã phủ biến thể "tenant đã move" của F07** trong một process: key của A bị từ chối, không tạo node |
| `hscontrol/cluster_p08_test.go:19` | `TestP08ClusterHostGuard` | 421 cho Host/SNI lạ |
| `hscontrol/endpoint_inventory_test.go:1-30`, `testdata/p08a_endpoint_allowlist.txt` | inventory route (`public`, `noise`, `debug`, `unix`, `cluster`) | chưa có surface của placement/bootstrap |

**Client tới nhầm cluster hôm nay (F07):** register bằng key của tenant thuộc cluster khác → `RegisterResponse.Error "invalid pre auth key"` (HTTP 200 trong Noise), không tạo node; map của node thuộc cluster khác → 401; Host lạ → 421. Trong cửa sổ ≤ `reconcileInterval` sau move, runtime cũ còn nạp: register đi tới ghi DB và bị **fence** (`ErrFenced`, rollback) nên vẫn không tạo node; map còn được phục vụ từ bộ nhớ tới khi retire.

### 2.5 Lab / E2E (chỉ đọc, không chạy)

| Path:dòng | Nội dung |
|---|---|
| `infra/lab/e2e/p05/run.sh:2-53` | client docker `tailscale/tailscale:v1.104.7` trên mc-ci → `https://c1.cp.gta5web.online:8443` → VPS 116.118.3.46 → WireGuard → mc-cp:8443 (`headscale-cluster.service`, `CLUSTER_ID=mc-cp-c1`); key chỉ trong temp dir mode 700, `--auth-key=file:/dev/stdin`, log redact; exit 0/1/2 |
| `run.sh:84-118` | DB E2E là **SQLite** `/var/lib/headscale/cluster-e2e/db.sqlite` trên mc-cp (không phải PostgreSQL) |
| `run.sh:459-512` | F01/F03 của P10 dùng `tailnet reassign --to mc-cp-c2`: cluster `mc-cp-c2` **không có process** — chưa từng có hai process cluster thật |
| `infra/lab/mc-cp/cluster/config.yaml:7-16,39-42` | `server_url https://c1.cp.gta5web.online:8443`, `listen_addr 0.0.0.0:8443`, metrics `127.0.0.1:9095`, cert `/etc/headscale/tls/fullchain.pem` |
| `infra/lab/README.md:19-30` | VPS chỉ forward **TCP :8443** (`systemd-socket-proxyd`) → mc-cp; control plane thấy nguồn `10.99.77.1` (**không có IP client** → rate limit theo IP không làm được trên đường public); nftables mc-cp chỉ mở 8443 từ wg và 22 từ LAN; DNS Cloudflare DNS-only |
| `evidence/source-inventory-P00.md:141-142`, `evidence/P05-e2e-cluster.md:19` | `cp` và `*.cp.gta5web.online` → VPS; cert LE wildcard `*.cp.gta5web.online` (hết hạn 2027-02-20) |
| `infra/lab/stage2-variables.tf:109-135` | stage 2 (chưa apply, `stage2_enabled=false`) vẫn theo topology **cũ**: `mc-placement` 7710, `mc-edge` 7711, `mc-shard-01/02`… — chưa có `mc-bootstrap` như PLAN §9 / `04-proxmox-lab.md:46` |

## 3. Constraints hiện có (ràng buộc phải giữ)

1. **Cluster không tin bootstrap** (ADR 0005 Bổ sung 2 §1; ticket mục tiêu 3; điều kiện dừng "BLOCK MERGE"). Hiện đã đúng ở `runtimeForRegister`/`runtimeFor`/fence. P11 **không** được thêm bất kỳ header/claim/token nào từ bootstrap mà cluster dùng để quyết định.
2. I15: tenant không tồn tại, `SUSPENDED`, `PENDING`, `DELETING`, credential sai → phản hồi giống nhau (status, body, thời gian trong ngưỡng) với caller không có credential hợp lệ.
3. AGENTS rule 6: không có API unscoped; tenant chỉ đến từ **credential đã xác thực**, fail closed. Slug/tenant trong payload chỉ để đối chiếu (giống `tailnet_id` payload, PLAN §3 mục 3).
4. AGENTS rule 9 / I09 / S14: listener public của bootstrap không có `/debug`, `/metrics`, pprof; metrics (nếu có) ở listener riêng hạn chế như `MetricsHandler`.
5. AGENTS rule 10: API bằng Huma; spec qua `cmd/gen-openapi`; không sửa tay `gen/`.
6. AGENTS rule 2: không log key/proof/hash; artifact `bootstrap-responses.json` không chứa credential.
7. ADR 0006: process nối PostgreSQL qua PgBouncer transaction mode → chỉ câu lệnh không trạng thái phiên (một `SELECT`, hoặc `BEGIN READ ONLY` ngắn).
8. AGENTS rule 3: expose bootstrap ra Internet, sửa firewall VPS/mc-cp hoặc thêm forward → dừng, xin duyệt.
9. ADR 0005 Bổ sung 2 §3.3: mọi SQL thô review tay và ghi vào inventory (§5.3).

## 4. Điểm còn giả định / chưa kiểm

- Chưa kiểm hành vi `tailscale up --login-server` khi URL có port `:8443` qua bootstrap khác host (đã chạy tốt trực tiếp trong P05; giả định không khác).
- Chưa đo độ chênh thời gian giữa "identifier không có" và "identifier có, proof sai" (một SHA-256 + so sánh hằng thời gian; giả định < nhiễu mạng, phải đo — F06/I15).
- Giả định DNS `*.cp.gta5web.online` vẫn trỏ VPS và cert wildcard còn hạn (theo evidence P00/P05; chưa kiểm lại hôm nay).
- Giả định không còn hash legacy (bcrypt/Argon2id) trong DB v0.1 mới (fresh schema).
- Chưa đọc mã client Tailscale cho việc đổi ControlURL (P12b).

## 5. Thiết kế đề xuất (chờ chủ dự án)

### 5.1 Credential của caller **[Đề xuất]**

| Phương án | Định danh tenant | Lộ tồn tại tenant? | Nhận xét |
|---|---|---|---|
| A. Tra ẩn danh theo slug | slug | **Có**: slug có/không có thì phản hồi phải khác (URL hay không) → dò được danh sách tenant và tenant nào chung cluster | Trái I15 và §7 threat model. **Không đề xuất** |
| B. Pre-auth key đầy đủ | `credentials.identifier` → `tailnet_id` | Không | Secret đi qua thêm một dịch vụ (log, TLS terminate, proxy) |
| **C. Proof của pre-auth key (khuyến nghị)** | `key_id` (identifier 12 hex) + `key_proof = hex(sha256(secret))` | Không | Đúng ý ticket "preauth key đã băm để tra". So sánh hằng thời gian với `hash` lưu sẵn (`"$sha256$"+hex`). Lộ request bootstrap **không** lộ key đăng ký được (cần preimage SHA-256 của 256 bit). Người đọc được DB thì vốn đã thấy mọi thứ |
| D. Enrollment token ký | chưa tồn tại trong code (không có symbol nào) | — | Để P04b; API chừa chỗ (`credential.kind`) |

- Tenant **chỉ** lấy từ credential. Trường `tenant` (slug) **tùy chọn**, chỉ để đối chiếu: khác slug của tailnet của credential → phản hồi từ chối đồng nhất (không trả URL của A lẫn B; test I08/S10).
- Credential phải **còn hiệu lực**: không `revoked`, chưa `expiration`, single-use thì chưa `used` (cùng luật `PreAuthKey.ValidAt`). `TailnetForAuthKey` hiện bỏ qua ba điều kiện này → bootstrap không gọi thẳng nó.
- Không nhận credential qua query string (log của proxy); chỉ body JSON qua `POST`.

### 5.2 API **[Đề xuất]**

```http
POST /v1/bootstrap
Content-Type: application/json
Cache-Control: no-store

{"credential": {"kind": "authkey_proof", "key_id": "<12 hex>", "proof": "<64 hex>"}, "tenant": "acme"}
```

| Trường hợp | Có credential hợp lệ của tenant | Không có / sai / hết hạn / thu hồi / đã dùng / slug không khớp |
|---|---|---|
| `ACTIVE`, owner cluster đã đăng ký và `ACTIVE`/`DRAINING` | **200** `{"control_url":"https://c1.cp.gta5web.online:8443","issued_at":"…"}` | 404 đồng nhất |
| `MOVING` (cần schema, Q2) | **503** + `Retry-After` + `{"error":"tenant is moving; retry"}` | 404 đồng nhất |
| owner cluster `DISABLED` hoặc chưa đăng ký | **503** + `Retry-After` (lỗi vận hành; chỉ người có key thấy) | 404 đồng nhất |
| `SUSPENDED`, `PENDING`, `DELETING`, `deleted_at`, không tồn tại | 404 đồng nhất | 404 đồng nhất |
| JSON hỏng, thiếu trường, sai định dạng hex | 400 đồng nhất (không đụng DB) | 400 đồng nhất |
| Vượt rate limit | 429 + `Retry-After` | 429 + `Retry-After` |
| DB không trả lời | 503 + `Retry-After` chung (body khác MOVING) | như trái |

- 404 đồng nhất: body cố định byte-for-byte (ví dụ `{"error":"not available"}`), cùng `Content-Type`, `Content-Length`, không header phụ thuộc DB; không dùng `apiError` của placement (trả `err.Error()`), không dùng body problem+json mặc định có `detail`.
- Phản hồi 200 **không** gồm `owner_epoch`, `cluster_id`, số node (giống `/ready` trong threat model §4). `noise_public_key` **không** trả trong MVP: cột đang rỗng (`clusterRegisterCmd` không ghi), và `tailscale` không có cờ pin key.
- OpenAPI: `cmd/gen-openapi -api bootstrap` → `openapi/bootstrap/mesh-bootstrap.yaml`. **Không** phục vụ `/v1/openapi` trên listener public. Không sinh client Go vào `gen/` (script onboarding dùng `curl`), trừ khi chủ dự án muốn.

### 5.3 Đường đọc DB **[Đề xuất]**

- Một câu đọc duy nhất, chạy cho **mọi** request hợp lệ về định dạng (kể cả identifier không tồn tại), để chi phí giống nhau:

  ```sql
  SELECT c.tailnet_id, c.hash, c.revoked, c.expiration, c.used, c.reusable,
         t.slug, t.status, t.deleted_at, cl.endpoint_url, cl.state
    FROM credentials c
    JOIN tailnets t ON t.id = c.tailnet_id
    LEFT JOIN clusters cl ON cl.id = t.owner_cluster_id
   WHERE c.kind = 'authkey' AND c.identifier = ?
  ```
  SQL thô đọc bảng tenant `credentials` (tenantscope không parse `Raw`), cùng loại với `TailnetForAuthKey` (`platform.go:191`): chỉ dùng hàng của **chính** credential được trình, không trả dữ liệu tenant ra ngoài ngoài `endpoint_url`. Ghi vào danh sách SQL thô đã review; nếu có lint đọc thô thì thêm allowlist có lý do.
- Không có dòng → vẫn so sánh hằng thời gian với một hash giả để thời gian gần như nhau.
- Mở DB bằng **hàm mới chỉ đọc** (ví dụ `db.OpenReadOnly`): kiểm migration cuối của DB khớp binary (không khớp → không khởi động), **không** tạo schema, **không** chạy migration, không ghi version; mỗi lookup trong `BEGIN READ ONLY` (hợp lệ với PgBouncer transaction mode) hoặc `SET LOCAL transaction_read_only`.
- **Không cache** trong MVP: lookup là một index unique + SHA-256; đợt re-bootstrap 5.000 client của P12b vẫn nhỏ. Nếu sau này cần cache: TTL ≤ `reconcileInterval` (2 s), không cache phản hồi 503/MOVING, và **không** coi cache là bảo đảm an toàn (cluster tự kiểm).
- Đọc từ primary qua PgBouncer (pool nhỏ, transaction mode). Replica chỉ khi chủ dự án chấp nhận độ trễ; an toàn vẫn đến từ cluster (§5.6). Lưu ý mâu thuẫn tài liệu: PLAN §9 dòng mạng và `04-proxmox-lab.md:36` liệt kê bootstrap là client **trực tiếp** 5432 (Q5).

### 5.4 Binary `cmd/mesh-bootstrap` **[Đề xuất]**

- Logic ở `internal/bootstrap` (`Service`, `Store` interface để test bằng store giả + SQLite/PostgreSQL thật), API Huma ở `internal/bootstrap/bootstrapapi`.
- Flag: `-config` (chỉ `database.*`, mật khẩu `file:`), `-listen`, `-tls-cert/-tls-key`, `-hostname` (Host/SNI guard bằng `frontdoor.NormalizeHost`, Host lạ → 421 như `hostGuard`), `-metrics-listen` (tùy chọn, loopback), giới hạn rate. TLS 1.2+ với cert thật/lab CA; timeout như `mesh-placement` + `MaxBytesReader` (vd. 4 KiB).
- Route public **chính xác**: `POST /v1/bootstrap`, `GET /health` (không chạm DB chi tiết, không lộ gì). Không `/debug`, `/metrics`, pprof, docs, openapi.
- Log: request id, kết quả (`ok|refused|moving|limited|error`), **không** key_id đầy đủ (chỉ hash ngắn của key_id), không proof, không slug khi bị từ chối.

### 5.5 Rate limit (I10, S13) **[Đề xuất]**

- Bucket theo IP nguồn (sau khi tin `X-Forwarded-For`/PROXY protocol **chỉ** từ proxy cấu hình), bucket toàn cục, và bucket **theo tailnet chỉ trừ sau khi credential xác thực xong**. Không có bucket theo `key_id` trước xác thực: identifier được log công khai (`maskedPrefix`), kẻ tấn công có thể dùng nó để làm cạn quota của tenant B.
- Phụ thuộc mới: `golang.org/x/time/rate` (đã có trong `go.mod:249` dạng indirect) → thành direct.
- Lab: đường VPS (`systemd-socket-proxyd`) che IP client (`README.md:28`) → bucket theo IP chỉ test được trong mạng lab, không trên đường public.

### 5.6 Cluster (F07) **[Đề xuất]**

- Không cần đổi logic: `runtimeFor` + `TenantDB` + fence + `hostGuard` đã từ chối. P11 thêm **test** (RED bằng cách tạm gỡ kiểm tra — mutation test) với **hai process cluster khác ID trên cùng PostgreSQL**, điều P10 ghi là còn thiếu (`source-inventory-P10.md` §4).
- Chấp nhận/đo cửa sổ ≤ 2 s (`reconcileInterval`) map còn phục vụ từ bộ nhớ sau move; ghi là giới hạn đã biết (P12b/P16).
- Ghi nhận UX: client có URL cũ nhận `"invalid pre auth key"`/401 — đúng I15 nhưng không gợi ý re-bootstrap; P12b quyết định thông báo.

### 5.7 Client / onboarding **[Đề xuất]**

- Không sửa client Tailscale. Script lab (vd. `infra/lab/e2e/p11/onboard.sh`):
  1. đọc key từ file mode 600; tách `key_id`, tính `proof = sha256(secret)` cục bộ;
  2. `curl --fail --cacert <ca> -X POST https://<bootstrap>/v1/bootstrap` (không `-k`);
  3. kiểm `control_url` là `https://` và hostname thuộc **suffix cho phép** (vd. `.cp.gta5web.online`), nếu không thì dừng;
  4. `tailscale up --login-server="$control_url" --auth-key=file:/dev/stdin` (như `run.sh:167`).
- Bước 3 là biện pháp cho rủi ro "bootstrap bị chiếm trả URL giả → client gửi pre-auth key cho server giả" (§9).

## 6. RED tests viết trước **[Đề xuất]**

| Test | Mức | Ánh xạ |
|---|---|---|
| `TestBootstrapActiveReturnsOwnerURL`: tenant ACTIVE ở c1 → đúng `endpoint_url` của c1; sau `MoveOwner` sang c2 → URL c2, không bao giờ c1 | unit (store thật SQLite + PostgreSQL) | F06, I12 |
| `TestBootstrapRefusalsIndistinguishable`: bảng {không credential, JSON hỏng-hợp-lệ-định-dạng, key_id lạ, proof sai, revoked, expired, single-use đã dùng, SUSPENDED, PENDING, DELETING, đã xóa, slug không khớp} → status + header (trừ `Date`) + body **byte-identical** | unit | F06, I15 |
| `TestBootstrapMovingOnlyForCredentialed`: MOVING + key hợp lệ → 503 + `Retry-After`; MOVING không key → giống 404 đồng nhất | unit (phụ thuộc Q2) | F06, I15 |
| `TestBootstrapCrossTenantCredential`: key của A + `tenant=B` → 404 đồng nhất; body không chứa URL của A hay B | unit | I08, S10 |
| `TestBootstrapRefusalTiming`: ≥ 2.000 mẫu mỗi nhóm (key_id lạ / proof sai / SUSPENDED / ACTIVE-key-sai) → chênh median và p95 trong ngưỡng (đề xuất < 1 ms trong process; đo lại qua mạng ở E2E) | unit + E2E | I15 |
| `TestBootstrapNoDebugOrMetrics`: route set của handler public = đúng `{POST /v1/bootstrap, GET /health}`; `/debug/*`, `/metrics`, `/v1/openapi` → 404; Host lạ → 421 | unit + endpoint inventory | I09, S14 |
| `TestBootstrapRateLimit`: flood từ IP X → 429; tenant B từ IP Y vẫn 200; proof sai mang `key_id` của B **không** làm cạn bucket của B | unit | I10, S13 |
| `TestBootstrapReadOnly`: store chỉ đọc; mọi ghi trong lookup tx lỗi; mở DB thiếu migration → không khởi động, không tạo schema | unit (PostgreSQL) | ADR 0005 Bổ sung 2 §3 |
| `TestBootstrapNoSecretsInLogs`: log không chứa proof, secret, key_id đầy đủ | unit | AGENTS rule 2 |
| `TestBootstrapStaleReadClusterStillRefuses`: store giả trả owner cũ (c1) sau khi tenant chuyển sang c2 → URL c1; client thật (servertest) tới c1 → register bị từ chối, không node mới; map → 401 | servertest (`p05cluster`) | I12, S17, F07 |
| `TestTwoClustersRefuseForeignTenant`: hai `Cluster` (c1, c2, hostname khác) cùng một PostgreSQL; key của tenant ở c1 đăng ký tại c2 → từ chối, `nodes` không đổi; node của c1 map tại c2 → 401; Host c1 tới listener c2 → 421 | servertest/integration PostgreSQL | F07, I01, I12 |
| E2E: `onboard.sh` → bootstrap → `tailscale up` → kết nối thẳng cluster; client tự cấu hình URL cluster sai → bị từ chối; artifact `bootstrap-responses.json` đã redact | lab | F06, F07, S16 |

## 7. Kế hoạch PR nhỏ **[Đề xuất]**

1. **P11-0** (file này): inventory + trả lời Q1–Q5.
2. **P11-1 RED:** test bảng §6 cho `internal/bootstrap` (store giả + interface), test hai cluster/PostgreSQL cho F07 (GREEN ngay ở phía cluster thì chứng minh RED bằng mutation: tạm vô hiệu `runtimeFor` → test fail).
3. **P11-2 DB:** `db.OpenReadOnly` + lookup một câu (§5.3) + review SQL thô; nếu Q2 = "làm trong P11": migration nối cuối thêm `MOVING` vào CHECK (SQLite + PostgreSQL, byte-identical với file schema, rule 7: rà `OwnedTailnets`, `reconcile`, `setStatus`, `SetTailnetStatus`, `EnrollmentAllowedTx`, CLI).
4. **P11-3 Service + API:** `internal/bootstrap`, `bootstrapapi` (Huma), responder đồng nhất, rate limit, `gen-openapi -api bootstrap`, inventory route.
5. **P11-4 Binary + script:** `cmd/mesh-bootstrap`, `onboard.sh`, cấu hình lab (không deploy public).
6. **P11-5 Evidence:** race unit + integration PostgreSQL trên mc-ci, E2E theo phương án lab được duyệt (Q4), đo timing, `bootstrap-responses.json`, reviewer bảo mật duyệt mô hình xác thực caller.

## 8. Câu hỏi mở cho chủ dự án (tối đa 5)

**Quyết định của chủ dự án (2026-11-23):**
- **Q1 — phương án C:** `key_id` + `sha256(secret)`; không tra ẩn danh theo slug; `tenant` chỉ để đối chiếu. Mọi trường hợp không hợp lệ trả cùng một 404.
- **Q2 — thêm `MOVING` trong P11:** migration nối cuối (SQLite + PostgreSQL) và hằng `types.TailnetMoving`; chưa có đường nào đặt `MOVING` cho tới P12b.
- **Q3 — từ chối** key đã thu hồi / hết hạn / single-use đã dùng (cùng luật với đăng ký). Re-bootstrap sau move (P12b) cần key reusable hoặc key mới.
- **Q4 — hostname public** cho E2E. Đụng VPS / firewall / DNS, nên theo AGENTS rule 3 diff chính xác (port, rule forward, bản ghi DNS) phải được chủ dự án duyệt **trước** khi áp dụng; tới lúc đó chỉ chạy nội bộ.
- **Q5 — chưa hỏi:** mặc định theo ADR 0006: đi qua PgBouncer khi lab có, đọc primary, không replica, không cache. Lab hiện tại: cluster E2E trên mc-cp dùng SQLite, nên bootstrap đọc cùng DB đó ở chế độ chỉ đọc trên mc-cp.


1. **Mô hình credential:** chọn phương án C (key_id + `sha256(secret)`) làm mặc định, **không** cho tra ẩn danh theo slug, `tenant` chỉ là trường đối chiếu tùy chọn? Hay phương án B (gửi nguyên pre-auth key)?
2. **`MOVING`:** schema và `types.TailnetStatus` chưa có `MOVING`. P11 thêm migration nối cuối (và hằng) ngay, hay P11 chỉ test nhánh MOVING bằng store giả và để P12b thêm migration? (F06 đầy đủ trên DB thật cần migration.)
3. **Key không còn hiệu lực:** bootstrap từ chối (404 đồng nhất) key đã thu hồi / hết hạn / single-use đã dùng? Hệ quả: re-bootstrap sau move (P12b) cần key reusable hoặc key mới — chấp nhận?
4. **Lab cho E2E:** (a) chạy bootstrap + cluster thứ hai + client nội bộ trên mc-ci với CA lab, không expose Internet (không cần duyệt firewall), hay (b) hostname public (vd. `boot.cp.gta5web.online`) qua port VPS mới → cần duyệt rule 3, và rate limit theo IP chỉ có nghĩa khi thêm PROXY protocol trên forwarder?
5. **Đường DB:** bootstrap đi qua PgBouncer (transaction mode, pool nhỏ, đọc primary, không cache) như đề xuất, hay nối thẳng 5432 như PLAN §9 / `04-proxmox-lab.md` ghi? Có cho đọc replica không?

## 9. Rủi ro và giả định

- **Bootstrap là điểm tin cậy của client (không của cluster):** bootstrap bị chiếm hoặc DNS bị chiếm có thể trả URL của server giả, và `tailscale up` sẽ gửi **pre-auth key** tới đó. Giảm thiểu: suffix allowlist + TLS hợp lệ trong script; không tự động theo URL lạ. Không loại bỏ hoàn toàn.
- **Timing (I15):** lookup có dòng và không dòng khác nhau ở bước đọc hàng/khóa; đã đề xuất một câu + so sánh giả, nhưng phải đo. Nếu chênh vượt ngưỡng: thêm sàn thời gian cố định cho phản hồi từ chối (đổi lại tăng latency).
- **Cluster DISABLED vẫn sở hữu tailnet** là trạng thái hợp lệ hiện nay; đề xuất 503 cho người có key — cần reviewer xác nhận không mâu thuẫn hợp đồng §8 (bảng chỉ liệt kê trạng thái tenant).
- **Cửa sổ 2 s** cluster cũ còn phục vụ map từ bộ nhớ sau move (`reconcileInterval`); ghi đã fence. Không do P11 gây ra, nhưng F07 E2E phải đo và ghi.
- **Chưa có lint SQL thô cho đọc:** câu §5.3 chỉ được bảo vệ bằng review tay.
- **Lab:** DB E2E hiện là SQLite trên mc-cp; chưa có process cluster thứ hai; stage 2 Terraform còn topology cũ (`mc-edge`, `mc-shard-*`), chưa có `mc-bootstrap`. Đổi Terraform/firewall/forwarder đều cần duyệt.
- **Proof SHA-256 là bearer token cho bootstrap:** lộ proof (log, proxy) cho phép người khác biết URL cluster của tenant đó (không đăng ký được). Chấp nhận được nếu không log proof.

## 10. Bằng chứng (P11-2 … P11-4)

- **Repo commit before / after:** `3269688` (master) → `f55694b` (migration), merge `master` (P10d) `dc81bc2`, `6d4eb87` (bootstrap), `d1d4154` (binary + E2E), `0426bd7` / `ae281f4` (sửa harness E2E), commit evidence.
- **Design choice:** phương án C (key_id + SHA-256 proof); `moving_to_cluster_id` thay cho status `MOVING` (§8). Public handler viết bằng `net/http`, không dùng Huma, vì phản hồi từ chối phải giống hệt nhau từng byte, kể cả header (không có body problem+json với `detail`). Chưa sinh spec OpenAPI; script onboarding dùng `curl`.

### 10.1 Thay đổi

| Path / symbol | Thay đổi |
|---|---|
| `hscontrol/db/moving.go` `movingMigration` (`202611232000-tailnets-moving-to`); `schema.sql`, `schema_postgres.sql`; `types.Tailnet.MovingToClusterID` | Cột `tailnets.moving_to_cluster_id text` (nullable), migration nối cuối trong transaction, có `Rollback` (`DROP COLUMN`) |
| `hscontrol/db/readonly.go` `OpenReadOnly`, `LatestMigrationID`, `ErrSchemaNotCurrent` | Mở DB cho bên chỉ đọc: không tạo schema, không migrate, không đăng ký tenantscope. Từ chối file SQLite không tồn tại và DB thiếu migration cuối của binary. SQLite: `PRAGMA query_only = ON` |
| `internal/bootstrap/bootstrap.go` `Service.Resolve`, `Request.Valid`, `proofMatches`, `ProofOf` | Tenant chỉ lấy từ credential đã kiểm. So proof bằng `subtle.ConstantTimeCompare`; key không tồn tại vẫn so với hash giả. Key thu hồi / hết hạn / single-use đã dùng / hash legacy, tailnet không ACTIVE / đã xóa, slug không khớp đều → `OutcomeRefused`. MOVING và cluster không dùng được (DISABLED / chưa đăng ký / không có endpoint) → 503, chỉ cho người có key hợp lệ |
| `internal/bootstrap/store.go` `GormStore`, `lookupSQL` | Một câu đọc SQL thô đã review (`credentials` ⋈ `tailnets` ⟕ `clusters`, chỉ hàng của key được trình). PostgreSQL: transaction `READ ONLY`. Hơn một hàng → lỗi (fail closed) |
| `internal/bootstrap/http.go` `Handler` | Chỉ phục vụ `POST /v1/bootstrap`, `GET /health`; mọi đường khác dùng chung body từ chối. Host lạ hoặc SNI ≠ Host → 421. Body ≤ 4 KiB, JSON chặt (`DisallowUnknownFields`, không cho dữ liệu thừa). Bucket toàn cục; bucket theo tenant chỉ trừ **sau** xác thực; bucket theo IP tùy chọn (mặc định tắt). Log chỉ có hash ngắn của key_id |
| `cmd/mesh-bootstrap/main.go` | Binary: `-config` (chỉ `database.*`), `-listen`, `-tls-cert/-tls-key`, `-hostname`, các giới hạn. TLS ≥ 1.2, timeout đọc/ghi, header ≤ 16 KiB |
| `infra/lab/e2e/p05/run.sh` `boot_req`, `t_bootstrap` | Ca B01–B06, chỉ chạy khi có `BOOT_BIN`. Bootstrap chạy trên loopback của mc-cp (không đổi firewall); request gửi bằng `curl --resolve` trên mc-cp |

### 10.2 Test (mc-ci, Go 1.27.4)

| Test | SQLite | PostgreSQL | Ánh xạ |
|---|---|---|---|
| `TestMigrationTailnetsMovingTo`: DB mới có cột; DB trước migration được nâng cấp khi mở, giữ dữ liệu, khớp `schema.sql` (squibble kiểm khi mở lại) | PASS | PASS | rule 11 |
| `TestBootstrapStoreFollowsOwner`: key của acme ở c1 → URL c1; sau `MoveOwner` sang c2 → URL c2, không bao giờ c1; `moving_to_cluster_id` → MOVING; SUSPENDED → từ chối; handle chỉ đọc từ chối ghi | PASS | PASS | F06, I12 |
| `TestOpenReadOnlyRefusesUnusableDatabase`: không tạo file; DB thiếu migration cuối → `ErrSchemaNotCurrent` | PASS | — | |
| `TestBootstrapRefusalsIndistinguishable`: 16 lý do từ chối → cùng status, header, body; đường lạ cũng vậy | PASS (store giả) | | F06, I15, I08 |
| `TestBootstrapActiveReturnsOwnerURL`, `…MovingAndUnavailableOnlyForCredentialed`, `…StoreFailureIsUnavailable`, `…MalformedNeverReachesStore`, `…NoDebugOrMetrics`, `…RateLimit`, `…NoSecretsInLogs` | PASS | | F06, I09, I10, rule 2, rule 9 |

- Lệnh: `go test -race -count=3 ./internal/bootstrap/` exit 0; `go test -race -count=1 ./hscontrol/db/ ./internal/placement/...` exit 0.
- **Mutation (thay cho RED vì package mới):**
  - M1 bỏ kiểm `revoked` và slug không khớp → `TestBootstrapRefusalsIndistinguishable` FAIL.
  - M2 đường từ chối trả URL → cùng test FAIL.
  - M3 bỏ kiểm Host/SNI → `TestBootstrapNoDebugOrMetrics` FAIL.
- Full race và suite cluster ở `ae281f4`: xem §10.4.

### 10.3 E2E client thật (`tailscale/tailscale:v1.104.7`)

Binary build trên mc-ci ở `d1d4154` (code không đổi tới `ae281f4`): cluster sha256 `5350075634e3a99c…`, bootstrap `5ca3d14595e7c893…`.

| Lần | Harness | Artifact | Exit | Kết quả |
|---|---|---|---|---|
| 1–2 | `d1d4154` | `artifacts/d1d4154/P05/…T170915Z`, `…T171723Z` | 1 | B01, B03, B06 FAIL: key hợp lệ bị 404 |
| 3–4 | `0426bd7` | `artifacts/0426bd7/P05/…T172603Z`, `…T173403Z` | 1 | như trên |
| 5–6 | `ae281f4` | `artifacts/ae281f4/P05/e2e-20261123T174347Z/`, `…T175051Z/` | 0 | **25/25 PASS ×2** (19 ca cũ + B01–B06); `cleanup_ok=1`; 0 key chưa redact |

**Lỗi ở lần 1–4 nằm ở harness, không ở bootstrap:**
- `python3` trên Windows in ra `
`, nên `secret` dính `` và proof tính trên `secret+`. Bootstrap từ chối đúng như thiết kế.
- `tr -d $''` trong Git Bash không loại được ký tự này (đã kiểm bằng `od -c`).
- Sửa ở `ae281f4`: một bước Python dựng cả body và tự tính proof bằng `hashlib`, nên không biến shell nào giữ secret.

Các ca E2E:
- **B01:** key của b → `https://c1.cp.gta5web.online:8443`, body chỉ có `control_url` và `issued_at`.
- **B02:** tenant c đang SUSPENDED, key của a ghi tenant b, key của b ghi tenant a, key_id lạ → cùng một 404, không có URL.
- **B03:** `tailscale up --login-server=<URL từ bootstrap>` đăng ký được node vào b.
- **B04:** `/metrics`, `/debug/pprof/`, `/v1/openapi`, `/` → 404; Host của cluster tới bootstrap → 421.
- **B05:** log bootstrap không có chuỗi hex 64 ký tự (proof) hay key.
- **B06:** tenant a đã bị F03 chuyển sang `mc-cp-c2` (chưa đăng ký) → 503 cho người có key, không có URL.

### 10.4 Full race, suite cluster

(cập nhật khi chạy xong)

### 10.5 Known limitations

- **Đường public chưa có:** E2E chỉ chạy loopback trên mc-cp. Hostname public (`boot.cp.gta5web.online:8444`) cần chủ dự án duyệt diff VPS và firewall mc-cp (AGENTS rule 3).
- Qua `systemd-socket-proxyd` của VPS, bootstrap chỉ thấy địa chỉ proxy; bucket theo IP để tắt.
- Chưa đo timing từ chối (`TestBootstrapRefusalTiming`) và chưa có test hai process cluster trên cùng PostgreSQL (F07 với hai cluster thật, cần lab giai đoạn 2). F07 trong một process đã có từ P10 (`TestClusterOwnershipMoveRetiresRuntime`).
- Trên PostgreSQL, handle chỉ đọc chỉ được bảo đảm trong transaction `READ ONLY` của lookup. Production nên dùng role DB chỉ có `SELECT` trên `credentials`, `tailnets`, `clusters`, `migrations`.
- `clusterRegisterCmd` không ghi `noise_public_key`; bootstrap không trả Noise key (MVP).
- Client có URL cũ sau khi tenant chuyển cluster nhận "invalid pre auth key" (đúng I15). Thông báo gợi ý re-bootstrap để P12b.
- Chưa có spec OpenAPI cho API bootstrap.
- **Reviewer approval:** chưa. Mô hình xác thực caller cần người duyệt bảo mật xác nhận.
