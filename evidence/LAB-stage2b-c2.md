# LAB stage 2b — c2 active/standby (mc-c2 + mc-c2b)

- Ngày: 2026-11-25 (UTC).
- Chủ dự án duyệt Q8a–c ngày 2026-11-25. Thiết kế và rollback: `infra/lab/stage2b-c2-ha.md`.
- Trạng thái: `Verified` ở mức lab (hạ tầng + failover lease). Chưa `Reviewed`.

## (a) VM mc-c2b — OpenTofu

| | |
|---|---|
| File | `infra/lab/c2b.tf` |
| VM | VMID 7707, `mc-c2b`, pool `meshcontrol-lab` |
| Mạng | 192.168.1.246, MAC `BC:24:11:77:02:46` |
| Cấu hình | 4 vCPU / 6 GB / 30 GB, cùng template cloud-init với mc-c1b |

- `tofu fmt -check`: exit 0. `tofu validate`: thành công.
- `tofu plan -out c2b.tfplan`: **2 to add, 0 to change, 0 to destroy**.
  - `proxmox_virtual_environment_file.mc_c2b_user_data`: create.
  - `proxmox_virtual_environment_vm.mc_c2b`: create, `vm_id=7707`, `pool_id=meshcontrol-lab`.
  - Mọi tài nguyên khác: no-op.
- `scripts/check-plan.py`: "In lab scope", exit 0.
- Agent tự apply theo AGENTS rule 3, vì plan chỉ create trong pool lab và dải VMID lab.
  - `tofu apply c2b.tfplan`: **2 added, 0 changed, 0 destroyed**. VM tạo trong 51 s; cloud-init `done`.
- State: `infra/lab/terraform.tfstate` của main checkout (gitignored). Có snapshot trong `%USERPROFILE%\.mesh-lab\tofu-state\`.

## (b) mc-c2b là process standby của cluster `mc-c2`

Cách làm giống mc-c1b trong P11b.

**Binary:** chép từ mc-c2. sha256 `fb8f22e9…4852`, giống mc-c2 (bản P08b, `master` `968acea`).

**Bí mật chép mc-c2 → mc-c2b chỉ qua pipe SSH:**
- cert/key TLS;
- Noise key của c2 (ADR 0005);
- `pg.pass`;
- `config.yaml`.

Sau khi chép, so sha256 hai bên cho từng file; **cả 5 file khớp**. Không giá trị nào được in ra hoặc ghi ở máy agent.

**Quyền file:**

| File | Owner | Mode |
|---|---|---|
| Noise key | `headscale:headscale` | 0600 |
| `privkey.pem`, `pg.pass`, `config.yaml` | `root:headscale` | 0640 |

**Unit `headscale-cluster.service`:**
- giống mc-c2 (cùng hardening), `--cluster-id mc-c2`;
- chỉ khác dòng `Description`.

**nftables (`table inet mc_c2b`, policy drop):**
- SSH 22 chỉ từ 192.168.1.0/24;
- 8443 chỉ từ 192.168.1.240–249.

## Kiểm chứng

Mọi `curl` dưới đây chạy từ mc-cp với `--resolve c2.cp.gta5web.online:8443:<ip>`. "Công khai" là `https://c2.cp.gta5web.online:8445/ready` (VPS → wg-mesh → HAProxy `mesh-c2-ha`).

### Trước failover

| | `/health` | `/ready` |
|---|---|---|
| mc-c2 (.244) | 200 | 200 (active, lease epoch 1) |
| mc-c2b (.246) | 200 | 503 (standby) |
| Công khai | | 200 |

### F1 — dừng có kiểm soát (`systemctl stop` trên mc-c2)

- 06:22:25Z: mc-c2 ghi `lease ended … reason=shutdown`.
- Cùng giây 06:22:25Z: mc-c2b ghi `lease acquired: active … lease_epoch=2`.
- Bật lại mc-c2: nó lên standby, `/ready` 503. mc-c2b vẫn active. Công khai 200.
- Số giây đo bằng script bị lỗi (Git Bash không có `bc`), nên thời điểm lấy từ journal.

### F2 — `SIGKILL` process đang active (mc-c2b), lease không được nhả

| Mốc | Thời gian |
|---|---|
| mc-c2 `/ready` 200 | **8,27 s** |
| Công khai `/ready` 200 | **9,81 s** |

- Ngưỡng kế hoạch: ≤ ~11 s.
- mc-c2 ghi `lease acquired: active … lease_epoch=3` lúc 06:24:48Z.
- systemd khởi động lại mc-c2b lúc 06:24:45Z; nó lên standby, `/ready` 503.

**Trạng thái cuối giống ban đầu:**
- mc-c2 active, epoch 3;
- mc-c2b standby;
- công khai 200.

Lease epoch tăng đơn điệu: 1 → 2 → 3.

## Giới hạn đã biết

- Cluster `mc-c2` trên DB `meshlab` hiện phục vụ **0 tailnet** (`tailnets=0` trong log). Vì vậy lần này chưa kiểm chứng "client c2 không phải đăng nhập lại" trên c2 lâu dài.
  - Hành vi client qua failover đã có E2E ở P11b (c1, 9/9 ×2).
  - Sẽ chạy lại với client thật cho c2 trong E2E P12 (bước 7).
- Chỉ HAProxy trên mc-cp là front door. mc-cp là điểm lỗi đơn của đường công khai trong lab; điều này có chủ đích.
- Unit `mesh-c2-relay.*` cũ vẫn giữ để rollback. Chỉ xóa sau khi P12 `Verified`.
- `infra/lab/c2b.tf` cũng phải có trong main checkout (nơi giữ state). Nếu thiếu, `tofu plan` sẽ đòi xóa mc-c2b; `prevent_destroy` chặn việc đó.

## Rollback

Xem `infra/lab/stage2b-c2-ha.md` § Rollback.

Trên mc-c2b: `systemctl disable --now headscale-cluster`, rồi xóa Noise key, `pg.pass` và cert. VM giữ lại; xóa VM thì phải có duyệt riêng.
