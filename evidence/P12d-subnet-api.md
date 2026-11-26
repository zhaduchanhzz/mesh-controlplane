# PR P12d Evidence (subnet router qua API của tenant: policy, DNS, key expiry)

- **Repo commit before / after:** `1cb1379` (ticket P12d, trên nhánh tích hợp P12c) → tip `feat/P12d-subnet-api` (bảng commit §1; tip ghi trong báo cáo cuối).
- **Trạng thái:** API (§1 ticket) và tài liệu (§3) `Implemented`; servertest/unit/DB `Verified` trên mc-ci (lệnh và exit code bên dưới). E2E S08c **chưa chạy** (harness do agent khác viết trong `wt-p12d-e2e`, coordinator chạy). Chưa `Reviewed`.
- **Review bảo mật (2026-11-26):** 8 phát hiện (không BLOCKER) và một test thiếu, sửa trên nhánh `feat/P12d-review-fixes` (`Implemented`, servertest/unit/DB `Verified`) — xem §7.
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
- Một tenant có thể đặt nameserver/split DNS là IP hoặc https bất kỳ, trừ loopback/link-local/unspecified/có zone (từ review §7, phát hiện 7) — chỉ ảnh hưởng client của chính nó; không có danh sách chặn IP nội bộ của cluster, và tên host trong URL DoH không được phân giải. Cần reviewer xác nhận.
- Bucket ghi cấu hình nằm trong bộ nhớ process (như các bucket P08b): failover làm đầy lại.
- `tenantAuditHook` là biến toàn package chỉ cho test.
- Reviewer approval: chưa.

## 7. Security review fixes (review độc lập 2026-11-26, nhánh `feat/P12d-review-fixes`)

Reviewer độc lập: **không BLOCKER**; mọi phát hiện chỉ chạm dữ liệu của chính tenant. Gốc: `7cacbc1` (`feat/P12d-subnet-api`). Inventory bổ sung: `evidence/source-inventory-P12d.md` §5 (commit `e0d9a30`, trước mọi thay đổi code).

### 7.1 Commit

| Commit | Nội dung |
|---|---|
| `e0d9a30` | Source inventory §5 (AGENTS rule 5) |
| `9870eb7` | Khung (không đổi hành vi): hook test của reconcile DNS (giữa đọc và cài), hook test của `State` (sau khi lưu policy, trước khi cài), `HSDatabase.SetPolicyRevisedIf` (vẫn kiểm trước transaction), khóa `tuning.tenant_dns.max_split_domains` (validate, tài liệu sinh lại bằng `tools/configgen`, chưa áp), `TenantWrite.NotInForce/DeviceID/KeyExpiryDisabled` |
| `09c91dd` | **RED**: 8 servertest `TestP12dReview*` (`hscontrol/servertest/cluster_p12d_review_test.go`), `TestTenantPolicyIfMatchInTransaction` (SQLite + PostgreSQL), `TestTenantDNSValidateReviewRules` |
| `67d5822` | RED (sửa test): `TestP12dReviewReconcileSkipsObservedPolicy` chờ mesh không cần SSH (policy của nó không có SSH); ở `09c91dd` nó timeout trước assertion |
| `242dd24` | GREEN 1 + 8 (reconcile) |
| `e212ecc` | GREEN 2 + 3 (+ 8: không chạy test của policy hai lần) |
| `0b6145a` | GREEN 4 |
| `f56c213`, `a8e7507` | GREEN 5 (+ sửa: `tenantPolicyLimit` khi API không có config, như khi đăng ký cho spec / inventory endpoint) |
| `5baa874` | GREEN 6 |
| `3bc1abe` | Lint (`nonamedreturns`, `testifylint`) |
| `7ae9f79` | GREEN 7 |
| commit tài liệu (tip) | `docs/tenant-api/subnet-routers.md` và mục này |

### 7.2 Phát hiện, test, sửa

