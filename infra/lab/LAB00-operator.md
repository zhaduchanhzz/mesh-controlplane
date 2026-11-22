# LAB00 — Chuẩn bị Proxmox cho lab

> **Cập nhật 2026-11-22:** chủ lab cho phép agent dùng `root@pam` và tự apply trong pool `meshcontrol-lab` + dải VMID `7700–7799` (xem `AGENTS.md` rule 3–4, `04-proxmox-lab.md` §5). Agent tự làm bước 1–3 qua API bằng root. Bước 4–5 (token tách quyền) giờ là **tùy chọn**, dành cho lúc chuyển sang CI hoặc dùng chung host. Người vận hành chỉ cần làm bước 0 khi kết thúc giai đoạn lab.

**Giai đoạn 1** chỉ tạo **một VM `mc-ci`** trên `vmbr0`. VM này vừa làm máy build/test Linux có Docker, vừa làm CI runner. Toàn bộ test của N1 (P01–P09) chạy bằng container Docker trên `mc-ci`. Các mạng cô lập `10.77.x.0/24` và các VM khác để sang **giai đoạn 2**, trước P09.

## 0. Đổi mật khẩu root (khi kết thúc giai đoạn lab)

Mật khẩu root đã bị gửi trong một phiên chat và đang được agent dùng. Hãy đổi nó khi kết thúc giai đoạn lab, hoặc ngay khi nghi bị lộ, rồi cập nhật file `proxmox.env`:

```sh
passwd root
```

## 1. Thu thập thông tin (chỉ đọc, không có secret)

Chạy các lệnh sau rồi gửi output cho agent. Agent cần chúng để chọn storage, bridge, VMID và kiểm tra tài nguyên.

```sh
pveversion -v | head -5
hostname -f
pvesh get /nodes --output-format json-pretty
pvesh get /nodes/$(hostname)/status --output-format json-pretty | grep -E '"(cpus|model|total|free|used)"'
pvesm status
ip -br addr
cat /etc/network/interfaces
pvesh get /pools --output-format json-pretty
qm list
openssl x509 -in /etc/pve/local/pve-ssl.pem -noout -subject -ext subjectAltName
```

## 2. Pool và dải VMID

- Pool: `meshcontrol-lab`
- Dải VMID của lab: `7700–7799` (agent chỉ tạo VM trong dải này). Nếu `qm list` cho thấy dải này đã có VM, hãy chọn dải khác và báo agent.

```sh
pveum pool add meshcontrol-lab --comment "mesh-controlplane lab"
```

## 3. Template Ubuntu 24.04 (root, chạy tay)

Template do người vận hành tạo, nên agent không cần quyền upload file hay SSH vào host. Thay `local-lvm` và `vmbr0` theo output của bước 1.

```sh
cd /root
wget -q https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
wget -q https://cloud-images.ubuntu.com/noble/current/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS   # phải in "OK"

qm create 7700 --name tpl-ubuntu-2404 --pool meshcontrol-lab \
  --memory 2048 --cores 2 --cpu host --ostype l26 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single \
  --agent enabled=1 --serial0 socket --vga serial0
qm set 7700 --scsi0 local-lvm:0,import-from=/root/noble-server-cloudimg-amd64.img,discard=on,ssd=1
qm set 7700 --ide2 local-lvm:cloudinit --boot order=scsi0
qm template 7700
```

## 4. User, role và token tối thiểu

Ví dụ trong docs của `bpg/proxmox` cấp `Sys.Modify`, `Permissions.Modify`, `User.Modify`, gần tương đương root, nên **không dùng**. Bộ quyền dưới đây chỉ cho phép tạo, sửa, bật/tắt VM **trong pool**, cấp dung lượng trên **một** storage và gắn NIC vào **một** bridge.

