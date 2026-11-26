# S08d: Exit node tương đương Tailscale qua API tenant (E2E client thật)

- **Trạng thái:** `Verified` (2026-11-27) trên binary P14f F3 (`~/binp14f3`, commit `fd6008d`), harness nhánh `feat/P14f-e2e`. Chưa `Reviewed`.
- **Harness:** `infra/lab/e2e/n1/cases6.sh` (`t_s08d`, S08d.0–.6), viết ở `feat/P12d-exitnode` (`bad0889`), sửa topology ở `753b6dd`.

## 1. Lỗi trước đó và nguyên nhân gốc

Lượt chạy trên binary P12d (2026-11-26) và trên binary F3 (2026-11-27, client 1.104.7 và 1.84.3) cho cùng kết quả: S08d.1, .3, .6 PASS; **S08d.2, .4, .5 FAIL**. Mọi đường dữ liệu qua exit node tới "internet giả" 203.0.113.10 và 2001:db8:77::10 đều timeout, kể cả với `--exit-node-allow-lan-access=true`.

Chẩn đoán từ artifact (`logs/S08d-data.out`):
- a1 đã chọn exit node: route tới 10.77.20.1 đi qua `tailscale0`, bảng 52.
- Dịch vụ "internet giả" trên a2 không nhận gói nào từ a1.

Như vậy a2 không chuyển tiếp lưu lượng exit.

**Nguyên nhân: harness đặt sai topology, không phải lỗi sản phẩm.**
- tailscaled trên exit node **không bao giờ chuyển tiếp lưu lượng exit tới các mạng nối trực tiếp với chính nó**. Đây là quy tắc "guest wifi" trong `tailscale.com/ipn/ipnlocal/local.go`: khi quảng bá default route, `localNets` = default route **trừ** route của các interface cục bộ và IP của host (`shrinkDefaultRoute`). Phiên bản kiểm: `tailscale.com@v1.105.0-pre.0.20261112091541-a2da5fbcaa53` dòng ~3629; cùng quy tắc trên client 1.84.3 và 1.104.7.
- Harness cũ gán 203.0.113.1/24 và 2001:db8:77::1/64 lên bridge `x77br` **của chính a2**. Vì vậy a2 coi dải "internet giả" là LAN của nó và bỏ gói. Đây là hành vi giống hệt Tailscale thật.
- Control plane không liên quan:
  - `util.TheInternet` (`hscontrol/util/net.go:41`) gồm 203.0.113.0/24 và 2001:db8::/32;
  - grant `autogroup:internet` biên dịch đúng;
  - exit route được duyệt và tới a1, đã thấy `AllowedIPs` 0.0.0.0/0 và ::/0 trên a1.

## 2. Sửa (harness, `753b6dd`)

"Internet giả" chuyển ra **sau một bước định tuyến**:
- bridge `x77br` trên VM chỉ mang dải transit 10.177.77.0/24 và fd77:77:ff::/64;
- netns `x77inet` (10.177.77.10, fd77:77:ff::10) giữ 203.0.113.10/32 và 2001:db8:77::10/128 trên `lo`;
- a2, b2 và netns của n1-ax1 định tuyến 203.0.113.0/24 và 2001:db8:77::/64 qua `x77inet`;
- địa chỉ SNAT mong đợi đổi theo: a2/b2 = 10.177.77.1 / fd77:77:ff::1, n1-ax1 = 10.177.77.2 / fd77:77:ff::2.

Mọi kiểm tra cô lập và mọi bước dọn dẹp giữ nguyên.

## 3. Kết quả

| Client | Lệnh | Kết quả |
|---|---|---|
| 1.104.7 | `CASES="t_s01 t_s08d" CI_BINDIR=/home/mesh/binp14f3 MODE=on V=1.104.7 bash infra/lab/e2e/n1/run.sh` | **S01, S08d.0–.6, S99 PASS**, 0 FAIL, `cleanup_ok=1`, 520 s |
| 1.84.3 | như trên, `V=1.84.3` | **S01, S08d.0–.6, S99 PASS**, 0 FAIL, `cleanup_ok=1`, 554 s |

Chi tiết (1.104.7):
- **S08d.1:**
  - policy (`tagOwners`, `autoApprovers.exitNode`, grant `autogroup:internet`) được ghi bằng key của A kèm If-Match;
  - chưa duyệt thì không có exit node nào được bật hay được đề xuất;
  - key của B trên route của a2 → 404;
  - duyệt qua API → a1 được đề xuất n1-a2 sau 1 853 ms;
  - b1 không được đề xuất gì.
- **S08d.2:**
  - a1 qua n1-a2: ping 3/3, TCP `INET-A src=10.177.77.1` (SNAT của a2);
  - IPv6: ping 3/3, TCP `src=fd77:77:ff::1`;
  - `allow-lan-access=false`: lưu lượng LAN đi vào tunnel, witness và gateway không trả lời (quy tắc guest wifi);
  - `true`: LAN đi thẳng (witness thấy src=10.77.20.11), internet vẫn qua a2.
- **S08d.3:** policy không có `autogroup:internet` → a1 không chọn được exit node; trả grant về → a1 được đề xuất lại sau 2 933 ms, approval còn giữ.
- **S08d.4:** `autoApprovers.exitNode` với key gắn tag do API cấp: n1-ax1 được bật exit route sau 1 918 ms mà không cần gọi lệnh duyệt; a1 qua n1-ax1 nhận `src=10.177.77.2`.
- **S08d.5 (cô lập):**
  - b1 qua exit node riêng n1-b2 nhận `INET-B` cho **cùng địa chỉ**, không bao giờ `INET-A`;
  - b1 không chọn được exit node của A, theo tên lẫn theo IP;
  - key chéo tenant → 404.
- **S08d.6:** `exit-node list` / `suggest` chỉ hiện exit node của tailnet mình.
- **S08d.0:** dọn qua API của từng tenant, policy khôi phục bằng If-Match.

### 3.1 Client 1.84.3

Cùng kết quả với 1.104.7:
- đề xuất sau khi duyệt: 2 937 ms; đề xuất lại sau khi trả grant: 2 519 ms;
- `autoApprovers.exitNode`: 3 448 ms;
- SNAT: 10.177.77.1 / fd77:77:ff::1 (a2) và 10.177.77.2 (n1-ax1);
- b1 chỉ nhận `INET-B`; `exit-node list` / `suggest` chỉ hiện exit node của tailnet mình.

## 4. Giới hạn

- Exit node không cho truy cập LAN của chính nó (guest wifi), giống Tailscale. Muốn vào LAN đó thì quảng bá thêm subnet route (đã kiểm ở S08b/S08c).
- Artifact không commit (`artifacts/` bị ignore); S99 xác nhận artifact không chứa key hay secret.
