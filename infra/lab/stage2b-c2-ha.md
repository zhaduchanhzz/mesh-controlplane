# Lab giai đoạn 2b — c2 active/standby (P12, Q8a–c)

Trạng thái: chủ dự án duyệt (b) và (c) ngày 2026-11-25.

**Đã áp dụng (b) bước 1–4, 2026-11-25 03:52 UTC:**
- Binary c2 trên mc-c2 nâng lên bản P08b (cây `1923634`, mã giống `master` `968acea`; sha256 `fb8f22e9…`). Bản cũ giữ tại `/usr/local/bin/headscale-cluster.15dd9f3`.
- c2 lấy lease epoch 1 sau 2 s; `/ready` 200.
- mc-cp: `mesh-c2-relay.{socket,service}` đã `disable --now`; `mesh-c2-ha.service` active, lắng nghe `10.99.77.2:8445`.
- `https://c2.cp.gta5web.online:8445/ready` công khai trả 200 (3/3 lần). Backend `mc-c2b` DOWN vì VM chưa có.
- POC c1 (`a.cp…:8443/health`) vẫn 200.

Chưa áp: (a) VM `mc-c2b` (đang plan) và cấu hình mc-c2b.

## (a) VM `mc-c2b` — tự áp theo AGENTS rule 3

- `infra/lab/c2b.tf`: VMID 7707, IP 192.168.1.246, MAC `BC:24:11:77:02:46`, 4 vCPU / 6 GB / 30 GB, pool `meshcontrol-lab`.
- Cùng template cloud-init và cùng cấu hình với `mc-c1b`.
- Chỉ tạo mới; plan có destroy hoặc replace thì dừng.

## (b) Front door `/ready` cho c2 trên mc-cp (thay relay `:8445`)

### Hiện tại (stage 2a, `stage2a-public-path.md`)

```
client → VPS 116.118.3.46:8445 → wg-mesh → mc-cp 10.99.77.2:8445
       → mesh-c2-relay.socket/.service (systemd-socket-proxyd, --connections-max=512)
       → mc-c2 192.168.1.244:8443
```

### Đề xuất

```
client → VPS :8445 (không đổi) → wg-mesh → mc-cp 10.99.77.2:8445
       → HAProxy mesh-c2-ha.service (TCP, TLS passthrough, health check HTTPS /ready)
       → mc-c2 192.168.1.244:8443  hoặc  mc-c2b 192.168.1.246:8443 (process đang giữ lease của c2)
```

Cùng mô hình đã chạy cho c1 trong E2E P11b (9/9 ×2), lần này là dịch vụ lâu dài thay vì transient.

### Thay đổi, theo thứ tự

**1. Điều kiện trước: nâng binary c2 trên mc-c2.**
- `/ready` chỉ có từ P11b. Binary c2 hiện tại là `15dd9f3` (stage 2a), `/ready` trả 404. Nếu chuyển sang HAProxy trước, mọi backend sẽ bị đánh dấu down và c2 sẽ ngừng phục vụ.
- Cài binary `master` sau khi P08b merge, rồi `systemctl restart headscale-cluster` trên mc-c2.
- Migration chạy trên DB lâu dài `meshlab`:
  - `202611251000-cluster-leases` (P11b): chỉ thêm bảng `cluster_leases`.
  - P08b không có migration.
- Client c2 mất kết nối vài giây khi restart; không phải đăng nhập lại.
- Kiểm tra sau khi nâng: `/ready` trả 200 trên mc-c2 (từ mc-cp, `--resolve c2.cp.gta5web.online:8443:192.168.1.244`).

**2. Cấu hình HAProxy mới** `/etc/haproxy/mesh-c2.cfg` (root:root 0644):

```
# mesh-controlplane lab (P12): c2 traffic from the VPS tunnel to whichever of
# mc-c2 / mc-c2b answers /ready 200. TCP, TLS passthrough.
global
  maxconn 4096
defaults
  mode tcp
  timeout connect 3s
  timeout client 1h
  timeout server 1h
  timeout tunnel 1h
frontend c2
  bind 10.99.77.2:8445
  default_backend c2
backend c2
  option httpchk
  http-check send meth GET uri /ready ver HTTP/1.1 hdr Host c2.cp.gta5web.online
  http-check expect status 200
  default-server inter 500ms fall 2 rise 2 check check-ssl check-sni c2.cp.gta5web.online verify required ca-file /etc/ssl/certs/ca-certificates.crt on-marked-down shutdown-sessions
  server mc-c2  192.168.1.244:8443
  server mc-c2b 192.168.1.246:8443
```

Kiểm cú pháp trước khi dùng: `haproxy -c -f /etc/haproxy/mesh-c2.cfg`.

**3. Unit mới** `/etc/systemd/system/mesh-c2-ha.service`:

