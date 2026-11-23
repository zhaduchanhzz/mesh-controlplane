# Handoff: P08 đang làm dở (2026-11-23)

Tài liệu để tiếp tục trên máy khác. **Không chứa secret.** Mật khẩu root Proxmox, token Cloudflare và SSH private key không bao giờ nằm trong Git (AGENTS rule 2). Phải chép chúng sang máy mới qua kênh riêng (§3).

## 1. Trạng thái nhánh

| Nhánh | Trạng thái |
|---|---|
| `master` @ `a316f78` | P00–P05 + P05-e2e đã merge |
| `feat/P10-placement-fencing` @ `cc435f3` | P10 xong, `Verified`, **chưa merge** (chờ reviewer bảo mật fencing) |
| `feat/P08-endpoint-hardening` | **Nhánh này.** Tạo chồng lên `feat/P10-placement-fencing` (P10 đã viết lại `cluster.go` và harness E2E), đã merge `feat/P08a-endpoint-audit` (inventory + RED tests). Code đang làm dở: `go build ./...` và `go vet` (hscontrol, derp, endpointaudit, config) exit 0; **chưa chạy test** |
| `feat/P00c-peermap-perf` | Đang làm, chưa có số đo |

Trên máy mới: `git fetch && git checkout feat/P08-endpoint-hardening`.

## 2. P08: đã làm / còn lại

Chú ý tên gọi: "N1" trong bảng P08a (`evidence/P08a-endpoint-inventory.md` §4.2) là **phát hiện** P08a-N1, không phải gate N1 của PLAN.

**Đã sửa trong code (chưa test):**
- **F1, N1, N3 (listener debug/metrics):** `hscontrol/operator_access.go` `operatorOnly` bọc toàn bộ listener, kể cả `/metrics` và statsviz. Chỉ chấp nhận loopback hoặc CIDR trong config mới `metrics_operator_cidrs` (mặc định rỗng). Không bao giờ tin dải Tailscale. Config validation (`hscontrol/config/validate.go`) từ chối CIDR `/0` và CIDR chồng dải Tailscale. Đã xóa `protectedDebugHandler`.
- **F2 (`/verify`):**
  - `VerifyHandler` bắt buộc `?secret=` khớp file `derp.verify_secret_path` (tối thiểu 32 ký tự, so sánh constant-time); không cấu hình thì mọi caller nhận cùng 403.
  - `handleVerifyRequest` từ chối node đã hết hạn.
  - Logger request chỉ ghi path, không ghi query.
- **F3, N2 (DERP verify):** `derp/server` `RegisterVerifyHandler` cấp cho mỗi runtime một URL riêng `headscale-derp-verify://<random>/verify`. Scheme chỉ đăng ký một lần; URL lạ thì lỗi, và DERP (fail-open tắt) từ chối. `NewDERPServer` nhận thêm tham số `verifyURL`. `Headscale.DERPVerifyURL()` dùng cho test.
- **N5 (SSH action):** dst không tồn tại và dst của máy khác trả cùng `401 "dst node is not this machine"`.

**Còn lại cho P08:**
1. `hscontrol/cluster.go`:
   - guard Host/SNI: Host (bỏ port) phải bằng host của `server_url`; trên TLS thì SNI == Host; sai → 421 (S11c);
   - listener metrics riêng của cluster (`metrics_listen_addr`, chỉ `/metrics`, bọc `operatorOnly`);
   - `validateClusterConfig` từ chối `tailsql.enabled` (N4);
   - cập nhật thông báo `errClusterEmbeddedDERP`.
2. Test:
   - sửa `hscontrol/endpointaudit/derp_verify_two_tailnets_test.go`: verify qua `appB.DERPVerifyURL()` phải ra false với node của A, và đối chứng `appA.DERPVerifyURL()` phải ra true;
   - sửa test upstream `TestHandleVerifyRequest_AdmitsByNodeKey` (expired → false) và các test của `VerifyHandler` (giờ cần secret);
   - test SSH action nào đang chờ 404 thì đổi thành 401;
   - thêm test validate `metrics_operator_cidrs`;
   - thêm test cluster: 421 khi Host lạ, IP hoặc SNI ≠ Host.
3. Sinh lại: `go generate ./hscontrol/types/` (docs config), `HEADSCALE_UPDATE_GOLDEN=1 go test ./hscontrol/config/` (golden có thêm trường mới), `MESH_PRINT_ROUTES=1 go test -run TestEndpointInventory ./hscontrol/` rồi cập nhật `hscontrol/testdata/p08a_endpoint_allowlist.txt` và §3 của inventory P08a.
4. E2E: thêm vào `infra/lab/e2e/p05/run.sh` các ca Host lạ, raw IP và SNI ≠ Host → 421; client thật vẫn PASS 13/13.
5. Viết `evidence/source-inventory-P08.md`: RED → GREEN cho từng test P08a, cùng lệnh và exit code.
6. **Để P08b (ghi rõ trong evidence):**
   - API admin theo tailnet ở cluster mode (F4; cluster hiện không mở API nào, nên fail-closed). `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` vẫn SKIP; SKIP không phải PASS.
   - DERP verify theo tenant cho DERP ngoài ở cluster mode (S15, cần lab giai đoạn 2).
   - Rate limit, queue limit (I10, S13) → P14.

## 3. Secrets: chép tay, không qua Git

