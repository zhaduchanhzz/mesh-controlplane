# ADR 0012 — Epoch không bao giờ lùi sau restore hoặc promote mất dữ liệu: epoch floor theo thời gian + phát hiện trong process

- **Trạng thái:** **Proposed** (2026-11-26). Chờ chủ dự án chọn: quyết định D4 trong `tickets/P13.md`.
- **Ticket:** P13c (backup/restore, F08); dùng lại ở P13a (promote cưỡng bức).
- **Liên quan:** ADR 0005 Bổ sung 2 §2, ADR 0010, ADR 0011 §B, PLAN §4 ("`owner_epoch` tăng đơn điệu, không bao giờ giảm, kể cả sau restore; giữ high-water mark ngoài backup"), `05-data-api-contract.md:110`, threat model I14, `06-tests-benchmarks.md` F08.

## Bối cảnh

- Trigger `trg_cluster_leases_epoch_monotonic` và `trg_tailnets_owner_epoch_monotonic` chỉ chặn **UPDATE** làm giảm epoch. Restore (vật lý hay logic) thay toàn bộ dữ liệu: trigger không chạy, epoch về giá trị lúc backup.
- `DELETE` một dòng `cluster_leases` rồi `Acquire` → dòng mới epoch **1**. Không có đường code production nào xóa dòng lease, nhưng DB không chặn.
- Process không nhớ epoch đã thấy: reconcile retire và **nạp lại** runtime khi `owner_epoch` khác (kể cả nhỏ hơn) (`hscontrol/cluster.go:464`); `lease.Manager` không so epoch mới với epoch cũ.
- Hệ quả của restore về thời điểm T (trước một failover và một move, đúng kịch bản F08):
  - lease của cluster về (H₁, e): epoch e+1 đã cấp cho H₂ sẽ được cấp **lần nữa** cho holder khác;
  - tenant đã chuyển A → B ở `owner_epoch` E+1 quay về (A, E): cluster A nạp lại tenant ở E; move kế tiếp cấp E+1 lần hai.
- Fence ở DB vẫn chặn ghi kép (so với DB đang chạy), nhưng epoch như token đơn điệu bị phá; mọi thứ bên ngoài DB đã thấy e+1, E+1 (log, audit, process còn sống, API operator mang epoch, idempotency key) có thể khớp nhầm với một quyền mới.

## Quyết định (đề xuất)

### 1. Epoch fence theo thời gian (bắt buộc sau mọi restore và mọi promote có thể mất commit)

Một thao tác vận hành, chạy **trước khi** PgBouncer mở lại cho process cluster, qua kết nối trực tiếp (ADR 0006 §4), trong **một transaction**:

```
F = max( floor_s(now_db, now_operator) × 2^20 ,  max(mọi epoch hiện có) + 1 ,  HWM_ngoài + 1 )
UPDATE cluster_leases SET epoch = GREATEST(epoch + 1, F), holder = 'fence:<ts>', expires_at = now();
UPDATE tailnets       SET owner_epoch = GREATEST(owner_epoch + 1, F);
-- move đang chờ (tailnet_moves / moving_to) bị hủy: chúng mang epoch cũ, CAS của chúng sẽ thất bại
INSERT INTO platform_epoch_fences(floor, fenced_at, reason, operator) VALUES (F, now(), …);
```

- `floor_s` là số giây Unix. Mọi epoch cấp ra sau một fence lúc t₀ có dạng `F₀ + k` với k = số lần takeover/move kể từ đó. Fence lúc t₁ > t₀ cho `F₁ − F₀ = (t₁ − t₀) × 2^20` ≥ 1 048 576 × số giây: lớn hơn k trừ khi có hơn một triệu takeover/move mỗi giây. Vì vậy **không cần biết** backup cũ bao nhiêu hay mất bao nhiêu commit.
- Lấy `max` của đồng hồ DB và đồng hồ máy operator, và của HWM ngoài nếu có, để một đồng hồ bị lùi không làm floor lùi.
- Giá trị: năm 2026 ≈ 1,8·10⁹ s × 2^20 ≈ 1,9·10¹⁵ < 2^53 (9·10¹⁵): an toàn với JSON/JS và với `CHECK`/kiểu `bigint`/`uint64` hiện có; đủ tới khoảng năm 2240.
- Holder đổi thành `fence:<ts>` và `expires_at = now()`: `Renew` của holder cũ thất bại (holder/epoch khác), active tự fence, rồi một process lấy lease ở `F + 1`. Mọi runtime thấy `owner_epoch` khác, retire rồi nạp lại ở epoch mới: cache trong bộ nhớ (policy revision, NodeStore) được dựng lại từ DB đã restore.
- Hai trigger đơn điệu cho phép các UPDATE này (epoch tăng, holder đổi với epoch tăng).

### 2. Chặn xóa dòng lease

