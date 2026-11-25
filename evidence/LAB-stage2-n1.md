# LAB stage 2 cho Gate N1 — DERP relay và underlay A/B

- **Ngày:** 2026-11-25.
- **Nhánh:** `feat/LAB-stage2-n1`.
- **Runbook:** `infra/lab/stage2-n1.md`.
- **Trạng thái:** `Planned`. Chờ chủ dự án duyệt phần SDN và firewall host (AGENTS rule 3). Chưa apply gì.

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