Trên máy hiện tại, tất cả nằm ở `%USERPROFILE%\.mesh-lab\` (Linux/WSL: `~/.mesh-lab/`). Chép **cả thư mục** sang máy mới bằng USB hoặc `scp` qua LAN, rồi `chmod 700 ~/.mesh-lab && chmod 600 ~/.mesh-lab/*`.

| File | Nội dung | Bí mật? |
|---|---|---|
| `proxmox.env` | `PVE_HOST`, `PVE_IP`, `PVE_CERT`, `PROXMOX_VE_ENDPOINT`, `PROXMOX_VE_USERNAME` (`root@pam`), `PROXMOX_VE_PASSWORD`, `PROXMOX_VE_SSH_USERNAME` | **Có** (mật khẩu root) |
| `cloudflare.env` | `CF_API_TOKEN`, `CF_ZONE`, `CF_ZONE_ID` (zone `gta5web.online`) | **Có** (token) |
| `mesh-lab`, `mesh-lab.pub` | SSH key vào `mesh@` trên mc-ci, mc-cp, mc-bench | private key: **Có** |
| `mesh-vps`, `mesh-vps.pub` | SSH key root vào VPS | private key: **Có** |
| `pve-root-ca.pem`, `pve-node-cert.pem` | CA và cert của PVE, để pin TLS (không dùng `insecure`) | Không |
| `known_hosts` | Host key các máy lab và VPS | Không |
| `wg-mesh.pub.env` | Public key WireGuard | Không |

**Khuyến nghị:** mật khẩu root PVE, mật khẩu VPS và hai token Cloudflare đã bị dán vào chat trước đây. Nên xoay chúng (rotate) rồi mới chép sang máy mới.

## 4. Thông tin kết nối (không bí mật)

| Thành phần | Giá trị |
|---|---|
| Proxmox | node `hanh`, `192.168.1.14:8006`. Cert PVE ghi tên `hanh.promox` và IP cũ, nên cần `/etc/hosts`: `192.168.1.14 hanh.promox`. Pool `meshcontrol-lab`, VMID `7700–7799` |
| mc-ci | VMID 7701, `mesh@192.168.1.240`, 16 vCPU / 40 GiB. Build/test Linux, Docker client E2E. Container `ts-home1` **không được dừng** |
| mc-cp | VMID 7702, `mesh@192.168.1.241`, 4 vCPU / 8 GiB. Cluster `headscale-cluster.service` (E2E), POC `headscale-tailnets.service` (mặc định chạy). nftables: 8443 chỉ từ 10.99.77.1 (wg-mesh), 22 từ LAN |
| mc-bench | VMID 7703, `mesh@192.168.1.242` |
| VPS | `root@116.118.3.46` (key `mesh-vps`, không có password auth). Host key ED25519 `SHA256:vQeQ2oUNyGXztzOSHwZyu67l2nL7hEEplWndPTOXa2w`. Forward `:8443` qua WireGuard `wg-mesh` (10.99.77.1 ↔ 10.99.77.2) tới mc-cp:8443. **Không đụng:** mg-l4proxy 80/443, mg-stund 3478, postfix/opendkim, WireGuard 51820, node `vps-ext` |
| DNS/TLS | `*.cp.gta5web.online` → VPS; cert Let's Encrypt wildcard trên mc-cp `/etc/headscale/tls/`. Cluster: `https://c1.cp.gta5web.online:8443` (cluster ID `mc-cp-c1`) |
| Không đụng | Proxmox VM 100/101/102 và mọi thứ ngoài pool lab |

## 5. Cách làm việc trên máy mới

- **OpenTofu:** `infra/lab/scripts/tofu.sh` (WSL), đọc `~/.mesh-lab/proxmox.env`. Chỉ tự apply plan đã lưu khi `infra/lab/scripts/check-plan.py` xác nhận plan chỉ create/update trong pool và dải VMID lab (AGENTS rule 3).
- **Đưa code lên mc-ci:** chỉ dùng `git bundle`, không rsync (CRLF trên Windows):
  ```bash
  git bundle create /tmp/p08.bundle feat/P08-endpoint-hardening
  scp -i ~/.mesh-lab/mesh-lab /tmp/p08.bundle mesh@192.168.1.240:~/
  # trên mc-ci: git -C ~/src/<dir> fetch ~/p08.bundle feat/P08-endpoint-hardening && git checkout -f FETCH_HEAD
  ```
- **Go:** `/usr/local/go/bin` (go 1.27.4).
- **Test PostgreSQL:** tạo `initdb` tạm (`$(ls -d /usr/lib/postgresql/*/bin|tail -1)`) trên unix socket, rồi đặt `MESH_TEST_POSTGRES_DSN`.
- **Full race:** chạy trong `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -race ./hscontrol/... ./internal/... ./cmd/...` (`TestAPIv2` cần `tofu`/`tscli`).
- **Servertest cluster:** `go test -race -tags p05cluster -run Cluster ./hscontrol/servertest/`.
- **E2E client thật:** `SSH_KEY=~/.mesh-lab/mesh-lab bash infra/lab/e2e/p05/run.sh`. Script tự dừng POC, dùng DB riêng, rồi dọn dẹp và khởi động lại POC. Kiểm artifact không còn chuỗi `hskey-auth-` chưa redact.
- **Quy tắc:** đọc `AGENTS.md`. Không merge khi chưa có lệnh rõ của chủ dự án. Lỗi isolation = BLOCK MERGE.
