# Source Inventory — P12d (subnet router qua API của tenant: policy, DNS, key expiry)

- **Commit gốc:** `1cb1379` (`feat/P12d-subnet-api`, tách từ nhánh tích hợp P12c `303f1bb`/`f4ea6d4`, chưa vào `master`), ngày 2026-11-26.
- **Nhánh:** `feat/P12d-subnet-api` (worktree `wt-p12d`). Chỉ push `mcci`, không push `origin`, không merge.
- **Trạng thái khi viết file này:** `Planned` (chưa đổi code).
- **Phạm vi phiên này:** API + test trong process (servertest `p05cluster`, unit, DB) + `docs/tenant-api/subnet-routers.md` + case E2E S08c **viết nhưng không chạy** (lab đang bận). Không migration mới (không cần: `tailnets.policy_revision`, `tailnets.config_revision`, `policies`, `tailnet_settings` đã có).

## 1. Đã đọc

| Nguồn | Nội dung liên quan |
|---|---|
| `AGENTS.md` | Rule 5 (inventory trước), 6 (scope từ context, fail closed), 8 (policy rỗng/`{}` = DENY, không fallback allow-all), 9 (không debug/metric trên listener công khai), 10 (Huma + `make openapi`/`make generate`, không sửa tay `gen/`), 13 (isolation = BLOCK MERGE) |
| `tickets/P12d.md` | Hợp đồng: §1 bảng endpoint, ràng buộc chung, §2 danh sách RED, §3 tài liệu, điều kiện dừng |
| `tickets/P08b.md` Q3, Q4 | Tenant trước đây không được ghi policy/DNS (`clusterForbidden`); P12d thay Q3 cho policy và DNS. Q4: mọi giới hạn cấu hình được dưới `tuning.*` |
| `design/adr/0008-per-tailnet-dns.md` | §2 trường tenant đặt được (`magic_dns`, `override_local_dns`, `nameservers.global/split`, `search_domains`, `extra_records`); §3 validate (split domain không nằm trong base domain của cluster, record chỉ dưới `<slug>.<base>`), giới hạn `tuning.tenant_dns.*`, ghi lỗi → giữ giá trị cũ; §4 "API v2 cho tenant: ticket sau, cần review authZ và rate limit riêng"; §5 reconcile `config_revision` → `ReplaceTenantDNS` + `change.DNSConfig` chỉ cho runtime đó |
| `evidence/REVIEW-2026-11-26-tickets.md` | A1–A8 (scope plugin: không `Table()`, không `.Or()`, không gán `tailnet_id`, raw SQL phải qua allowlist), A6 (policy rỗng phải deny ngay), B4 (thứ tự bucket), L4, L9 (revision ghi khi reload lỗi) |
| `hscontrol/api/v2/api.go` | `Backend{State, Change, Cfg, Connected, Tailnet}`; `clusterForbidden` (`setACL`, `updateTailnetSettings`, `authorizeDevice`, `setDeviceKey`, `updateDeviceClient`); `authMiddleware` (xác thực lại API key qua handle scoped; trong cluster mode op bị cấm: tailnet khác/thiết bị khác → 404 trước 403); `requireTailnet` (`-` hoặc `b.Tailnet`, khác → 404) |
| `hscontrol/api/v2/acl.go` | `getACL`/`setACL`/`validateACL`; `defaultPolicy` (allow-all, dùng khi không có policy — **sai cho tenant**: tenant không có policy = deny); `UpdatePolicyIf` với `If-Match` dưới khóa; ETag = sha256 của byte lưu; `MaxBodyBytes` 1 MiB cố định |
| `hscontrol/api/v2/settings.go`, `devices.go` (`setDeviceKey:426`, `setDeviceRoutes:452`, `getDeviceRoutes:483`, `lookupNode:583`), `keys.go`, `errors.go` (`mapError`, `policyRefusal`), `helpers.go` | Mẫu đăng ký Huma, lỗi kiểu Tailscale, thiết bị tra qua state của runtime (thiết bị tailnet khác không có → 404) |
| `hscontrol/cluster_apiv2.go` | `APIv2Handler`: standby → 503; key → `TailnetForAPIKey`; bucket key → tailnet → global (P12c B4); tailnet không phục vụ → 404; `rt.apiV2()` dựng `apiv2.Backend` theo runtime |
| `hscontrol/cluster.go` | `load` (runtime config `cfg.ForTailnet(slug, dns)`, `policyRev`/`configRev`), `reconcile` (so revision, `ReloadPolicy`, `ReplaceTenantDNS`), `tailnetDNS` (validate với base domain cluster + `tuning.tenant_dns`), `cfg.PolicyEmptyDenies = true` |
| `hscontrol/cluster_admin.go` | Mẫu audit log của kênh quản trị live |
| `hscontrol/state/state.go` | `policyOptions` (cùng option cho validate và cài), `ReloadPolicy` (luôn trả `PolicyChange`), `reloadPolicyLocked` (P12c A6: tenant rỗng → deny ngay), `ValidatePolicy`, `UpdatePolicyIf` (validate → khóa `policyMu` → match → `db.SetPolicy` → reload), `SetNodeKeyExpiryDisabled` |
| `hscontrol/policy/v2/test.go:826` `ValidatePolicy`, `policy.go:761` `SetPolicy` | Chạy `tests` của policy trong sandbox trước khi cài; rỗng → `ErrPolicyEmpty` |
| `hscontrol/db/policy.go` | `SetPolicy` (insert + xóa dòng cũ trong một `Write`), `PolicyBytes` (rỗng → nil) |
| `hscontrol/db/tailnet_settings.go` | `Platform.SetTailnetDNS` (một transaction: `LockOwnership`, kiểm owner, validate, upsert raw SQL, bump `config_revision`), `Platform.TailnetDNS` |
| `hscontrol/db/platform_admin.go:210` `BumpPolicyRevision` | Transaction riêng trên handle platform, `LockOwnership` trước |
| `hscontrol/db/tenantscope/tenantscope.go`, `tables.go` | `TenantTables` gồm `policies`, `tailnet_settings`; `checkFence` (advisory shared → `FOR SHARE` dòng `tailnets` → lease); `LockOwnership` phải là câu đầu của transaction cập nhật dòng `tailnets`; thứ tự khóa: advisory tailnet → dòng tailnets → advisory cluster |
| `hscontrol/db/tenant.go` (`scopeTo`, `ScopedContext`), `db.go` (`Write`, `Tailnet()`) | Handle runtime: scope + fence (owner/epoch + lease) trong context |
| `cmd/headscale/cli/cluster.go` (`setTailnetPolicyCLI:245`, `validateTailnetPolicy:288`, `tailnetDNSCmd`) | Đường operator: validate với ngữ nghĩa tenant, `hs.SetPolicy` rồi `BumpPolicyRevision` (hai transaction); DNS qua `Platform.SetTailnetDNS` |
| `hscontrol/types/tenant_dns.go` | `TenantDNS`, `Validate(slug, clusterBase, lim)`, `ParseTenantDNS` (strict), `ForTailnet`, `ReplaceTenantDNS` |
| `hscontrol/types/config.go:1048-1095`, `hscontrol/config/{default,validate}.go`, `types/config_fields_test.go`, `tools/configgen` | `TenantAPIConfig` (rate/burst theo giây), validate ≥ 1, tài liệu sinh bằng `go generate ./hscontrol/types` |
| `hscontrol/endpoint_inventory_test.go`, `hscontrol/testdata/p08a_endpoint_allowlist.txt`, `evidence/P08a-endpoint-inventory.md` §8 | Mọi route `public`/`cluster-v2`/`unix` phải có trong allowlist và tài liệu |
| `hscontrol/servertest/cluster_p05.go`, `cluster_p06.go`, `cluster_p08b.go`, `cluster_p10.go`, `cluster_p11b.go`, `cluster_p08b_api_test.go`, `cluster_p08b_test.go`, `cluster_p12c_policy_test.go`, `issues_test.go` (`advertiseRoutes`) | Harness: `NewTestClusterWith` (SQLite), `MintAPIKey`, `apiCall`, `tailnetRevisions`, `settledFrames`/`assertQuiet`, `twoTailnetMesh`, `newHACluster`/`StartProcess` |
| `hscontrol/db/{tailnet_settings_test,fence_platform_test,suite_test}.go` | `backends(t)` (SQLite + PostgreSQL khi có `MESH_TEST_POSTGRES_DSN`), `openPlatform`, `platformTailnet`, `tenantDB` |
| `tailscale.com/client/tailscale/v2@v2.12.0/dns.go` (module cache trên mc-ci) | Hình dạng API DNS của Tailscale: `{"dns":[…]}`, `{"searchPaths":[…]}`, split-dns map (PATCH: `null` xóa domain; PUT: thay toàn bộ, `{}` xóa hết), `{"magicDNS":bool}` |
| `infra/lab/e2e/n1/run.sh`, `cases3.sh` (`api_v2`), `cases4.sh` (S08b) | Mẫu cho S08c |

