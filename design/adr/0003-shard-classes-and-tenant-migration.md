# ADR 0003 — Lớp shard và tenant migration

- **Trạng thái:** Proposed. **Một phần đã bị thay** bởi [ADR 0005](0005-cluster-architecture-shared-postgres-bootstrap-incremental-netmap.md): move không copy dữ liệu (đổi `owner_cluster_id`/`owner_epoch` trong PostgreSQL dùng chung); Noise key không đi cùng tenant (mỗi cluster một key), nên client re-bootstrap, đổi ControlURL và đăng nhập lại; bước 3–4 (pre-copy, read-replica) không còn cần. Lớp shard và quota giữ nguyên; shard = cluster.
- **Ngày:** 2026-11-22
- **Người quyết định:** chủ dự án (hướng đã chọn); chờ số đo P00b và review bảo mật
- **Liên quan:** PLAN §0, §1, §6; tickets P00b, P10, P12b, P14, P16; ADR 0001, 0002

## Bối cảnh

- Mục tiêu là multi-tenant. Tailnet lớn nhất dự kiến 1k–10k thiết bị, nên một process phải chịu được 10k thiết bị cho một tenant.
- Chi phí peer theo cặp là O(n²) (`BuildPeerMap`, `hscontrol/policy/v2/policy.go:831-939`); với `allow_all` ở 10k node có 99.990.000 directed peer entries (`hscontrol/servertest/README.md`). Bộ nhớ ở 5k/10k hiện chỉ là ước tính (inventory P00 §5), **chưa đo**.
- Tenant tăng trưởng theo thời gian; tenant trên shard dùng chung có thể vượt ngưỡng và ảnh hưởng tenant khác (I10).
- Bản kế hoạch 1.0 để migration ngoài MVP. Chủ dự án muốn có migration khi tenant vượt ngưỡng.
- Upstream có state chỉ nằm trong bộ nhớ: `authCache`, `sshCheckAuth`, `pings`, ephemeral GC timers (`hscontrol/state/state.go:214-300`).

## Quyết định

### Lớp shard

| Lớp | Tenant | Giới hạn |
|---|---|---|
| *shared* | nhiều tenant | hard quota **5.000 thiết bị mỗi tenant**; cảnh báo ở 80% (4.000) |
| *dedicated* | một tenant | 5.000–10.000 thiết bị |

- Đạt hard quota: enrollment mới bị từ chối với lỗi rõ ràng (fail closed), không ảnh hưởng tenant khác.
- Tổng ngân sách thiết bị của một shard *shared* **chưa chốt**; chốt sau P00b/P14.
- Placement registry lưu lớp shard và đếm thiết bị theo tenant (P10).

### Tenant migration: một cơ chế, làm tăng dần

| Bước | Mô tả | Thuộc |
|---|---|---|
| 1. Fenced move (cold) | `MOVING`, fence nguồn bằng epoch, copy dữ liệu theo `tailnet_id`, đích kiểm `key_inventory` + revision, activate ở epoch mới | MVP (P12b) |
| 2. Rolling drain | Nguồn đóng stream theo đợt có giới hạn tốc độ để client reconnect sang đích | MVP (P12b) |
| 3. Pre-copy | Copy trước phần lớn dữ liệu trong khi nguồn vẫn ACTIVE, rồi fence và copy phần chênh theo revision log | Sau khi đo |
| 4. Read-replica trong lúc drain | Nguồn phục vụ netmap chỉ-đọc cho stream chưa drain, không mutation | Sau khi đo |

- Zero-staleness live migration giữ ở **P16**.
- State trong bộ nhớ (`authCache`, `sshCheckAuth`, `pings`, ephemeral GC timers) **không** được copy: request đang dở bị từ chối hoặc reset; timers dựng lại ở đích từ DB.
- Schema v0.1 phải có `key_inventory` và revision/event log theo tailnet (ADR 0002).
- Trong MVP, move do operator khởi động và được duyệt; không tự động.
- **Phương án "chuyển hàng xóm":** khi một tenant tăng trên shard *shared*, có thể chuyển các tenant nhỏ khác ra khỏi shard thay vì chuyển tenant lớn. Dùng chung cơ chế; chỉ khác lựa chọn tailnet.

## Phương án thay thế

1. **Một lớp shard duy nhất.** Đơn giản hơn nhưng tenant 10k và tenant nhỏ cạnh tranh tài nguyên; khó đặt quota công bằng.
2. **Chia một tailnet qua nhiều shard.** Ngoài mục tiêu (PLAN §0): netmap cần toàn bộ peer của tailnet, đòi đồng bộ giữa shard.
3. **Placement bất biến, không migration trong MVP** (bản 1.0). Bị loại theo yêu cầu chủ dự án; tenant vượt ngưỡng sẽ không có lối ra.
4. **Làm live migration zero-staleness ngay.** Bị loại cho MVP: rủi ro fencing cao, cần số đo trước.
5. **Chuyển cả VM bằng Proxmox live migration.** Không giải quyết placement theo tenant; chỉ là underlay.

## Hệ quả

- Cần đo trước: P00b quyết định 10k trong một process có khả thi không. Nếu không, lớp *dedicated* và ADR này phải xem lại.
- P12b thêm vào đường găng N2; ước lượng N2 tăng (PLAN §11).
- Trong lúc move: control plane không nhận mutation; client có thể thấy netmap cũ; pending auth phải làm lại. Ảnh hưởng data plane phải **đo**, không tuyên bố.
- Fencing bằng epoch là điểm bảo mật chính: lỗi owner kép là BLOCK MERGE (I12, I14).
- Noise key theo tailnet phải đi cùng tenant khi move; private key truyền qua kênh mTLS và không ghi log.
- Placement registry cần thêm lớp shard, quota và trạng thái move; operator API cần `BeginMove`/`AbortMove` có audit.
