# PR P08b Evidence — quản trị live, `/api/v2` công khai theo key, metric theo tailnet

- **Nhánh:** `feat/P08b-control-plane` (từ `master` `b2b5d8d`, đã merge `feat/P08b-inventory`). Trạng thái: `Verified`, chưa `Reviewed`.
- **Inventory:** `evidence/source-inventory-P08b.md` (§9 cập nhật khi viết code). **Ticket:** `tickets/P08b.md`. **ADR:** 0005, 0007 §4, 0008 §4.
- **Phạm vi theo quyết định chủ dự án (2026-11-25):**
  - Q1: S13/I10 ở P14.
  - Pool DB không giới hạn theo tailnet.
  - Q2: hướng A.
  - Q3: `/api/v2` công khai theo key; key do CLI và placement API cấp; tenant không ghi policy/DNS.
  - Q4: dùng giá trị mặc định.
  - Q5: trần 2000 nhãn.
  - Q6: DERP làm sau (ADR 0009).

## Repo commit before / after

| | Commit |
|---|---|
| Trước | `master` `b2b5d8d` |
| Gauge | `73b1756` `nodestore_nodes` = tổng; `internal/tenantlabel` |
| Quản trị live | `0b68370` admin API trên unix socket |
| API v2 + key | `2dd56ed` `/api/v2` cluster, `Platform.TailnetForAPIKey`, placement `CreateAPIKey/ListAPIKeys/RevokeAPIKey`, CLI, `tuning.tenant_api.*` |
| Metric | `7770ca0`, `7ac17ad`, `10b738b` metric theo tailnet |
| Harness | `bdb1529`, `539a08a`, `f415fcb` |
| Evidence | commit chứa file này |

## Verified source paths & symbols

| Path | Symbol |
|---|---|
| `internal/tenantlabel/tenantlabel.go` | `Label(id)`, `SetLimit`, `Other`, `DefaultLimit = 2000`. Đã cấp nhãn thì giữ; vượt trần thì gộp vào `other` |
| `hscontrol/state/node_store.go` | `setCount/addCount/SetMetricsTailnet`; `Stop` trừ phần của store; `headscale_tailnet_nodes{tailnet}` |
| `hscontrol/cluster_admin.go` | `Cluster.AdminHandler`, `admin()` (ID → runtime, standby 503, audit log), `listenAdminSocket` (chỉ xóa socket cũ, không xóa file thường) |
| `hscontrol/cluster_apiv2.go` | `Cluster.APIv2Handler`, `clusterAPI` (bucket toàn process / theo key / theo tailnet), `tenantRuntime.apiV2()` |
| `hscontrol/api/v2/api.go`, `keys.go` | `Backend.Tailnet`, `requireTailnet`, `clusterForbidden`; ở cluster mode từ chối OAuth access token và OAuth client |
| `hscontrol/db/platform_apikey.go` | `GenerateAPIKeySecret`, `Platform.TailnetForAPIKey` (chỉ nhận `hskey-api-`, chưa thu hồi, chưa hết hạn) |
| `internal/placement/apikeys.go` | `CreateAPIKey(ctx, op, tailnet, lifetime, user)`, `ListAPIKeys`, `RevokeAPIKey`; tối đa 365 ngày; user phải thuộc đúng tailnet |
| `internal/placement/placementapi/api.go` | `POST/GET /v1/tailnets/{id}/apikeys`, `POST /v1/tailnets/{id}/apikeys/{key_id}/revoke` (mTLS, `Idempotency-Key`) |
| `internal/ratelimit/ratelimit.go` | `Bucket`, `Buckets` (chép mô hình từ bootstrap, tối đa 100 000 khóa) |
| `cmd/headscale/cli/cluster_live.go`, `cluster_apikey.go` | `cluster tailnet --slug S node list\|delete\|expire`, `routes list\|approve`, `user delete`, `apikey create\|list\|revoke` |
| `hscontrol/metrics.go`, `poll.go`, `state/node_store_reconcile.go` | `headscale_tailnet_map_sessions`, `headscale_tailnet_mapresponses_sent_total{tailnet,type}`, `headscale_tailnet_netmap_divergence_total` |
| `hscontrol/types/config.go`, `config/*` | `tuning.tenant_api.{global,tailnet,key}_{rate,burst}` (mặc định 200/400, 20/40, 10/20), `tuning.tailnet_metric_label_limit` (2000); docs và nix sinh lại |

