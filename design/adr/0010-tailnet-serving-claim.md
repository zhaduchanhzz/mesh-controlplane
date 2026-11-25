# ADR 0010 — Claim phục vụ tailnet: không hai cluster cùng giữ runtime của một tailnet

- **Trạng thái:** Đề xuất (P12, 2026-11-25). Chờ chủ dự án duyệt trước khi cài.
- **Bối cảnh quyết định:** chủ dự án trả lời Q7 ngày 2026-11-25. Cửa sổ R11 không chấp nhận được và phải loại bỏ ngay trong P12, không đẩy sang P12b.
- **Liên quan:**
  - ADR 0003: move không tự động.
  - ADR 0005: một Noise key cho mỗi cluster; lease cluster; failover không đổi `owner_epoch`.
  - ADR 0007: runtime chỉ cho tailnet mà cluster sở hữu.
  - P11b: `cluster_leases`.

## Bối cảnh

Ownership của tailnet nằm ở `tailnets.owner_cluster_id` / `owner_epoch` (P10).

- Mọi lệnh ghi dữ liệu tenant đều bị fence theo owner+epoch và theo lease (`tenantscope.checkFence`), nên **dữ liệu** không bao giờ có hai owner.
- Còn **bộ nhớ** thì mỗi cluster tự reconcile theo ticker riêng, `reconcileInterval = 2 s`:
  - cluster nguồn retire runtime khi thấy mình không còn sở hữu tailnet;
  - cluster đích nạp runtime khi thấy mình đã sở hữu.
- Hai sự kiện đó độc lập nhau. Đích có thể nạp **trước** khi nguồn retire.

**Số đo P12 bước 3** (`evidence/P12-placement-reconcile.md`): sau `MoveOwner`, hai cluster cùng giữ runtime trong **0–213 ms** (12 lần đo). Giới hạn trên theo thiết kế ≈ 2 s cộng thời gian nạp.

**Hệ quả trong cửa sổ đó:**
- Client còn nối với nguồn vẫn nhận MapResponse từ bộ nhớ của nguồn, trong khi đích đã phục vụ.
- Nguồn không ghi được, nhưng vẫn đọc và đẩy netmap cũ.

## Quyết định

**1. Bảng mới `tailnet_serving`** (bảng platform, không phải bảng tenant):
- Một dòng cho mỗi tailnet **đang có runtime ở một cluster nào đó**.
- Khóa chính `tailnet_id`, nên mỗi tailnet có nhiều nhất một cluster phục vụ.

```
tailnet_serving(
  tailnet_id  text PRIMARY KEY,
  cluster_id  text NOT NULL,
  owner_epoch bigint NOT NULL,   -- epoch mà runtime được nạp
  holder      text NOT NULL,     -- process (lease holder) đang giữ
  updated_at  timestamp NOT NULL
)
```

- Migration mới thêm vào **cuối** chuỗi, chạy trong transaction, không có FK (giống `cluster_leases`).
- `schema.sql` và `schema_postgres.sql` được cập nhật song song.

**2. Claim trước khi nạp.** `load()` (gọi từ reconcile và từ `onLeaseAcquired`) chỉ dựng runtime sau khi claim thành công. Claim là **một câu lệnh** upsert có điều kiện. Nó thành công khi:
- chưa có dòng nào cho tailnet; hoặc
- dòng hiện có thuộc chính cluster này (process khác cùng cluster sau failover, hoặc epoch mới của cùng cluster); hoặc
- cluster đang giữ dòng **không còn lease sống**, theo đồng hồ của DB (process đã chết hẳn; TTL lease bảo đảm nó không còn phục vụ).

Nếu claim không thành công, tailnet chưa được nạp ở lượt này. Log ghi "đợi cluster X nhả", và lượt reconcile sau thử lại.

**3. Nhả sau khi đã ngừng phục vụ.** `retire()` xóa dòng claim sau khi runtime đã:
- ra khỏi routing (request mới bị từ chối);
- đóng các Noise session;
- kết thúc mọi map stream (`clientStreamsOpen.Wait`).

Câu xóa có điều kiện `cluster_id = mình AND owner_epoch = epoch của runtime`, nên không xóa nhầm claim mới của chính cluster sau khi tăng epoch.

