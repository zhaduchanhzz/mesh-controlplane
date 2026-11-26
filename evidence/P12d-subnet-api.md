# PR P12d Evidence (subnet router qua API của tenant: policy, DNS, key expiry)

- **Repo commit before / after:** `1cb1379` (ticket P12d, trên nhánh tích hợp P12c) → tip `feat/P12d-subnet-api` (bảng commit §1; tip ghi trong báo cáo cuối).
- **Trạng thái:** API (§1 ticket) và tài liệu (§3) `Implemented`; servertest/unit/DB `Verified` trên mc-ci (lệnh và exit code bên dưới). E2E S08c **chưa chạy** (harness do agent khác viết trong `wt-p12d-e2e`, coordinator chạy). Chưa `Reviewed`.
- **Verified source paths & symbols:** `evidence/source-inventory-P12d.md`.
- **Môi trường test:** mc-ci, Go 1.27.4; `-race` với `CGO_ENABLED=1`; PostgreSQL tạm (`initdb`, unix socket, port ngẫu nhiên, xóa sau khi chạy) qua `MESH_TEST_POSTGRES_DSN` + `MESH_TEST_REQUIRE_POSTGRES=1`; servertest `-tags p05cluster` (SQLite). Không chạy toàn bộ suite p05cluster (mc-ci dùng chung với E2E đang chạy); chỉ các test bị đụng.

## 1. Commit

| Commit | Nội dung |
|---|---|
| `e90eddd` | Source inventory (AGENTS rule 5), trước mọi thay đổi code |
| `0054e2c` | Khung (không có hành vi): khóa cấu hình `tuning.tenant_api.config_write_rate/config_write_burst/max_policy_bytes` (validate, tài liệu sinh lại bằng `tools/configgen`), `apiv2.Tenant/TenantWrite`, `Cluster.SetTenantAuditHookForTest`, stub DB, servertest `ProcessDo` + gộp mặc định `TenantAPI`. Để test RED biên dịch được và fail vì hành vi, không vì thiếu symbol |
| `ac7af56` | **RED**: servertest P12d, test DB (SQLite + PostgreSQL), test DNS single-tailnet |
| `0800a80` | Tài liệu `docs/tenant-api/subnet-routers.md` (hợp đồng API trước, để S08c code theo) |
| GREEN | Hiện thực + client sinh lại + inventory endpoint + evidence (xem `git log`) |

## 2. RED → GREEN

| Yêu cầu (ticket §2) | Test | RED (`ac7af56`) | GREEN |
|---|---|---|---|
| Key A ghi policy của A, B không đổi; audit | `TestP12dTenantPolicyWrite` | FAIL: `403 managed by the platform operator` | PASS |
| `{tailnet}` = ID của B → 404 | `TestP12dTenantPolicyWrite`, `TestP12dTenantDNS` | (policy) đã 404 từ P08b; DNS: route không tồn tại | PASS |
| Policy rỗng → deny ngay | `TestP12dTenantPolicyWrite` (`{}` → mọi client A mất peer và filter), `TestP12dTenantDefaultPolicyIsDeny` (không có policy đọc ra deny, không phải allow-all) | FAIL: 403; GET trả `defaultPolicy` allow-all `"*:*"` | PASS |
| Policy lỗi → 400, policy cũ còn hiệu lực | `TestP12dTenantPolicyWrite` (không parse, trường lạ, `tests` fail, body rỗng; filter port-22 của a1 giữ nguyên, revision không đổi) | FAIL: 403 | PASS |
| ETag lệch → 412 | `TestP12dTenantPolicyWrite`, `TestP12dTenantGoSDK` | FAIL: 403 | PASS |
| Kích thước | `TestP12dTenantPolicyWrite` (300 KiB > 256 KiB → 413) | FAIL: 403 | PASS |
| Process standby → 503; không phục vụ → 404 | `TestP12dTenantWritesStandbyAndForeign` | PASS (đã đúng từ P08b: cluster chặn trước runtime) — giữ làm hồi quy cho route mới | PASS |
| Rate limit ghi | `TestP12dTenantConfigWriteRateLimit` (burst 2: lần 3 → 429 + `Retry-After`, cả acl/validate/DNS; GET không tính; ID của B → 404 không phải 429; B có bucket riêng) | FAIL: validate 200 lần 3, DNS 404 | PASS |
| Audit có bản ghi | `TestP12dTenantPolicyWrite`, `TestP12dTenantDNS` (op, key ID ≠ 0, revision = DB, sha256 = nội dung lưu; không chứa nội dung) | FAIL | PASS |
| DNS vượt miền tailnet → 400 | `TestP12dTenantDNS` (split domain trong miền cluster, miền của A, miền của B; nameserver không phải IP/https; tên sai; 65 nameserver; body không phải object → 400; sai kiểu JSON → 422; không lưu gì, revision không đổi) | FAIL: 404 (route chưa có) | PASS |
| DNS tới client A, không tới B | `TestP12dTenantDNS` (a1 nhận nameserver, search path, split DNS `corp.example → 192.168.77.53`, MagicDNS tắt/bật; B không frame nào, `tailnet_settings` của B trống) | FAIL | PASS |
| SDK Tailscale | `TestP12dTenantGoSDK` (`PolicyFile().Set` với ETag, `DNS().SetNameservers/SetSearchPaths/SetSplitDNS/UpdateSplitDNS(nil xóa)/SetPreferences`) | FAIL | PASS |
| `keyExpiryDisabled` chỉ thiết bị của mình | `TestP12dTenantDeviceKeyExpiry` (key B → 404; authorize/update-client/settings vẫn 403) | FAIL: 403 | PASS |
| Duyệt route: HA + `autoApprovers` | `TestP12dSubnetRouterAutoApproveHA` (policy có `tagOwners`/`autoApprovers`/`tests` qua API; auth key gắn `tag:router` qua API; 2 router, v4+v6 tự duyệt; a1 chuyển router khi router chính mất; key B → 404; B quảng bá cùng dải không được duyệt bởi policy của A) | FAIL: 403 ở bước ghi policy | PASS |
| Ghi + bump revision trong một transaction có fence | `TestTenantPolicyWriteBumpsRevisionFenced`, `TestTenantDNSWriteBumpsRevisionFenced`, `TestTenantConfigWriteNeedsFence` (SQLite **và PostgreSQL**) | FAIL: not implemented | PASS |
| Single tailnet: DNS đọc từ config, ghi 501 | `TestAPIv2DNSSingleTailnetReadsConfig` | FAIL: 404 | PASS |

