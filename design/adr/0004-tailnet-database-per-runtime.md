# ADR 0004 — Một database và một runtime cho mỗi tailnet trong process của shard

- **Trạng thái:** Proposed (2026-11-22). Cần chủ dự án quyết định, vì thay đổi hướng "Storage" trong PLAN §1 và phạm vi P03–P07.
- **Liên quan:** ADR 0001 (runtime theo tailnet), ADR 0002 (fresh schema + ID toàn cục), P02 POC (`serve-tailnets` + front door), ticket P03, P12b.

## Bối cảnh

PLAN chọn **một SQLite cho mỗi shard**: mọi bảng có `tailnet_id`, composite FK, ID toàn cục. Cách này buộc phải refactor sâu `hscontrol/db`, `state`, `mapper` và `policy` (P03–P07), vì upstream viết cho đúng một tailnet.

P02 POC đã chạy một mô hình khác **trên client thật**: mỗi tailnet là một runtime Headscale đầy đủ (DB SQLite, Noise key, NodeStore, policy, batcher và `server_url` riêng) trong **cùng một process**, đứng sau front door chọn tenant theo Host. E2E với client thật đã xanh: R01 (netmap tách biệt), R02 (cùng machine key → 2 node độc lập), R03 (key không dùng chéo được).

### Số đo chi phí khi không tải (mc-ci, 8 vCPU, binary P02 `515b596c`, 25 s sau khi khởi động)

| N runtime | RSS | Heap in-use | Goroutine | FD |
|---|---|---|---|---|
| 1 | 49.4 MiB | 5.7 MiB | 29 | 13 |
| 10 | 65.0 MiB | 18.1 MiB | 182 | 58 |
| 50 | 131.7 MiB | 67.4 MiB | 862 | 258 |

Chi phí biên mỗi tailnet (10 → 50): khoảng **1.7 MiB RSS, 1.2 MiB heap, 17 goroutine, 5 FD**. Script đo: `/tmp/rt-bench.sh` trên mc-ci (sẽ đưa vào repo). Chưa đo khi có tải; chi phí theo số node sẽ do P00b đo.

## Phương án

| | **A. Một DB cho shard + `tailnet_id`** (PLAN hiện tại) | **B. Một DB và một runtime cho mỗi tailnet** (mở rộng POC) |
|---|---|---|
| Cách cô lập | Ở tầng query: mọi query, cache, event và map phải mang `tailnet_id`. Mỗi chỗ quên là một lỗ rò (rule 7) | **Theo cấu trúc:** không có cấu trúc dữ liệu nào dùng chung giữa các tailnet |
| Khối lượng code | Lớn: schema, migration, state/NodeStore, mapper, policy, API | Nhỏ: phần lõi upstream gần như giữ nguyên; thêm catalog tenant, vòng đời runtime, chia sẻ tài nguyên |
| ID toàn cục | **Bắt buộc** (StableID không được đổi khi chuyển shard) | **Không cần**: ID chỉ có nghĩa trong một tailnet; chuyển tenant thì chuyển nguyên DB, ID giữ nguyên |
| Chuyển tenant (P12b) | Export/import các row theo `tailnet_id` | **Copy một file SQLite** (cộng Noise key), rồi fence epoch |
| Backup, restore, xóa tenant | Theo row | Theo file; xóa tenant = xóa file sau khi tombstone |
| Chi phí theo tenant khi không tải | Thấp nhất | ~1.7 MiB và 17 goroutine mỗi tenant (đã đo); 1000 tenant ≈ 1.7 GiB |
| Rủi ro riêng | Thiếu `WHERE` hoặc cache key, lộ dữ liệu xuyên tenant | Nhiều file và FD (cần `ulimit`); việc nền nhân theo N (DERP map fetch, scheduled tasks, GC) phải gom về dùng chung; thêm/xóa tenant khi đang chạy |
| Upgrade schema | Một lần cho mỗi shard | Mỗi DB tenant một lần, khi runtime khởi động (đã có sẵn trong code upstream) |

## Đề xuất

**Chọn B** cho shard. Giữ A làm phương án dự phòng nếu benchmark có tải (P00b/P14) cho thấy chi phí theo tenant không chấp nhận được.

Hệ quả nếu được chấp nhận:

1. **P03** đổi phạm vi thành:
   - **catalog tenant của shard**: một SQLite nhỏ chứa `tailnets(id, status, hostnames, revision, epoch)` và `key_inventory`;
   - **vòng đời tạo tailnet**: tạo DB tenant với policy deny và trạng thái PENDING, rồi mới ACTIVE; làm theo saga, vì catalog và DB tenant là hai file khác nhau;
   - policy mặc định deny (P03a);
   - từ chối cấu hình Postgres;
   - thêm, bỏ, nạp lười runtime khi đang chạy.

   Bỏ ID toàn cục và composite FK.
2. **P04–P07** thu hẹp: phần lớn cô lập về state, map, event và policy đã có sẵn theo cấu trúc. Phần còn lại là các tài nguyên **dùng chung trong process** phải được kiểm toán:
   - Prometheus registry (metric không được mang dữ liệu tenant);
   - `http.DefaultTransport.RegisterProtocol` (DERP verify);
   - log;
   - DERP map fetch;
   - các biến toàn cục `envknob`.
3. **P12b** đơn giản hơn nhiều: chuyển file DB, Noise key và mục catalog, rồi fence epoch.
4. PLAN §1 hàng "Storage", ADR 0002 và ticket P03 phải được sửa nếu chấp nhận.

## Câu hỏi mở

- Hạn mức tenant trên mỗi shard dùng chung, theo FD và RAM khi có tải. Chờ P00b.
- Nạp lười (chỉ dựng runtime khi tenant có kết nối) có cần không, nếu 1000 tenant không tải chỉ tốn khoảng 1.7 GiB.
- Gom việc nền dùng chung: DERP map, cập nhật DNS extra records, kiểm tra update.
