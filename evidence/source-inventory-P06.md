# PR P06 Evidence — source inventory: policy/ACL, DNS, routes, exit node giữa các tailnet

- **Trạng thái:** `Verified` (unit/race/cluster suite + E2E client thật 28/28 ×2, §6–§9); chưa `Reviewed`. §2–§4 là inventory ban đầu tại `d678618`.
- **Nhánh:** `feat/P06-policy-dns-routes` (từ `master` `d678618`).
- **Phạm vi (PLAN dòng 275, `06-tests-benchmarks.md`):** S06 (IP/hostname/subnet route trùng giữa A và B), S07 (B sửa DNS/extra records không ảnh hưởng A), S08 (route/exit node, CIDR trùng; không tự approve, không lộ default route), S09 (tag bounded theo tailnet), I02, I05, I07; AGENTS 8 (`*` không vượt tailnet).
- Chưa có `tickets/P06.md`; file này đóng vai hợp đồng cho tới khi có ticket.

## 1. Repo commit

| | Commit |
|---|---|
| Before | `d678618` (`master`, sau P11) |
| After | `04342ed` (nhánh `feat/P06-policy-dns-routes`, đã gộp `master` `44ad880` có P00d) |

## 2. Path & symbol đã đọc (tại `d678618`)

| Chủ đề | Path:dòng | Phát hiện |
|---|---|---|
| DNS gửi client | `hscontrol/mapper/mapper.go:103` `generateDNSConfig`; `types/config.go:1323` `CloneTailcfgDNSConfig` | Base domain, MagicDNS, nameservers, search domains, extra_records lấy từ **một** `cfg` dùng chung cho mọi runtime. Chỉ NextDNS profile (capMap theo node) và `CertDomains` là theo node. `cluster.go:120-122` ghi DNS theo tailnet là "ticket sau". |
| `StartDNS` | `types/config.go:1306` (`c.dns = &liveDNS{…}`, không khóa); gọi từ `app.go:236` `newHeadscale` | Mỗi runtime (`Cluster.load` → `newHeadscale`, kể cả runtime nạp nóng bởi `reconcile`) **gán lại con trỏ dùng chung** mà mapper của mọi runtime đang đọc: data race, và đặt lại extra_records về giá trị trong file config cho tới `SetExtraRecords` kế tiếp. Comment yêu cầu "Call it before the config is shared" bị vi phạm ở cluster mode. |
| extra_records | `app.go:1594-1603`, `app.go:491`; `types/config.go:1377` | Mỗi runtime tạo một `ExtraRecordsManager` theo dõi **cùng một file** và cùng ghi vào `cfg` dùng chung. Tenant không ghi được file (chỉ operator), nhưng N watcher cho một file là thừa. |
| DNS tenant ghi được | `noise.go:201`, `:954` `/set-dns`, `:1048` `validateSetDNS` | Đường duy nhất: TXT ACME. Không có API nameserver/search/split DNS cho tenant. |
| Chứng chỉ node | `types/node.go:939-961` `CertDomain`; `cluster.go:189-210` `validateClusterConfig` | `CertDomain = given_name + BaseDomain dùng chung`. Given name chỉ unique **trong** tailnet (`cluster_p05_test.go`: A và B cùng có `phone.<base>`). Cluster mode **không** từ chối `dns.certificates.enabled` → node của B có thể đặt challenge và lấy chứng chỉ cho **đúng tên** node của A. **Lỗi isolation** (I02/I15) khi bật certificates. |
| DERP map | `app.go:1563-1577`; `state/state.go:234-238, 560, 570, 633`; `derp/derp.go:212` | Theo runtime (`State.derpMap`), policy derpMap merge vào bản của runtime; region được clone. `derpSeed` (`derp/derp.go:320`) là seed xáo trộn toàn process, không mang dữ liệu tenant. |
| IPAM | `state/state.go:379` `NewIPAllocator`; `db/ip.go:57-90`; `db/tenantscope/tenantscope.go:109` | Mỗi runtime một allocator, đọc IP qua handle có `tailnet_id = ?`. Hai tailnet có thể trùng IP (thiết kế; `TestClusterOverlapAndConfigIndependence/names_and_ipam_independent`). |
| Ràng buộc IP | `db/schema.sql:110-169`, `db/schema_postgres.sql` | **Không** có unique `(tailnet_id, ipv4)` / `(tailnet_id, ipv6)`. Trong một tailnet, chỉ allocator trong bộ nhớ ngăn trùng IP (thiếu lớp phòng thủ DB, AGENTS 7). |
| Routes / HA | `state/node_store.go:901` `electPrimaryRoutes`; `app.go:401-421`; `state/ha_health.go:36` | Theo NodeStore/runtime; auto-approver từ PolicyManager của runtime. Không có cache route dùng chung. |
| Ping | `cluster.go:609-628` | Hỏi mọi runtime `CompletePing(id)`; dựa vào ping ID không đoán được, không dựa vào tenant. |
| Users / tags | `state/state.go:435-445, 617, 857`; `schema.sql:59-61` | PolicyManager chỉ thấy user/node của handle tenant; `hscontrol/policy` không truy cập DB; user unique theo `(tailnet_id, name)`. Không thấy đường để tên user/tag của B ảnh hưởng A. |
| Trạng thái toàn process | Prometheus (`state/node_store.go:50`, `state.go:2208`, `mapper/batcher.go:35,43`, `metrics.go:25`); `connIDCounter`; `sync.OnceValue` hằng | Không mang dữ liệu tenant, trừ `metrics.go:11` có label `id` node khi bật env debug (chưa kiểm tính unique của node ID xuyên tailnet). |

