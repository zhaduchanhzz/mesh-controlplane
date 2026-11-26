# ADR 0005 — Kiến trúc cluster: PostgreSQL dùng chung, bootstrap URL theo cluster, netmap cập nhật tăng dần

- **Trạng thái:** **Accepted** bởi chủ dự án (2026-11-22). Thay thế các phần tương ứng của ADR 0002 (storage, cấp khối ID), ADR 0003 (copy dữ liệu khi move, Noise key đi cùng tenant), ADR 0001 (Noise key theo runtime), PLAN §1 (Storage, Endpoint, Noise identity) và quyết định "không làm netmap tăng dần" trong P00c.
- **Lịch sử:** bản đầu → "Bổ sung quyết định" (2026-11-22) → **"Bổ sung 2" (2026-11-22, hiện hành)**. Không xóa text cũ; chỗ nào đã bị thay được đánh dấu **[Đã thay bởi Bổ sung 2]**. Khi hai đoạn mâu thuẫn, Bổ sung 2 thắng.
- **Nguồn tham khảo:** bài "Headscale Series: Build a Headscale Cluster That Scales to Almost Unlimited Devices" (ownding.com, 2025-10-01). Đây là đề xuất thiết kế, không có số đo.

## Quyết định

1. **Storage:** một **PostgreSQL dùng chung** cho mọi shard (cluster). Thay cho SQLite riêng mỗi shard. Mọi bảng tenant có `tailnet_id NOT NULL` + composite FK + unique `(tailnet_id, …)`. Mỗi process shard chỉ thao tác trên các tailnet mà nó đang sở hữu (lease + epoch).
2. **Định tuyến:** client gọi **bootstrap** (dịch vụ điều phối, thay vai trò edge định tuyến theo Host) để nhận **URL của cluster** chứa tenant, rồi kết nối thẳng tới cluster đó. (Chi tiết hiện hành: Bổ sung 2 §1.)
3. **Netmap:** được phép **cập nhật peer map tăng dần** và **gửi map delta chỉ cho node bị ảnh hưởng**, kèm các biện pháp bắt buộc ở §3.

## Hệ quả (chủ dự án đã được trình bày trước khi chọn)

| Hệ quả | Chi tiết |
|---|---|
| PostgreSQL là điểm lỗi và điểm nghẽn chung | DB sập thì mọi shard sập. Phải có HA (primary + replica, failover có kiểm thử) và giới hạn kết nối theo shard |
| ID toàn cục trở nên đơn giản | Sequence của PostgreSQL vốn duy nhất toàn cục, không cần bộ cấp dải ID theo shard (thay §2.3 của inventory P03b) |
| Chuyển tenant giữa cluster | Không còn phải copy dữ liệu: đổi chủ sở hữu (epoch) trong registry. **Nhưng** URL cluster thay đổi, nên client **phải đổi ControlURL và đăng nhập lại**, trừ khi giữ một tầng ổn định ở giữa (xem câu hỏi mở; **[Đã thay bởi Bổ sung 2]**: không có tầng ổn định, client gọi lại bootstrap). Mục tiêu P12b "không phải login lại" không còn đạt được |
| Noise identity | Mỗi **cluster** có một Noise key, dùng chung cho các tenant trong cluster đó. Kết quả P02 (mỗi tenant một key, theo Host) không còn là yêu cầu. Ranh giới tenant dựa vào: credential đăng ký gắn tailnet, `tailnet_id` của node trong DB, và scope fail-closed ở tầng ORM. AGENTS rule 12 được thỏa vì chủ dự án đã quyết định tường minh |
| Rủi ro isolation của netmap tăng dần | Bỏ sót một trường hợp thì có node thấy peer không được phép, hoặc vẫn thấy peer đã bị thu hồi. Rule 13 vẫn áp dụng: mọi lỗi isolation đều chặn merge |

## Biện pháp bắt buộc

### Netmap tăng dần

> **Cập nhật 2026-11-26 (ADR 0015):** công tắc netmap tăng dần chuyển **ON mặc định**; mọi thay đổi chỉ gửi phần khác so với cái client đã nhận (delta-first). Biện pháp 1–5 dưới đây vẫn hiệu lực; §2 (đổi policy thì dựng lại toàn bộ) áp cho **tính** adjacency, không cho cái gửi đi.

