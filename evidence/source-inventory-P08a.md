# Source Inventory — P08a (inventory endpoint + RED security tests)

- **Trạng thái:** `Implemented` (chỉ test + tài liệu). Sửa lỗi thuộc P08. Chưa `Reviewed`.
- **Nhánh:** `feat/P08a-endpoint-audit` từ `master` @ `2b3e98d`.
- **Chi tiết:** `evidence/P08a-endpoint-inventory.md` (bảng route, phát hiện, hợp đồng dự kiến, RED/GREEN, mẫu evidence).

## 1. Path/symbol đã đọc

Xem `evidence/P08a-endpoint-inventory.md` §7 "Verified source paths & symbols". Tài liệu: `AGENTS.md` (rule 6, 9, 13), `03-isolation-threat-model.md` (I09, I11, I15, §4), `06-tests-benchmarks.md` (S14, S15), `PLAN.md` v2.0 (P08), ADR 0005 §5 (`serve-tailnets` deprecated).

## 2. Constraints hiện có

- Master không có `tailnet_id`, `CreateTailnet` hay API key gắn tailnet; mỗi runtime một DB. Mọi endpoint đọc toàn DB của runtime.
- Router Noise dựng trong `NoiseUpgradeHandler` (không walk được khi không sửa code). `http.ServeMux` của listener debug không liệt kê được.
- `http.DefaultTransport` là toàn cục: test DERP verify hai tailnet đặt trong package riêng `hscontrol/endpointaudit` để không ảnh hưởng test khác.
- Test upstream `TestHandleVerifyRequest_AdmitsByNodeKey` ghim `expired → Allow=true`, mâu thuẫn hợp đồng mới; P08 phải sửa cả hai.

## 3. Giả định còn mở

- Quy tắc operator debug (loopback + allowlist CIDR tường minh) và hình thức xác thực DERP cho `/verify` (mTLS hoặc secret theo DERP) là đề xuất, chưa có ADR/duyệt.
- Mã trả về cho cross-tailnet API là 404 (không phải 403) để không lộ tailnet tồn tại — đề xuất.
- `serve-tailnets` được dùng làm mô hình hai tenant cho DERP verify vì là mô hình multi-tailnet duy nhất trên master; biến thể cho runtime P05 mô tả trong inventory §5.