```
[Unit]
Description=mesh-controlplane lab: c2 front door (/ready) on 10.99.77.2:8445
After=network-online.target wg-quick@wg-mesh.service
Wants=network-online.target

[Service]
ExecStartPre=/usr/sbin/haproxy -c -q -f /etc/haproxy/mesh-c2.cfg
ExecStart=/usr/sbin/haproxy -Ws -f /etc/haproxy/mesh-c2.cfg
ExecReload=/bin/kill -USR2 $MAINPID
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
```

- Gói `haproxy` 2.8.30 đã có trên mc-cp (cài từ P11b, `haproxy.service` mặc định đang tắt và giữ nguyên tắt).
- Không đụng tới cổng 8443 hay 8444 của mc-cp.

**4. Chuyển** (c2 gián đoạn vài giây):

```
systemctl disable --now mesh-c2-relay.socket mesh-c2-relay.service
systemctl enable --now mesh-c2-ha.service
```

- Giữ nguyên file unit `mesh-c2-relay.*` để rollback; chỉ xóa sau khi P12 `Verified`.

**5. Không thay đổi:**
- VPS: socket proxy `:8445`, nft, HAProxy 80/443.
- WireGuard.
- nft trên mc-cp: rule `iifname "wg-mesh" ip saddr 10.99.77.1 tcp dport 8445 accept` đã có.
- nft trên mc-c2: đã cho 8443 từ .240–.249, gồm mc-cp .241.
- Endpoint đăng ký của c2: `https://c2.cp.gta5web.online:8445`.

### mc-c2b (sau (a))

Cấu hình theo cùng cách đã làm với `mc-c1b` trong P11b:

- **Bí mật, chép từ mc-c2 qua pipe SSH** (không qua dòng lệnh, log hay Git):
  - user `headscale`;
  - cert/key wildcard;
  - **Noise key của c2** (ADR 0005: một key cho mỗi cluster, standby dùng cùng key);
  - `pg.pass` của DB `meshlab`;
  - `/etc/headscale/cluster/config.yaml` của c2.
- **Unit** `headscale-cluster.service` giống mc-c2, `--cluster-id mc-c2`.
  - Process ở trạng thái standby cho đến khi giữ lease.
- **nftables riêng của mc-c2b:**
  - SSH 22 từ 192.168.1.0/24.
  - 8443 từ 192.168.1.240–249.
  - Còn lại drop.

### Kiểm chứng sau khi áp

- Từ Internet:
  - `https://c2.cp.gta5web.online:8445/ready` → 200;
  - client c2 hiện có vẫn kết nối.
- `curl --resolve` từ mc-cp:
  - mc-c2 `/ready` 200;
  - mc-c2b `/ready` 503 (standby), `/health` 200.
- `systemctl stop headscale-cluster` trên mc-c2:
  - mc-c2b lên active trong ≤ ~11 s;
  - `/ready` công khai trở lại 200;
  - client không phải đăng nhập lại;
  - bật lại mc-c2 thì nó thành standby.
- Ghi lại kết quả vào `evidence/LAB-stage2b-c2.md`.

### Rollback

```
systemctl disable --now mesh-c2-ha.service
systemctl enable --now mesh-c2-relay.socket
```

Relay cũ nối thẳng mc-c2, như stage 2a.

Nếu cần lùi binary c2: cài lại bản `15dd9f3`. Bảng `cluster_leases` ở lại, bản cũ bỏ qua nó (migration chỉ thêm, không xóa).

Trên mc-c2b: `systemctl disable --now headscale-cluster`, xóa Noise key, `pg.pass` và cert. VM giữ lại; xóa VM thì phải có duyệt riêng.

## (c) `mesh-placement` trên mc-cp — chỉ trong lúc chạy E2E

- Không thêm dịch vụ lâu dài.
- Giống harness P08b (`infra/lab/e2e/p08b/run.sh`):
  - unit transient `mesh-placement-e2e`, user `headscale`;
  - listen `127.0.0.1:9443` (chỉ loopback);
  - mTLS với CA tạm sinh trên mc-cp trong `/run/<harness>` (tmpfs, 0751 root:headscale, key 0600/0640);
  - operator `spiffe://mesh/operator/e2e`;
  - dùng config DB của chính lần E2E (DB tạm trên mc-db).
- Cleanup dừng unit, xóa `/run/<harness>` và kiểm lại bằng `! test -e`.
- Không mở cổng, không đổi nft.
- Muốn có `mesh-placement` lâu dài (CA thật, operator thật) thì cần ticket và duyệt riêng.

## Cần chủ dự án duyệt

1. (b) bước 1–4: nâng binary c2 (sau khi P08b merge) và chuyển `:8445` của mc-cp từ relay sang HAProxy `mesh-c2-ha`.
2. Cấu hình mc-c2b: chép Noise key, `pg.pass` và cert của c2 sang mc-c2b.
3. (c) như trên: chỉ transient trong E2E.
