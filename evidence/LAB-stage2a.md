# LAB stage 2a: cluster c2, lab RAM, state import (2026-11-24)

- **Nhánh:** `feat/LAB-stage2a` (từ `master` `fcce29a`). Trạng thái: `Verified`, chưa `Reviewed`.
- **Quyết định chủ dự án (2026-11-24):** làm 2a và giảm RAM lab (mc-ci 40→24 GiB, tắt mc-bench khi không benchmark); c2 dùng hostname công khai qua VPS; import VM cũ vào state mới của `infra/lab`; duyệt diff `infra/lab/stage2a-public-path.md`; duyệt tạo lại VM 7705 sau sự cố đĩa (mục 3).

## Source inventory

- **Đã đọc:**
  - `infra/lab/{vm,cp,bench,image,cloud-init,variables}.tf`, `infra/lab/scripts/{check-plan.py,pve_api.py,tofu.sh}`, `infra/lab/db/{main.tf,setup-db.sh}`;
  - `infra/lab/e2e/p05/run.sh` (cách cài cluster và `cluster --cluster-id`);
  - `cmd/headscale/cli/cluster.go` (`serve`, `register --endpoint/--class/--max-devices`), `hscontrol/types/config_values.go` (`Secret` `file:`);
  - unit `headscale-cluster.service` và `/etc/headscale/cluster/config.yaml` trên mc-cp;
  - unit `mesh-cp-proxy.*`/`mesh-boot-proxy.*` và `/etc/meshguard/vps-firewall.nft` trên VPS.
- **Ràng buộc:** AGENTS rule 3 (tự apply chỉ khi plan create/update trong pool `meshcontrol-lab` và VMID 7700–7799; destroy/replace hay firewall thật cần người duyệt), rule 2 (không secret trong Git/log), rule 9 (metrics/debug không lên listener công khai), ADR 0005 (một Noise key cho mỗi cluster).
- **Giả định còn lại:** chưa tìm ra nguyên nhân đĩa 7705 lần đầu bị hỏng (mục 3).

## 1. State `infra/lab` dựng lại trên máy này (import)

State gốc không có trên máy này. Đã `tofu import` 7 resource: `proxmox_download_file.ubuntu_2404` (`local:import/mc-ubuntu-2404-release-20261017.qcow2`), 3 snippet `mc-{ci,cp,bench}-user-data.yaml`, VM 7701/7702/7703. Plan đầu tiên sau import đòi **replace** mọi VM: `disk[0].import_from`, `initialization[0].user_data_file_id`, `source_raw` của snippet và `url/checksum` của file tải về đều không được PVE trả lại. Sửa:

- `lifecycle { prevent_destroy = true; ignore_changes = [disk[0].import_from, initialization[0].user_data_file_id] }` cho mọi VM (các thuộc tính này chỉ có tác dụng lúc tạo);
- `ignore_changes = [source_raw]` cho snippet, `[url, checksum, checksum_algorithm]` cho image;
- `scripts/check-plan.py`: snippet không có `source_raw` thì kiểm `file_name`; file tải về chỉ bắt buộc checksum khi được **tạo**.

State (`terraform.tfstate`) vẫn chỉ nằm cục bộ (gitignored), không commit.

## 2. Plan và apply giai đoạn 2a

`tofu fmt -check` 0, `validate` OK. Plan: **2 to add, 7 to change, 0 to destroy**:

- tạo snippet `mc-c2-user-data.yaml` và VM `mc-c2` 7705 (4 vCPU / 6 GiB / 30 GB, 192.168.1.244, MAC `BC:24:11:77:02:44`);
- mc-ci `memory.dedicated` 40960 → 24576;
- các update còn lại chỉ là thuộc tính mặc định của provider (`stop_on_destroy`, `overwrite`, `verify`…).

`check-plan.py`: "In lab scope". Apply đúng file plan, exit 0. mc-ci khởi động lại với 24 GiB.

