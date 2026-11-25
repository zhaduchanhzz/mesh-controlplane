# ADR 0010 — Trao tay tailnet giữa cluster: nguồn ngừng phục vụ rồi mới lật owner

- **Trạng thái:** Chấp nhận. Chủ dự án duyệt ngày 2026-11-25 ("Đồng ý cách trao tay").
- **Ticket:** P12, quyết định Q7 ngày 2026-11-25. Cửa sổ R11 phải loại bỏ ngay trong P12.
- **Liên quan:**
  - ADR 0003: move không tự động.
  - ADR 0005: một Noise key cho mỗi cluster; lease cluster.
  - ADR 0007: runtime chỉ cho tailnet mà cluster sở hữu.
  - P10: CAS owner/epoch, fence.
  - P11: bootstrap trả `MOVING`.
  - P11b: `cluster_leases`.
- Tên file giữ nguyên như bản đề xuất đầu (bảng claim) để link không gãy. Bản đó đã bị thay bằng cơ chế dưới đây; xem §Phương án đã loại.

## Bối cảnh

Ownership của tailnet là cặp (`tailnets.owner_cluster_id`, `owner_epoch`), gọi tắt là (owner, epoch). Từ P10:
- `owner_epoch` chỉ tăng; trigger trong DB chặn giảm.
- Mọi câu ghi dữ liệu tenant bị fence theo (owner, epoch) và theo lease (`tenantscope.checkFence`).

Vì vậy **dữ liệu** không bao giờ có hai owner. **Bộ nhớ** thì khác. Trước ADR này, `MoveOwner` lật owner ngay lập tức. Sau đó mỗi cluster tự reconcile theo ticker riêng mỗi 2 s:
- cluster nguồn retire runtime;
- cluster đích nạp runtime.

Hai việc này độc lập, nên đích có thể nạp trước khi nguồn retire. P12 bước 3 đo được hai bên cùng giữ runtime trong 0–213 ms (R11). Trong khoảng đó, client còn nối với nguồn vẫn nhận netmap từ bộ nhớ của nguồn, trong khi đích đã phục vụ.

## Quyết định

Một lần chuyển sang cluster khác gồm **hai bước**:
1. **Yêu cầu.**
2. **Lật owner.** Chỉ làm sau khi nguồn đã ngừng phục vụ.

Không thêm bảng, không thêm migration. Dùng cột `tailnets.moving_to_cluster_id` có từ P11.

### Vai trò các cột

| Cột | Ý nghĩa |
|---|---|
| `owner_cluster_id`, `owner_epoch` | Quyền sở hữu, **duy nhất** quyết định ai nạp runtime và ai được ghi. Không đổi. |
| `moving_to_cluster_id` | Chỉ là yêu cầu đang chờ. **Không** cho cluster đích quyền gì. |

### 1. Yêu cầu: `Registry.MoveOwner(from, fromEpoch, to)` với `to ≠ from`

- Chữ ký hàm giữ nguyên. Kiểm như cũ: đích đã đăng ký, `ACTIVE`, luật dedicated.
- Ghi bằng CAS:
  ```
  SET moving_to_cluster_id = to
  WHERE owner = from AND epoch = fromEpoch AND moving_to_cluster_id IS NULL
  ```
  Owner và epoch **không đổi**.
- Mỗi tailnet có nhiều nhất một yêu cầu đang chờ. Yêu cầu thứ hai trả `ErrConflict`. Owner/epoch lệch trả `ErrStaleEpoch`.
- `to = from` (tăng epoch trong cùng cluster) vẫn lật ngay như cũ, vì một process tự retire rồi tự nạp, không có hai bên.
- **Hủy yêu cầu:** `Registry.CancelMove(id, epoch)` xóa `moving_to` khi owner/epoch vẫn như lúc đặt.

### 2. Nguồn ngừng phục vụ

- Ở lượt reconcile kế (≤ 2 s), process đang giữ lease của cluster nguồn thấy tailnet của mình có `moving_to`.
- `OwnedTailnets` không còn liệt kê tailnet có `moving_to`, nên cả `reconcile` lẫn `onLeaseAcquired` đều không nạp nó.
- Runtime, nếu có, bị retire theo thứ tự hiện có:
  1. ra khỏi routing;
  2. đánh dấu `retired`;
  3. đóng Noise session;
  4. kết thúc map stream;
  5. drain request đang chạy.

### 3. Lật owner: `Platform.HandOver(id, epoch)`

Chạy **sau khi** retire xong. Nếu tailnet không có runtime (không `ACTIVE`, hoặc chưa nạp được) thì chạy ngay. Một transaction, dưới `LockOwnership`:

```
SET owner_cluster_id = moving_to, owner_epoch = owner_epoch + 1, moving_to_cluster_id = NULL
WHERE owner = mình AND epoch = E AND moving_to IS NOT NULL
  AND lease của cluster mình, đúng holder và epoch lease của process này, còn sống theo đồng hồ DB
```