Nếu drain quá `runtimeDrainTimeout`, vẫn nhả claim. Lý do: runtime đã bị đánh dấu `retired` (mọi request mới bị từ chối) và session đã đóng; giữ claim mãi sẽ khóa tailnet.

**4. Lease mất** (`onLeaseLost`): retire mọi runtime, nên nhả mọi claim.
- Process chết đột ngột không kịp nhả: claim của nó hết hiệu lực khi lease cluster hết hạn (TTL P11b, ~10 s).
- Standby của **cùng cluster** claim lại ngay, vì cùng `cluster_id`.

**5. Báo cáo reconcile:** thêm mục R10 (thay ý nghĩa cũ của R10 trong inventory, vốn chưa có đường ghi). R10 báo claim thuộc cluster không còn sở hữu tailnet, hoặc không còn lease sống.

## Hệ quả

**Tốt:**
- Tại mọi thời điểm, nhiều nhất một cluster giữ runtime của một tailnet. Bất biến này do khóa chính trong DB đảm bảo, không dựa vào thời gian.
- R11 trong bộ nhớ = 0.
- Failover trong một cluster không chậm thêm, vì cùng `cluster_id` claim lại ngay.

**Chi phí:**
- **Move chậm hơn.** Đích chỉ nạp sau khi nguồn nhả:
  - nguồn mất ≤ 2 s để thấy mình mất ownership, cộng thời gian drain;
  - đích mất thêm ≤ 2 s tới lượt reconcile kế.
  - Tổng ≈ 2–4 s, trước đây ≈ 2 s.
  - Trong khoảng này **không cluster nào** phục vụ tailnet: client đang nối với nguồn bị cắt, client tới đích nhận lỗi tạm thời và thử lại. Đây là đánh đổi có chủ đích: thà gián đoạn ngắn còn hơn hai owner.
- **Nếu process nguồn treo nhưng vẫn giữ lease** (renew được, không reconcile được), đích bị chặn cho tới khi nguồn tự fence hoặc mất lease. R10 trong báo cáo reconcile làm việc này lộ ra cho operator.
- **Mỗi lần nạp/retire thêm một câu ghi** vào bảng platform. Không nằm trên đường nóng (map, register).

**Không đổi:**
- Fence dữ liệu.
- `owner_epoch`.
- Lease cluster.
- Bootstrap.
- Một Noise key mỗi cluster (ADR 0005, AGENTS 12).

## Phương án đã loại

- **Rút `reconcileInterval` xuống ngắn (ví dụ 200 ms):** chỉ thu nhỏ cửa sổ, không loại bỏ; tăng tải đọc DB trên mọi cluster.
- **Đích chờ một khoảng cố định sau `MoveOwner`:** dựa vào thời gian, không bảo đảm khi nguồn chậm.
- **LISTEN/NOTIFY của PostgreSQL:** không có trên SQLite; qua PgBouncer transaction mode không dùng được LISTEN. Ngoài ra vẫn cần một bất biến trong DB.
- **Dùng `moving_to_cluster_id` (MOVING) làm bước bàn giao:** thuộc thủ tục move có kế hoạch của P12b (thông báo, re-bootstrap). Claim là bất biến độc lập, P12b dùng lại được.

## Kiểm chứng

- **Test RED** `TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`: lấy mẫu mỗi 5 ms; không lần lấy mẫu nào được thấy hai cluster cùng giữ runtime. Test cũ cho thấy chồng lấn ở 6/12 lần move.
- **Unit test claim** trên SQLite và PostgreSQL:
  - chiếm khi đã có người giữ thì bị từ chối;
  - nhả xong thì chiếm được;
  - lease của người giữ hết hạn thì chiếm được;
  - cùng cluster thì claim lại được;
  - xóa với epoch cũ thì không xóa claim mới.
- **Test khi nguồn drain chậm** (một request treo): đích không nạp cho tới khi nguồn nhả.
- **E2E P12** (bước 7): `MoveOwner` giữa c1 và c2 với client thật. Log hai cluster cho thấy thời điểm retire của nguồn < thời điểm nạp của đích.