## 2. Ràng buộc hiện có (không được làm yếu)

- Tailnet của request chỉ lấy từ API key; `{tailnet}` chỉ `-` hoặc ID của key; tailnet khác và thiết bị tailnet khác → 404 (không lộ tồn tại).
- Thứ tự rate limit P12c B4: tiền xác thực → key → tailnet → global. Standby → 503; tailnet không phục vụ → 404.
- Mọi ghi dữ liệu tenant đi qua plugin `tenantscope` (scope + fence owner/epoch/ACTIVE/lease trong cùng transaction). Raw SQL mới phải có trong allowlist đã review.
- Policy tenant: rỗng/`{}` = DENY và cài ngay (P12c A6); lỗi parse không bao giờ dẫn tới allow-all; `tests` chạy trong `SetPolicy`.
- DNS tenant: ràng buộc ADR 0008 §3 và giới hạn `tuning.tenant_dns.*`; ghi lỗi → giá trị cũ còn nguyên; DNS của A không phát change sang runtime B.
- Không thêm debug/metrics trên listener công khai.

## 3. Thiết kế dự kiến (chi tiết và RED/GREEN trong `evidence/P12d-subnet-api.md`)

1. `setACL` ra khỏi `clusterForbidden`. Ghi qua `State.UpdatePolicyIf` của runtime; khi handle có fence, lưu policy **và** tăng `tailnets.policy_revision` trong **một** transaction trên handle runtime: `LockOwnership` (câu đầu) → `UPDATE tailnets … WHERE id, owner_cluster_id, owner_epoch của fence, ACTIVE, chưa xóa` → insert policy (plugin fence) → xóa dòng cũ. Thứ tự khóa giữ đúng tenantscope. Runtime ghi lại revision mới để reconcile không nạp lại lần nữa.
2. Policy mặc định hiển thị khi không có policy: ở tailnet tenant là deny (`{"grants": []}`), không phải allow-all; `ts-default` khớp với nó.
3. Giới hạn kích thước policy `tuning.tenant_api.max_policy_bytes`; bucket ghi cấu hình theo tailnet `tuning.tenant_api.config_write_rate` (mỗi phút) / `config_write_burst`, áp cho `setACL`, `validateACL` và mọi ghi DNS, **sau** `requireTailnet` (request tới tailnet khác không tốn token).
4. Audit: một dòng log `tenant API config write` (tailnet ID, op, API key ID số, revision, sha256 của nội dung lưu), không bao giờ ghi nội dung.
5. DNS kiểu Tailscale: `GET/POST …/dns/nameservers`, `GET/POST …/dns/searchpaths`, `GET/PATCH/PUT …/dns/split-dns`, `GET/POST …/dns/preferences`. Ghi = đọc-sửa-ghi `tailnet_settings` trong một transaction trên handle runtime (model GORM có `TailnetID`, không raw SQL) + `LockOwnership` + tăng `config_revision`; validate bằng `TenantDNS.Validate(slug, base domain cluster, tuning.tenant_dns)`; áp ngay cho runtime đó (`ReplaceTenantDNS` + `change.DNSConfig`). Ngoài cluster mode: đọc từ file config, ghi → 501.
6. `setDeviceKey` ra khỏi `clusterForbidden` (body chỉ có `keyExpiryDisabled`); `authorizeDevice`, `updateDeviceClient`, `updateTailnetSettings` vẫn bị cấm.