| # | Mức | Phát hiện | Test RED (`09c91dd`/`67d5822`) | Sửa (GREEN) |
|---|---|---|---|---|
| 1 | MEDIUM | Reconcile đọc DNS qua handle platform rồi `ReplaceTenantDNS` không giữ `tenantDNSMu`: ghi của tenant (N+1) cài xong, reconcile cài S_N đè lên và không bao giờ nạp lại (revision = DB) | `TestP12dReviewReconcileDNSRaceKeepsNewest`: hook trong reconcile (sau khi đọc S_N của operator, trước khi cài) gửi ghi của tenant; RED: a1 kết thúc với `1.1.1.1` (timeout 20 s chờ `2.2.2.2`) | `242dd24`: revision đã cài nằm trong `State` (`policyRev`/`configRev`, ghi dưới `policyMu`/`tenantDNSMu`); reconcile gọi `State.ReloadTenantDNS(check)`: dưới `tenantDNSMu` đọc revision **rồi** settings qua handle scoped của runtime (`HSDatabase.TailnetRevisions`, `TenantDNS`), bỏ qua revision đã cài, validate, cài. Ghi của tenant cài và ghi nhận revision dưới cùng khóa, nên reconcile không bao giờ cài settings cũ hơn settings đã cài |
| 2 | LOW-MEDIUM | Policy đã lưu + bump revision, cài sau đó lỗi: 500 trơn, không audit, không nói đã lưu | `TestP12dReviewPolicyStoredNotInForce` (lỗi cài được tiêm sau commit); RED: `{"message":"setting policy","data":["injected …"]}`, không `ETag`, không audit | `e212ecc`: validate, lưu, cài là **một bước dưới `policyMu`**; lỗi sau commit là `state.PolicyNotInForceError` (chỉ unwrap ra `ErrPolicyNotInForce`, không bao giờ thành 400); `setACL` audit revision đã lưu (`NotInForce`), trả 500 kèm `ETag` của policy đã lưu và thông điệp "the policy was stored but is not in force yet …". **Quy tắc:** ghi bị từ chối (4xx/503) không lưu, không bump, nên không reconcile nào cài được nó; ghi đã lưu là thứ sẽ được thực thi: policy cũ giữ hiệu lực tới khi reconcile cài policy đã lưu (revision chưa được ghi nhận là đã cài). Test kiểm cả hai vế (a1 nhận policy đã lưu qua reconcile; policy 400 sau đó không bao giờ có hiệu lực) |
| 3 | LOW | `If-Match` kiểm ngoài transaction: ghi của operator CLI xen giữa bị mất | `TestTenantPolicyIfMatchInTransaction` (SQLite + PostgreSQL): `match` khởi động ghi của operator (handle có fence riêng) ngay sau khi kiểm; RED: policy của operator bị ghi đè (`{}` thay vì policy của operator), cả hai backend | `e212ecc`: `SetPolicyRevisedIf` kiểm trong transaction ghi, sau `LockOwnership` + bump (PostgreSQL: ghi có fence giữ advisory shared của tailnet nên xếp sau; SQLite tuần tự hóa người ghi): ghi của operator hoặc commit trước và bị thấy (412), hoặc chờ và đến sau. Đường một tailnet (không fence) giữ kiểm dưới `policyMu` |
| 4 | LOW | 503 của ghi bị fence lộ cluster/holder/epoch; 500 của handler P12d lộ lỗi DB/parse | `TestP12dReviewWriteAfterLeaseLost` (lease hết hạn trong DB, owner/epoch còn khớp; RED: `data` chứa `lease is held by …`, cluster ID, holder); `TestP12dReviewNoInternalsIn500` (dòng DNS hỏng; RED: GET 500 có lỗi `json: cannot unmarshal`, ghi trả **400** kèm lỗi parse) | `0b6145a`: 503 chỉ `{"message":"temporarily unavailable, retry"}` (nguyên nhân vào log); `internalError`/`mapTenantError` cho handler policy/DNS/device key: 500 chỉ có thông điệp; settings lưu không parse được là `db.ErrStoredTenantDNS` (500), không còn là `ErrInvalidTenantDNS` (400). 400 về input của chính tenant giữ lý do |
| 5 | LOW | Body tới 1 MiB đọc hết trước `max_policy_bytes` và trước bucket | `TestP12dReviewBodyRefusedBeforeRead` (TCP thô: header khai `Content-Length` lớn, chỉ gửi vài byte); RED: 408 sau 5 s (Huma chờ body) thay vì 413/429 ngay | `f56c213`: middleware `tenantWriteMiddleware` (sau xác thực, **trước** khi Huma đọc body) cho `setACL`, `validateACL`, mọi ghi DNS: `{tailnet}` (404, không tốn token), `Content-Length` khai lớn hơn giới hạn (413, không đọc, không tốn token), rồi bucket (429); trả lời từ chối kèm `Connection: close` (body không được đọc). `MaxBodyBytes` của `setACL`/`validateACL` = `max_policy_bytes` ở cluster mode (body chunked vượt giới hạn bị cắt ở giới hạn) |
| 6 | LOW | `setDeviceKey` không audit, không tốn bucket | `TestP12dReviewDeviceKeyAuditedAndRateLimited` (burst 2: lần 3 → 429; audit 2 bản ghi; key B trên thiết bị A → 404 không tốn token của B); RED: lần 3 = 200, không audit | `5baa874`: ở cluster mode, sau khi tìm thấy thiết bị của chính tailnet: tốn bucket ghi cấu hình, audit (`op=setDeviceKey`, key ID, device ID, giá trị). Comment + tài liệu: `keyExpiryDisabled=true` làm thiết bị đã hết hạn dùng lại được (như Tailscale) |
| 7 | Info | DNS: search domain trong miền cluster/tailnet khác; split rỗng không tính; split là miền cha của miền cluster; nameserver loopback/link-local | `TestTenantDNSValidateReviewRules` (18 trường hợp RED), `TestP12dReviewDNSRules` (API: 12 ghi phải 400, RED: 200) | `7ae9f79`: mọi **ghi** (tenant và operator, `TenantDNS.Validate`): search domain ngoài miền cluster hoặc tại/dưới miền của chính tailnet; split domain không chứa miền cluster; `tuning.tenant_dns.max_split_domains` (mặc định 64, domain map tới `[]` vẫn tính); nameserver (global, split, host IP của URL DoH) không loopback, link-local, interface-local, unspecified, có zone; IP riêng (`10/8`, `192.168/16`, `fd00::/8`) vẫn nhận. **Khi nạp** dữ liệu đã lưu: `TenantDNS.ValidateStored` chỉ kiểm hình thức như trước; luật mới chỉ cảnh báo (như hạ giới hạn, ADR 0008 §3): `TestP12dReviewDNSRules` kiểm settings `127.0.0.1` lưu tay vẫn được nạp; `TestTenantDNSValidateStored` |
| 8 | Info | Test của policy chạy hai lần mỗi ghi; reconcile gửi lại `PolicyChange` cho revision mà ghi qua API đã cài (commit, rồi reconcile so revision, rồi observer) | `TestP12dReviewReconcileSkipsObservedPolicy` (hook giữ ghi sau commit qua một tick reconcile; ghi lại policy đang hiệu lực); RED (`67d5822`): client nhận frame từ `PolicyChange` của reconcile | `242dd24`: reconcile so sánh lại dưới `policyMu` (`State.ReloadTenantPolicy`), bỏ qua revision đã cài, chỉ gửi `PolicyChange` khi policy thật sự đổi. `e212ecc`: cài từ chính byte đã validate bằng `PolicyManager.SetValidatedPolicy` (không chạy `tests`/`sshTests` lần hai, không đọc lại DB). Phần "không chạy test hai lần" là refactor, không có test RED riêng (không quan sát được từ ngoài); các test policy hiện có giữ xanh. Compile vẫn hai lần (sandbox validate + cài live): bỏ được thì phải thay policy manager live, không "dễ" |
| – | test thiếu | Ghi của tenant khi owner/epoch còn khớp nhưng lease của cluster đã mất: 503, không lưu/bump | `TestP12dReviewWriteAfterLeaseLost` (lease `TTL` 10 phút, `ExpireLease`, process chưa nhận ra nên fence DB từ chối): 6 ghi policy/DNS đều 503, revision, policy, DNS không đổi, không audit. Mã 503 đã đúng từ trước; RED chỉ vì body lộ nội bộ (phát hiện 4) | – |

