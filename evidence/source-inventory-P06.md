# PR P06 Evidence — source inventory: policy/ACL, DNS, routes, exit node giữa các tailnet

- **Trạng thái:** `Planned`. Chỉ là inventory: chưa đổi code, chưa chạy test. Mục **[Đề xuất]** chờ chủ dự án chốt.
- **Nhánh:** `feat/P06-policy-dns-routes` (từ `master` `d678618`).
- **Phạm vi (PLAN dòng 275, `06-tests-benchmarks.md`):** S06 (IP/hostname/subnet route trùng giữa A và B), S07 (B sửa DNS/extra records không ảnh hưởng A), S08 (route/exit node, CIDR trùng; không tự approve, không lộ default route), S09 (tag bounded theo tailnet), I02, I05, I07; AGENTS 8 (`*` không vượt tailnet).
- Chưa có `tickets/P06.md`; file này đóng vai hợp đồng cho tới khi có ticket.

## 1. Repo commit

| | Commit |
|---|---|
| Before | `d678618` (`master`, sau P11) |
| After | chưa có |

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

## 5. Tests / E2E / migration / benchmark / review

Chưa chạy. Migration chỉ khi Q3 được chốt. Không phải hot path (trừ khi Q2 đụng mapper). Reviewer approval: chưa có.