## 4. Giả định (kiểm lại trong PR)

- G1: Trong một transaction PostgreSQL, giữ advisory lock exclusive (`LockOwnership`) rồi lấy advisory shared cùng khóa (fence) không tự chặn (lock manager không xung đột với chính backend). Sẽ chạy test DB trên PostgreSQL.
- G2: Cập nhật dòng `tailnets` từ handle runtime (bảng platform, không phải bảng tenant) không bị plugin chặn; đây là đường mới — trước đây chỉ code platform ghi `tailnets`. Chấp nhận vì điều kiện WHERE lặp lại đúng fence (owner/epoch/ACTIVE) và cùng transaction với ghi có fence.
- G3: Tailscale trả 412 khi `If-Match` lệch (ticket ghi 409/412); giữ 412 như hiện có.
- G4: `preferences` của Tailscale chỉ có `magicDNS`; `override_local_dns` (ADR 0008) không có endpoint Tailscale tương ứng ngoài `dns/configuration` (alpha). Thêm trường tùy chọn `overrideLocalDNS` vào `preferences` (bỏ qua khi vắng) và ghi khác biệt trong tài liệu.

## 5. Sửa theo review bảo mật P12d (nhánh `feat/P12d-review-fixes`)

- **Commit gốc:** `7cacbc1` (`feat/P12d-subnet-api`), ngày 2026-11-26. Reviewer độc lập: không BLOCKER; 2 MEDIUM/LOW-MEDIUM, 4 LOW, 2 Info, thêm một test thiếu (lease mất khi owner/epoch còn khớp → 503). Mọi phát hiện chỉ chạm dữ liệu của chính tenant.
- **Trạng thái khi viết mục này:** `Planned` (chưa đổi code).

