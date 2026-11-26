# ADR 0014 — Xoay Noise key của cluster: xoay "lạnh" qua switchover, không chồng lấn hai key

- **Trạng thái:** **Proposed** (2026-11-26). Chờ chủ dự án chọn: quyết định D8 trong `tickets/P13.md`. **Không đổi mô hình Noise** (AGENTS rule 12, ADR 0005): vẫn một key cho mỗi cluster, `/key` trả một key.
- **Ticket:** P13e.
- **Liên quan:** ADR 0005 (Bổ sung quyết định §5: lộ/xoay key ảnh hưởng mọi tenant của cluster), P12 (registry ghi Noise key, lệch thì fail closed), threat model `03-isolation-threat-model.md:84, 103`, `evidence/P02-noise-client-research.md` Q1.

## Bối cảnh

Code hiện tại (`evidence/source-inventory-P13.md` §2.4):
- key nạp một lần lúc khởi động (`noise.private_key_path`); file thiếu thì **tự tạo key mới**;
- registry `clusters.noise_public_key`; lệch → process không nhận lease (ở lại standby, thử lại); chỉ kiểm **lúc lấy lease**;
- `PATCH /v1/clusters/{id}` ghi đè key trong registry, không kiểm định dạng;
- không có key kế tiếp, không chồng lấn.

Client (P02 Q1, đọc code): `/key` được lấy lại ở mỗi `doLogin`, nhưng Noise client được tạo một lần trong `Direct` với key cũ và chỉ bị bỏ khi `Direct` đóng (restart `tailscaled`, đổi profile). Hành vi thật khi handshake thất bại (client có tự tạo lại `Direct` không) **chưa đo**. Node key và machine key của client không đổi khi key server đổi, nên dự kiến **không** phải đăng nhập lại, chỉ có thể phải restart `tailscaled`. Ngoài ra client **không** enforce TLS của `/ts2021` (P02 dòng 112): bảo vệ duy nhất của phiên là Noise key server.

## Quyết định (đề xuất)

### 1. Xoay có kế hoạch (runbook, không cần đổi mô hình)

1. Thông báo admin của **mọi** tenant trên cluster (danh sách từ registry), kèm cửa sổ thời gian và hướng dẫn restart client nếu cần.
2. Sinh key mới trên VM standby: `umask 077; headscale generate private-key > noise_private.key.next` (không in ra terminal/log); chép sang VM active qua pipe ssh.
3. Dừng standby; thay file key; **PATCH registry** sang public key mới (qua lệnh CLI mới có kiểm định dạng `mkey:`, xem §3).
4. Khởi động standby (chưa lấy được lease vì active đang giữ).
5. Switchover: SIGTERM active → nhả lease → standby lấy lease, `RecordNoiseKey` khớp key mới → phục vụ. Process cũ thay key rồi chạy lại làm standby.
6. Theo dõi tỉ lệ client nối lại theo thời gian (1, 5, 15, 60 phút) theo phiên bản client; sau ngưỡng thì yêu cầu restart `tailscaled` cho client còn lại.

### 2. Xoay khẩn cấp (key bị lộ)

- Làm ngay §1 bước 2–5, bỏ qua cửa sổ thông báo; thông báo admin mọi tenant **cùng lúc** với xoay, nêu: key cũ bị coi là lộ, client chưa restart vẫn tin key cũ và có thể bị giả control server nếu kẻ tấn công chặn được đường mạng (TLS của `/ts2021` không cứu được), nên phải restart `tailscaled` sớm nhất.
- Kiểm tra nguồn lộ: quyền file key trên hai VM, ai đọc được, log; xoay luôn secret `/verify` của DERP cluster đó và cert TLS nếu VM bị chiếm.
- Không "thu hồi" được key cũ phía client: không có danh sách thu hồi trong giao thức. Ghi rõ là rủi ro còn lại.

### 3. Thay đổi code nhỏ (không đổi mô hình)

- CLI `headscale cluster rotate-noise-key --cluster <id> --public-key mkey:…` (hoặc qua `mesh-placement`), kiểm định dạng, ghi audit; `PATCH` kiểm định dạng `mkey:` (hôm nay chỉ `maxLength`).
- Active **kiểm lại** registry key mỗi chu kỳ reconcile (hiện mỗi 2 s): lệch → nhả lease (giống bước 5, nhưng tự động). Lý do: sau xoay khẩn cấp, một active cũ còn giữ key lộ không được tiếp tục phục vụ cho tới lần takeover sau.
- File key thiếu ở cluster mode khi registry đã có key → **không tự tạo** key mới; lỗi rõ ràng (hôm nay tạo key mới rồi bị chặn ở lease, an toàn nhưng khó chẩn đoán).

## Hệ quả

- Phạm vi ảnh hưởng: mọi client của mọi tenant trên cluster mất control plane cho tới khi nối lại; data plane chạy tiếp theo netmap cuối (giống failover). Có thể cần restart `tailscaled` từng client.
- Không có xoay "nóng". Muốn chồng lấn hai key (server thử giải mã handshake bằng cả key cũ và key mới trong một khoảng) là **đổi mô hình Noise**: cần ADR riêng do chủ dự án mở, kèm log protocol thật (AGENTS 12). ADR này không đề xuất.

## Phương án đã loại

- **Hai key chồng lấn / trial-decrypt:** đổi mô hình Noise (AGENTS 12, PLAN §1).
- **Xoay bằng chuyển mọi tenant sang cluster khác (P12b):** client phải đổi ControlURL và đăng nhập lại; nặng hơn xoay lạnh.
- **Sửa registry khi active đang chạy mà không switchover:** không có hiệu lực tới lần takeover sau (code hôm nay); dễ hiểu sai là đã xoay.

## Kiểm chứng

- Unit/servertest (RED trước): sửa registry khi active giữ lease → active nhả lease trong ≤ 1 chu kỳ reconcile (hôm nay: không); file key thiếu khi registry có key → lỗi rõ, không tạo key; `PATCH` key sai định dạng → 400.
- Drill lab trên c2 (mc-c2 + mc-c2b, client 1.104.7 và 1.84.3, ít nhất 2 tenant): đo thời gian từ switchover tới ping/`tailscale status` phục hồi, tỉ lệ tự phục hồi không cần restart, có phải đăng nhập lại không (dự kiến không), NodeID/IP giữ nguyên; client tenant khác cluster không bị ảnh hưởng.
