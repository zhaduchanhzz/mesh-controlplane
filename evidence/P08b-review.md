# P08b — mô tả PR và checklist review bảo mật

Nhánh `feat/P08b-control-plane` → `master`. Evidence đầy đủ: `evidence/P08b-control-plane.md`. Ticket: `tickets/P08b.md`. Quyết định của chủ dự án: Q1–Q6 trong ticket.

## Mô tả PR

### Thay đổi

1. **Gauge `nodestore_nodes` đúng ở cluster mode.**
   - Trước: mỗi runtime `Set` đè giá trị của nhau. Nay mỗi NodeStore cộng phần chênh lệch của mình và trừ ra khi `Stop`.
   - Thêm `headscale_tailnet_nodes{tailnet}`.
2. **Quản trị live trên unix socket** (`unix_socket`, quyền file `unix_socket_permission`).
   - Route: `GET /cluster/v1/tailnet/{id}/nodes`; `DELETE …/node/{n}`; `POST …/node/{n}/expire`; `POST …/node/{n}/routes`; `DELETE …/user/{name}`.
   - CLI: `cluster tailnet --slug S node|routes|user …`.
   - Thao tác chạy trong runtime của tailnet, nên client nhận thay đổi qua đường delta.
3. **`/api/v2` trên listener công khai của cluster.**
   - API key do platform cấp chọn tailnet: CLI `cluster tailnet apikey create|list|revoke`, hoặc placement API mTLS `/v1/tailnets/{id}/apikeys`.
   - Tenant quản lý device và pre-auth key. Ghi policy, settings và trust thiết bị trả 403.
   - Giới hạn tần suất toàn process, theo key, theo tailnet (`tuning.tenant_api.*`).
4. **Metric theo tailnet**, nhãn là ID tailnet, trần 2000 (`tuning.tailnet_metric_label_limit`).
   - Series: `headscale_tailnet_{nodes,map_sessions,mapresponses_sent_total,netmap_divergence_total,api_requests_total}`, `headscale_cluster_api_refused_total`.

### Không đổi

- Không có migration.
- Không đổi Noise key (ADR 0005), `/health`, pool DB, bootstrap.
- Không có `/api/v1` trên cluster.
- Không có debug hoặc metrics trên listener công khai.

### Kiểm chứng

- **E2E lab:** 16/16 ×2 với client v1.104.7 và v1.84.3; B nhận 0 frame update khi thao tác trên A.
- **RED → GREEN:** 13 biến thể.
- **Endpoint inventory:** allowlist thêm 35 dòng.
- **Race toàn repo, PostgreSQL, suite cluster:** xem §"Lần chạy cuối" trong evidence.

### Rollback

- Không có migration, nên revert merge là đủ.
- API key đã cấp vẫn nằm trong `credentials` (`kind='api'`). Sau khi revert, cluster không mount `/api/v2`, nên các key này không dùng được nữa. Muốn xóa hẳn thì thu hồi bằng `apikey revoke`.

## Checklist review bảo mật

Đánh dấu từng mục. Mục nào sai = **BLOCK MERGE** (AGENTS 13).

### A. Chọn tailnet và fail closed (AGENTS 6)

- [ ] `hscontrol/cluster_apiv2.go` `APIv2Handler`: tailnet chỉ lấy từ `Platform.TailnetForAPIKey`. Không có header, path hay host nào đổi được tailnet.
- [ ] `hscontrol/db/platform_apikey.go`:
  - chỉ nhận prefix `hskey-api-` (không nhận legacy, auth key hay OAuth token);
  - `revoked IS NULL`, hash khác rỗng, `expiration` chưa qua;
  - lỗi tra cứu là `503`, không phải bỏ qua;
  - mọi lỗi credential trả cùng `ErrUnknownTenant` → 401 cùng body.
- [ ] Runtime xác thực key lần hai qua handle scoped: `apiv2.authMiddleware` → `State.AuthenticateAPIKey`. Key của tailnet khác không qua được lớp này.
- [ ] `requireTailnet` (`hscontrol/api/v2/api.go`): chỉ `-` hoặc `Backend.Tailnet`. Cả 12 chỗ gọi đã đổi; không còn `requireDefaultTailnet`.
- [ ] `/api/v2/device/{id}`: `lookupNode` tra trong NodeStore của runtime. Node ID toàn cục (ADR 0002), nên device của tailnet khác → 404.
- [ ] Route bị cấm trả 404 trước 403 khi nhắm tailnet hoặc device khác, để 403 không thành dấu hiệu "tailnet/device này tồn tại".
- [ ] Tailnet không do process phục vụ → 404; standby (`!ready || !leaseHeld`) → 503; runtime đang retire (`!servable`) → 404.

### B. Phạm vi ghi của tenant (Q3, AGENTS 8)

