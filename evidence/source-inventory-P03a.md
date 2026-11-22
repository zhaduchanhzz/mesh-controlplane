# Source Inventory — P03a (tailnet tenant: policy rỗng/thiếu = DENY)

- **Trạng thái:** `Verified` (unit + servertest với control client thật in-process, Windows và Linux `mc-ci`). Chưa `Reviewed`.
- **Nhánh:** `feat/P03a-tenant-default-deny` từ `master` @ `7adf255`
- **Commit:** RED `d126a96` → GREEN `00133b3` → evidence (commit này)
- **Ticket:** `tickets/P03.md` (đoạn "Policy tenant", RED test hàng I05/S03, S04 phần policy). Không đụng `hscontrol/db`.

## 1. Path/symbol đã đọc

| Path:line (sau thay đổi) | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/policy/v2/policy.go:257` | `NewPolicyManager` | `unmarshalPolicy` → `updateLocked`; lỗi parse → trả lỗi |
| `policy.go:~365-385` | `updateLocked`, nhánh filter | Trước: `pol == nil \|\| (ACLs == nil && Grants == nil)` → `noRulesFilter` (allow-all) |
| `policy.go:~890` | `BuildPeerMap` | Peer = matcher **hoặc** `sameUser` (thiết bị cùng user luôn là peer, bất kể rule) |
| `policy.go:~1100` | `sameUser` | Không phụ thuộc policy → phải chặn riêng khi deny-all |
| `policy.go:~560`, `~620` | `SSHPolicy`, `SSHCheckParams` | SSH biên dịch từ khối `ssh`, độc lập với acls/grants |
| `policy.go:749` | `SetPolicy` | Bytes rỗng → `ErrPolicyEmpty`; lỗi parse/validate/tests → giữ policy cũ |
| `policy/v2/filter.go:111-137,194-200` | `compileFilterRules`, `allowAllPolicy`, `noRulesFilter`, `compileFilterRulesForNode` | allow-all có nguồn là dải tailnet + route đã duyệt |
| `policy/v2/compiled.go:182,267` | `compileNodeAttrs`, `compileGrants` | nil/không rule → không capability, không grant |
| `policy/v2/types.go:2140-2141, 3427` | `Policy.ACLs/Grants` (`omitempty`), `unmarshalPolicy` | Rỗng → nil; round-trip `{"grants":[]}` → `{}` |
| `policy/v2/test.go:344, 826` | `evaluateTests(For)`, `ValidatePolicy`, `CheckPolicy` | Biên ghi policy; `tests` từng đánh giá với allow-all khi không có rule |
| `hscontrol/policy/pm.go:113-135` | `NewPolicyManager`, `ValidatePolicy` | Wrapper |
| `hscontrol/state/state.go:425-440, 495, 590-620, 1830, 1935-2010` | `NewState`, `policyOptions`, `policyPeersFunc`, `reloadPolicyLocked`, `SetPolicy`, `ValidatePolicy`, `UpdatePolicyIf` | Nơi tạo PolicyManager; reload bỏ qua khi bytes nil |
| `hscontrol/db/policy.go:68` | `PolicyBytes` | (chỉ đọc) file rỗng → `[]byte{}`; DB không có/rỗng → nil |
| `hscontrol/config/files.go:43,127` | `Files`, `CheckPolicyFile` | Chỉ dùng cho `headscale check`, không chạy lúc khởi động |
| `hscontrol/types/config.go:276`, `config_fields.go:30` | `ConfigDir koanf:"-"`, `ConfigTree` | Trường `koanf:"-"` bị bỏ qua → không sinh key/doc mới |
| `hscontrol/servertest/{server.go,harness.go,client.go,policy_test.go,README.md}` | `NewServer`, `TestClient`, `WaitForCondition`, `UpdateCount` | Harness |
| `cmd/headscale/cli/serve_tailnets.go` | `serveTailnetsCmd` | P02 POC, một runtime mỗi tailnet |

## 2. Thiết kế

- **Công tắc lập trình, không phải YAML:** `types.Config.PolicyEmptyDenies bool` (`json:"-" koanf:"-"`), mặc định `false`. `State` chuyển nó thành `policy.WithEmptyPolicyDenies(...)` (`policyv2.Option`) cho **cả** PolicyManager sống và mọi `ValidatePolicy` (`state.policyOptions`). `serve-tailnets` đặt `true` cho mọi runtime tailnet.
- **Tiêu chí "không có access rule":** `hasNoAccessRules(pol) = pol == nil || (len(ACLs) == 0 && len(Grants) == 0)` — coi nil và `[]` như nhau, nên miễn nhiễm với bẫy `omitempty`/round-trip. `denyAll = emptyDenies && hasNoAccessRules(pol)` tính trong `updateLocked` (pol nằm trong `filterHash`, nên đổi denyAll luôn là đổi filter).
- **Khi denyAll:** filter = nil (không gọi `noRulesFilter`), matcher rỗng; `BuildPeerMap` trả map rỗng (bỏ cả adjacency `sameUser`); `SSHPolicy` trả `SSHPolicy{Rules: []}` không nil (nil làm client giữ rule cũ) kể cả khi policy có khối `ssh`; `SSHCheckParams` → `(0,false)`.
- **`SetPolicy([]byte{})` khi bật:** cài deny-all (như `{}`) thay vì từ chối, để một file policy bị làm rỗng rồi reload không giữ lại policy cho phép cũ. `ValidatePolicy` (biên ghi API) **vẫn từ chối** bytes rỗng ở cả hai chế độ (`ErrPolicyEmpty`), vì DB lưu `""` thì reload đọc ra nil và không cài gì.
- **`tests`** được đánh giá theo cùng ngữ nghĩa (`evaluateTestsFor(..., emptyDenies)`): ở tenant, policy không rule mà có test `accept` → ghi bị từ chối.
- **Tắt công tắc:** ngữ nghĩa upstream không đổi (`{}`/nil = allow-all; `{"grants":[]}` = deny filter nhưng peer cùng user vẫn có). Không sửa test upstream nào; `evaluateTests` giữ chữ ký cũ làm wrapper.

## 3. Lỗi parse (hiện trạng, đã kiểm)

Không bao giờ fallback sang allow-all, ở cả hai chế độ:
- Lúc khởi động: `NewPolicyManager` trả lỗi → `NewState` lỗi → server không khởi động.
- `SetPolicy`/reload/API: trả lỗi, giữ nguyên policy đã biên dịch trước đó (`TestTenantEmptyPolicyDenies_ParseErrorNeverAllows`).
- Lưu ý: "giữ policy cũ" nghĩa là nếu policy cũ là allow-all (chế độ upstream) thì vẫn allow-all; ở tenant, trạng thái ban đầu luôn là deny.

## 4. Kiểm thử

### Unit — `hscontrol/policy/v2/tenant_deny_test.go`
Bảng 14 input × bật/tắt: nil, bytes rỗng, file tồn tại nhưng rỗng (`os.ReadFile`), `{}`, `null`, `// comment\n{}`, `{"grants":[]}`, `{"acls":[]}`, `{"acls":[],"grants":[]}`, round-trip (unmarshal→`json.Marshal`→load) của `{}`, `{"grants":[]}`, `{"acls":[]}`, `null`, và round-trip hai lần. Kiểm: global filter + matcher, `BuildPeerMap` (gồm 2 node cùng user), `FilterForNode`, `MatchersForNode`, `SSHPolicy`, `NodeCapMap`. Thêm: `SetPolicy` từng input đè lên policy allow+ssh; `SetPolicy(nil)` upstream vẫn lỗi; policy tường minh vẫn áp dụng; ssh-only ở tenant bị deny; lỗi parse; `ValidatePolicy`; `SetUsers`/`SetNodes` sau khi tạo.