Kiểm chứng trực tiếp (không chỉ qua báo cáo của agent đọc nguồn): `types/config.go:1306`, `app.go:236`, `cluster.go:189-210`, `types/node.go:939-961`.

## 3. Test có sẵn

`servertest/cluster_p05_test.go`: `TestClusterTwoTailnetsPeersIsolated`, `TestClusterAuthKeyPicksTailnet`, `TestClusterSameMachineKeyTwoTailnets`, `TestClusterSessionCannotSwitchTenant`, `TestClusterOverlapAndConfigIndependence` (names/IPAM, policy change A↔B im lặng, rename), `TestClusterRefusesForeignOrInactiveTailnet`; `tenant_default_deny_test.go`; `p08a_apiv2_tailnet_scope_test.go`; `cluster_p10_test.go`.
**Chưa có** test xuyên tailnet cho: route/exit node/HA trùng CIDR, auto-approver, policy derpMap, extra_records, chứng chỉ/`set-dns` trùng tên, tag trùng tên ở A và B.

## 4a. Quyết định của chủ dự án (2026-11-24)

- **Q1 → base domain theo tailnet** (không phải từ chối certificates): tên MagicDNS của node trong tailnet `slug` là `<node>.<slug>.<base_domain cluster>`; `CertDomain` theo đó nên tên chứng chỉ không trùng giữa hai tailnet.
- **Q2/Q4 → DNS theo tailnet ngay trong P06:** mỗi tailnet có cấu hình DNS riêng (nameservers, search domains, split DNS, extra_records, MagicDNS bật/tắt) lưu trong DB; không còn đối tượng DNS dùng chung giữa các runtime (đồng thời bỏ race `StartDNS`).
- **Q3 → thêm migration** unique `(tailnet_id, ipv4)` / `(tailnet_id, ipv6)`.

Thiết kế cụ thể (bảng/cột, cách nạp lại khi đổi, API/CLI ghi, phần config file còn dùng làm mặc định) sẽ được ghi thành ADR 0008 trước khi viết code (AGENTS: thay đổi thiết kế → ADR). Phác thảo:
- Cột mới trên `tailnets`: `dns_config` (JSON đã validate, NULL = mặc định của cluster) và `dns_revision`; ghi qua CLI cluster `tailnet dns --slug … --file …` (operator) và API v2 scoped theo tailnet nếu ADR chốt; `Cluster.reconcile` nạp lại khi `dns_revision` đổi, như `policy_revision` (P10).
- Mỗi runtime nhận một bản `Config` riêng (sao chép phần DNS) dựng trong `Cluster.load`: `BaseDomain = <slug>.<base>`, nameservers/extra_records từ DB; `StartDNS` chạy trên bản riêng nên không còn gán đè con trỏ dùng chung.
- extra_records trong file config của cluster: không áp cho tenant (hoặc chỉ là mặc định), ADR chốt.

## 4. Đề xuất ban đầu (đã được chốt ở §4a)

- **Q1 [Đề xuất] Chứng chỉ node trong cluster mode:** từ chối khởi động khi `dns.certificates.enabled` ở cluster mode (fail closed, giống OIDC/embedded DERP), cho tới khi có DNS theo tailnet. Phương án khác: base domain theo tailnet (`<slug>.<base>`), lớn hơn và đổi tên MagicDNS của client.
- **Q2 [Đề xuất] `StartDNS`:** gọi một lần ở `NewCluster`; `newHeadscale` không gọi lại khi config đã có DNS sống; một `ExtraRecordsManager` cho cả cluster. Test race: nạp nóng runtime trong lúc mapper đọc DNS.
- **Q3 [Đề xuất] Ràng buộc IP:** migration mới (chỉ thêm vào cuối) với unique `(tailnet_id, ipv4)` và `(tailnet_id, ipv6)` (partial, bỏ NULL) trên SQLite và PostgreSQL.
- **Q4 [Đề xuất] DNS theo tailnet (nameservers, search, extra_records):** để ticket sau; P06 chỉ test rằng không có đường nào để tenant đổi DNS của tenant khác.
- **Kế hoạch test RED (servertest cluster, hai tailnet):** S06 (IP + hostname + subnet route trùng; peers và AllowedIPs không lẫn), S08 (A có exit node + auto-approver `0.0.0.0/0`; B quảng bá cùng CIDR: không tự approve, B không thấy route của A), S09 (`tag:srv` ở cả A và B; tagOwners của A không cấp cho node B), policy `*` của A không đưa node B vào filter, policy derpMap của A không đổi DERP map của B, chứng chỉ trùng tên (sau Q1), race `StartDNS`.