### 7.3 Lệnh và exit code (mc-ci, `~/src/p12dfix`)

PostgreSQL tạm: `~/p12dfix-pg.sh` (như `.github/workflows/ci.yml`: `initdb -U ci --auth=trust`, socket unix, port tự do, `max_connections=300`; dừng và xóa sau khi chạy), đặt `MESH_TEST_POSTGRES_DSN` + `MESH_TEST_REQUIRE_POSTGRES=1`. `export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false`.

| Bước | Commit | Lệnh | Exit |
|---|---|---|---|
| RED DB | `09c91dd` | `~/p12dfix-pg.sh go test -count=1 -run TestTenantPolicyIfMatchInTransaction ./hscontrol/db/` | 1 (sqlite FAIL, postgres FAIL) |
| RED types | `09c91dd` | `go test -count=1 -run TestTenantDNSValidateReviewRules ./hscontrol/types/` | 1 (18 subtest FAIL) |
| RED servertest | `09c91dd` | `go test -count=1 -p 4 -tags p05cluster -run TestP12dReview -timeout 30m ./hscontrol/servertest/` | 1 (8/8 FAIL; F8 vì lỗi test, xem `67d5822`) |
| RED F8 | `67d5822` | `go test -count=1 -tags p05cluster -run TestP12dReviewReconcileSkipsObservedPolicy ./hscontrol/servertest/` | 1 (`assertQuiet`: frame từ reconcile) |
| GREEN 1+8 | `242dd24` | `go test -count=1 -tags p05cluster -run TestP12dReviewReconcile ./hscontrol/servertest/` | 0 |
| GREEN 2+3 | `e212ecc` | `~/p12dfix-pg.sh go test -count=1 -v -run 'TestTenantPolicyIfMatchInTransaction\|TestTenant(PolicyWrite\|DNSWrite\|ConfigWrite)' ./hscontrol/db/` | 0 (4 test, sqlite + postgres) |
| GREEN 2+8 | `e212ecc` | `go test -count=1 -p 4 ./hscontrol/policy/... ./hscontrol/state/`; `go test -count=1 -p 4 -tags p05cluster -run 'TestP12dReviewPolicyStoredNotInForce\|TestP12dReviewReconcile\|TestP12dTenantPolicyWrite\|TestP12dTenantDefaultPolicyIsDeny\|TestP12dTenantGoSDK\|TestP12dSubnetRouterAutoApproveHA\|TestClusterEmptyPolicyReloadDenies\|TestP12cPolicy' ./hscontrol/servertest/` | 0; 0 |
| GREEN 4–7 | `a8e7507` | `go test -count=1 -run 'TestTenantDNS\|TestConfigFields\|TestDefault' ./hscontrol/types/ ./hscontrol/config/`; `go test -count=1 -p 4 -tags p05cluster -run TestP12dReview -timeout 30m ./hscontrol/servertest/` | 0; 0 (8/8 PASS) |

