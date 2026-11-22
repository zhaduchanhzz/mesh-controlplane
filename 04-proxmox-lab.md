# 04 — Proxmox VE Lab as Code

## 1. Mục đích

Tạo môi trường kiểm thử có thể tái lập để coding agent kiểm chứng TCP/TLS/Noise, cùng host nhiều Tailscale profile, P2P, NAT, DERP fallback, bootstrap, hai cluster (mỗi cluster active + standby), failover lease, PostgreSQL dùng chung (primary + replica), backup/restore và traffic isolation. Topology theo PLAN 2.0 §9 và ADR 0005. **Không** tự động triển khai ngoài pool lab.

## 2. Stack đề xuất

- Proxmox VE 9.x (provider `bpg/proxmox` hiện hướng tới 9.x; nếu host là 8.x phải xác minh tương thích trong ticket lab).
- OpenTofu/Terraform provider `bpg/proxmox` **pin version trong lockfile** và inspect provider schema trước khi viết HCL; dùng cloud-init Ubuntu Server image.
- Ansible (cấu hình OS, Docker, systemd, TLS test CA, node firewall); OpenTofu chỉ tạo resources, Ansible cấu hình services.
- DNS private authoritative cho `*.mesh.test` (hostname của cluster và bootstrap, **không** hostname theo tenant); CA lab và chứng chỉ hợp lệ với hostname, client VM cài trust chain. Không dùng `curl -k` để 'chứng minh' TLS.
- PostgreSQL dùng chung (primary + replica streaming), bootstrap + placement API, hai cluster mỗi cluster hai VM (active/standby, cùng Noise key, endpoint chuyển theo lease bằng VIP hoặc L4 health check), DERP/STUN riêng. **PgBouncer bắt buộc** trước PostgreSQL ([ADR 0006](design/adr/0006-pgbouncer-connection-pooling.md)): mỗi VM PostgreSQL chạy một PgBouncer (container, image pin tag, PgBouncer ≥ 1.21, chế độ transaction, `max_prepared_statements` > 0); VM cluster chỉ nối tới PgBouncer qua VIP hoặc multi-host. Không còn edge reverse proxy định tuyến theo Host và không còn SQLite riêng mỗi shard (bản 1.1).

## 3. Sơ đồ underlay và IP tham khảo

```text
Proxmox lab pool: meshcontrol-lab
  vmbr-mgmt: 10.77.10.0/24      (only runner/operator)
  vmbr-client-a: 10.77.20.0/24  (A underlay)
  vmbr-client-b: 10.77.30.0/24  (B underlay)
  vmbr-service: 10.77.40.0/24   (PostgreSQL/bootstrap/cluster/DERP)

  Runner / CI       10.77.10.10
  PostgreSQL pri    10.77.40.11
  PostgreSQL rep    10.77.40.12
  PgBouncer         :6432 trên .11 và .12 (VIP PgBouncer: .10; hoặc multi-host)
  Bootstrap         10.77.40.13
  Cluster 1 a / b   10.77.40.21 / .22   (VIP cluster 1: .20)
  Cluster 2 a / b   10.77.40.25 / .26   (VIP cluster 2: .24)
  DERP/STUN         10.77.40.30
  Client A1/A2    10.77.20.11 / .12
  Client B1/B2    10.77.30.11 / .12
```

Đây chỉ là kế hoạch địa chỉ; agent phải kiểm tra tránh xung đột với mạng Proxmox hiện hữu. Nếu host hỗ trợ VLAN, dùng SDN VNet hoặc VLAN-backed Linux bridges tương ứng. Firewall chặn A↔B underlay: nếu client A ping được B qua LAN, phép thử “VPN ngăn liên lạc” không hợp lệ. Mở đúng đường đến cluster/bootstrap/DERP/DNS cần thiết; PgBouncer 6432 chỉ mở cho VM cluster; PostgreSQL 5432 chỉ mở cho PgBouncer (localhost), bootstrap/placement và replica; network faults phải inject có kiểm soát.

## 4. Ma trận VM (có thể chỉnh theo host)