## Design choice / ADR

- **Kênh quản trị live (Q2, hướng A): chỉ unix socket (`unix_socket`), không dùng listener metrics.**
  - Lý do: `operatorOnly` chỉ kiểm IP. Như vậy đủ cho đọc metric, nhưng không đủ cho thao tác ghi.
  - Quyền truy cập socket dựa trên file mode, giống mô hình socket single-tailnet có sẵn.
  - Handler chạy trong runtime phục vụ tailnet, dùng cùng lệnh `State` + `Change` với API v1/v2, nên peer nhận thay đổi qua đường delta.
  - `{tailnet}` chỉ nhận ID, không nhận slug. CLI tự đổi slug sang ID qua platform DB.
  - Hạn chế: CLI phải chạy trên host của process active; standby trả 503.
- **`/api/v2` công khai (Q3):**
  1. Bucket toàn process trước khi tra key, để tràn request chưa xác thực không làm cạn DB.
  2. `TailnetForAPIKey` chọn tailnet.
  3. Bucket theo key, rồi bucket theo tailnet (key vượt giới hạn không tiêu token của tailnet).
  4. Tailnet không do process phục vụ trả 404; standby trả 503.
  5. Runtime của tailnet xác thực key lần nữa qua handle scoped (fail closed hai lớp).
  - `{tailnet}` chỉ nhận `-` hoặc ID tailnet của key; tailnet hoặc device khác trả 404, kể cả với route bị cấm (không dùng được để dò tailnet nào tồn tại).
  - Không có `/api/v1`, không có `/api/v2/oauth/token`, không nhận OAuth token.
- **Tenant được ghi:** xóa/expire/đổi tên device, duyệt route, gắn tag (theo `tagOwners` của policy), tạo/thu hồi pre-auth key.
- **Tenant bị chặn (403):**
  - policy: `setACL`;
  - settings: `updateTailnetSettings`;
  - `authorizeDevice`, `setDeviceKey` (tắt key expiry), `updateDeviceClient`;
  - tạo hoặc xóa OAuth client.
- **Cấp API key:**
  - Chỉ platform cấp: CLI (actor `cli:<cluster>`) và placement API mTLS (actor là identity trong cert). Có audit, idempotent.
  - Key chỉ hiện một lần. Bản ghi idempotent và audit không chứa key; replay trả metadata với `replayed: true`.
  - **Bổ sung so với ticket:** key có thể gắn một user của chính tailnet đó (`--user`, `"user"`). Không có user thì tenant không tạo được pre-auth key không tag qua API (`createKey` yêu cầu API key có user). User của tailnet khác bị từ chối ở registry và ở trigger của `credentials`.
- **Metric (Q5):**
  - Nhãn là ID tailnet, không bao giờ là slug, chỉ có trên listener metrics operator.
  - Trần 2000 mỗi process, cấu hình được; vượt trần thì gộp vào `other` và tăng `headscale_tailnet_metric_label_overflow_total`.
  - Thêm `headscale_tailnet_mapresponses_sent_total{tailnet,type}`, để E2E đo được "B không nhận frame update".
- **Không đổi:** ADR 0005 (một Noise key mỗi cluster); `/health`; pool DB; bootstrap.

## Tests + exact commands + exit codes

Máy: mc-ci, go 1.27.4, `GOFLAGS=-buildvcs=false`. Log ở `~/p08b-ev/` và `~/p08b-full/final/` trên mc-ci.