1. **Kiểm tra tương đương:** property test (`rapid`) so peer map tăng dần với bản dựng lại toàn bộ sau mỗi chuỗi thay đổi ngẫu nhiên (node, user, tag, route, exit node, policy, xóa, thu hồi). Hai kết quả phải giống hệt nhau.
2. **Đổi policy thì luôn dựng lại toàn bộ.** Chỉ thay đổi topology mới được cập nhật tăng dần.
3. **Đối chiếu khi chạy:** định kỳ (cấu hình được) dựng lại toàn bộ để so sánh. Có khác biệt thì ghi metric và log, rồi **thay bằng bản dựng lại toàn bộ** (fail safe).
4. **Delta thu hồi và xóa peer luôn được gửi**, và định kỳ gửi lại toàn bộ map để client tự sửa.
5. **E2E với client thật:** test thu hồi node, xóa user và đổi policy chặn: peer phải biến mất trong khoảng thời gian giới hạn.

### PostgreSQL dùng chung
1. Scope fail-closed ở tầng ORM (plugin GORM, P03b): không có tailnet trong context thì query lỗi.
2. ~~**Row-level security** của PostgreSQL theo `tailnet_id` làm lớp phòng thủ thứ hai, nếu khả thi. Đánh giá trong P03b.~~ **[Đã thay bởi Bổ sung 2 §3]**: không dùng RLS.
3. ~~Mỗi shard dùng một role DB riêng.~~ **[Đã thay bởi Bổ sung 2 §3]**: không có role DB theo shard. Mỗi mutation kiểm epoch (fencing) trong cùng transaction (vẫn hiệu lực, xem Bổ sung 2 §2).
4. HA và backup/restore được kiểm thử (N3).

### Bootstrap
1. Bootstrap chỉ trả URL của cluster đang sở hữu tenant ở epoch hiện tại. Tenant đang `MOVING`/`SUSPENDED` thì từ chối.
2. Cluster tự kiểm lại quyền sở hữu tenant trước khi phục vụ, không tin hoàn toàn vào bootstrap.

## Câu hỏi mở

- **[Đã trả lời bởi Bổ sung 2 §1: không.]** Có giữ một **hostname ổn định cho mỗi tenant** (DNS CNAME tới cluster) để chuyển tenant mà client không phải đổi ControlURL không? Hostname đó vẫn trỏ tới Noise key của cluster đích. Lưu ý: key đổi khi chuyển cluster thì client phải reconnect, và có thể phải re-auth (xem `evidence/P02-noise-client-research.md`: key đổi thì client không tự hồi phục).
- **[Đã trả lời bởi Bổ sung 2 §5.]** Front door và `serve-tailnets` (P02): giữ lại cho môi trường lab, hay bỏ.

## Bổ sung quyết định (2026-11-22, chủ dự án)

1. **Mỗi cluster một Noise key** (giữ nguyên). Chủ dự án đã xem so sánh với phương án mỗi tenant một key và chọn giữ phương án này.
2. ~~**Hostname ổn định cho mỗi tenant**: `<tenant>.cp.<domain>` là CNAME tới endpoint của cluster đang sở hữu tenant. Client cấu hình ControlURL một lần.~~ **[Đã thay bởi Bổ sung 2 §1]**: không có hostname theo tenant; ControlURL là URL trực tiếp của cluster, lấy từ bootstrap.
3. **Chuyển tenant giữa cluster BUỘC client đăng nhập lại.** Cluster đích có Noise key khác, mà client không tự hồi phục khi key của control URL thay đổi (`evidence/P02-noise-client-research.md`, Q1: key chỉ được giữ trong `Direct`, cần restart hoặc reset profile). Hệ quả:
   - Dù hostname ổn định (**[Đã thay bởi Bổ sung 2 §1]**: nay client còn phải đổi ControlURL), mỗi lần chuyển tenant vẫn gián đoạn control plane cho **mọi** client của tenant, cho tới khi từng client đăng nhập lại.
   - Node identity (node key, IP) có thể thay đổi sau khi đăng nhập lại; P12b phải đo và ghi lại.
   - P12b có bước thông báo và yêu cầu đăng nhập lại (xem `tickets/P12b.md`).
