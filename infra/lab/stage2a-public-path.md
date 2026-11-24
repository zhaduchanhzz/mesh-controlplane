# Stage 2a — public path for cluster c2 (chủ dự án duyệt và đã áp dụng 2026-11-24)

Quyết định chủ dự án (2026-11-24): cluster thứ hai `c2` dùng hostname công khai qua VPS, như `c1`. Tài liệu này là diff chính xác cần duyệt (AGENTS.md rule 3: firewall thật, mở cổng ra Internet). Chủ dự án duyệt ngày 2026-11-24; đã áp dụng cùng ngày, backup và kiểm chứng trong `evidence/LAB-stage2a.md`.

## Hiện trạng (đọc, không đổi)

- DNS: `c2.cp.gta5web.online` đã phân giải về `116.118.3.46` (bản ghi wildcard sẵn có) → **không cần đổi DNS**.
- TLS: cert trên mc-cp là `*.cp.gta5web.online` (Let's Encrypt, hết hạn 2027-02-20) → dùng lại cho c2.
- VPS: `mesh-cp-proxy.socket` 116.118.3.46:8443 → 10.99.77.2:8443 (c1); `mesh-boot-proxy.socket` :8444 → bootstrap; nft `inet mg_vps` có `tcp dport 8443/8444 accept` (`/etc/meshguard/vps-firewall.nft` dòng 27–28).
- Tunnel `wg-mesh` 10.99.77.0/30: VPS .1, mc-cp .2 (chỉ hai địa chỉ).

## Đường đi đề xuất

```
client → 116.118.3.46:8445 (VPS, socket proxy) → wg-mesh → mc-cp 10.99.77.2:8445 (socket proxy) → LAN → mc-c2 192.168.1.244:8443
```

Đi qua mc-cp để không đổi cấu hình WireGuard (không thêm peer, không đổi dải /30). Cổng riêng 8445 vì proxy VPS là L4 (không định tuyến theo SNI). URL của c2: `https://c2.cp.gta5web.online:8445`.

## Diff

### VPS (116.118.3.46)
1. nft live: `nft add rule inet mg_vps <chain input> tcp dport 8445 accept` (cùng chain với rule 8443/8444), và thêm dòng tương ứng sau dòng 28 của `/etc/meshguard/vps-firewall.nft`:
   `tcp dport 8445 accept  # mesh-controlplane: cluster c2 -> tunnel -> mc-cp -> mc-c2 (owner-approved 2026-11-24, Internet-wide)`; kiểm `nft -c -f` trước khi ghi.
2. Unit mới, giống `mesh-cp-proxy`:
   - `mesh-c2-proxy.socket`: `ListenStream=116.118.3.46:8445`
   - `mesh-c2-proxy.service`: `Requires=mesh-c2-proxy.socket wg-quick@wg-mesh.service`, `ExecStart=/usr/lib/systemd/systemd-socket-proxyd --connections-max=512 10.99.77.2:8445`
   - `systemctl enable --now mesh-c2-proxy.socket`
3. Không đụng HAProxy 80/443, 8443, 8444, stund, mail, WireGuard.

### mc-cp (7702)
1. nft: thêm `iifname "wg-mesh" ip saddr 10.99.77.1 tcp dport 8445 accept` (live + `/etc/nftables.conf`, kiểm `nft -c`), cạnh rule 8443/8444 hiện có.
2. Unit mới `mesh-c2-relay.socket` (`ListenStream=10.99.77.2:8445`) + `mesh-c2-relay.service` (`systemd-socket-proxyd --connections-max=512 192.168.1.244:8443`), `enable --now`.

### mc-c2 (7705, VM mới, tạo bằng plan trong lab scope)
1. Cert/key wildcard chép từ mc-cp qua SSH vào `/etc/headscale/tls/` (root:headscale 0640); không vào Git/log. Ghi chú: cert gia hạn trên mc-cp (certbot), phải chép lại trước 2027-02-20 (hoặc thêm deploy hook ở ticket sau).
2. Firewall trên mc-c2 (nft, VM lab): chỉ cho 8443 từ 192.168.1.241 (mc-cp) và LAN lab .240–.249; SSH 22 từ LAN.

## Rollback

- VPS: `systemctl disable --now mesh-c2-proxy.socket mesh-c2-proxy.service`, xóa hai unit; xóa rule nft theo handle; khôi phục `vps-firewall.nft` từ backup `*.before-stage2a-*`.
- mc-cp: tương tự với `mesh-c2-relay.*` và rule 8445; backup `/etc/nftables.conf.before-stage2a-*`.
- mc-c2: xóa cert/key.

## Kiểm chứng sau khi áp dụng

`curl https://c2.cp.gta5web.online:8445/health` → 200 với cert hợp lệ; Host lạ → 421; `/metrics` không có trên cổng công khai; HAProxy 80/443 và c1 :8443, bootstrap :8444 vẫn chạy.