```sh
# PVE 9.x. Nếu là PVE 8.x: thay VM.GuestAgent.Audit bằng VM.Monitor
pveum role add MeshLabVM -privs "VM.Allocate,VM.Clone,VM.Audit,VM.PowerMgmt,VM.Config.CPU,VM.Config.Memory,VM.Config.Disk,VM.Config.Network,VM.Config.Cloudinit,VM.Config.Options,VM.Config.HWType,VM.Config.CDROM,VM.Snapshot,VM.Snapshot.Rollback,VM.GuestAgent.Audit,Pool.Audit"
pveum role add MeshLabStorage -privs "Datastore.AllocateSpace,Datastore.Audit"
pveum role add MeshLabNet -privs "SDN.Use,SDN.Audit"
pveum role add MeshLabNodeRead -privs "Sys.Audit"

pveum user add mesh-lab@pve --comment "mesh-controlplane lab automation"
# privsep=1 (token tách quyền), hết hạn sau 30 ngày. Secret chỉ hiện MỘT lần.
pveum user token add mesh-lab@pve tofu --privsep 1 \
  --expire $(date -d '+30 days' +%s) --comment "LAB01-LAB04, rotate sau 30 ngày"
```

Gán quyền cho cả user lẫn token. Với `privsep=1`, quyền thực tế là phần giao của hai bên. Dấu nháy đơn là bắt buộc, vì `!` trong nháy kép sẽ bị bash hiểu là history expansion.

```sh
NODE=$(hostname); STORE=local-lvm; BR=vmbr0   # chỉnh theo bước 1

pveum acl modify /pool/meshcontrol-lab        --roles MeshLabVM       --users mesh-lab@pve
pveum acl modify /pool/meshcontrol-lab        --roles MeshLabVM       --tokens 'mesh-lab@pve!tofu'
pveum acl modify /storage/$STORE              --roles MeshLabStorage  --users mesh-lab@pve
pveum acl modify /storage/$STORE              --roles MeshLabStorage  --tokens 'mesh-lab@pve!tofu'
pveum acl modify /sdn/zones/localnetwork/$BR  --roles MeshLabNet      --users mesh-lab@pve
pveum acl modify /sdn/zones/localnetwork/$BR  --roles MeshLabNet      --tokens 'mesh-lab@pve!tofu'
pveum acl modify /nodes/$NODE                 --roles MeshLabNodeRead --users mesh-lab@pve
pveum acl modify /nodes/$NODE                 --roles MeshLabNodeRead --tokens 'mesh-lab@pve!tofu'
```

## 5. Negative test: token không thấy VM ngoài pool

```sh
read -rs TOKEN   # dán secret, không hiện lên màn hình, không lưu vào history
curl -s --cacert /etc/pve/pve-root-ca.pem \
  -H "Authorization: PVEAPIToken=mesh-lab@pve!tofu=$TOKEN" \
  "https://$(hostname -f):8006/api2/json/cluster/resources?type=vm" | python3 -m json.tool
unset TOKEN
```

Kết quả mong đợi: chỉ thấy VM `7700` (template). Nếu thấy VM khác, **dừng lại** và kiểm tra ACL.

## 6. Lưu token trên máy operator (không gửi qua chat, không commit)

Tạo file `%USERPROFILE%\.mesh-lab\proxmox.env` (nằm ngoài repo):

```text
PROXMOX_VE_ENDPOINT=https://<hostname-hoặc-IP-có-trong-SAN>:8006/
PROXMOX_VE_API_TOKEN=mesh-lab@pve!tofu=<SECRET>
```

Không dùng `insecure = true` (rule 4). Copy `/etc/pve/pve-root-ca.pem` (cert công khai, không phải secret) về Windows rồi import cho user hiện tại:

```powershell
Import-Certificate -FilePath .\pve-root-ca.pem -CertStoreLocation Cert:\CurrentUser\Root
```

Endpoint phải là tên hoặc IP có trong `subjectAltName` ở bước 1.

## 7. Trên máy Windows của operator

```powershell
winget install --id OpenTofu.Tofu
ssh-keygen -t ed25519 -f $HOME\.ssh\mesh-lab -C mesh-lab   # private key giữ ở máy này
```

Public key `mesh-lab.pub` được đưa vào cloud-init của VM lab. Key này **không** dùng để SSH vào Proxmox host.

## 8. Agent sẽ làm tiếp (LAB01–LAB02) và không làm

- **Làm:** viết `infra/lab/*.tf` (pin version `bpg/proxmox`, credential lấy từ env) cho riêng `mc-ci`, chạy `tofu fmt` / `validate` / `plan`, rồi trình bản plan.
- **Không làm:** `tofu apply`, `tofu destroy`, sửa network hoặc firewall của host, dùng root. `apply` chỉ chạy sau khi người vận hành duyệt plan (LAB03).