### RED → GREEN

| Biến thể RED (bản sao cây, sửa đúng một chỗ) | Test FAIL | GREEN |
|---|---|---|
| Gauge `Set` (trước sửa) | `TestP08bNodeStoreGaugeIsNotLastWriter`: 2+3 node đọc ra 3 | PASS |
| Admin: không mở socket | 4/4 `TestP08bAdmin*` | PASS |
| Admin: dùng runtime đầu tiên thay vì tailnet được nêu | `TestP08bAdminExpireRoutesUserLive` (listing của B chứa node A) | PASS |
| Admin: bỏ kiểm tra standby | `TestP08bAdminStandbyRefuses` | PASS |
| API: `requireTailnet` nhận mọi tailnet | `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` | PASS |
| API: bỏ `setACL`, `updateTailnetSettings` khỏi danh sách cấm | `TestP08bTenantAPIRefusals` | PASS |
| API: chuyển request vào runtime đầu tiên thay vì tailnet của key | F4, `TestP08bTenantAPIRefusals`, `TestP08bTenantAPIRateLimits` | PASS |
| API: bỏ bucket theo tailnet | `TestP08bTenantAPIRateLimits` | PASS |
| Registry: user không lọc theo tailnet | `TestAPIKeyUserOfTailnet` | PASS |
| Tra key: nhận cả auth key, bỏ kiểm thu hồi/hết hạn | `TestAPIKeyLifecycle` | PASS |

Các test RED đều được chạy bằng `go test -race -count=1 [-tags p05cluster] -run <test>`. Script: `p08b-red-admin.sh`, `p08b-red-api.sh`. Mỗi biến thể có `diff` và log trong `~/p08b-ev/`.

### Test mới

- `hscontrol/state`: `TestP08bNodeStoreGaugeIsNotLastWriter` (tổng, theo tailnet, sau `Stop`, ghi muộn sau `Stop` không cộng lại).
- `internal/tenantlabel`: `TestLabelCap`, `TestSetLimitDefault`.
- `internal/ratelimit`: `TestBucket`, `TestBucketsIndependentAndBounded`.
- `internal/placement`: `TestAPIKeyLifecycle`, `TestAPIKeyUserOfTailnet` (SQLite và PostgreSQL).
- `internal/placement/placementapi`: `TestOperatorAPIKeys` (mTLS, idempotent, audit không có key).
- `hscontrol/servertest` (tag `p05cluster`):
  - `TestP08bAdminDeleteNodeLive`, `TestP08bAdminExpireRoutesUserLive`, `TestP08bAdminNotOnNetworkListeners`, `TestP08bAdminStandbyRefuses`;
  - `TestP08bTenantAPIWritesLiveAndIsolated`, `TestP08bTenantAPIRefusals`, `TestP08bTenantAPIRateLimits`, `TestP08bTenantAPIStandby`;
  - `TestP08bTailnetMetrics`;
  - `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet`: trước đây SKIP, nay chạy trên cluster. Không có tag thì vẫn SKIP, và SKIP không tính là PASS.
- `hscontrol`: `TestEndpointInventory` (allowlist thêm 35 dòng `cluster …`, `cluster-v2 …`, `cluster-unix …`), các test config/docs.

### Lần chạy cuối

Script `p08b-final.sh` trên mc-ci, cây `1923634`, log ở `~/p08b-full/final/`.

| Bước | Lệnh | Exit | Thời gian |
|---|---|---|---|
| vet | `go vet ./...` | 0 | 12 s |
| vet-cluster | `go vet -tags p05cluster ./hscontrol/servertest/` | 0 | 3 s |
| race-all | `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -race -count=1 -timeout 45m ./hscontrol/... ./internal/... ./cmd/...` | 0 | 2080 s |
| race-pg | `MESH_TEST_POSTGRES_DSN=<pg tạm> go test -race -count=1 -timeout 30m ./hscontrol/db/... ./internal/...` | 0 | 163 s |
| cluster-suite | `go test -race -count=1 -timeout 40m -tags p05cluster -run 'Cluster\|P11b\|P08b\|P08a' ./hscontrol/servertest/` | 0 | 656 s |