- [ ] `clusterForbidden` gồm `setACL`, `updateTailnetSettings`, `authorizeDevice`, `setDeviceKey`, `updateDeviceClient`. Đối chiếu với 20 operation trong `cluster-v2 …` của `hscontrol/testdata/p08a_endpoint_allowlist.txt`. Không có đường ghi policy/DNS nào khác (`validateACL` chỉ đọc).
- [ ] OAuth: ở cluster mode từ chối access token (401), không mount `/api/v2/oauth/token`, `createKey keyType=client` → 403, `deleteKey` không đi nhánh OAuth.
- [ ] Không có operation nào tạo API key. Key chỉ đến từ `internal/placement/apikeys.go`.
- [ ] `setDeviceTags`: tag chỉ hợp lệ khi `tagOwners` trong policy của operator cho phép (dùng `State.SetNodeTags` có sẵn).

### C. Cấp API key (Q3)

- [ ] `internal/placement/apikeys.go`:
  - SQL thô (tenantscope không lọc): mọi câu `INSERT/UPDATE/SELECT` đều có `tailnet_id` của tailnet được nêu;
  - `RevokeAPIKey` có điều kiện `tailnet_id = ? AND kind = 'api' AND id = ?`;
  - user của key tra bằng `tailnet_id = ? AND name = ?`, và trigger `credentials` chặn thêm một lần.
- [ ] Key không được lưu và không được ghi log. `platform_idempotency.response` và `platform_audit.detail` chỉ chứa metadata (test `TestAPIKeyLifecycle`, `TestOperatorAPIKeys`, E2E K01).
- [ ] Placement API: ba operation mới dùng `op()` (mutation) và `readGuard` (đọc). Mutation bắt buộc `Idempotency-Key`. Thời hạn tối đa 365 ngày.
- [ ] CLI `apikey create` in key ra stdout đúng một lần (công cụ của operator). Harness redact key trong mọi artifact (E2E S01).

### D. Quản trị live (Q2)

- [ ] `hscontrol/cluster_admin.go`: chỉ phục vụ trên unix socket. Không có trên `HTTPHandler` hay `MetricsHandler` (test `TestP08bAdminNotOnNetworkListeners`, allowlist `cluster-unix`).
- [ ] `{tailnet}` chỉ nhận ID; node/user tra trong runtime đó; standby → 503.
- [ ] `listenAdminSocket` chỉ xóa file cũ nếu là socket. Quyền file theo `unix_socket_permission` (lab: `0770`, nhóm `headscale`).
- [ ] Mỗi lệnh ghi một dòng audit log (op, tailnet, target), không có secret.
- [ ] `DeleteUser` vẫn từ chối user còn node (409).

### E. DoS và giới hạn (Q4)

- [ ] Bucket toàn process được kiểm **trước** khi tra DB.
- [ ] Bucket theo key được kiểm trước bucket theo tailnet.
- [ ] `ratelimit.Buckets` giới hạn 100 000 khóa; khóa chỉ là identifier của key đã xác minh hoặc ID tailnet.
- [ ] Body: Huma giới hạn theo operation; admin socket giới hạn 64 KiB.
- [ ] Đã chấp nhận: không giới hạn theo IP (sau VPS mọi request cùng nguồn); bucket mất khi failover.

### F. Metric và lộ thông tin (AGENTS 9, I15)

- [ ] Nhãn `tailnet` là ID, không bao giờ là slug (`internal/tenantlabel`); trần `tuning.tailnet_metric_label_limit`.
- [ ] Các series này chỉ có trên listener metrics operator. `/metrics` trên listener công khai → 404 (E2E A07).
- [ ] Error body của `/api/v2` không chứa ID, tên hay slug của tailnet khác.

### G. Cách chạy lại bằng chứng

- Unit + servertest:

  ```
  go test -race -count=1 ./internal/tenantlabel/ ./internal/ratelimit/ ./internal/placement/... ./hscontrol/state/ -run 'P08b|APIKey|Label|Bucket'
  go test -race -count=1 -tags p05cluster -run 'P08b|P08aAPIv2' ./hscontrol/servertest/
  ```

- RED: script `p08b-red-admin.sh`, `p08b-red-api.sh` (bản ghi trong evidence). Mỗi biến thể sửa một dòng trong bản sao cây; test tương ứng phải FAIL.
- E2E:

  ```
  BIN=<headscale linux/amd64> PLBIN=<mesh-placement linux/amd64> [IMAGE=tailscale/tailscale:v1.84.3] infra/lab/e2e/p08b/run.sh
  ```

  Exit 0 khi không có FAIL và cleanup đã chứng minh.

### Kết luận của reviewer

- Người review:
- Ngày:
- Kết quả: ☐ Duyệt ☐ Duyệt có điều kiện ☐ Từ chối
- Ghi chú:
