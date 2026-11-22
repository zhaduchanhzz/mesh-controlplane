# 04 — Proxmox VE Lab as Code

## 1. Mục đích

Tạo môi trường kiểm thử có thể tái lập để coding agent kiểm chứng TCP/TLS/Noise, cùng host nhiều Tailscale profile, P2P, NAT, DERP fallback, hai shard, lease failure, backup/restore và traffic isolation. **Không** tự động triển khai ngoài pool lab.

## 2. Stack đề xuất

- Proxmox VE 9.x (provider `bpg/proxmox` hiện hướng tới 9.x; nếu host là 8.x phải xác minh tương thích trong ticket lab).
- OpenTofu/Terraform provider `bpg/proxmox` **pin version trong lockfile** và inspect provider schema trước khi viết HCL; dùng cloud-init Ubuntu Server image.
- Ansible (cấu hình OS, Docker, systemd, TLS test CA, node firewall); OpenTofu chỉ tạo resources, Ansible cấu hình services.
- DNS private authoritative cho `*.mesh.test`; CA lab và chứng chỉ hợp lệ với hostname, client VM cài trust chain. Không dùng `curl -k` để 'chứng minh' TLS.
- Hạ tầng registry PostgreSQL, hai shard SQLite độc lập trên local disks, edge reverse proxy, DERP/STUN riêng.

## 3. Sơ đồ underlay và IP tham khảo

```text
Proxmox lab pool: meshcontrol-lab
  vmbr-mgmt: 10.77.10.0/24      (only runner/operator)
  vmbr-client-a: 10.77.20.0/24  (A underlay)
  vmbr-client-b: 10.77.30.0/24  (B underlay)
  vmbr-service: 10.77.40.0/24   (registry/shards/DERP/edge)

  Runner / CI     10.77.10.10
  Registry        10.77.40.11
  Edge            10.77.40.12
  Shard A         10.77.40.21
  Shard B         10.77.40.22
  DERP/STUN       10.77.40.30
  Client A1/A2    10.77.20.11 / .12
  Client B1/B2    10.77.30.11 / .12
```

Đây chỉ là kế hoạch địa chỉ; agent phải kiểm tra tránh xung đột với mạng Proxmox hiện hữu. Nếu host hỗ trợ VLAN, dùng SDN VNet hoặc VLAN-backed Linux bridges tương ứng. Firewall chặn A↔B underlay: nếu client A ping được B qua LAN, phép thử “VPN ngăn liên lạc” không hợp lệ. Mở đúng đường đến edge/DERP/DNS/Registry cần thiết; network faults phải inject có kiểm soát.

## 4. Ma trận VM (có thể chỉnh theo host)

| VM | vCPU | RAM | Disk | Nhận xét |
|---|---:|---:|---:|---|
| `mc-ci` | 8 | 16–32 GiB | 120 GiB | Docker/test runner+Go build |
| `mc-placement` | 2 | 4 GiB | 30 GiB | PostgreSQL registry |
| `mc-edge` | 2 | 2 GiB | 20 GiB | Reverse proxy, certs |
| `mc-shard-01` | 4 | 8 GiB | 50 GiB | SQLite data 01 |
| `mc-shard-02` | 4 | 8 GiB | 50 GiB | SQLite data 02 |
| `mc-relay` | 2 | 2 GiB | 20 GiB | DERP + STUN |
| `mc-client-a1,a2,b1,b2` | 1 mỗi máy | 1–2 GiB | 12 GiB | Real Tailscale clients |

Tổng đặt trước khi chạy đủ cấu hình (với CI 24 GiB và client 2 GiB): khoảng **26 vCPU, 56 GiB RAM guest, 338 GiB disk guest**; chưa tính overhead của Proxmox, backup snapshots, image cache và workload burst. Với một host vật lý, cân nhắc **ít nhất ~96 GiB RAM và SSD 500 GiB trở lên** để có headroom, hoặc triển khai theo từng giai đoạn trên host nhỏ hơn. Đây không phải chuẩn phần cứng bắt buộc.

Lab quy mô nhỏ: dùng Docker namespaces/containers cho client unit test; vẫn giữ các VM isolated để chứng minh routing/NAT/DERP và host failure. `mc-ci` có thể đặt ngoài Proxmox nếu CI runner đã có.

## 5. Quyền Proxmox và policy an toàn

- Tạo Proxmox Pool độc lập `meshcontrol-lab`, đặt ID VM/disk/netnames trong một namespace để tránh thao tác nhầm.
- Tạo **API token privilege-separated** cho agent/CI, chỉ có các quyền tối thiểu trên pool và storage/template sử dụng, không gán `Administrator` ở `/` hoặc root@pam.
- Tránh các operation provider cần root SSH; nếu provider yêu cầu root-only, **ghi blocker** và người vận hành làm thủ công hoặc thay phương án. Không cung cấp root SSH cho agent như fallback tự động.
- Secrets dùng env / secret manager và TLS verification thật; `*.tfstate` chứa thông tin nhạy cảm, không lưu trong Git/ZIP public.
- Không cấp public IP của host PVE hoặc giao diện `:8006` cho Internet.
- Tắt SSH password auth trên VM test, dùng cloud-init SSH key công khai. Tất cả service credential chỉ là test fixtures xoay vòng được.

## 6. Pipeline IaC bắt buộc

```text
LAB00 — Người vận hành cấp pool/storage/network/permission và kiểm tra PVE version
LAB01 — Agent tạo infra/lab/*.tf + cloud-init + Ansible, chạy fmt/validate
LAB02 — Agent chạy tofu plan, export bản tóm tắt create/update/delete cho reviewer
LAB03 — Human approves apply trên lab pool
LAB04 — Agent/operator áp dụng bằng credential giới hạn, chụp inventory + IP
LAB05 — Ansible bootstrap máy, cài Tailscale client và Go binaries
LAB06 — E2E self-tests (TLS, trust CA, direct underlay isolation, DERP)
LAB07 — Snapshot/backup only lab (không là backup cơ chế cũ)
LAB08 — Human approves destroy; xác minh only lab resources trong state
```

Lưu ý: `tofu plan` với provider thực có thể đọc API và ghi state local; chỉ dùng credentials đúng scope; không cho phép `apply -auto-approve` hay `destroy -auto-approve`.

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
- DNS và TLS tương ứng `tailnet-a.mesh.test`, `tailnet-b.mesh.test` đến cùng edge.
- Underlay A→B blocked; cả A/B vẫn kết nối edge và DERP ở test scenario tương ứng.
- PVE token không thể xóa/sửa một VM outside pool lab (negative authorization test bằng API read-only hoặc controlled denied action do operator duyệt).
- E2E capture: Tailscale status, DERP map, routing logs, edge hostname mapping, packet capture đã redact.
- Failure scenarios: stop shard A, disconnect registry, restart relay, stale signed route manifest; xác minh ảnh hưởng phù hợp và không cross-tenant leak.

## 9. Không tự động xóa dữ liệu

Chỉ được hủy lab sau khi xác nhận inventory VM ID và diff (`tofu plan -destroy`), chờ human approval riêng. `terraform.tfstate` có dữ liệu thật không đồng nghĩa VM được xóa an toàn.
