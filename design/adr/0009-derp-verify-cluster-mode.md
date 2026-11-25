# ADR 0009 — DERP relay ngoài cho cluster mode: một region mỗi cluster, verify theo cluster

- **Trạng thái:** Đề xuất (P09, 2026-11-26). Chờ chủ dự án duyệt trước khi cài.
- **Ticket:** P09 (Gate N1, S15). Quyết định Q1 ngày 2026-11-25: relay riêng `mc-relay` (VMID 7714, 10.77.40.30) trên underlay `mcsvc`.
- **Liên quan:**
  - ADR 0005: một Noise key cho mỗi cluster; lease active/standby.
  - ADR 0007: runtime chỉ cho tailnet mà cluster sở hữu.
  - ADR 0010: trao tay tailnet.
  - `evidence/P08a-endpoint-inventory.md` F2 và §4.2.
  - Threat model I11 (`03-isolation-threat-model.md:38, 69`).

## Bối cảnh

**Hiện trạng code:**
- Cluster mode từ chối DERP nhúng (`errClusterEmbeddedDERP`, `hscontrol/cluster.go:43`), vì một DERP nhúng sẽ relay cho mọi tailnet của cluster.
- Cluster mode chưa có `/verify`.

**Hiện trạng lab:** lab đang dùng **DERP công cộng của Tailscale** (`derp.urls: controlplane.tailscale.com/derpmap/default`). Vì vậy:
- S15 không kiểm được: relay công cộng không hỏi control plane của mình;
- dữ liệu relay đi qua hạ tầng bên ngoài lab.

**Có sẵn:**
- **Bản đơn tenant** (P08a, F2) có `POST /verify`: cần secret (`derp.verify_secret_path`, gửi kèm `?secret=`); caller không có secret nhận cùng một 403; chỉ trả `Allow` cho node còn sống, chưa hết hạn, của **chính runtime đó**.
- **Yêu cầu cho cluster mode** (`P08a-endpoint-inventory.md:258`): DERP đã xác thực được ánh xạ tới tập tailnet mà nó phục vụ; không xác định được phạm vi thì fail closed; không đăng ký trên `http.DefaultTransport` toàn cục.

**Về DERP:** relay chuyển các gói WireGuard đã mã hóa, đánh địa chỉ theo node key. Relay không đọc được nội dung, và không tự tạo đường đi giữa hai node không phải peer. Việc ai nói chuyện được với ai vẫn do netmap và policy quyết định. Verify chỉ trả lời câu hỏi: **ai được phép dùng relay**.

## Quyết định

### 1. Một DERP region cho mỗi cluster

Trên `mc-relay` chạy **một tiến trình `derper` cho mỗi cluster**:

| Cluster | Region | Cổng DERP (TLS) | Hostname DERP |
|---|---|---|---|
| c1 | 901 | 443 | `relay-c1.cp.gta5web.online` |
| c2 | 902 | 444 | `relay-c2.cp.gta5web.online` |

- **STUN:** một listener udp/3478, đặt ở tiến trình region 901. STUN không giữ trạng thái và không cần xác thực.
- **TLS:** dùng cert wildcard `*.cp.gta5web.online` sẵn có, chép từ mc-cp qua pipe.
- **DERP map trong config cluster:** mỗi node có `HostName` (cho SNI và kiểm cert) và `IPv4: 10.77.40.30` (địa chỉ để client quay số). Vì vậy **không cần bản ghi DNS công khai nào** trỏ tới IP private.
- **Mỗi cluster chỉ quảng bá region của chính nó** (`derp.paths`, file DERP map riêng).

### 2. Verify theo cluster

- Tiến trình `derper` của cluster N chạy với `--verify-client-url=https://<URL công khai của cluster N>/verify?secret=<secret của cluster N>` và `--verify-client-url-fail-open=false`.
- Request verify đi theo **cùng đường công khai mà client dùng** (VPS → wg-mesh → HAProxy → process active). Vì thế nó luôn tới process **đang giữ lease**, kể cả sau failover (yêu cầu "cả sau failover" của S15).
- **Secret riêng cho mỗi cluster:**
  - phía cluster: `derp.verify_secret_path`;
  - phía relay: file 0600 đọc bởi unit của `derper`;
  - chép giữa hai bên qua pipe ssh, không bao giờ vào repo, log hay artifact.

### 3. `POST /verify` trong cluster mode

