# LAB stage 2 cho Gate N1 — DERP relay và underlay A/B

- **Ngày:** 2026-11-25.
- **Nhánh:** `feat/LAB-stage2-n1`.
- **Runbook:** `infra/lab/stage2-n1.md`.
- **Trạng thái:** `Verified`. Chủ dự án duyệt ngày 2026-11-25 (phương án 1: agent thực hiện). Đã apply. L01–L08 PASS. Chưa `Reviewed`.

## Hiện trạng host trước thay đổi (đọc qua API PVE, chỉ đọc)

- PVE 9.2.27.
- Firewall tắt ở mọi cấp (datacenter, node, VM 7701/7702/7706); không có rule.
- Chưa có SDN zone hay VNet nào.

## OpenTofu

- **Cách chạy:** plan trong worktree `wt-lab-s2`, với **bản sao** state của main checkout. State này chỉ dùng để plan.
- Apply sẽ làm từ main checkout sau khi nhánh được merge và plan lại.
- `-refresh=false`.

**Kết quả:**

| Bước | Kết quả |
|---|---|
| `tofu fmt -check` | exit 0 |
| `tofu validate` | Success |
| `plan` với biến mặc định (`stage2_enabled=false`) | **No changes** (exit 0) |
| `plan -var stage2_enabled=true -out=lab-n1.tfplan` | **19 to add, 0 to change, 0 to destroy** |
| `check-plan.py` | **exit 2**: 9 tài nguyên SDN cần người duyệt; phần VM nằm trong phạm vi lab |

**Tài nguyên tạo mới:**

| Nhóm | Tài nguyên |
|---|---|
| SDN (người duyệt) | `proxmox_sdn_zone_simple.stage2` (`mclab`, IPAM pve, DHCP tắt). VNet `mccla`, `mcclb`, `mcsvc` (`isolate_ports=false`, không VLAN). Subnet 10.77.20.0/24, 10.77.30.0/24, 10.77.40.0/24: gateway `.1`, SNAT bật; dải DHCP khai báo nhưng không có hiệu lực vì DHCP tắt. 2 applier. |
| VM (trong phạm vi agent) | `mc-relay` 7714 (`mcsvc`, 10.77.40.30). `mc-client-a1` 7715 và `-a2` 7716 (`mccla`, .11/.12). `mc-client-b1` 7717 và `-b2` 7718 (`mcclb`, .11/.12). Tất cả trong pool `meshcontrol-lab`, một NIC, MAC `BC:24:11:77:<net>:<host>`. |
| Snippet | 5 file `mc-*-user-data.yaml` (cloud-init: user `mesh`, SSH key công khai, không có secret). |

Mọi tài nguyên hiện có (mc-ci, mc-cp, mc-bench, mc-c2, mc-c1b, mc-c2b, image, snippet cũ): no-op.

## Firewall host (đề xuất, người áp dụng)

`infra/lab/stage2-host-firewall.nft` là bảng `inet mclab`, gồm hai chain `forward` và `input`:
- Priority `filter - 1`, policy accept.
- Chỉ xét gói có nguồn hoặc đích là 10.77.0.0/16. Không đụng `pve-firewall`, vmbr0, lan10 hay rule SNAT của SDN.

Các luật:
- Chặn A ↔ B.
- Client → relay: chỉ tcp/443–449 và udp/3478.
- mc-ci → VM lab: SSH và ping.
- VM lab → mọi dải private: chặn. VM lab → Internet: cho phép.
- VM lab → dịch vụ của host: chặn.

## Kiểm chứng sau khi áp dụng

Các ca L01–L08 trong runbook; điền kết quả vào đây.

## Thực hiện (2026-11-25)

### 1. Host PVE (shell root, host key đã ghim; không in credential)

**Kiểm tra trước:**
- `/etc/network/interfaces` đã có `source /etc/network/interfaces.d/*` (dòng 30).
- **`net.ipv4.ip_forward=0`**.
- Host có IP trên `vmbr0` (192.168.1.14), `lan10` (192.168.10.0/24) và `tailscale0`.
- `iptables FORWARD` có policy ACCEPT (chain `ts-forward` của Tailscale); `br_netfilter` không được nạp.

**Hệ quả:** nếu chỉ bật forwarding, host sẽ bắt đầu định tuyến giữa LAN nhà, `lan10` và Tailscale, những đường trước đó không tồn tại. Firewall được siết thêm trước khi bật (commit `cdc3c96`, hẹp hơn diff đã duyệt): chain `forward` drop mọi gói IPv4 được định tuyến không thuộc lab. Gói đi qua bridge của các VM hiện có không đi qua hook này.

