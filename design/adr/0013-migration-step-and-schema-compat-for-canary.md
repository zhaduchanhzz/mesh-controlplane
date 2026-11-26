# ADR 0013 — Upgrade canary theo cluster: migration là bước riêng của operator, binary chấp nhận migration "mở rộng" mới hơn nó

- **Trạng thái:** **Accepted** (2026-11-26). Chủ dự án duyệt toàn bộ khuyến nghị D1–D13 trong `tickets/P13.md` (gói quyết định ngày 2026-11-26).
- **Ticket:** P13d. Sửa luôn L7 (`evidence/REVIEW-2026-11-26-tickets.md`).
- **Liên quan:** ADR 0002 (fresh schema v0.1), ADR 0005 (PG dùng chung), ADR 0006 §4 (migration đi đường trực tiếp), AGENTS rule 11, PLAN §7 N3 ("upgrade có canary tại cấp cluster, nâng standby trước").

## Bối cảnh (đọc code, `evidence/source-inventory-P13.md` §2.3)

- Mọi process gọi `OpenPlatform` **tự migrate** khi khởi động: cluster, `mesh-placement`, mọi lệnh CLI. Không có cờ tắt.
- `checkV01Database` từ chối DB có migration lạ ("unknown migration"), với thông báo như thể DB là của Headscale.
- `mesh-bootstrap` (chỉ đọc) từ chối DB **thiếu** migration mới nhất nó biết, chấp nhận DB mới hơn.
- DB dùng chung cho mọi cluster ⇒ migration là **toàn cục**, không canary theo cluster được.

Hệ quả nếu nâng standby của cluster c2 lên bản N+1 có migration:
1. standby N+1 khởi động → migrate DB ngay (mọi cluster thấy schema N+1);
2. mọi process bản N đang chạy tiếp tục (chỉ kiểm lúc mở DB);
3. **process bản N nào khởi động lại** (failover, crash, restart có kế hoạch, ở bất kỳ cluster nào) bị từ chối → cluster mất standby, failover kế tiếp không có người nhận; rollback về N không thể nếu không restore.
4. Hai process cùng mở một DB chưa migrate: một bên lỗi khóa chính `migrations` hoặc DDL trùng và thoát (L7).

## Quyết định (đề xuất)

1. **Process cluster không migrate.** Ở cluster mode, `OpenPlatform` chỉ kiểm: DB phải chứa mọi migration binary biết (như bootstrap), nếu không → lỗi rõ "DB ở schema X, binary cần Y: chạy `mesh-placement migrate`". CLI admin live cũng không migrate.
2. **Migration chỉ chạy bằng lệnh operator** `mesh-placement migrate` (kết nối trực tiếp tới primary, không qua PgBouncer transaction mode), dưới `pg_advisory_xact_lock` cố định trong transaction của migration: hai lệnh đồng thời thì một bên chờ rồi thấy đã xong (sửa L7). `mesh-placement serve` cũng không tự migrate (cần `--migrate` tường minh hoặc lệnh riêng; chốt ở P13d).
3. **Metadata tương thích:** mỗi migration khai báo `compat`:
   - `expand`: thêm bảng/cột nullable/index/trigger không làm hỏng binary cũ; ghi kèm `min_reader` = migration mới nhất mà binary cũ phải biết.
   - `contract`: đổi/xóa thứ binary cũ dùng; `min_reader` = chính nó.
   Bảng nền tảng mới `schema_compat(migration_id PK, min_reader_id)` được ghi **trong cùng transaction** với migration. Bảng này là migration đầu tiên dùng cơ chế mới; các migration đã commit (9 cái) coi là `contract` (không sửa chúng, AGENTS 11).
4. **Binary bản N chấp nhận DB** khi mọi migration nó không biết đều có `min_reader_id` thuộc tập nó biết. Ngược lại từ chối, thông báo đúng nguyên nhân ("DB đã qua migration contract X; binary quá cũ"). Áp cho cluster, `mesh-placement`, `mesh-bootstrap`.
5. **Thứ tự upgrade chuẩn** (runbook P13d):
   1. backup + kiểm backup (P13c);
   2. `mesh-placement migrate` (chỉ `expand`): mọi binary N chạy tiếp và khởi động lại được;
   3. `mesh-bootstrap`, `mesh-placement` lên N+1;
   4. mỗi cluster: standby → N+1, theo dõi (health, log, metric) → switchover (SIGTERM active, standby N+1 lấy lease) → canary trong khoảng quan sát → active cũ → N+1 làm standby; cluster đầu là canary, các cluster sau chỉ làm khi canary đạt;
   5. `contract` chỉ ở bản phát hành sau, khi mọi binary ≥ N+1.
   Rollback: binary về N được khi DB chỉ có `expand` mới; sau `contract` chỉ restore (kèm fence ADR 0012).
6. `versioncheck` (major/minor) giữ nguyên, chạy trước kiểm tương thích.

## Hệ quả

- Canary thật theo cluster: hai bản binary cùng chạy trên một DB trong cửa sổ upgrade.
- Nhà phát triển phải phân loại mỗi migration; review migration thêm câu hỏi "binary cũ có chạy được không" và một test tự động: binary (gói `db` ở commit trước) mở DB đã có migration `expand` mới.
- Dev/unit test (SQLite) vẫn có thể tự migrate qua một tùy chọn rõ ràng, không bật ở cluster mode.

## Phương án đã cân nhắc

- **Giữ tự migrate, chỉ thêm khóa:** sửa L7 nhưng không cứu được bước 3 ở trên.
- **Chỉ thứ tự "migrate trước, upgrade sau" mà không có metadata:** binary cũ vẫn từ chối migration lạ khi khởi động lại.
- **Cho binary cũ bỏ qua mọi migration lạ:** không an toàn với `contract`.

## Kiểm chứng (RED trước)

- Process cluster mở DB thiếu migration mới nhất → hôm nay migrate (RED) → sau: từ chối với thông báo đúng.
- Binary N mở DB có thêm một migration `expand` thử nghiệm (chỉ trong test) → hôm nay "unknown migration" (RED) → sau: chạy; với migration `contract` → từ chối, thông báo đúng.
- Hai `mesh-placement migrate` đồng thời → cả hai exit 0, migration chạy một lần (L7).
- Lab: drill canary trên c2 với hai build N và N+1 (N+1 có một migration `expand` vô hại chỉ dùng cho drill, không merge vào chuỗi chính), client thật, đo gián đoạn mỗi bước, rollback standby về N.