### 7.4 Kiểm tra cuối

| Commit | Lệnh | Exit |
|---|---|---|
| `a8e7507` | `~/p12dfix-pg.sh env CGO_ENABLED=1 go test -race -count=1 -p 4 ./hscontrol/db/... ./hscontrol/state/ ./hscontrol/api/... ./hscontrol/scope/ ./hscontrol/config/ ./hscontrol/types/ ./hscontrol/policy/... ./cmd/headscale/cli/ ./hscontrol/` (gồm `TestEndpointInventory`, `TestAPIv2OAuthMatrix_*`, raw SQL guard, test DB trên PostgreSQL) | 0 |
| `a8e7507` | `CGO_ENABLED=1 go test -race -count=1 -run TestAPIv2DNSSingleTailnetReadsConfig ./hscontrol/servertest/` | 0 |
| `a8e7507` | `CGO_ENABLED=1 go test -race -count=1 -p 4 -tags p05cluster -run 'TestP12d\|TestP08bTenantAPI\|TestP08bAdmin\|TestClusterEmptyPolicyReloadDenies\|TestClusterP06\|TestP12cPolicy\|TestClusterTwoTailnetsPeersIsolated\|TestS04CreateEnrolAndFirstPolicyRace\|TestClusterEpochBumpReloadsRuntime\|TestClusterLoadsNewTailnetLive' -timeout 60m ./hscontrol/servertest/` (mọi `TestP12d*` cũ và mới, P08b, P06 DNS reload, P12c) | 0 (396 s) |
| `3bc1abe` | `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --timeout 30m ./hscontrol/... ./cmd/...` | 0 (0 issues; lần đầu ở `7ae9f79`: 6 issues, sửa ở `3bc1abe`) |
| `3bc1abe` | `go vet ./...`; `go vet -tags p05cluster ./hscontrol/servertest/` | 0; 0 |
| `3bc1abe` | `~/p12dfix-pg.sh go test -race -count=1 -run TestTenant ./hscontrol/db/`; `go test -race -count=1 -p 4 -tags p05cluster -run 'TestP12dReview\|TestClusterP06\|TestP12dTenantPolicyWrite\|TestP12dTenantDNS' ./hscontrol/servertest/` (sau đổi chữ ký do lint) | 0; 0 |
| `3bc1abe` vs `7cacbc1` | `go run ./cmd/gen-openapi -api v2 -downgrade <file>` ở hai commit, `diff -q` | giống hệt: không đổi spec, không cần `make generate`, `gen/` không đổi |

