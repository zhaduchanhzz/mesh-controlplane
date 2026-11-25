# Lab giai đoạn 2 cho Gate N1 — DERP relay và underlay A/B

- **Ticket:** LAB-stage2-n1 (nhánh `feat/LAB-stage2-n1`). Phục vụ P09, quyết định Q1 của chủ dự án ngày 2026-11-25.
- **Trạng thái:** đề xuất. Chờ chủ dự án duyệt diff của SDN và firewall host.
- **Ngoài phạm vi** (để P13): replica PostgreSQL, PgBouncer thứ hai, `mc-bootstrap` riêng.

## Topology

| VNet (SDN zone `mclab`) | Subnet | VM (VMID, IP) |
|---|---|---|
| `mccla` | 10.77.20.0/24 | mc-client-a1 7715 `.11`, mc-client-a2 7716 `.12` |
| `mcclb` | 10.77.30.0/24 | mc-client-b1 7717 `.11`, mc-client-b2 7718 `.12` |
| `mcsvc` | 10.77.40.0/24 | mc-relay 7714 `.30` (DERP + STUN) |

**Cấu hình mỗi VM:**
- Một NIC duy nhất trên VNet của nó; IP tĩnh qua cloud-init.
- User `mesh`, chỉ SSH bằng key; `prevent_destroy`.
- DNS công cộng (`var.stage2_dns`).
- Tổng RAM 10 GiB (budget 12 GiB).

**Đường đi:**
- Cluster c1/c2, PostgreSQL và bootstrap giữ nguyên trên vmbr0.
- Client tới control plane qua URL công khai (VPS → wg-mesh → mc-cp), y như các E2E hiện tại.
- Client tới DERP qua relay trong `mcsvc`.
- mc-ci quản lý các VM lab qua host: route `10.77.0.0/16 via 192.168.1.14`.

**Cấu hình phần mềm của relay** (DERP map, `verify_clients`) **không** thuộc ticket này: nó theo ADR 0009, viết ở P09.

## Thay đổi trong repo

| File | Thay đổi |
|---|---|
| `stage2-variables.tf` | Bỏ VNet `mgmt` và các VM `mc-placement`, `mc-edge`, `mc-shard-0{1,2}` (topology bản 1.1, chưa từng được tạo). Còn lại `mc-relay` cùng 4 client. Budget RAM 12 GiB. Thêm `stage2_dns`. |
| `stage2-network.tf`, `stage2-vms.tf` | Chú thích theo topology N1; firewall chuyển sang file riêng. |
| `stage2-host-firewall.nft` (mới) | Bảng nftables `inet mclab` trên host, **do người áp dụng**. |
| `stage2-n1.md` (file này) | Runbook. |

## Các bước

### 1. Người (chủ dự án) — chuẩn bị host PVE

1. `/etc/network/interfaces` có dòng `source /etc/network/interfaces.d/*`. Đây là mặc định từ PVE 8.
2. Bật IPv4 forwarding: `sysctl -w net.ipv4.ip_forward=1`, và lưu vào `/etc/sysctl.d/99-mclab.conf`.

### 2. Người — duyệt và apply phần SDN của plan

- Plan do agent tạo, file `lab-n1.tfplan`, với `-var stage2_enabled=true`.
- `check-plan.py` sẽ trả exit 2, vì mọi tài nguyên `proxmox_sdn_*` đều cần người duyệt.
- Phần SDN gồm zone `mclab`, VNet `mccla`, `mcclb`, `mcsvc`, ba subnet (gateway `.1`, SNAT) và applier.

### 3. Người — áp dụng firewall host

```bash
install -m 0644 stage2-host-firewall.nft /etc/mclab.nft
nft -c -f /etc/mclab.nft && nft -f /etc/mclab.nft
```

Để rule còn sau reboot, tạo unit systemd oneshot `mclab-nft.service` chạy `nft -f /etc/mclab.nft`, `After=network-pre.target`. File này **không** đụng `pve-firewall` (đang tắt).

### 4. Agent

1. Tạo 5 VM cùng snippet cloud-init trong pool `meshcontrol-lab`, VMID 7714–7718. Đây là plan chỉ tạo mới trong phạm vi lab, theo AGENTS rule 3.
2. Thêm route `10.77.0.0/16 via 192.168.1.14` trên mc-ci, lưu bằng netplan. Đây là thay đổi trong VM lab.
3. Cài Tailscale trên 4 client; giữ ở trạng thái logout cho tới E2E.

### 5. Kiểm chứng (agent, ghi vào `evidence/LAB-stage2-n1.md`)

| Ca | Kỳ vọng |
|---|---|
| L01 | a1 ↔ a2 và b1 ↔ b2 ping được (cùng underlay) |
| **L02** | a1 → b1 và b1 → a1: ping, TCP 22 và UDP đều **thất bại**; bộ đếm drop của `mclab` tăng |
| L03 | Client → relay: TCP 443 và UDP 3478 mở; TCP 22 của relay từ client **đóng** |
| L04 | Mọi VM lab → 192.168.1.0/24 (LAN nhà, kể cả mc-cp và mc-db) **thất bại** |
| L05 | Mọi VM lab → host `10.77.x.1` và `192.168.1.14` trên TCP 22 và 8006 **thất bại** |
| L06 | Mọi VM lab → Internet: `curl https://c1.cp.gta5web.online:8443/health` và `…:8445/health` trả 200 |
| L07 | mc-ci → mọi VM lab: SSH được |
| L08 | Các VM hiện có (mc-ci, mc-cp, mc-db, mc-c2, mc-c1b, mc-c2b), cổng công khai và POC không đổi |

**Lưu ý:** P09 chỉ được dùng các ca "VPN isolation" khi L02 PASS.

## Rollback

- **Firewall (người):** `nft delete table inet mclab`, rồi xóa unit `mclab-nft.service`.
- **VM (người duyệt):** `prevent_destroy` chặn mọi lệnh xóa; xóa VM cần duyệt riêng.
- **SDN (người):** đặt lại `stage2_enabled=false` thì plan sẽ là destroy, nên cần duyệt riêng.