Round-trip ở chế độ tắt xác nhận bẫy: `{"grants":[]}` sau marshal thành allow-all (đã ghi trong test).

### Servertest (client điều khiển thật in-process) — `hscontrol/servertest/tenant_default_deny_test.go`
`TestTenantDefaultDeny`: server bật công tắc, không có policy, 3 node (a1,a2 cùng user alice; b1 của bob). Kiểm: không peer, `PacketFilter`/`PacketFilterRules` rỗng, không SSH rule, `State.ListPeers` rỗng. Barrier `{"grants":[]}` qua `UpdatePolicy` (validate+store+reload) → vẫn deny. Allow-all tường minh → mesh đầy đủ. Về `{}` → bị thu hồi hết. `TestTenantDefaultDenyOffKeepsUpstream`: tắt công tắc → allow-all như upstream.

### Lệnh và exit code

| Bước | Lệnh | Exit | Kết quả |
|---|---|---|---|
| RED unit (Windows, @`d126a96`) | `go test ./hscontrol/policy/v2/ -run TestTenantEmptyPolicyDenies -count=1 -v` | 1 | 17 PASS / 33 FAIL (mọi ca bật công tắc FAIL; ca tắt PASS) |
| RED servertest (Windows) | `go test ./hscontrol/servertest/ -run TestTenantDefaultDeny -count=1 -v` | 1 | `TestTenantDefaultDeny` FAIL: client thấy peer và filter allow-all khi không có policy |
| GREEN unit (Windows, @`00133b3`) | như trên | 0 | 50 PASS / 0 FAIL |
| GREEN servertest (Windows) | như trên | 0 | 2/2 PASS |
| Windows | `go build ./...`; `go vet` policy/state/servertest/cmd | 0 | |
| Windows | `go test ./hscontrol/policy/... ./hscontrol/state/... -count=1` | 1 | policy/* ok; state: chỉ `TestSetLastControlAddress*` FAIL do Windows khóa file SQLite khi dọn TempDir (môi trường, không liên quan; Linux pass) |
| Linux `mc-ci` | `go build ./...`, `go vet ...`, `gofmt -l` các package đã sửa | 0 | gofmt sạch |
| Linux race | `CGO_ENABLED=1 go test -race -count=1 ./hscontrol/policy/... ./hscontrol/state/...` | 0 | policy, policy/matcher, policy/policyutil, policy/v2 (479 s), state (283 s) đều ok |
| Linux race (test mới) | `CGO_ENABLED=1 go test -race -count=1 -run 'TestTenantDefaultDeny\|TestTenantEmptyPolicyDenies' ./hscontrol/servertest/ ./hscontrol/policy/v2/` | 0 | 52 PASS / 0 FAIL (unit + servertest) |
| Linux full | `go test -timeout 120m ./...` | 1 | 35 package ok, 1 FAIL: `servertest` chỉ `TestAPIv2/TSCLI` (`exec: "tscli": executable file not found`), giống baseline P00/P01. `TestTenantDefaultDeny*` pass. servertest 2237 s |

Nguồn được chuyển lên `mc-ci` chỉ bằng `git bundle` + `git clone` vào `~/src/p03a`.

## 5. Những gì policy KHÔNG kiểm soát (deny-all không chặn)

- **Đăng ký node:** node vẫn đăng ký được khi deny-all; chặn khi `PENDING` thuộc P03 chính (`CreateTailnet`).
- **Policy tenant có rule:** `sameUser` vẫn cho thiết bị cùng user là peer (ngữ nghĩa SaaS, không cấp quyền truy cập); wildcard `*` trong tailnet thuộc P06.
- **Khi deny-all nhưng policy có nội dung khác acls/grants:** `nodeAttrs`/`randomizeClientPort` (capability), `autoApprovers` (tự duyệt route), `tagOwners`, `derpMap` vẫn theo nội dung tường minh. Không có filter và peer nên route/exit node không dùng được, nhưng capability (vd. funnel) vẫn được gắn — cần reviewer quyết định.
- **Route/exit node do admin duyệt qua API**, tag từ pre-auth key, DERP map cấu hình, DNS (extra records, nameserver), Taildrop baseline cap: không qua policy.
- **DERP verify-clients** tra node key toàn cục (`handlers.go`), không theo policy; P08.
- **DB mode:** `policies.data = ""` (chỉ có thể do ghi DB trực tiếp vì `ValidatePolicy` từ chối) → reload đọc nil, **giữ policy cũ** thay vì deny. File mode bị xóa file lúc reload → lỗi đọc, giữ policy cũ.

## 6. Giả định / câu hỏi mở

- Giữ chế độ không-tenant với ngữ nghĩa upstream (mặc định tắt) — **giả định**, cần reviewer chốt (ticket P03).
- Lưu policy canonical (không phụ thuộc round-trip) và `{"grants":[]}` + `PENDING` cùng transaction thuộc P03 chính; P03a chỉ đảm bảo mọi round-trip đều deny.
- Ở tenant, ssh-only policy bị deny SSH hoàn toàn (chọn fail closed).
- Benchmark: `BuildPeerMap` chỉ thêm một nhánh return sớm; không đo benchmark.
