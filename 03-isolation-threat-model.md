# 03 — Isolation & Threat Model (release blockers)

> Cập nhật theo PLAN 2.0 và [ADR 0005](design/adr/0005-cluster-architecture-shared-postgres-bootstrap-incremental-netmap.md) (gồm "Bổ sung 2"): PostgreSQL dùng chung, bootstrap trả URL cluster, một Noise key cho mỗi cluster, active/standby với lease, cô lập chỉ ở tầng ứng dụng, netmap tăng dần có biện pháp bảo vệ.

## 1. Tài sản cần bảo vệ

- Public/private node keys, machine keys, **Noise private key của mỗi cluster** (dùng chung bởi active và standby) và mapping; key private không được xuất trong log.
- Membership/user identity, nodes, IP, endpoints, Hostinfo, SSH capabilities, policy grants, routes/exit nodes, MagicDNS records.
- Service credentials/admin tokens, pending web registration, preauth keys, per-tenant event streams và backup snapshots.
- **PostgreSQL dùng chung** (chứa dữ liệu mọi tenant của mọi cluster) và credential của role ứng dụng.
- Cluster identity, lease cluster (`holder`, `epoch`), `owner_cluster_id`/`owner_epoch` của tenant, phản hồi bootstrap và toàn bộ APIs.

### Attacker model

A là tenant độc hại nhưng có account và client hợp lệ. A có thể:
- gửi MapRequest/Hostinfo lớn, sửa Host/SNI/header;
- dùng machine key từng dùng ở tenant B, thử key/token của B;
- tạo nhiều pending registration, đăng ký node với tag/route trùng, dùng relay;
- **ở trên cùng cluster với B**, nên dùng chung ControlURL và Noise key với B;
- gọi bootstrap để dò tenant khác, hoặc tự cấu hình ControlURL tới cluster không sở hữu tenant của mình.

Kẻ khác có thể điều khiển gateway không tin cậy hoặc replay credential hết hạn. **Không giả định** tenant có root trên process cluster. Không tuyên bố bảo vệ được nếu hypervisor, root, DB superuser, role ứng dụng PostgreSQL hoặc Noise key của cluster bị chiếm.

## 2. Invariants không thể thỏa hiệp

| ID | Bất biến | Bắt buộc chứng minh |
|---|---|---|
| I01 | Một request/Noise session/MapResponse có chính xác một `VerifiedTailnetScope`. Vì Noise key dùng chung cho cả cluster, scope **không** đến từ Host hay Noise key: register lấy tailnet từ **credential đăng ký**; map/poll lấy tailnet từ **bản ghi node** khớp đúng một `(machine_key, node_key)`; một phiên đã gắn tailnet thì không đổi được | Credential A + node key B trên cùng phiên fail; `(machine_key, node_key)` mơ hồ/không có → fail; Host lạ → 421 |
| I02 | Node của A không hiển thị trong bất kỳ netmap/key/SSH grant/DNS/route/event nào của B, kể cả qua đường netmap tăng dần | MapResponse negative tests; M01, M03 |
| I03 | Machine key có thể tồn tại ở A/B độc lập (kể cả A/B cùng cluster, cùng ControlURL) nhưng không tạo liên kết node hoặc quyền xuyên hai tailnet | Profile switch A↔B |
| I04 | Không có giai đoạn `CreateTailnet` allow-all dù chỉ một MapResponse | Concurrent `Create/Activate/Register` |
| I05 | ACL/grants `*` chỉ đánh giá đối tượng trong cùng tailnet; policy lỗi không fallback allow | Policy fuzz + wildcard |
| I06 | Truy vấn tenant-sensitive không bao giờ thiếu scope (plugin ORM fail-closed); SQL thô chỉ có trong allowlist đã review; constraints DB (composite FK, unique `(tailnet_id, …)`) chặn lỗi ghi lẫn tenant. **Không có RLS**: đây là lớp duy nhất | Plugin/repository/API scoped tests; lint SQL thô |
| I07 | Change A không ảnh hưởng danh sách peer hoặc event delivery B | Event/map diff hash |
| I08 | Service credential A/epoch cũ không ghi, xóa hay đọc tài nguyên B | mTLS/authZ/epoch tests |
| I09 | Debug/metrics/API không lộ IP, node ID, key, metadata tenant khác cho caller public (gồm cả listener của bootstrap) | Endpoint scan |
| I10 | Rate limit pending enrollment/map streams/quota/bootstrap theo tailnet, không đẩy B ra do A spam | Flood simulation |
| I11 | DERP verify và revocation được tenant-aware; relay không bypass node membership | Relay test |
| I12 | Không có hai owner đồng thời: một tenant có đúng một cluster sở hữu (`owner_cluster_id`, `owner_epoch`), mỗi cluster có đúng một process giữ lease (`holder`, `epoch`). Mọi mutation kiểm cả hai trong cùng transaction; stale epoch fail-closed; process quá hạn lease tự fence | CAS split brain test; F01–F04; S17 |
| I13 | Delete/revoke tenant không tái dùng secret, IP/record cũ một cách nguy hiểm; thu hồi luôn tới client (kể cả khi netmap tăng dần bật, kể cả qua failover) | Purge/revoke/recreate test; M04, M06 |
| I14 | Backup/restore (PostgreSQL) không phục hồi lease/`owner_epoch` cũ gây split-brain | Stale restore test (F08) |
| I15 | API, bootstrap và exports giới hạn dữ liệu, không leak qua error, timing, logs hoặc tracing nhạy cảm; bootstrap không cho dò tenant tồn tại | Cross-tenant audit; F06 |