**Lần chạy trước (cây `10b738b`) có FAIL, đã sửa:**
- `TestNoRawTenantWritesOutsideFence`: hai câu SQL thô ghi `credentials` trong `internal/placement/apikeys.go` chưa được review. Đã đưa vào dưới `tenantscope.LockOwnership` và thêm vào allowlist có lý do (`1923634`).
- `TestACMETLSSettings`: trùng cổng loopback khi bind, không liên quan P08b; chạy lại riêng 3/3 PASS, và PASS trong lần chạy đầy đủ ở trên.

Sau khi sửa đã chạy lại E2E: 16/16 ×2 (`artifacts/1923634/P08b/e2e-20261125T025226Z`, `…T025500Z`).

## E2E artifacts / protocol map redacted

Harness: `infra/lab/e2e/p08b/run.sh`.
- Đường đi: client trên mc-ci / laptop → VPS → wg-mesh → mc-cp:8443.
- Một process c1, PostgreSQL trên mc-db (DB tạm). mesh-placement chạy trên loopback của mc-cp với CA tạm trong `/run`.
- Client Tailscale thật: a1 a2 a3 (a1 quảng bá 10.88.0.0/24), b1 b2.

| Ca | Lần 1 (v1.104.7, `artifacts/f415fcb/P08b/e2e-20261125T015342Z`) | Lần 2 (v1.84.3, `…/e2e-20261125T015611Z`) |
|---|---|---|
| M01 duyệt route qua socket | a2 thấy route của a1 sau 1544 ms | 498 ms |
| M02 expire a2 | a1 thấy sau 536 ms, a2 `NeedsLogin` | 576 ms |
| M03 xóa a3 | a1 bỏ a3 sau 1332 ms, row mất | 566 ms |
| M04 xóa user | user rỗng: xóa; user còn node: 409 | như lần 1 |
| M05 ID/user của B qua A | 404, B không đổi; socket `srwxrwx---` | như lần 1 |
| B không nhận frame update (M01–M05, A02, A04) | `headscale_tailnet_mapresponses_sent_total{tailnet=b,type=update}` 8 → 8 | 8 → 8 |
| K01 cấp key | CLI (a, user e2e-a) và placement mTLS (b); replay không lộ key; thiếu `Idempotency-Key` → 422; audit đủ, không chứa key | như lần 1 |
| A01 key của a | devices [a1 a2] với `-` và ID của a; Bearer và Basic | như lần 1 |
| A02 8 request nhắm tới b/b1 bằng key của a | tất cả 404 | như lần 1 |
| A03 ghi bị cấm | acl, settings, authorized, key, update-client, OAuth client → 403; revision 2/1 không đổi | như lần 1 |
| A04 pre-auth key tạo qua API | a4 vào a (787 ms); xóa qua API → a1 bỏ a4 | 517 ms |
| A05 credential sai | garbage, auth key, key bị CLI thu hồi, key bị placement thu hồi → 401 | như lần 1 |
| A06 giới hạn tần suất | 60 request song song: 34×200, 26×429; key b ngay sau → 200 | 36×200, 24×429 |
| A07 listener công khai | `/api/v1`, `/metrics`, `/cluster/v1` → 404; `oauth/token` → 401 | như lần 1 |
| X01 metric | `tailnet_nodes` a=2 b=2, `nodestore_nodes 4`, có `limited` cho a, nhãn là ID | như lần 1 |
| S01 | không có key nào chưa redact trong 25 file | như lần 1 |
| Tổng | **16/16 PASS**, 134 s, CLEAN_OK=1 | **16/16 PASS**, 125 s, CLEAN_OK=1 |