## 3. Sự cố: đĩa VM 7705 hỏng lúc tạo

- **Triệu chứng:** VM chạy nhưng không ping được, guest agent không lên. Serial console (đọc qua `termproxy` của API) dừng ở `(initramfs)`: không tìm thấy `LABEL=cloudimg-rootfs`, `/proc/partitions` chỉ có `sda`, `wipefs` chỉ thấy PMBR.
- **Chẩn đoán:**
  - cấu hình VM giống mc-cp (khác IP/MAC/RAM);
  - log `qmcreate`: import đủ 3,5 GiB, TASK OK; resize lên 30 GiB OK;
  - hai GPT header đều hợp lệ (CRC đúng), nhưng mảng partition entry ở **cả bản chính lẫn bản backup** có byte rác (entry 37, 69…), CRC `0x21d7b268` khác `0xdaf3a718` ghi trong header, nên kernel loại cả hai;
  - mc-db (7704) tạo từ cùng file image boot bình thường;
  - file image trên host: sha256 `04e8f246…6c94` khớp checksum pin trong `image.tf`, `qemu-img check` không lỗi;
  - thin pool `nvme01` dùng 10,8%, kernel log của host từ 2026-11-23 không có lỗi MCE/EDAC/NVMe/I/O.
- **Đã làm:**
  - reset 2 lần;
  - thử chép mảng entry backup lên bản chính (không đủ, vì bản backup cũng hỏng);
  - được chủ dự án duyệt thì tạo lại VM: tạm bỏ `prevent_destroy` riêng cho `mc_c2`, `plan -refresh=false -replace=proxmox_virtual_environment_vm.mc_c2` ra **1 to add, 0 to change, 1 to destroy**, chỉ `mc_c2` (VMID 7705 trước/sau, pool `meshcontrol-lab`, MAC giữ nguyên); `check-plan.py` chặn đúng ("HUMAN APPROVAL REQUIRED"); apply file plan đó, exit 0; đặt lại `prevent_destroy = true`.
- **Sau khi tạo lại:** kernel thấy `sda1 sda14 sda15 sda16` (chỉ cảnh báo alt header, giống mc-db), growpart mở `sda1` 29 GiB, cloud-init `done`, guest agent active. Host key ed25519 mới: `SHA256:+VHhgPEfO20+SV4d+AIVzPHFRg0UVJVsHfnl1k1uec8`.
- **Nguyên nhân gốc:** chưa rõ (convert qcow2 nén → thin LV). Nếu lặp lại khi tạo VM mới, cần kiểm RAM/NVMe của host.
- **Truy cập host:** để đọc sha256 và kernel log, SSH root vào host PVE bằng mật khẩu đọc từ `proxmox.env` (không in ra), chỉ chạy lệnh đọc. Host key PVE được lưu theo TOFU vào `%USERPROFILE%\.mesh-lab\pve_known_hosts`: `SHA256:kRHmbI8da5wrXqfLppcAkmfbiCk34uvssVlemwKED74`.

## 4. Cluster c2 trên mc-c2

`infra/lab/c2/setup-c2.sh` (chạy lại được) thực hiện:

- binary lấy từ mc-cp (sha256 `cde8c6904e5d89c1…`, cùng build với E2E PostgreSQL), kiểm sha256 hai đầu;
- cert/key wildcard chép qua pipe SSH (key 0640 `root:headscale`, so sha256 hai đầu, không in ra);
- mật khẩu role `headscale` qua stdin vào `/etc/headscale/cluster/pg.pass` (0640);
- config: `server_url https://c2.cp.gta5web.online:8445`, listen `0.0.0.0:8443`, metrics `127.0.0.1:9095`, DB `meshlab` qua PgBouncer `192.168.1.243:6432`, `policy: database`, `base_domain c2.tailnet.internal`;
- unit `headscale-cluster.service` (hardening giống c1);
- nft trên mc-c2 (bảng riêng `inet mc_c2`, policy drop): SSH 22 từ 192.168.1.0/24; 8443 chỉ từ 192.168.1.240–249;
- `cluster register --cluster-id mc-c2 --endpoint https://c2.cp.gta5web.online:8445` → `ACTIVE`, class `shared`, `max_devices` 5000.