## 5. Thay đổi (theo §4a và ADR 0008)

| Phần | Commit | Nội dung |
|---|---|---|
| Unique IP theo tailnet | `4ca4918` (RED), `6872628` | migration `202611241200-nodes-ip-unique-per-tailnet`: unique `(tailnet_id, ipv4)`/`(tailnet_id, ipv6)` bỏ NULL; trùng sẵn trong một tailnet → migration báo lỗi, không đổi DB |
| ADR | `c27b1f4`, `95f01c7` | ADR 0008 Accepted |
| Test isolation | `2d9ec49` | S06, S08, S09, wildcard (I05), policy derpMap |
| DNS theo tailnet | `16111cc` | `types.TenantDNS` (+ parse chặt, validate), `Config.ForTailnet`/`ReplaceTenantDNS`, migration `202611241300-tailnet-settings`, `Platform.SetTailnetDNS`/`TailnetDNS`, slug là nhãn DNS, cluster: config riêng mỗi runtime, từ chối DNS dạng tenant và thiếu `base_domain`, reload theo `config_revision`; `tuning.tenant_dns.*`; CLI `cluster tailnet dns` |
| Test cũ theo tên mới | `8e275a8` | tên `<node>.<slug>.mesh.test`; token chống rò rỉ luôn gồm FQDN của tailnet kia (`Headscale.BaseDomain`) |
| Gộp `master` (P00d) | `623cd23`, `ecbd6c9` | xung đột config `tenant_dns` + `incremental_netmap`, giữ cả hai, docs sinh lại |
| E2E | `9d14ebf`, `04342ed` | D01–D03 trong harness P05; test hàng DNS hỏng gồm cả JSON không parse |

## 6. Tests + lệnh + exit code (mc-ci, go1.27.4)

| Lệnh | Exit | Ghi chú |
|---|---:|---|
| RED: `go test -run 'TestValidSlug\|TestTailnetBaseDomain\|TestTenantDNS\|TestParseTenantDNS\|TestConfigForTailnet' ./hscontrol/types/` | 1 | 6 test fail trên stub |
| RED: `go test -run 'TestCreateTailnetRefusesInvalidSlug\|TestTailnetDNS' ./hscontrol/db/` | 1 | fail trên SQLite và PostgreSQL |
| RED: `go test -run TestP06 ./hscontrol/` | 1 | |
| RED: `go test -race -tags p05cluster -run TestClusterP06DNS ./hscontrol/servertest/` | 1 | 5/5 fail ở assertion |
| RED unique IP: `go test -run TestNodeIPUniquePerTailnet ./hscontrol/db/` | 1 | SQLite + PostgreSQL: "expected error, got nil" |
| GREEN: `go test ./hscontrol/types/ ./hscontrol/config/` | 0 | |
| GREEN: `go test ./hscontrol/db/...` (PostgreSQL có trong PATH, 0 skip) | 0 | |
| GREEN: `go test -race -tags p05cluster -run Cluster ./hscontrol/servertest/` | 0 | 218 s; gồm 5 test isolation + 5 test DNS + test cũ |
| `go test -race -count=1 -timeout 150m ./...` (sau khi gộp `master`) | 1 | 40 package ok; chỉ `TestAPIv2` fail vì thiếu `tscli` (môi trường) |
| `nix shell … tscli opentofu -c go test -race -run '^(TestAPIv2\|TestAPIv2OAuthScopes)$' ./hscontrol/servertest/` | 0 | |
| `go test -race -tags p05cluster -run 'Cluster\|P05\|P10' ./hscontrol/servertest/` (sau khi gộp) | 0 | 237 s |

## 7. Mutation

Test isolation (agent, bản sao tạm `~/src/p06-tests-mut` trên mc-ci, không commit):