Migration mới (nối cuối, trong transaction, SQLite + PostgreSQL): trigger `BEFORE DELETE ON cluster_leases` → lỗi. Test hiện có xóa dòng lease để dựng tình huống phải đổi sang `UPDATE` hoặc dùng DB mới.

### 3. Phát hiện trong process (lớp phòng thủ thứ hai, high-water mark ngoài backup)

- `lease.Manager` nhớ epoch lease lớn nhất đã thấy cho cluster của nó (bộ nhớ) và ghi vào file `cluster_lease.epoch_hwm_path` (tùy chọn, 0600, chỉ ghi khi epoch đổi, tức hiếm). Thấy epoch trong DB **nhỏ hơn** HWM → log `CRITICAL epoch regression`, metric, **không acquire, không phục vụ** (fail closed) cho tới khi DB có epoch > HWM (tức sau fence).
- Reconcile của cluster: `owner_epoch < rt.epoch` → retire và **không nạp lại** tenant đó cho tới khi `owner_epoch` > HWM của tenant (bộ nhớ; nếu có file HWM thì cả file).
- Đây là "high-water mark ngoài backup" của PLAN §4 và contract §110: nằm trên đĩa của VM cluster, không nằm trong backup PG.

### 4. Khi nào phải fence

| Sự kiện | Fence |
|---|---|
| Restore (PITR, full, logic) | **Bắt buộc**, trước khi `RESUME`/bật lại database trong PgBouncer |
| Promote cưỡng bức node không nằm trong `/sync` (ADR 0011 §B), DR từ backup | **Bắt buộc** |
| Failover Patroni thường (node trong `/sync`) | Không bắt buộc. F05 phải chứng minh epoch không lùi. Tùy chọn (D4b): callback `on_role_change` chạy fence tự động, đổi lại mỗi failover PG làm mọi cluster nạp lại mọi tailnet |

Runbook restore: `PAUSE`/`DISABLE` database trên cả hai PgBouncer → restore → fence → kiểm (`SELECT max(epoch)…` > HWM ghi trước restore) → `RESUME`. Process nào lỡ nối trước fence bị lớp §3 chặn nếu nó còn sống từ trước restore.

## Hệ quả

- F08 đạt mà không phải ghi HWM đồng bộ ra hệ thống ngoài ở mỗi takeover/move (không thêm phụ thuộc trên đường nóng).
- Sau fence, mọi cluster mất lease một lần và nạp lại mọi tailnet: client reconnect, **không** đăng nhập lại (Noise key, node key không đổi). Đo trong F08.
- Restore vẫn làm mất mọi thay đổi sau điểm backup (node đăng ký mới, move đã xong): runbook ghi rằng operator phải làm lại move; client đã đăng nhập lại ở cluster đích sau điểm backup phải đăng nhập lại.
- Epoch nhảy từ số nhỏ lên ~1,9·10¹⁵ ở lần fence đầu: log/metric/hiển thị phải chịu được số lớn (kiểm trong P13c).

## Phương án đã cân nhắc

- **HWM ngoài DB ghi đồng bộ ở mỗi takeover/move** (file trên kho backup, etcd…): đúng nhưng thêm một hệ thống phải sẵn sàng trên đường lấy lease; loại, chỉ dùng HWM ngoài như đầu vào tùy chọn của `max`.
- **Cộng một hằng số lớn** (ví dụ +10⁶) khi restore: phải đoán số lần tăng bị mất; theo thời gian thì không phải đoán.
- **Era/generation** (cột riêng, epoch so theo cặp): đúng nhưng đổi mọi fence và mọi API mang epoch; loại.
- **Chỉ phát hiện trong process (§3), không fence:** không cứu được trường hợp mọi process đã khởi động lại sau restore.
- **Chặn bằng timeline/system identifier của PG** (process từ chối khi timeline đổi mà chưa fence): cần quyền `pg_control_checkpoint()` cho role ứng dụng và đổi theo mỗi failover Patroni; để mở, xem lại nếu §3 không đủ.

## Kiểm chứng (RED trước)

- PG: chụp DB (template), tăng lease (takeover) và `owner_epoch` (move), "restore" bằng DB chụp: `lease.Manager` có HWM cũ phải từ chối (hôm nay: acquire ở epoch thấp hơn — RED); sau fence: mọi epoch > HWM, acquire được, ghi của holder trước restore `ErrFenced`.
- Fence: idempotent theo nghĩa đơn điệu (chạy hai lần vẫn tăng); move đang chờ bị hủy; trigger cho phép.
- `DELETE FROM cluster_leases` → lỗi (SQLite + PG).
- Reconcile: `owner_epoch` nhỏ hơn runtime → retire, không nạp lại.
- E2E F08 trên lab: backup → F01 → move → restore về trước hai việc đó → fence → không cluster/process nào nhận lại quyền cũ; chuỗi epoch đo được.