| Nguồn đã đọc | Liên quan tới phát hiện |
|---|---|
| `hscontrol/cluster.go` `load` (`policyRev`/`configRev`, `ObserveRevisions`), `reconcile:436-530` (đọc DNS qua `c.tailnetDNS` = handle platform, rồi `rt.h.cfg.ReplaceTenantDNS` **không** giữ `tenantDNSMu`; ghi revision trước khi nạp; `ReloadPolicy` luôn trả `PolicyChange`), `tailnetDNS:586` | 1 (DNS cũ cài đè DNS mới, không tự lành vì `configRev` = DB), 8 (reconcile gửi lại `PolicyChange` cho revision mà ghi qua API đã cài: thứ tự commit → reconcile so revision → observer) |
| `hscontrol/state/state.go` `UpdateTenantPolicyIf:2063-2126` (validate **ngoài** `policyMu`; `If-Match` đọc `GetPolicy` ngoài transaction; lưu; `reloadPolicyLocked` đọc lại DB và chạy `polMan.SetPolicy` — test của policy chạy lần hai; lỗi sau commit → trả lỗi, không audit), `UpdateTenantDNS:2140-2157`, `ValidatePolicy:2025`, `ReloadPolicy:658`, `reloadPolicyLocked:680`, `SetNodeKeyExpiryDisabled:1413` | 1, 2, 3, 6, 8 |
| `hscontrol/policy/pm.go` (interface `PolicyManager`, một hiện thực), `policy/v2/policy.go:761` `SetPolicy` (parse → `validateUserReferences` → `evaluateTestsFor`/`evaluateSSHTests` → `updateLocked`; lỗi thì giữ policy cũ), `policy/v2/test.go:826` `ValidatePolicy` (sandbox `NewPolicyManager` + `SetPolicy` + kiểm SSH) | 2, 8 (test chạy hai lần mỗi ghi) |
| `hscontrol/db/tenant_config.go` (`bumpTailnetRevision`: `LockOwnership` → `UPDATE tailnets` có điều kiện fence; `SetPolicyRevised`; `tenantDNSRow`: lỗi parse bọc `ErrInvalidTenantDNS`), `db/policy.go` (`GetPolicy`, `PolicyBytes`), `db/tenantscope/tenantscope.go:408-454` (`LockOwnership`: advisory exclusive, no-op SQLite), `checkFence`/`checkLease:518` | 1, 3 (kiểm `If-Match` trong transaction sau `LockOwnership`: ghi có fence giữ advisory shared nên bị xếp sau), 4 (lỗi stored DNS hỏng thành 400 có nội dung parse) |
| `hscontrol/db/platform_admin.go:210` `BumpPolicyRevision`, `cmd/headscale/cli/cluster.go:245` `setTailnetPolicyCLI` (operator: `hs.SetPolicy` rồi bump, hai transaction), `db/tailnet_settings.go:76` `SetTailnetDNS` | 3 (ghi của operator xen giữa kiểm và lưu) |
| `hscontrol/api/v2/acl.go` (`setACL`/`validateACL`: `MaxBodyBytes` 1 MiB cố định, kích thước + bucket kiểm **sau** khi Huma đã đọc hết body), `dns.go` (`writeDNS`, `readDNS` 500 kèm lỗi), `tenant.go` (`tenantWriteGate`, `auditTenantWrite`), `errors.go:61-85` (transformer đưa `ErrorModel.Errors` vào `data`), `errors.go:198-199` (503 kèm lỗi fence: cluster, holder, epoch), `devices.go:425` `setDeviceKey` (không audit, không bucket), `api.go` (`authMiddleware`, `requireTailnet`, `NewAPI`: middleware chụp lúc đăng ký) | 4, 5, 6 |
| Huma v2.40.0 (`huma.go:909` middleware của API rồi của operation chạy **trước** khi đọc body; `huma.go:1158` `readBody` với `MaxBodyBytes`, vượt → 413; `ensureMaxBodyBytes` mặc định 1 MiB) | 5 |
| `hscontrol/types/tenant_dns.go` (`Validate`: search domain chỉ kiểm cú pháp; split domain chỉ cấm trong miền cluster; split rỗng không tính; nameserver nhận mọi IP), `design/adr/0008-per-tailnet-dns.md` §3 (record chỉ dưới miền của chính tailnet hoặc ngoài miền cluster; hạ giới hạn: dữ liệu cũ vẫn nạp, có cảnh báo; DNS lưu không validate được → không nạp runtime) | 7 |
| `hscontrol/servertest/cluster_p05.go` (`ClusterOptions.Lease`), `cluster_p11b.go` (`ExpireLease`), `cluster_p06.go` (`SetTailnetDNS`, `StoreRawTailnetDNSForTest`), `cluster_p12d_api_test.go` | Test |

Ràng buộc giữ nguyên: mục 2 ở trên; raw SQL guard và allowlist không đổi (không thêm raw SQL); inventory endpoint và ma trận OAuth không đổi (không route mới).

Giả định:
- R1: Trên PostgreSQL, ghi policy của operator qua handle có fence (`hs.SetPolicy`) lấy advisory shared của tailnet nên bị chặn khi ghi của tenant đang giữ `LockOwnership` (exclusive); trên SQLite người ghi được tuần tự hóa. Vì vậy kiểm `If-Match` trong transaction sau `LockOwnership` thấy mọi ghi đã commit trước nó. Kiểm bằng test DB trên cả hai backend.
- R2: Luật mới cho DNS (search domain trong miền cluster, split là miền cha của miền cluster, nameserver loopback/link-local/unspecified, số split domain) áp cho mọi lần **ghi** (tenant và operator); khi **nạp** dữ liệu đã lưu trước đó, chúng chỉ cảnh báo như giới hạn bị hạ (ADR 0008 §3), không chặn runtime.
