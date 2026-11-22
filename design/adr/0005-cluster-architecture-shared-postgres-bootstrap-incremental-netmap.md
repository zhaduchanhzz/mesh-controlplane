# ADR 0005 — Kiến trúc cluster: PostgreSQL dùng chung, bootstrap URL theo cluster, netmap cập nhật tăng dần

- **Trạng thái:** **Accepted** bởi chủ dự án (2026-11-22). Thay thế các phần tương ứng của ADR 0002 (storage), PLAN §1 (Storage, Endpoint, Noise identity) và quyết định "không làm netmap tăng dần" trong P00c.
- **Nguồn tham khảo:** bài "Headscale Series: Build a Headscale Cluster That Scales to Almost Unlimited Devices" (ownding.com, 2025-10-01). Đây là đề xuất thiết kế, không có số đo.

## Quyết định

1. **Storage:** một **PostgreSQL dùng chung** cho mọi shard (cluster). Thay cho SQLite riêng mỗi shard. Mọi bảng tenant có `tailnet_id NOT NULL` + composite FK + unique `(tailnet_id, …)`. Mỗi process shard chỉ thao tác trên các tailnet mà nó đang sở hữu (lease + epoch).
2. **Định tuyến:** client gọi **bootstrap** (dịch vụ điều phối, thay vai trò edge định tuyến theo Host) để nhận **URL của cluster** chứa tenant, rồi kết nối thẳng tới cluster đó.
3. **Netmap:** được phép **cập nhật peer map tăng dần** và **gửi map delta chỉ cho node bị ảnh hưởng**, kèm các biện pháp bắt buộc ở §3.

## Hệ quả (chủ dự án đã được trình bày trước khi chọn)

| Hệ quả | Chi tiết |
|---|---|
| PostgreSQL là điểm lỗi và điểm nghẽn chung | DB sập thì mọi shard sập. Phải có HA (primary + replica, failover có kiểm thử) và giới hạn kết nối theo shard |
| ID toàn cục trở nên đơn giản | Sequence của PostgreSQL vốn duy nhất toàn cục, không cần bộ cấp dải ID theo shard (thay §2.3 của inventory P03b) |
| Chuyển tenant giữa cluster | Không còn phải copy dữ liệu: đổi chủ sở hữu (epoch) trong registry. **Nhưng** URL cluster thay đổi, nên client **phải đổi ControlURL và đăng nhập lại**, trừ khi giữ một tầng ổn định ở giữa (xem câu hỏi mở). Mục tiêu P12b "không phải login lại" không còn đạt được |
| Noise identity | Mỗi **cluster** có một Noise key, dùng chung cho các tenant trong cluster đó. Kết quả P02 (mỗi tenant một key, theo Host) không còn là yêu cầu. Ranh giới tenant dựa vào: credential đăng ký gắn tailnet, `tailnet_id` của node trong DB, và scope fail-closed ở tầng ORM. AGENTS rule 12 được thỏa vì chủ dự án đã quyết định tường minh |
| Rủi ro isolation của netmap tăng dần | Bỏ sót một trường hợp thì có node thấy peer không được phép, hoặc vẫn thấy peer đã bị thu hồi. Rule 13 vẫn áp dụng: mọi lỗi isolation đều chặn merge |

## Biện pháp bắt buộc

### Netmap tăng dần
1. **Kiểm tra tương đương:** property test (`rapid`) so peer map tăng dần với bản dựng lại toàn bộ sau mỗi chuỗi thay đổi ngẫu nhiên (node, user, tag, route, exit node, policy, xóa, thu hồi). Hai kết quả phải giống hệt nhau.
2. **Đổi policy thì luôn dựng lại toàn bộ.** Chỉ thay đổi topology mới được cập nhật tăng dần.
3. **Đối chiếu khi chạy:** định kỳ (cấu hình được) dựng lại toàn bộ để so sánh. Có khác biệt thì ghi metric và log, rồi **thay bằng bản dựng lại toàn bộ** (fail safe).
4. **Delta thu hồi và xóa peer luôn được gửi**, và định kỳ gửi lại toàn bộ map để client tự sửa.
5. **E2E với client thật:** test thu hồi node, xóa user và đổi policy chặn: peer phải biến mất trong khoảng thời gian giới hạn.

### PostgreSQL dùng chung
1. Scope fail-closed ở tầng ORM (plugin GORM, P03b): không có tailnet trong context thì query lỗi.
2. **Row-level security** của PostgreSQL theo `tailnet_id` làm lớp phòng thủ thứ hai, nếu khả thi. Đánh giá trong P03b.
3. Mỗi shard dùng một role DB riêng. Mỗi mutation kiểm epoch (fencing) trong cùng transaction.
4. HA và backup/restore được kiểm thử (N3).

### Bootstrap
1. Bootstrap chỉ trả URL của cluster đang sở hữu tenant ở epoch hiện tại. Tenant đang `MOVING`/`SUSPENDED` thì từ chối.
2. Cluster tự kiểm lại quyền sở hữu tenant trước khi phục vụ, không tin hoàn toàn vào bootstrap.

## Câu hỏi mở

- Có giữ một **hostname ổn định cho mỗi tenant** (DNS CNAME tới cluster) để chuyển tenant mà client không phải đổi ControlURL không? Hostname đó vẫn trỏ tới Noise key của cluster đích. Lưu ý: key đổi khi chuyển cluster thì client phải reconnect, và có thể phải re-auth (xem `evidence/P02-noise-client-research.md`: key đổi thì client không tự hồi phục).
- Front door và `serve-tailnets` (P02): giữ lại cho môi trường lab, hay bỏ.

## Bổ sung quyết định (2026-11-22, chủ dự án)

1. **Mỗi cluster một Noise key** (giữ nguyên). Chủ dự án đã xem so sánh với phương án mỗi tenant một key và chọn giữ phương án này.
2. **Hostname ổn định cho mỗi tenant**: `<tenant>.cp.<domain>` là CNAME tới endpoint của cluster đang sở hữu tenant. Client cấu hình ControlURL một lần.
3. **Chuyển tenant giữa cluster BUỘC client đăng nhập lại.** Cluster đích có Noise key khác, mà client không tự hồi phục khi key của control URL thay đổi (`evidence/P02-noise-client-research.md`, Q1: key chỉ được giữ trong `Direct`, cần restart hoặc reset profile). Hệ quả:
   - Dù hostname ổn định, mỗi lần chuyển tenant vẫn gián đoạn control plane cho **mọi** client của tenant, cho tới khi từng client đăng nhập lại.
   - Node identity (node key, IP) có thể thay đổi sau khi đăng nhập lại; P12b phải đo và ghi lại.
   - P12b có bước thông báo và yêu cầu đăng nhập lại (xem `tickets/P12b.md`).
4. **Quyền sở hữu tenant:** thêm `owner_cluster_id` và `owner_epoch` vào bảng `tailnets` (PostgreSQL dùng chung). Cluster chỉ tạo scope cho tenant mà nó sở hữu ở epoch hiện tại. Mọi mutation kiểm owner + epoch trong cùng transaction (I12). Cluster ID **không** nằm trên từng dòng dữ liệu; `tailnet_id` vẫn là ranh giới cô lập duy nhất.
5. **Phạm vi khi lộ hoặc xoay key cluster:** ảnh hưởng **mọi tenant** trên cluster đó. Quy trình xoay key là yêu cầu của gate N3 (PLAN §7).