## 3. Noise / tenant binding decision gate

**Quyết định (ADR 0005, chủ dự án):** **một Noise key cho mỗi cluster**, dùng chung bởi mọi tenant trên cluster và bởi cả active lẫn standby. POC P02 (mỗi tenant một key, chọn theo Host) đã `Verified` với client thật nhưng chỉ còn là hồ sơ. Bảo đảm của phiên Noise giờ chỉ là "client đang nói chuyện với đúng cluster"; nó **không** chứng minh tenant nào.

Hệ quả và yêu cầu:

- **Gắn tenant (I01):**
  - Register: tailnet = tailnet của credential đăng ký (pre-auth key, enrollment token, phê duyệt web); credential của tailnet không thuộc cluster này → từ chối.
  - Map/poll: tailnet = tailnet của node khớp **đúng một** `(machine_key, node_key)`; machine key được Noise xác thực, node key do client khai. Không có hoặc nhiều hơn một → fail closed.
  - Một phiên Noise đã gắn tailnet thì mọi request sau phải cùng tailnet; khác → từ chối, đóng phiên.
  - `tailnet_id` trong payload, header (`X-Tailnet-ID`) hay Host **không** bao giờ là nguồn scope.
- **Host/SNI:** listener kiểm Host thuộc allowlist hostname của cluster (chuẩn hóa như P02: lowercase, bỏ port, bỏ dấu chấm cuối; trên TLS: SNI == Host); Host lạ → 421. Đây là lớp chống nhầm cấu hình, không phải xác thực tenant. Phục vụ hoặc từ chối rõ ràng `/ts2021` trên cổng 80.
- **Hai profile cùng ControlURL:** khi A và B ở cùng cluster, client có hai profile với cùng ControlURL. Research P02 Q3/Q5 ghi client dedup profile theo `ControlURL + (UserID hoặc NodeID)` và dùng chung dial plan khi ControlURL trùng. UserID/NodeID toàn cục ngăn dedup nhầm. Phải có test S05 với A/B **cùng cluster** bằng client thật. Nếu client trộn hai profile: dừng, ADR (AGENTS rule 12).
- **Phê duyệt web:** với ControlURL chung, URL đăng ký không mang tenant; tenant được gắn khi admin phê duyệt. Rủi ro: kẻ tấn công gửi link đăng ký cho admin của B để node lạ vào B (giống phishing trên Tailscale). Biện pháp: trang phê duyệt hiển thị rõ tenant, thiết bị, thời hạn; pending gắn requester và nonce, dùng một lần.
- **Bootstrap:** không phải nguồn tin cậy. Cluster luôn tự kiểm `owner_cluster_id`, `owner_epoch` và lease. Client tới nhầm cluster (bootstrap cũ, tự cấu hình) → từ chối (F07).
- **Đổi key:** client không tự hồi phục khi key của ControlURL đổi trong vòng đời `Direct` (P02 research Q1). Vì vậy:
  - failover active/standby **phải** dùng cùng key (F01: không đăng nhập lại);
  - chuyển tenant sang cluster khác **buộc** đăng nhập lại (P12b);
  - xoay key cluster ảnh hưởng mọi client của cluster (gate N3).

## 4. Endpoint và dịch vụ đặc biệt

- Inventory toàn bộ route public/private: `/debug/*`, `/metrics`, `/api/*`, gRPC admin, Noise upgrade, `/verify`, DERP endpoints, healthchecks (`/ready` của P11b), và các route của bootstrap. Dùng router list tự động trong CI.
- Debug/logs chỉ trên management listener private, mTLS hoặc quyền operator; **tắt** public debug ở production. Prometheus labels chỉ aggregate theo cluster; tenant_id label bị giới hạn cardinality và không chứa PII.
- `/ready` chỉ trả trạng thái giữ lease (có/không), không trả epoch, tenant hay số node.
- Embedded DERP: **disabled trong lab ban đầu**; chỉ bật sau audit `verify_clients`, per-tenant revocation và resource budgeting. DERP external cần verify callback fail-closed khi không liên hệ được control plane; không dùng cấu hình fail-open. Sau failover, verify phải đi tới process đang giữ lease.
- STUN thường là UDP không có khái niệm tailnet auth; shared STUN có thể dùng cho NAT discovery nhưng phải rate-limit toàn dịch vụ và per-IP. Không coi STUN là cơ chế kết nối VPN peer đã được cấp quyền.
- Queues: có giới hạn memory và admission per-tailnet, tổng budget của cluster, fair scheduling + timeout + 429/503 có Retry-After. Không cho một tenant chiếm hết `pending registration` slots, map stream workers, hay kết nối PostgreSQL của cluster.

