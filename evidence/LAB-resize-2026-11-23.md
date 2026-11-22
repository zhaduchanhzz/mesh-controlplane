# LAB: đổi cấu hình mc-ci / mc-cp (2026-11-23)

- **Yêu cầu chủ lab:** tăng cấu hình VM chạy test/CI. Chọn phương án "mc-ci 16 vCPU / 40 GB, lấy RAM từ mc-cp".
- **Lý do:** host `hanh` có 36 thread, 125.7 GiB RAM. Trước khi đổi, RAM đã cấp 120 GiB (VM 100/101/102 = 48 GiB + 3 VM lab × 24 GiB). mc-cp chỉ dùng 1.9 GiB.

## Plan (`tofu plan -out=resize.tfplan`)
- `tofu fmt -check`: 0; `tofu validate`: Success.
- `Plan: 0 to add, 2 to change, 0 to destroy.`

| VM | vCPU | RAM (MiB) |
|---|---|---|
| `proxmox_virtual_environment_vm.mc_ci` (7701) | 8 → 16 | 24576 → 40960 |
| `proxmox_virtual_environment_vm.mc_cp` (7702) | 8 → 4 | 24576 → 8192 |

- `check-plan.py`: "In lab scope: agent may apply this exact plan file." (exit 0). Plan chỉ update trong pool `meshcontrol-lab`, VMID 7700–7799 (AGENTS rule 3).
- **Apply:** `Apply complete! Resources: 0 added, 2 changed, 0 destroyed.` (mc-cp 20 s, mc-ci 61 s, có reboot).

## Kiểm tra sau apply
- mc-ci: `nproc` 16, RAM 39 GiB. Container `ts-home1` (restart policy `no`) được `docker start` lại.
- mc-cp: `nproc` 4, RAM 7 GiB. `headscale-tailnets` và `wg-quick@wg-mesh` đều active. `https://a.cp.gta5web.online:8443/health` trả 200.
- Không đụng VM 100/101/102, storage, network hay firewall.
- **RAM đã cấp sau thay đổi:** 48 + 40 + 8 + 24 = 120 GiB, không đổi tổng.