**Đã làm:**
- `/etc/mclab.nft` (LF, `nft -c` OK) được nạp và lưu bằng `mclab-nft.service` (enabled, active).
- `net.ipv4.ip_forward=1`, lưu trong `/etc/sysctl.d/99-mclab.conf`.
- IPv6 forwarding vẫn 0.
- Ngay sau bước này: SSH vào 6 VM hiện có đều được; POC và c2 công khai trả 200.

### 2. OpenTofu (main checkout)

Diễn biến:
1. Snapshot state vào `%USERPROFILE%\.mesh-lab\tofu-state\`: trước khi apply, giữa chừng và sau khi apply.
2. Plan `-var stage2_enabled=true`: **19 to add, 0 to change, 0 to destroy**, đúng như đã duyệt.
3. Apply **bị lệnh `timeout 590` của agent cắt ngang** sau khoảng 9,5 phút, lúc `mc-client-a2` đang chờ guest agent.
   - Kết quả: SDN, snippet và `mc-relay` đã nằm trong state. a1, b1 và b2 đã được tạo trên PVE nhưng chưa vào state. a2 đã tạo nhưng **không boot được**: `netout=0` sau reset và sau stop/start; đĩa đầy 14,8% trong khi các client khác 19%.
4. Lock cũ đã tự hết hiệu lực.
5. Import a1, b1, b2 vào state.
   - Phải sửa output `stage2_vms`: nó đọc tài nguyên của mọi key, nên import từng VM bị lỗi (`f3e4405`).
   - Thêm `ignore_changes = [disk[0].import_from, initialization[0].user_data_file_id]`, giống `vm.tf` (`2ecb016`).
6. **Chủ dự án cho phép xóa VM 7716.** Trước khi xóa đã kiểm tên `mc-client-a2` và thuộc pool `meshcontrol-lab`. Đã chạy `qm destroy 7716 --purge`; 2 volume của nó bị xóa.
7. Plan lại: **1 to add (a2), 3 to change** (chỉ `stop_on_destroy` false → true trên a1, b1, b2), **0 to destroy**. `check-plan.py` trả 0, vì SDN đã tồn tại.
   - Apply: a2 tạo trong 1 phút 0 giây.
8. `stage2_enabled` đổi mặc định thành `true` (`619c99e`). Plan với biến mặc định: **No changes**.

### 3. mc-ci

- Route `10.77.0.0/16 via 192.168.1.14 dev eth0`, có hiệu lực ngay (`ip route replace`).
- Lưu bằng `/etc/netplan/60-mclab-route.yaml` (0600, `netplan generate` OK, không chạy `netplan apply`).
- Quản lý VM lab bằng `ssh -o ProxyCommand="ssh … mesh@192.168.1.240 nc -q0 %h %p"`. Không dùng `-J`, vì `-J` không chuyển `-i` và `IdentitiesOnly` cho kết nối tới host trung gian.

### 4. Kiểm chứng: `infra/lab/scripts/stage2-n1-checks.sh`

| Ca | Kết quả |
|---|---|
| L01 | PASS: a1 ↔ a2 và b1 ↔ b2 ping được trên underlay của mình |
| **L02** | **PASS:** A ↔ B bị chặn ở ping, tcp/22 và udp theo cả hai chiều; bộ đếm drop A↔B trên host tăng 0 → 13 |
| L03 | PASS: client tới relay ở tcp/443 (nhận *refused*, vì chưa có dịch vụ lắng nghe) và udp/3478 (datagram tới nơi); tcp/22 của relay từ client: timeout |
| L04 | PASS: VM lab không tới được LAN nhà (mc-cp:22, mc-db:6432, router:80 đều timeout) |
| L05 | PASS: VM lab không tới được dịch vụ của host (gateway `.1` và 192.168.1.14, tcp/22 và 8006) |
| L06 | PASS: VM lab tới được endpoint công khai (c2 `:8445/ready`, POC `:8443/health` trả 200) qua SNAT và VPS |
| L07 | PASS: mc-ci SSH được vào cả 5 VM lab |
| L08 | PASS: 6 VM hiện có vẫn SSH được; c2 và POC công khai trả 200 |

### 5. Tailscale trên 4 client (chưa chạy, chưa đăng nhập)

| Bản | Nguồn | Vị trí |
|---|---|---|
| v1.104.7 | `pkgs.tailscale.com/stable`, sha256 đã kiểm | `/opt/tailscale/1.104.7` |
| v1.84.3 | Image `tailscale/tailscale:v1.84.3` trên mc-ci (bản static không còn trên `/stable`: 404), tailscaled sha256 `682d64b65b37ad76…` | `/opt/tailscale/1.84.3` |

## Còn lại cho P09

- Cấu hình relay (DERP, STUN, `verify_clients`) theo ADR 0009, cùng DERP map của cluster.
- Harness N1 dùng client VM: chọn phiên bản Tailscale theo thư mục trong `/opt/tailscale/`.