Ở A06 có nhiều hơn 20 request được 200 (burst của key) vì 60 request đi qua Internet trải trong vài giây và key được nạp lại 10/s. Điều cần chứng minh là có 429, còn key khác tailnet thì không bị ảnh hưởng.

Hai lần chạy thử trước đó dừng ở setup vì lỗi của harness, không phải lỗi code:
- tách đường dẫn Windows `C:` theo dấu `:`;
- quyền 0750 của `/run/p08b-placement` không cho user ssh đi qua.

Cả hai lần cleanup đều chạy đủ: DB tạm đã xóa, POC đã khôi phục.

## Negative tenant isolation tests

- Live admin: ID node/user của B gọi qua đường dẫn của A trả 404 và B không đổi (servertest và E2E M05). Listing của mỗi tailnet chỉ có node của nó. Không có frame nào tới client của B (servertest `assertQuiet`; E2E dùng metric update của b).
- `/api/v2`: key của A không đọc, ghi hay xóa được gì của B (F4 12 route, E2E A02 8 route). Key của tailnet không do cluster phục vụ trả 404. Key bị thu hồi, hết hạn, sửa đổi, auth key hay OAuth token đều trả 401 với cùng body. User của B không gắn được vào key của A.
- Giới hạn tần suất của A không chặn B (servertest và E2E A06).
- Metric không chứa slug.

## DB migrations and downgrade constraints

- Không có migration mới. API key dùng bảng `credentials` có sẵn (`kind='api'`, `tailnet_id NOT NULL`, trigger khớp user/tailnet). Thu hồi = `revoked` + `hash = NULL` (ràng buộc `chk_credentials_hash` có sẵn).
- Placement ghi `platform_audit` và `platform_idempotency` có sẵn.

## Benchmark

Không đổi hot path DB. `writeMap` thêm một lần `Inc` counter, đã cache theo phiên. Không chạy benchmark riêng.

## Known limitations / security blockers

- **Cần review bảo mật trước `Reviewed`** (Q3: endpoint công khai có xác thực): `cluster_apiv2.go`, `api/v2/api.go` (`clusterForbidden`, `requireTailnet`), `platform_apikey.go`, `apikeys.go`, `cluster_admin.go`.
- **Quản trị live:**
  - CLI phải chạy trên host của process active; standby trả 503. Chưa tìm process active từ xa.
  - Quyền truy cập dựa trên quyền file socket (nhóm `headscale`).
- **Giới hạn tần suất của `/api/v2`:**
  - Chỉ có toàn process, theo key, theo tailnet. Không có theo IP, vì sau VPS/WireGuard mọi request có cùng địa chỉ nguồn; cần PROXY protocol.
  - Bucket nằm trong bộ nhớ của từng process, nên mất khi failover.
- **Chưa làm, theo quyết định chủ dự án:**
  - Giới hạn register/MapRequest theo tailnet, trần phiên Noise chưa xác thực, `/health` không truy vấn DB mỗi request, và S13 (Q1 → P14).
  - DERP verify, S15 (Q6).
  - Pool DB theo tailnet: chủ dự án quyết định không giới hạn.
- **Trùng lặp mã:** `internal/ratelimit` chép bucket từ `internal/bootstrap`; bootstrap chưa chuyển sang dùng nó.
- **Phạm vi verify:**
  - `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` chỉ chạy với `-tags p05cluster`.
  - Chưa chạy `golangci-lint`.
  - `gofmt` báo 4 file có sẵn từ trước (`db/nodekeys.go`, `types/credential.go`, `types/tenant_dns_test.go`, `state/node_store_test.go`), không thuộc P08b.
- **Metric:** nhãn đã cấp giữ đến khi process khởi động lại. Tailnet đã chuyển đi vẫn còn series với giá trị 0.

## Reviewer approval

Chưa.