Lệnh:

```shell
# DB (SQLite + PostgreSQL tạm)
go test -count=1 -v -run 'TestTenant(PolicyWrite|DNSWrite|ConfigWrite)' ./hscontrol/db/
# servertest cluster
go test -count=1 -v -tags p05cluster -run 'TestP12d|TestP08bTenantAPIRefusals' -timeout 20m ./hscontrol/servertest/
go test -count=1 -run TestAPIv2DNSSingleTailnetReadsConfig ./hscontrol/servertest/
```

RED (`ac7af56`): DB exit 1 (3/3 FAIL × 2 backend), servertest exit 1 (7 FAIL, `StandbyAndForeign` PASS), single exit 1.
GREEN: xem §5.

Test được sửa ở GREEN (ghi rõ):
- `TestP08bTenantAPIRefusals`: bỏ `setACL` và `setDeviceKey` khỏi danh sách 403 (quyết định 2026-11-26), thêm PATCH settings theo ID tailnet.
- `TestP12dTenantDNS`: hai body sai kiểu JSON (`{"dns":"9.9.9.9"}`, `{"magicDNS":"yes"}`) mong **422** (kiểm schema của Huma, như mọi body có kiểu ở v2) thay vì 400; `preferences` trả thêm `overrideLocalDNS`.
- `TestTenantDNSWriteBumpsRevisionFenced`: đổi owner phải tăng epoch (trigger P10).
- `TestKnownIsComplete`: 20 scope (thêm `dns`, `dns:read`); `TestAPIv2OAuthMatrix_Enforcement`: thêm 5 op DNS.

## 3. Thiết kế