## 5. Lifecycle fail-closed

| Operation | Chính sách bảo mật |
|---|---|
| `CreateTailnet` | Atomic metadata + empty grants + owner trong một transaction PostgreSQL; PENDING không đăng ký client; bootstrap từ chối |
| `Activate` | Chỉ khi policy validator, owner + epoch và lease của cluster đích hợp lệ |
| `Suspend` | Chặn enrollment/mutation, revoke active credentials hoặc explicit policy theo ADR; đẩy map update; bootstrap từ chối |
| `Delete` | Mark DELETING, reject new requests, revoke tokens, disconnect/expire nodes, xoá dữ liệu trong transaction phân kỳ an toàn, audit tombstone |
| `RevokeNode` | Không phát node key/route tới peers nữa (delta thu hồi luôn gửi, kể cả khi netmap tăng dần bật), clear registered sessions, DERP verification phản ánh state |
| `Failover` (P11b) | Standby lấy lease với epoch mới; active cũ tự fence và bị fence ở DB; state trong bộ nhớ bị bỏ; không đổi owner của tenant |
| `MoveTenant` (P12b) | `MOVING` + tăng `owner_epoch`; nguồn fence; bootstrap từ chối tới khi đích ACTIVE; client đăng nhập lại |
| `RotateClusterKey` | Có runbook (N3); ảnh hưởng mọi tenant trên cluster; không silent replace key khiến client tin nhầm endpoint |

## 6. Báo cáo lỗ hổng

Bất kỳ test I01–I15 thất bại → **block release**. Phải lưu:

1. Tình huống và request đã redact; peer key hashed hoặc dummy fixtures.
2. Root cause đi qua file/function/query/cache/event path.
3. Test regression RED trước, GREEN sau; không tắt test để qua gate.
4. Reviewer độc lập xác nhận tác động đến các tailnet khác, restart, failover và recovery.

## 7. Kiểm tra phân quyền người vận hành

Một identity có quyền admin trên một tailnet không được xem tailnet khác, kể cả qua pagination, list API, policy dry-run, audit stream, debug stack traces, status RPC, bootstrap hoặc error details. Operator/superuser được định nghĩa bằng role riêng, có audit và MFA/phê duyệt cho lệnh phá hủy.

## 8. Rủi ro còn lại đã được chủ dự án chấp nhận (ADR 0005)

| Rủi ro | Hệ quả | Biện pháp bù (không loại bỏ rủi ro) |
|---|---|---|
| **Noise key dùng chung cho cả cluster** | Lộ key → kẻ tấn công giả được control server với **mọi** client của **mọi** tenant trên cluster (cần thêm khả năng chặn đường mạng); xoay key buộc mọi client reconnect, có thể đăng nhập lại. Key nằm trên hai process (active + standby) | Key trong file/secret store ngoài repo, quyền đọc tối thiểu, không log; `/key` chỉ qua HTTPS có cert hợp lệ; runbook xoay và quy trình khẩn cấp (N3) |
| **PostgreSQL dùng chung** | Một điểm lỗi và điểm nghẽn cho mọi cluster; DB sập thì control plane mọi cluster dừng. Lộ credential role ứng dụng hoặc chiếm DB = lộ mọi tenant | HA primary + replica, failover có kiểm thử (F05); giới hạn kết nối theo cluster; mạng tới 5432 chỉ từ VM cluster/bootstrap; backup không làm lùi epoch (F08) |
| **Cô lập chỉ ở tầng ứng dụng** | Không có RLS, không có role theo cluster. Một lỗi app bỏ qua scope (SQL thô chưa review, handle DB không gắn scope, cache thiếu `tailnet_id`) đọc hoặc ghi được dữ liệu tenant khác **trên toàn PostgreSQL**, không chỉ trên một cluster. Không có lớp chặn ở DB | Plugin GORM fail-closed; review tay mọi SQL thô; lint/test CI chặn SQL thô mới ngoài allowlist; composite FK + unique `(tailnet_id, …)`; test isolation âm tính; rule 13 |
| **Netmap tăng dần** | Bỏ sót một trường hợp → node thấy peer không được phép, hoặc vẫn thấy peer đã thu hồi | Công tắc mặc định OFF; equivalence test với full rebuild (M01); đổi policy luôn full rebuild (M02); đối chiếu định kỳ, lệch thì thay bằng full rebuild (M03); delta thu hồi luôn gửi + full map định kỳ (M04); E2E thu hồi với client thật (M06) |
| **Failover active/standby** | Active cũ treo rồi tỉnh có thể phát MapResponse cũ (ví dụ chưa thu hồi) trước khi biết mất lease | Tự fence theo deadline monotonic trước khi lease hết hạn ở DB; mọi ghi kiểm lease trong transaction; F02, F03 |
| **Hai tenant cùng ControlURL** | Client có hai profile cùng URL; hành vi dedup/dial plan của client là ràng buộc ngoài tầm kiểm soát | UserID/NodeID toàn cục; S05 với A/B cùng cluster bằng client thật |