4. **Quyền sở hữu tenant:** thêm `owner_cluster_id` và `owner_epoch` vào bảng `tailnets` (PostgreSQL dùng chung). Cluster chỉ tạo scope cho tenant mà nó sở hữu ở epoch hiện tại. Mọi mutation kiểm owner + epoch trong cùng transaction (I12). Cluster ID **không** nằm trên từng dòng dữ liệu; `tailnet_id` vẫn là ranh giới cô lập duy nhất.
5. **Phạm vi khi lộ hoặc xoay key cluster:** ảnh hưởng **mọi tenant** trên cluster đó. Quy trình xoay key là yêu cầu của gate N3 (PLAN §7).

## Bổ sung 2 (2026-11-22, chủ dự án)

Ba quyết định dưới đây là cuối cùng. Chúng thay dòng "hostname ổn định cho mỗi tenant" (Bổ sung quyết định, mục 2), mục 2–3 của "Biện pháp bắt buộc / PostgreSQL dùng chung", và trả lời hai câu hỏi mở. Các mục khác giữ nguyên: mỗi cluster một Noise key, `owner_cluster_id` + `owner_epoch` trên `tailnets`, chuyển tenant buộc đăng nhập lại, phạm vi khi lộ key cluster.

### 1. Định tuyến: bootstrap trả URL trực tiếp của cluster, không có hostname theo tenant

- Client (hoặc công cụ onboarding thay mặt client) gọi **bootstrap** với định danh tenant và nhận **URL trực tiếp của cluster** đang sở hữu tenant, ví dụ `https://cluster-2.cp.gta5web.online:8443`. Client đặt URL đó làm ControlURL và kết nối thẳng tới cluster. Bootstrap không nằm trên đường đi của `/key`, `/ts2021` hay map stream.
- **Không có hostname ổn định theo tenant**, không có CNAME `<tenant>.cp.<domain>`. Các tenant trên cùng cluster dùng chung một ControlURL và một Noise key.
- Khi tenant chuyển sang cluster khác (P12b), client **phải gọi lại bootstrap, đổi ControlURL và đăng nhập lại**. Đây là thủ tục có thông báo trước, không phải failover.
- Bootstrap **từ chối** tenant ở trạng thái `MOVING` hoặc `SUSPENDED` (và cả `PENDING`, `DELETING`).
- Cluster **luôn tự kiểm lại** quyền sở hữu (`owner_cluster_id`, `owner_epoch`, lease của cluster) trước khi phục vụ register, map hay mutation. Cluster không tin bootstrap: client tự cấu hình sai URL, hoặc bootstrap trả dữ liệu cũ, đều phải bị cluster từ chối fail closed.
- Ràng buộc tenant của một phiên (I01) không còn đến từ Host hay Noise key. Nó đến từ **credential đăng ký** gắn `tailnet_id` (pre-auth key, enrollment token, phê duyệt web), và sau khi đăng ký thì từ **bản ghi node** khớp `(machine_key, node_key)` trong DB. Host chỉ được kiểm là một trong các hostname của chính cluster; Host lạ bị từ chối.

### 2. Failover tự động thay cho dừng dịch vụ

- Mỗi cluster gồm **một process active và một process standby**. Hai process dùng chung **Noise key của cluster** và cùng nối tới PostgreSQL dùng chung. Endpoint của cluster chỉ đưa traffic tới process đang giữ lease (VIP hoặc L4 health check; chốt trong ticket P11b). Việc định tuyến này chỉ phục vụ tính sẵn sàng; an toàn đến từ fencing.
- **Lease của cluster** nằm trong PostgreSQL, có TTL, và được đánh giá bằng đồng hồ của PostgreSQL (`now()`). Active gia hạn định kỳ. Khi active chết hoặc treo, lease hết hạn và standby **tự động** lấy lease với **epoch mới** (tăng đơn điệu).
- Client reconnect tới cùng ControlURL, bắt tay bằng cùng Noise key và dùng node key đã có trong DB, nên **không phải đăng nhập lại**. State chỉ nằm trong bộ nhớ (pending web auth, `authCache`, `sshCheckAuth`, pings) bị mất; request đang dở phải làm lại.
- Active cũ sống lại (hết treo, mạng phục hồi) thấy epoch của mình đã cũ. Nó **từ chối mọi ghi** và không phát thêm MapResponse nào. Không có split brain vì:
  - mọi mutation kiểm, trong **cùng transaction**, cả lease của cluster (holder + epoch, chưa hết hạn theo `now()` của DB) lẫn `owner_cluster_id` + `owner_epoch` của tenant (I12);
  - process tự fence theo đồng hồ monotonic: quá thời điểm gia hạn thành công cuối + TTL − biên an toàn thì ngừng phục vụ, kể cả khi chưa liên lạc được DB.