| VM | vCPU | RAM | Disk | Nhận xét |
|---|---:|---:|---:|---|
| `mc-ci` | 8 | 16–32 GiB | 120 GiB | Docker/test runner+Go build (đã có, 7701) |
| `mc-pg-01` | 4 | 8 GiB | 50 GiB | PostgreSQL primary dùng chung + PgBouncer (cổng 6432) |
| `mc-pg-02` | 4 | 8 GiB | 50 GiB | PostgreSQL replica (streaming) + PgBouncer dự phòng |
| (tùy chọn) `mc-pgb-01`, `mc-pgb-02` | 2 mỗi máy | 2 GiB | 12 GiB | Chỉ khi P14 đo thấy PgBouncer tranh CPU với PostgreSQL; khi đó gỡ PgBouncer khỏi VM PG |
| `mc-bootstrap` | 2 | 2 GiB | 20 GiB | `mesh-bootstrap` + `mesh-placement` |
| `mc-c1-a`, `mc-c1-b` | 4 mỗi máy | 8 GiB | 30 GiB | Cluster 1 active / standby |
| `mc-c2-a`, `mc-c2-b` | 4 mỗi máy | 8 GiB | 30 GiB | Cluster 2 active / standby |
| `mc-relay` | 2 | 2 GiB | 20 GiB | DERP + STUN |
| `mc-client-a1,a2,b1,b2` | 1 mỗi máy | 1–2 GiB | 12 GiB | Real Tailscale clients |

`mc-cp` (7702, endpoint public, `serve-tailnets` deprecated) và `mc-bench` (7703, benchmark) đã có từ giai đoạn 1 và không nằm trong bảng trên. Trên `mc-bench`, benchmark B2/B3 chạy PostgreSQL 17 + PgBouncer bằng container tạm, chỉ bind `127.0.0.1`, mật khẩu sinh ngẫu nhiên trên VM và xóa hết sau khi đo (`evidence/P14-db-benchmarks.md`). IaC giai đoạn 2 hiện có (`infra/lab/stage2-*.tf`, mặc định tắt) vẫn theo topology bản 1.1 (`mc-placement`, `mc-edge`, `mc-shard-01/02`); phải cập nhật trong ticket LAB riêng trước khi bật.

Tổng đặt trước khi chạy đủ cấu hình (với CI 24 GiB và client 2 GiB): khoảng **40 vCPU, 84 GiB RAM guest, 428 GiB disk guest** (ước tính theo bảng trên); chưa tính overhead của Proxmox, backup snapshots, image cache và workload burst. Với một host vật lý, cân nhắc **ít nhất ~128 GiB RAM và SSD 500 GiB trở lên** để có headroom, hoặc triển khai theo từng giai đoạn trên host nhỏ hơn. Đây không phải chuẩn phần cứng bắt buộc.

Lab quy mô nhỏ: dùng Docker namespaces/containers cho client unit test; vẫn giữ các VM isolated để chứng minh routing/NAT/DERP và host failure. `mc-ci` có thể đặt ngoài Proxmox nếu CI runner đã có.

## 5. Quyền Proxmox và policy an toàn

**Quyết định của chủ lab (2026-11-22):** agent được dùng `root@pam` để tạo và cấu hình VM lab, và được tự `apply` trong phạm vi lab. Token tách quyền ([`infra/lab/LAB00-operator.md`](infra/lab/LAB00-operator.md) §4) vẫn là phương án khuyến nghị khi chuyển sang CI hoặc khi có nhiều người dùng chung host.

Khi dùng root, các giới hạn sau vẫn bắt buộc:

- Tạo Proxmox Pool độc lập `meshcontrol-lab`. Mọi VM, disk và tên mạng của lab đặt trong một namespace riêng (VMID `7700–7799`) để tránh thao tác nhầm.
- Chỉ tạo, sửa, bật hoặc tắt VM **trong pool `meshcontrol-lab` và dải VMID lab**. Không sửa, dừng, clone hay xóa VM nào khác trên host.
- Không thay đổi network, firewall, storage, cluster, user hay ACL của host, trừ khi ticket LAB ghi rõ thay đổi đó và người vận hành đã duyệt.
- Dùng SSH root vào host chỉ khi provider bắt buộc (upload snippet, import image) hoặc khi tạo template. Không dùng SSH root để chạy lệnh tùy ý trên host.
- Credential root nằm trong file env ngoài repo (`%USERPROFILE%\.mesh-lab\proxmox.env`). Không ghi vào Git, log, output, artifact, ảnh chụp hay chat. Đổi mật khẩu root khi kết thúc giai đoạn lab, hoặc ngay khi nghi bị lộ.
- Secrets dùng env / secret manager và TLS verification thật (pin certificate PVE, không `insecure = true`). `*.tfstate` chứa thông tin nhạy cảm, không lưu trong Git/ZIP public.
- Không cấp public IP của host PVE hoặc giao diện `:8006` cho Internet.
- Tắt SSH password auth trên VM test, dùng cloud-init SSH key công khai. Tất cả service credential chỉ là test fixtures xoay vòng được.