Raw SQL guard và allowlist: không đổi (không thêm raw SQL; `TailnetRevisions` dùng GORM trên handle scoped). Inventory endpoint và ma trận OAuth: không route mới, không đổi.

### 7.5 Giới hạn và điểm cần reviewer xác nhận

- **Trả lời "stored but not in force" là 500** (kèm `ETag` và thông điệp nói rõ đã lưu), không phải 2xx: SDK Tailscale coi là lỗi, nhưng thông điệp và `ETag` cho biết đã lưu; `GET` trả policy đã lưu. Reconcile thử cài lại **một lần** (revision được ghi nhận kể cả khi lỗi, như trước); nếu vẫn lỗi thì log, policy cũ giữ hiệu lực tới lần ghi/reload kế tiếp. Lỗi sau commit thực tế chỉ còn là lỗi compile của chính byte vừa validate (hiếm); test dùng hook tiêm lỗi.
- **Luật DNS mới (phát hiện 7) áp cho ghi của operator** (`Platform.SetTailnetDNS`) như của tenant; dữ liệu đã lưu trước đó vẫn nạp (cảnh báo), lần ghi kế tiếp phải theo luật mới cho **toàn bộ** settings (kể cả khi tenant chỉ sửa một phần, ví dụ `PATCH split-dns` khi nameserver cũ là loopback). ADR 0008 §3 chưa được sửa câu chữ cho các luật mới (đề xuất bổ sung khi duyệt).
- **Body chunked** (không `Content-Length`) vượt giới hạn: bị cắt ở giới hạn (413) **sau** khi đã tốn một token bucket; chỉ body khai `Content-Length` lớn mới bị từ chối không tốn token.
- `If-Match` trong transaction phụ thuộc ghi của operator đi qua handle có fence (advisory shared, PostgreSQL) — đúng cho `setTailnetPolicyCLI`; một công cụ ghi thẳng bảng `policies` không qua fence sẽ không bị xếp hàng (ngoài phạm vi, raw SQL guard chặn trong code).
- Phát hiện 8: test của policy không còn chạy hai lần; **compile** vẫn hai lần (sandbox của `ValidatePolicy` + cài live).
- Hook test mới (`SetReconcileDNSHookForTest`, `State.SetPolicyInstallHookForTest`) là biến/trường chỉ cho test, như `SetTenantAuditHookForTest`.
- Không chạy toàn bộ suite `p05cluster` (mc-ci dùng chung); E2E S08c không chạy lại (không đụng lab). Reviewer approval: chưa.