| Mutation | Bị bắt bởi |
|---|---|
| M1 peer list của tailnet gồm node tailnet khác | S06, S08, S09, wildcard |
| M2 auto-approve hỏi policy tailnet khác | S08 |
| M3n `NodeCanHaveTag` hỏi policy tailnet khác | S09 |
| M3b tag owner khớp theo tên user xuyên tailnet | S09 |
| M4 các runtime chung DERP map | derpMap |
| M5 `{"grants":[]}` rơi về allow-all | wildcard |
| M3 khớp theo user ID | không bắt (không rò rỉ: ID khác tailnet) |

DNS (bản sao tạm trên mc-bench, không PostgreSQL nên test DB chạy nhánh SQLite; lần đối chứng không đột biến: pass):

| Mutation | Bị bắt bởi |
|---|---|
| D1 bỏ kiểm record dưới domain tailnet khác | `TestTenantDNSValidate`, `TestTailnetDNSRoundTripAndRevision`, `TestClusterP06DNSRefusesInvalid` |
| D2 `ForTailnet` trả config dùng chung | `TestConfigForTailnet`, `TestClusterP06DNS{NamesPerTailnet,SettingsPerTailnet,HotLoadNoRace}` |
| D3 hàng DNS không parse → mặc định | ban đầu **không bắt** (test chỉ có hàng parse được nhưng sai luật); thêm hàng JSON hỏng (`04342ed`) → `TestClusterP06DNSCorruptRowNotLoaded` |
| D3b hàng sai luật vẫn được phục vụ | `TestClusterP06DNSCorruptRowNotLoaded` |
| D4 runtime dùng config cluster | `TestClusterP06DNS{NamesPerTailnet,SettingsPerTailnet,HotLoadNoRace}` |
| D5 base domain = domain cluster | 8 test (types, db, cluster) |
| D6 ghi DNS không validate | `TestTailnetDNSRoundTripAndRevision`, `TestClusterP06DNSRefusesInvalid` |

## 8. E2E với client thật

Harness `infra/lab/e2e/p05/run.sh` (đường public `c1.cp.gta5web.online:8443`, client `tailscale/tailscale:v1.104.7` trên mc-ci), binary cluster build từ `28e1bfc` (sha256 `627c328cc3cb4f59…`; mã server giống `04342ed`), `mesh-bootstrap` sha256 `c8d5649fc009235f…`.

| Lần | Kết quả | Artifact (không commit) |
|---|---|---|
| `04342ed` lần 1 | **28/28 PASS**, cleanup_ok=1, 0 key chưa redact | `artifacts/04342ed/P05/e2e-20261124T083612Z` |
| `04342ed` lần 2 | **28/28 PASS**, cleanup_ok=1, 0 key chưa redact | `artifacts/04342ed/P05/e2e-20261124T084009Z` |

Ca mới: D01 `p05-a1.a.c1.tailnet.internal` / `p05-b1.b.c1.tailnet.internal`; D02 nameserver + search domain của B chỉ tới b1; D03 record dưới domain của A bị từ chối khi ghi cho B.
Cặp chạy trước tại `28e1bfc` (26/28 ×2) **không tính**: D01 hỏng do lỗi trích dẫn trong harness (mọi trường rỗng), D02 kỳ vọng sai (nameserver global thành fallback resolver khi `override_local_dns` false, mà `tailscale dns status` không liệt kê); log của chính lần đó cho thấy server đúng (suffix `b.c1…`, search domain của B chỉ ở b1).

## 9. Migration, triển khai, hạn chế

- **Migration (chỉ thêm vào cuối):** `202611241200-nodes-ip-unique-per-tailnet`, `202611241300-tailnet-settings`. Cả hai có rollback (drop index / drop bảng). Không chạy được khi DB có IP trùng trong một tailnet, hoặc slug không phải nhãn DNS: operator sửa trước, migration không tự đổi dữ liệu.
- **Thứ tự triển khai:** `mesh-bootstrap` (P11) chỉ nhận DB đã chạy migration mới nhất (`tailnet_settings`): chạy migration trước khi khởi động bootstrap mới.
- **Tên MagicDNS đổi** từ `node.<base>` sang `node.<slug>.<base>` (lab, chưa production).
- **Hạn chế:**
  - Test hot-load chưa tái hiện được data race `StartDNS` dưới `-race` trước khi sửa (RED chỉ fail ở assertion domain); race đã được loại bỏ theo thiết kế (mỗi runtime một `Config`/`liveDNS`), chưa có bằng chứng race detector.
  - Chưa có API v2 cho tenant ghi DNS (ADR 0008 Q-b); `GET /api/v2/tailnet/-/settings` đọc config của runtime.
  - Chưa test bầu chọn primary nhiều router HA; gắn tag qua đăng nhập web (cluster chưa có).
  - `servertest.TestCluster.Tenant()` vẫn mang config của cluster (chỉ ảnh hưởng helper test).
- **Reviewer approval:** chưa có. Merge theo lệnh chủ dự án ngày 2026-11-24.