## 6. Pipeline IaC bắt buộc

```text
LAB00 — Agent (root, đã được cho phép) kiểm tra PVE version, tài nguyên, storage, bridge; tạo pool lab
LAB01 — Agent tạo infra/lab/*.tf + cloud-init + Ansible, chạy fmt/validate
LAB02 — Agent chạy tofu plan -out, export bản tóm tắt create/update/delete vào evidence
LAB03 — Plan chỉ create/update trong pool + dải VMID lab → agent tự apply;
        plan có destroy/replace hoặc ngoài phạm vi → human approves
LAB04 — Agent áp dụng plan đã lưu, chụp inventory + IP
LAB05 — Ansible bootstrap máy, cài Tailscale client và Go binaries
LAB06 — E2E self-tests (TLS, trust CA, direct underlay isolation, DERP)
LAB07 — Snapshot/backup only lab (không là backup cơ chế cũ)
LAB08 — Human approves destroy; xác minh only lab resources trong state
```

Lưu ý: `tofu plan` với provider thực có thể đọc API và ghi state local. Không dùng `apply -auto-approve` hay `destroy -auto-approve`. Agent chỉ apply **đúng file plan đã lưu**, sau khi đã kiểm tra (`tofu show -json`) rằng plan không có destroy/replace và chỉ chạm tới tài nguyên trong pool + dải VMID lab.

## 7. File IaC agent phải sinh

```text
infra/lab/
  versions.tf             # pin bpg/proxmox provider version
  providers.tf            # credentials qua env, TLS verify
  variables.tf
  network.tf              # bridges/VNET/VLAN theo topology được duyệt
  cloud-init.tf
  vm.tf                   # VM pool, explicit tags/labels
  outputs.tf              # hostnames/IPs, tuyệt đối không secret
  terraform.tfvars.example
  ansible/inventory.tmpl
  ansible/playbooks/bootstrap.yml
  ansible/playbooks/controlplane.yml
  scripts/validate-lab.sh
  scripts/collect-evidence.sh
  README.md
```

**Không cung cấp cấu hình HCL 'chắc chắn chạy được' khi chưa biết storage IDs, PVE version, bridge IDs, cloud image và provider schema của người dùng.** Agent phải inspect và render `plan` tương ứng host sau khi được cấp API test.

## 8. Nghiệm thu lab (không nhầm với ứng dụng)

- `tofu fmt -check`/`validate` pass; plan chỉ tạo VM thuộc pool lab, không thay tài nguyên khác.
- Ansible idempotent; chạy lại lần 2 không thay ngoài ý muốn.
- DNS và TLS tương ứng `cluster-1.mesh.test`, `cluster-2.mesh.test` (VIP của từng cluster) và hostname bootstrap. Không có hostname theo tenant.
- Underlay A→B blocked; cả A/B vẫn kết nối bootstrap, cluster và DERP ở test scenario tương ứng.
- Khi dùng `root@pam`, không có cơ chế kỹ thuật nào chặn agent thao tác ngoài pool. Vì vậy mỗi lần apply phải lưu bằng chứng rằng plan chỉ chạm tới VMID `7700–7799` trong pool lab, và inventory VM ngoài pool trước/sau phải giống nhau. Khi chuyển sang token tách quyền cho CI, phải chạy thêm negative test: token không xóa hoặc sửa được VM ngoài pool.
- E2E capture: Tailscale status, DERP map, routing logs, phản hồi bootstrap, audit lease, packet capture đã redact.
- Failure scenarios: kill/treo active của cluster (F01–F02), cắt active ↔ PostgreSQL (F03), failover PostgreSQL (F05), restart relay, bootstrap trả dữ liệu cũ (F07); xác minh ảnh hưởng phù hợp, không split brain và không cross-tenant leak.

## 9. Không tự động xóa dữ liệu

Chỉ được hủy lab sau khi xác nhận inventory VM ID và diff (`tofu plan -destroy`), chờ human approval riêng. `terraform.tfstate` có dữ liệu thật không đồng nghĩa VM được xóa an toàn.