Route mới trên listener của cluster:
- **Xác thực:** so secret của cluster bằng constant-time. Thiếu hoặc sai secret: luôn trả cùng một 403, body không phụ thuộc node key.
- **Process không giữ lease** (standby, đang failover): trả `Allow=false`. Fail closed: standby không có runtime nào.
- **Process giữ lease:** duyệt các runtime đang `servable()` (đã nạp, chưa retire, còn lease). Trả `Allow=true` khi và chỉ khi node key thuộc một node còn sống, chưa hết hạn, của **một tailnet mà cluster này sở hữu**. Node key lạ, node đã xóa, hết hạn, hoặc thuộc cluster khác: `Allow=false`.
- **Phạm vi của một DERP = tập tailnet của cluster của nó.** Không có verify xuyên cluster: relay của c1 không bao giờ chấp nhận node của tailnet thuộc c2.
- **Không đổi `http.DefaultTransport`:** verify của DERP ngoài là HTTP thường; DERP nhúng vẫn bị cấm trong cluster mode.
- **Endpoint inventory:** thêm dòng `public POST /verify` cho cluster, auth `verify-secret`. Đây là thay đổi bề mặt listener công khai, nên cần chủ dự án duyệt (AGENTS 9).

### 4. Phạm vi triển khai

- **Chỉ cấu hình E2E N1** (`config-*` của harness) mới trỏ tới region 901 và 902.
- Config lâu dài (`meshlab`, POC) giữ DERP công cộng cho tới khi chủ dự án quyết khác.
- Client VM trên `mccla`/`mcclb` tới relay qua host, theo firewall đã có: tcp/443–449, udp/3478.

## Hệ quả

**Tốt:**
- S15 kiểm được trên lab.
- Dữ liệu relay của E2E không còn đi qua DERP công cộng.
- Relay của mỗi cluster chỉ phục vụ tailnet của cluster đó; trùng khớp ranh giới fence owner/epoch.
- Failover và trao tay (ADR 0010) không cần sửa relay:
  - verify luôn hỏi process đang active;
  - một tailnet sau khi chuyển cluster dùng region của cluster đích, vì client đăng nhập lại ở đó.

**Chi phí và giới hạn:**
- **Trong cùng một cluster, mọi tailnet dùng chung relay.** Đây là cô lập ở mức "được dùng relay", không phải region riêng cho từng tenant. Cô lập lưu lượng vẫn dựa vào khóa WireGuard và netmap (S01–S03). Region riêng cho từng tailnet không mở rộng được tới 2000 tailnet mỗi cluster.
- **Mỗi lần client kết nối DERP sẽ gọi verify một lần** qua đường công khai. `derper` có cache cục bộ, nhưng tải verify khi storm kết nối lại thuộc phạm vi P14.
- **Secret nằm trong tham số dòng lệnh** của `derper` (`--verify-client-url` chỉ nhận URL), nên user cục bộ trên `mc-relay` thấy được qua `ps`. `mc-relay` là VM lab chỉ có user `mesh`. Muốn tránh thì cần một proxy verify cục bộ; ghi nhận cho P13.
- **Mất kết nối tới cluster thì từ chối** (`fail-open=false`): khi cluster không trả lời verify, client mới không vào được relay. Kết nối đang có vẫn giữ.

## Phương án đã loại

- **Một region dùng chung cho mọi cluster, đằng sau một proxy verify fan-out:** relay phải biết mọi cluster, và trở thành oracle xuyên cluster.
- **Một region cho mỗi tailnet:** không mở rộng được, và lộ số lượng tailnet qua DERP map.
- **Bật DERP nhúng trong cluster mode:** một relay cho mọi tailnet của process. Còn vướng `RegisterProtocol` toàn cục (ADR 0004). Bị cấm từ P05.
- **Dùng tiếp DERP công cộng:** không kiểm được S15, và dữ liệu E2E đi ra ngoài lab.

## Kiểm chứng (S15)

- **Unit / servertest** (RED trước):
  - `/verify` của cluster: không có hoặc sai secret → 403, body giống nhau;
  - node của tailnet thuộc cluster → `Allow`;
  - node hết hạn, đã xóa, lạ, hoặc thuộc cluster khác → từ chối;
  - process standby → từ chối;
  - sau `MoveOwner` và trao tay: cluster nguồn từ chối, cluster đích chấp nhận.
- **E2E N1 (S15):**
  - client hợp lệ của A và B dùng relay của cluster mình;
  - node key lạ, và node đã bị thu hồi, bị `derper` từ chối;
  - sau F01 trên cluster, client mới vẫn được relay chấp nhận;
  - kiểm bằng log `derper` cộng với `tailscale netcheck` / `tailscale status --json` (DERP home region).