## 8. Kiểm tra tích hợp trước merge (2026-11-26)

| Kiểm tra | Commit | Lệnh | Kết quả |
|---|---|---|---|
| E2E N1 đầy đủ, netmap ON, client 1.104.7 | `7dd8a8a` | `CI_BINDIR=/home/mesh/binp12d MODE=on V=1.104.7 bash infra/lab/e2e/n1/run.sh` | 30 PASS, 1 FAIL (S08c.6: S08b để lại route đã duyệt) → sửa `06c9f7b` |
| E2E N1 đầy đủ, netmap OFF, client 1.84.3 | `06c9f7b` | `CI_BINDIR=/home/mesh/binp12d MODE=off V=1.84.3 bash infra/lab/e2e/n1/run.sh` | **0 FAIL**, `cleanup_ok=1`, 1564 s (E00, S01–S18, S08b, S08c.0–6, M06, S13/S13b, S99); S08c.2 HA gap 16 212 ms (≤ 20 000) |
| E2E N1 ON 1.104.7, ca sửa | `d1650fa` | `CASES="t_s01 t_s08b t_s08c" … MODE=on V=1.104.7` | **10 PASS, 0 FAIL**, `cleanup_ok=1`, 629 s |
| E2E P12b (Q7) | `d1650fa` | `CI_BINDIR=/home/mesh/binp12d N=24 PAR=8 LEAD=60 IMAGES="…v1.104.7 …v1.84.3" VM_TARGETS=… bash infra/lab/e2e/p12b/run.sh` | **13/13 PASS**, `cleanup_ok=1`, 480 s; t100 từ `scheduled_at` 8 008 ms (1.104.7) / 1 273 ms (1.84.3) ≤ 15 000; 0 retry, 0 can thiệp tay |
| Suite `p05cluster` (shard-test, `nix shell` tscli/tofu) | `06c9f7b` | `shard-test.sh` trên mc-ci | **235/235**, 0 fail, 0 skip, `SHARD_RC=0`, 1588 s |
| GitHub CI | `06c9f7b` | — | success |
| Tích hợp thử với P13 | `0f5f363` (`feat/P13-integration`) | build, vet, lint, race toàn repo, servertest P12d + P13c–f | 0 lỗi; 1 xung đột (`cluster.go` reconcile) giải quyết giữ cả hai hành vi; tenant write đi qua fence P13 |

Binary: `/home/mesh/binp12d` trên mc-ci (headscale `b1cd7663d43b3d3e`). Artifact (không commit): `artifacts/06c9f7b/N1/e2e-20261126T151208Z-off-1.84.3`, `artifacts/d1650fa/N1/e2e-20261126T153820Z-on-1.104.7`, `artifacts/d1650fa/P12b/…`. S99/M07: không có key/secret trong artifact.

**Trạng thái sau §8:** `Verified`; merge vào `master` theo lệnh chủ dự án; chưa `Reviewed` (chờ chủ dự án). S08d (exit node, nhánh `feat/P12d-exitnode`) chưa merge: lượt ON bị dừng giữa chừng khi chủ dự án đổi hướng (S08d.1–3 PASS).