Log khởi động: tự tạo Noise key riêng (`no private key file at path, creating...`), `database=postgres`, `cluster listening on [::]:8443 cluster=mc-c2 tailnets=0`, `/health` từ mc-cp 200. Noise public key của c2 (`/key`) bắt đầu `mkey:7738de34436…` (khác c1).

## 5. Đường công khai c2 (diff đã duyệt, đã áp dụng)

| Máy | Thay đổi | Backup |
|---|---|---|
| mc-cp | nft `iifname "wg-mesh" ip saddr 10.99.77.1 tcp dport 8445 accept` (handle 10, live + `/etc/nftables.conf`, `nft -c` trước khi ghi); `mesh-c2-relay.socket` (`10.99.77.2:8445`, `FreeBind`) + `.service` (`systemd-socket-proxyd --connections-max=512 192.168.1.244:8443`) | `/etc/nftables.conf.before-stage2a-20261124T152249Z` |
| VPS | nft `tcp dport 8445 accept` trong `inet mg_vps input` (handle 26, live + `/etc/meshguard/vps-firewall.nft` dòng 29, `nft -c` trước khi ghi); `mesh-c2-proxy.socket` (`116.118.3.46:8445`) + `.service` (`--connections-max=512 10.99.77.2:8445`) | `/etc/meshguard/vps-firewall.nft.before-stage2a-20261124T152300Z` |

Không đụng HAProxy (`mg-l4proxy`), 80/443, 8443, 8444, stund, mail, WireGuard. Rollback: `infra/lab/stage2a-public-path.md` mục Rollback.

**Kiểm chứng (từ LAN qua IP công khai, curl có xác thực cert):**

| Kiểm | Kết quả |
|---|---|
| `https://c2.cp.gta5web.online:8445/health` | 200, `ssl_verify_result=0`, cert `CN=cp.gta5web.online` hết hạn 2027-02-20 |
| Host `evil.example` / Host `c1.cp.gta5web.online` trên :8445 | 421 / 421 |
| `/metrics`, `/debug/pprof/` trên :8445 | 404 / 404 |
| `/key` trên :8445 | Noise key của c2 |
| POC trên :8443 (`hs.cp.gta5web.online`) | 200 (không đổi) |
| :8444 | TCP mở; bootstrap chỉ chạy trong E2E (như trước) |
| HAProxy :443 `vnflashlearner.click` (từ LAN và trên VPS) | 200; container `mg-l4proxy` restarts=0 |

## 6. RAM lab

- mc-ci 24 GiB (16 vCPU giữ nguyên).
- mc-bench (7703) đã shutdown qua API; biến mới `mc_bench_running` (mặc định `false`) giữ nó tắt trong OpenTofu; đặt `-var mc_bench_running=true` khi chạy benchmark.
- RAM host sau khi tắt: 62,0 / 125,7 GiB.

Plan sau cùng: mọi resource đều `no-op`, `check-plan.py` "In lab scope".

## Hạn chế

- Cert wildcard trên mc-c2 là bản chép: phải chép lại sau mỗi lần certbot gia hạn trên mc-cp (trước 2027-02-20) hoặc thêm deploy hook (ticket sau).
- c2 chưa có tailnet nào (`tailnets=0`); c1 chỉ chạy cluster trong E2E (bình thường mc-cp chạy POC). Test hai cluster dùng chung `meshlab` (chuyển tailnet c1↔c2, P11 F07) là bước tiếp theo.
- `ssl: disable` tới PgBouncer (LAN lab), như E2E PostgreSQL.
- Chưa rõ nguyên nhân đĩa 7705 hỏng lần đầu (mục 3).