- Process mất lease giữa chừng thì không lật được. Standby của cùng cluster, khi lên, làm tiếp bước 2–3.
- Drain quá `runtimeDrainTimeout`: vẫn lật. Runtime đã `retired` (mọi request mới bị từ chối) và session đã đóng.

### 4. Nguồn chết hẳn: `Platform.TakeOver(id, from, epoch)`

- Điều kiện: cluster nguồn không còn lease sống, nên không process nào của nó phục vụ.
- Process đang giữ lease của cluster đích lật bằng cùng câu CAS, kèm điều kiện thêm: **không có** dòng `cluster_leases` còn hạn của nguồn.
- Nếu standby của nguồn lên cùng lúc và cũng lật, hai bên CAS cùng (owner, epoch, moving_to), nên chỉ một bên thắng.

### 5. Đích nạp

Đích nạp tailnet ở lượt reconcile kế, khi `owner_cluster_id = mình`. Tới lúc đó nguồn chắc chắn đã ngừng phục vụ: thứ tự ghi trong DB bảo đảm điều này, không dựa vào thời gian.

### 6. Bootstrap

Trả `MOVING` (503) trong khi `moving_to` được đặt; P11 đã có sẵn nhánh này. Sau khi lật, trả URL của đích.

### 7. Báo cáo reconcile

Thêm **R10_MOVE_PENDING_TOO_LONG**: yêu cầu chuyển tồn tại quá `Config.MoveMaxAge` (mặc định 5 phút). Tình huống điển hình: nguồn treo nhưng vẫn giữ lease. Operator chọn hủy hoặc xử lý cluster nguồn.

## Bất biến

1. Tại mọi thời điểm, nhiều nhất một cluster nạp runtime của một tailnet. Chỉ cluster bằng `owner_cluster_id` được nạp, và owner chỉ đổi sau khi runtime của owner cũ đã ngừng, hoặc khi owner cũ không còn lease sống.
2. `owner_epoch` chỉ tăng; mỗi epoch ứng với đúng một owner. Chuyển qua lại nhiều lần chỉ làm epoch tăng tiếp.
3. Ghi dữ liệu tenant vẫn bị fence theo (owner, epoch, lease) như P10b/P11b.

## Hệ quả

**Tốt:**
- R11 trong bộ nhớ = 0.
- Không đổi schema.
- Client trong lúc chuyển nhận `MOVING` đúng ngữ nghĩa.
- P12b dùng lại nguyên cơ chế, chỉ thêm thông báo, re-bootstrap và đổi ControlURL.

**Chi phí:**
- **Move ≈ 2–4 s.** Nguồn mất ≤ 2 s để thấy yêu cầu, cộng thời gian drain; đích mất ≤ 2 s tới lượt reconcile kế. Trong khoảng này không cluster nào phục vụ tailnet: client của nguồn bị cắt stream, client mới nhận `MOVING` và thử lại.
- **Nguồn treo nhưng vẫn giữ lease:** move kẹt cho tới khi nguồn mất lease, hoặc operator hủy. R10 làm việc này lộ ra.
- **Đường lật trực tiếp:** từ khi gộp `feat/P12-cli` (2026-11-25), CLI `reassign` đi qua `Registry.MoveOwner` và theo cơ chế trao tay. `Platform.ReassignTailnet` (lật thẳng) không còn được code sản phẩm gọi; chỉ test dùng.

## Phương án đã loại

- **Bảng claim `tailnet_serving`** (bản đề xuất đầu của ADR này): cùng bất biến, nhưng cần bảng và migration mới, thêm một câu ghi mỗi lần nạp/retire, và client không nhận được `MOVING`.
- **Rút `reconcileInterval`:** chỉ thu nhỏ cửa sổ, không loại bỏ.
- **Đích chờ một khoảng cố định:** dựa vào thời gian.
- **LISTEN/NOTIFY:** không có trên SQLite; không dùng được qua PgBouncer transaction mode.

## Kiểm chứng

- **Servertest** `TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`:
  - lấy mẫu mỗi 5 ms; không mẫu nào thấy hai cluster cùng giữ runtime (đã RED: 3/6 lần move có chồng lấn);
  - sau khi lật, ghi qua runtime cũ của nguồn bị `ErrFenced`;
  - epoch tăng đúng 1 mỗi lần move.
- **Servertest nguồn chết:** dừng mọi process của nguồn sau khi đã yêu cầu chuyển; đích tự lật sau khi lease nguồn hết hạn rồi nạp.
- **Unit (SQLite + PostgreSQL):**
  - `MoveOwner` chỉ đặt `moving_to`; yêu cầu thứ hai `ErrConflict`; `CancelMove`;
  - `HandOver` cần lease sống đúng holder;
  - `TakeOver` bị từ chối khi lease nguồn còn sống;
  - CAS đồng thời: chỉ một bên thắng;
  - bootstrap trả `MOVING` rồi URL đích;
  - R10 trong báo cáo reconcile.
- **E2E P12 bước 7:** chuyển tailnet giữa c1 và c2 với client thật; log thời điểm retire của nguồn < thời điểm nạp của đích.
