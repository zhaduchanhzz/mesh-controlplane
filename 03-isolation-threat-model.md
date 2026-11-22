# 03 — Isolation & Threat Model (release blockers)

## 1. Tài sản cần bảo vệ

- Public/private node keys, machine keys, control-server Noise public identities và mapping; key private không được xuất trong log.
- Membership/user identity, nodes, IP, endpoints, Hostinfo, SSH capabilities, policy grants, routes/exit nodes, MagicDNS records.
- Service credentials/admin tokens, pending web registration, preauth keys, per-tenant event streams và backup snapshots.
- Shard identity, ownership epochs, route manifest và toàn bộ APIs.

### Attacker model

A là tenant độc hại nhưng có account và client hợp lệ; có thể gửi MapRequest/Hostinfo lớn, sửa Host/SNI/header, dùng machine key từng dùng ở tenant B, thử key/token B, tạo nhiều pending registrations, đăng ký node với tag/route trùng, dùng relay. Kẻ khác có thể điều khiển gateway không tin cậy hoặc replay credential hết hạn. **Không giả định** tenant có root trên shard process; không tuyên bố bảo vệ được nếu hypervisor/root/DB master key bị chiếm.

## 2. Invariants không thể thỏa hiệp

| ID | Bất biến | Bắt buộc chứng minh |
|---|---|---|
| I01 | Một request/Noise session/MapResponse có chính xác một `VerifiedTailnetScope` | Host/Noise mismatch fail |
| I02 | Node của A không hiển thị trong bất kỳ netmap/key/SSH grant/DNS/route/event nào của B | MapResponse negative tests |
| I03 | Machine key có thể tồn tại ở A/B độc lập nhưng không tạo liên kết node hoặc quyền xuyên hai tailnet | Profile switch A↔B |
| I04 | Không có giai đoạn `CreateTailnet` allow-all dù chỉ một MapResponse | Concurrent `Create/Activate/Register` |
| I05 | ACL/grants `*` chỉ đánh giá đối tượng trong cùng tailnet; policy lỗi không fallback allow | Policy fuzz + wildcard |
| I06 | Truy vấn tenant-sensitive không bao giờ thiếu scope; DB constraints bảo vệ lỗi lập trình | Repository/API scoped tests |
| I07 | Change A không ảnh hưởng danh sách peer hoặc event delivery B | Event/map diff hash |
| I08 | Service credential A/epoch cũ không ghi, xóa hay đọc tài nguyên B | mTLS/authZ/epoch tests |
| I09 | Debug/metrics/API không lộ IP, node ID, key, metadata tenant khác cho caller public | Endpoint scan |
| I10 | Rate limit pending enrollment/map streams/quota theo tailnet, không đẩy B ra do A spam | Flood simulation |
| I11 | DERP verify và revocation được tenant-aware; relay không bypass node membership | Relay test |
| I12 | Không có hai owner đồng thời khi placement thay đổi; stale epoch fail-closed | CAS split brain test |
| I13 | Delete/revoke tenant không tái dùng secret, IP/record cũ một cách nguy hiểm | Purge/revoke/recreate test |
| I14 | Backup/restore không phục hồi lease/epoch cũ gây split-brain | Stale restore test |
| I15 | API và exports giới hạn dữ liệu, không leak qua error, timing, logs hoặc tracing nhạy cảm | Cross-tenant audit |

## 3. Noise / hostname decision gate

**Yêu cầu thiết kế:** hostname riêng, key danh tính riêng theo tailnet là cấu hình ưu tiên. **Không khẳng định được thực hiện cho đến khi P02 POC qua client thật**.

- Audit `/key` response trên URL A/B, TLS certificate trust, Noise public key pinning/caching và map stream identity.
- Client A chỉ tin key A; host A + key B hoặc Host/SNI mismatch phải bị từ chối.
- Dùng session scope cố định được xác lập ở TLS edge và được shard kiểm tra; không dùng header do client tự cấp.
- Test client profile A→B→A cùng machine key; trên restart/resume stream vẫn bound A/B.
- Nếu Tailscale client không tương thích với per-tailnet key trong shared process, dừng ở P02, lập ADR giải thích phương án: tách front listener/virtual server identity theo tailnet trong cùng process (ưu tiên), hoặc chứng minh tenant binding an toàn khác bằng protocol test; không tự thay sang shard-wide key.

## 4. Endpoint và dịch vụ đặc biệt

- Inventory toàn bộ route public/private: `/debug/*`, `/metrics`, `/api/*`, gRPC admin, Noise upgrade, `/verify`, DERP endpoints và healthchecks. Dùng router list tự động trong CI.
- Debug/logs chỉ trên management listener private, mTLS hoặc quyền operator; **tắt** public debug ở production. Prometheus labels chỉ aggregate theo shard; tenant_id label bị giới hạn cardinality và không chứa PII.
- Embedded DERP: **disabled trong lab ban đầu**; chỉ bật sau audit `verify_clients`, per-tenant revocation và resource budgeting. DERP external cần verify callback fail-closed khi không liên hệ được control plane; không sử dụng cấu hình fail-open.
- STUN thường là UDP không có khái niệm tailnet auth; shared STUN có thể dùng cho NAT discovery nhưng phải rate-limit toàn dịch vụ và per-IP. Không coi STUN là cơ chế kết nối VPN peer đã được cấp quyền.
- Queues: có giới hạn memory và admission per-tailnet, tổng shard budget, fair scheduling + timeout + 429/503 có Retry-After. Không cho một tenant chiếm hết `pending registration` slots hay map stream workers.

## 5. Lifecycle fail-closed

| Operation | Chính sách bảo mật |
|---|---|
| `CreateTailnet` | Atomic metadata+empty grants; PENDING không đăng ký client |
| `Activate` | Chỉ khi policy validator, Noise binding, settings and epoch đồng bộ |
| `Suspend` | Chặn enrollment/mutation, revoke active credentials hoặc explicit policy theo ADR; đẩy map update |
| `Delete` | Mark DELETING, reject new requests, revoke tokens, disconnect/expire nodes, xoá dữ liệu trong transaction phân kỳ an toàn, audit tombstone |
| `RevokeNode` | Không phát node key/route tới peers nữa, clear registered sessions, DERP verification phản ánh state |
| `RotateIdentityKey` | Phải có protocol procedure; không silent replace key khiến client tin nhầm endpoint |

## 6. Báo cáo lỗ hổng

Bất kỳ test I01–I15 thất bại → **block release**. Phải lưu:

1. Tình huống và request đã redact; peer key hashed hoặc dummy fixtures.
2. Root cause đi qua file/function/query/cache/event path.
3. Test regression RED trước, GREEN sau; không tắt test để qua gate.
4. Reviewer độc lập xác nhận tác động đến các tailnet khác, restart và recovery.

## 7. Kiểm tra phân quyền người vận hành

Một identity có quyền admin trên một tailnet không được xem tailnet khác, kể cả qua pagination, list API, policy dry-run, audit stream, debug stack traces, status RPC hoặc error details. Operator/superuser được định nghĩa bằng role riêng, có audit và MFA/phê duyệt cho lệnh phá hủy.