- Khi không truy cập được PostgreSQL thì không process nào gia hạn hay lấy được lease. Control plane của mọi cluster dừng (fail closed); data plane hiện có của client chạy tiếp theo netmap cuối. Đây là hệ quả đã biết của PostgreSQL dùng chung (bảng Hệ quả ở trên); HA PostgreSQL là yêu cầu N3.
- **Chuyển tenant có kế hoạch giữa hai cluster KHÁC nhau** (cân bằng tải) vẫn là thủ tục P12b: thông báo trước, re-bootstrap, đổi ControlURL, đăng nhập lại. Failover active/standby **không** đổi `owner_cluster_id` hay `owner_epoch` của tenant; nó chỉ đổi epoch của lease cluster.

### 3. Cô lập tenant chỉ ở tầng ứng dụng

- **Không** dùng row-level security của PostgreSQL. **Không** có role DB theo shard hay cluster. Mọi cluster dùng cùng một role ứng dụng; role chạy migration tách riêng.
- Cô lập dựa vào bốn thứ:
  1. scope fail-closed ở ORM (plugin GORM, P03b): thiếu `TailnetID` trong context thì query lỗi;
  2. kiểm owner + epoch (và lease cluster) trong cùng transaction cho mọi mutation;
  3. **review tay từng câu SQL thô** (`Raw`, `Exec`, SQL trong migration), ghi vào inventory của PR;
  4. một **test hoặc lint** trong CI đánh dấu mọi câu SQL thô mới nằm ngoài danh sách đã review (allowlist có ghi lý do).
- **Rủi ro còn lại được chấp nhận:** một lỗi ứng dụng bỏ qua scope (SQL thô mới chưa review, đường code dùng DB handle không gắn scope, cache keyed thiếu `tailnet_id`) **không có lớp chặn nào ở DB**. Lỗi đó đọc hoặc ghi được dữ liệu của tenant khác trên toàn bộ PostgreSQL dùng chung, không chỉ trên một cluster. Biện pháp bù: composite FK + unique `(tailnet_id, …)` (chặn phần lớn lỗi ghi lẫn tenant), test isolation âm tính, và rule 13 (lỗi isolation = BLOCK MERGE).

### 4. Hệ quả lên tài liệu khác

- PLAN 2.0 §0–§9, §11; `03-isolation-threat-model.md` (I01, §3, rủi ro còn lại); `05-data-api-contract.md`; `06-tests-benchmarks.md` (S11, S16, S17, F01–F08, M01–M06).
- Ticket mới: `tickets/P00d.md` (netmap tăng dần + biện pháp bảo vệ), `tickets/P10.md` (registry + fencing trên PostgreSQL dùng chung), `tickets/P11.md` (bootstrap, thay edge route manifest), `tickets/P11b.md` (lease cluster + failover active/standby). Ticket sửa: `tickets/P03.md`, `tickets/P12b.md`.

### 5. Front door và `serve-tailnets` (trả lời câu hỏi mở)

- **`hscontrol/frontdoor`: giữ.** Listener của cluster vẫn cần chuẩn hóa Host, đối chiếu SNI == Host, bỏ header client tự đặt và trả 421 khi Host lạ. Chỉ đổi bảng tra từ "host → tailnet" thành "allowlist hostname của cluster". Không còn chọn tailnet theo Host.
- **`serve-tailnets`** (N runtime, mỗi runtime một DB và một Noise key): **deprecated**. Vẫn chạy trên `mc-cp` cho tới khi P05 có runtime theo tailnet trên PostgreSQL dùng chung, vì endpoint public và harness E2E của P02 đang dựa vào nó và chưa có thay thế. Không thêm tính năng, không dùng làm cơ sở thiết kế. Gỡ trong P05 hoặc ngay sau gate N1 (P09), kèm cập nhật harness.