- **Policy (`setACL`) ở cluster mode:** thứ tự kiểm: `{tailnet}` (khác → 404, không tốn token) → kích thước (`max_policy_bytes`, 413) → bucket ghi cấu hình theo tailnet (429) → `State.UpdateTenantPolicyIf`: validate với `policyOptions` của runtime (tenant: rỗng/`{}` = deny) và chạy `tests` → khóa `policyMu` → `If-Match` (412) → `HSDatabase.SetPolicyRevised` → cài từ bản đã lưu. Lỗi bất kỳ trước khi lưu: policy lưu và policy đang áp không đổi; không có nhánh nào cài allow-all.
- **Một transaction có fence** (`hscontrol/db/tenant_config.go`): `LockOwnership` (câu đầu) → `UPDATE tailnets SET <revision> = <revision> + 1 WHERE id, owner_cluster_id, owner_epoch của fence, ACTIVE, chưa xóa` (0 dòng → `ErrFenced`) → ghi dòng tenant qua plugin (fence kiểm owner/epoch/status/lease trong cùng transaction). Thứ tự khóa giữ đúng tenantscope (advisory tailnet → dòng tailnets → advisory cluster). G1 (exclusive rồi shared advisory cùng khóa trong một transaction PG) xác nhận bằng test PostgreSQL. Không raw SQL mới (allowlist không đổi).
- **Revision không nạp lại hai lần:** `State.ObserveRevisions` báo revision đã lưu và đã cài; `tenantRuntime.policyRev/configRev` thành `atomic.Uint64`. Ghi thứ tự lệch chỉ gây thêm một lần reload (tự lành). Áp dụng cả cho `servertest.SetPolicy` (cùng đường state).
- **DNS:** `State.UpdateTenantDNS` đọc-sửa-ghi `tailnet_settings` của chính tailnet (model GORM mới `types.TailnetSettings`, có `TailnetID`, qua plugin) trong transaction trên, dưới `tenantDNSMu`; callback validate `TenantDNS.Validate(slug, base domain cluster, tuning.tenant_dns)`; rồi `Config.ReplaceTenantDNS` + `change.DNSConfig` cho runtime đó.
- **Mặc định policy của tenant** là `{"grants": []}` (deny), `ts-default` khớp với nó; ngoài tenant giữ allow-all như upstream.
- **Audit:** `log.Info` "tenant API: configuration stored" với `tailnet`, `op`, `api_key_id`, `revision`, `sha256`.
- **Scope OAuth mới** `dns`, `dns:read` (tên Tailscale); OAuth không dùng được ở cluster.
- **Ngoài cluster mode:** DNS đọc từ config, ghi 501; `setACL` như cũ.

## 4. Bề mặt API mới (AGENTS rule 9)

27 dòng allowlist mới (9 op DNS × `cluster-v2`/`public`/`unix`), mô tả trong `evidence/P08a-endpoint-inventory.md` §8; `TestEndpointInventory` xanh. Không endpoint debug/metrics mới; không trả node/IP/key. `clusterForbidden` còn `updateTailnetSettings`, `authorizeDevice`, `updateDeviceClient`.

## 5. Kiểm tra cuối (GREEN)

Trên mc-ci, cây làm việc = commit GREEN (script `verify.sh`, PostgreSQL tạm cho gói DB):

| Lệnh | Exit |
|---|---|
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --timeout 20m ./hscontrol/... ./cmd/...` | 0 (0 issues) |
| `go vet ./...`; `go vet -tags p05cluster ./hscontrol/servertest/` | 0; 0 |
| `CGO_ENABLED=1 go test -race -count=1 -p 4 ./hscontrol/db/... ./hscontrol/state/ ./hscontrol/api/... ./hscontrol/scope/ ./hscontrol/config/ ./hscontrol/types/ ./cmd/headscale/cli/ ./hscontrol/` (gồm `TestEndpointInventory`, `TestAPIv2OAuthMatrix_*`, raw SQL guard, test DB trên PostgreSQL) | 0 |
| `CGO_ENABLED=1 go test -race -count=1 -run TestAPIv2DNSSingleTailnetReadsConfig ./hscontrol/servertest/` | 0 |
| `CGO_ENABLED=1 go test -race -count=1 -tags p05cluster -run 'TestP12d\|TestP08bTenantAPI\|TestClusterEmptyPolicyReloadDenies\|TestClusterP06\|TestP08bAdmin\|TestP12cPolicy\|TestClusterTwoTailnetsPeersIsolated' ./hscontrol/servertest/` | 0 (283,7 s) |
| `nix develop --command bash -c "go run ./cmd/gen-openapi -api v2 && make generate"` (generator đã pin, oapi-codegen v2.8.2) | 0; chỉ `gen/client/v2/client.gen.go` đổi (9 op DNS); spec `openapi/` không commit (như quy ước) |

Toàn bộ suite `p05cluster` **không** chạy (mc-ci dùng chung với E2E); CI chạy phần còn lại.

## 6. Giới hạn đã biết / câu hỏi

- E2E S08c (client thật, hai phiên bản client) chưa chạy: điều kiện nghiệm thu còn thiếu.
- `dns/configuration` (alpha của Tailscale) không làm; `override_local_dns` đặt qua trường mở rộng `overrideLocalDNS` của `preferences`.
- Body sai kiểu JSON → 422 (Huma), Tailscale trả 400.
- Một tenant có thể đặt nameserver/split DNS bất kỳ (IP hoặc https) — chỉ ảnh hưởng client của chính nó; không có danh sách chặn (ví dụ IP nội bộ của cluster). Cần reviewer xác nhận.
- Bucket ghi cấu hình nằm trong bộ nhớ process (như các bucket P08b): failover làm đầy lại.
- `tenantAuditHook` là biến toàn package chỉ cho test.
- Reviewer approval: chưa.
