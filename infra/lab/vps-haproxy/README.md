# VPS: separate HAProxy for the lab ports 8443–8445 (P13f, D9a) — NOT APPLIED

Owner decision 2026-11-26: B7 per-IP limiting of the public bootstrap uses PROXY protocol v2, **only** by replacing the forwarders of the lab ports 8443–8445 on the VPS (116.118.3.46) with a separate HAProxy instance. The MeshGuard HAProxy (80/443), stund (3478/udp), postfix/opendkim (25/587) and WireGuard (51820/udp) are not touched. This directory is the change; the coordinator shows this diff to the owner before anything runs (AGENTS rule 3).

## Exact diff on the VPS

| | Before (today, `infra/lab/stage2a-public-path.md`, `infra/lab/README.md`) | After |
|---|---|---|
| 116.118.3.46:8443 | `mesh-cp-proxy.socket` → `systemd-socket-proxyd --connections-max=512 10.99.77.2:8443` | `mesh-lab-haproxy` frontend `lab_c1` → `10.99.77.2:8443`, TCP, **no** PROXY header, `maxconn 512` |
| 116.118.3.46:8444 | `mesh-boot-proxy.socket` → socket-proxyd → `10.99.77.2:8444` | frontend `lab_bootstrap` → `10.99.77.2:8444` **`send-proxy-v2`**, `maxconn 512` |
| 116.118.3.46:8445 | `mesh-c2-proxy.socket` → socket-proxyd → `10.99.77.2:8445` | frontend `lab_c2` → `10.99.77.2:8445`, TCP, no PROXY header, `maxconn 512` |
| Files added | — | `/etc/mesh-lab-haproxy/haproxy.cfg` (= `haproxy.cfg`), `/etc/systemd/system/mesh-lab-haproxy.service` (= `mesh-lab-haproxy.service`) |
| Units | `mesh-{cp,boot,c2}-proxy.socket` enabled + active | the three sockets **disabled + stopped**, their `.service` stopped; unit files left as they are (rollback re-enables them). `mesh-lab-haproxy.service` enabled + active |
| Process | 3 × `systemd-socket-proxyd` (root, started on demand) | 1 × HAProxy master + worker, systemd `DynamicUser`, no capabilities, `ProtectSystem=strict` |
| nftables `inet mg_vps` | `tcp dport 8443/8444/8445 accept` | **unchanged** (apply.sh compares the ruleset hash before/after and rolls back on any change) |
| WireGuard `wg-mesh` (51821/udp), `wg0`/51820 | — | **unchanged** |
| MeshGuard HAProxy 80/443, stund 3478, postfix/opendkim 25/587 | — | **unchanged** (apply.sh records their listeners before/after and rolls back on any difference) |
| Packages | — | **none installed**: apply.sh stops if `/usr/sbin/haproxy` (≥ 2.0) is missing; installing it would be a separate decision |

What clients see: on 8443/8445 the same bytes reach mc-cp (TLS end to end, cluster listeners unchanged). On 8444 mc-cp's bootstrap receives a 28-byte PROXY v2 header (client IPv4 + port) before the TLS ClientHello.

## Dependency: mesh-bootstrap must be started with PROXY v2

After the switch, a `mesh-bootstrap` on `10.99.77.2:8444` **must** run the P13f binary with

```
-proxy-protocol-from 10.99.77.1/32
```

(the VPS end of wg-mesh). Without it the header is read as the start of TLS and every bootstrap handshake through the VPS fails; with it but from another peer, a PROXY header is refused (anti-spoofing). The E2E harnesses that start the bootstrap (`infra/lab/e2e/{n1/cases3.sh,p05,p12,p12b,twocluster}/run.sh`, `BOOT_LISTEN=10.99.77.2:8444`) take it from the environment: run them with `BOOT_PROXY_PROTOCOL_FROM=10.99.77.1/32` once the HAProxy is applied (default off, as today; P13 integration). With the flag, the per-address limit defaults to 10 req/s, burst 20 (`-ip-rate`, `-ip-burst`; IPv6 per /64).

Recommended order: (1) merge P13f; (2) run the harnesses with `BOOT_PROXY_PROTOCOL_FROM=10.99.77.1/32` from then on; (3) owner approves; (4) `apply.sh`; (5) run one bootstrap E2E case through the VPS.

## Apply (owner-approved only)

```bash
# from the laptop: copy this directory (no secret in it)
scp -r infra/lab/vps-haproxy root@116.118.3.46:/root/mesh-lab-haproxy-p13f
ssh root@116.118.3.46 'cd /root/mesh-lab-haproxy-p13f && ./apply.sh --check'   # preflight, changes nothing
ssh root@116.118.3.46 'cd /root/mesh-lab-haproxy-p13f && ./apply.sh'
```

`apply.sh`: preflight (right host, wg-mesh up, haproxy ≥ 2.0 present, the three socket units exist and hold the ports, config validates) → backup of the unit files, their enabled/active state, the untouched listeners and the nft hash to `/root/mesh-lab-haproxy-backup-<UTC>` → install the two files → disable/stop the sockets → start `mesh-lab-haproxy` → verify (haproxy holds 8443–8445; nft hash and the other listeners unchanged; `/health` of c1 and c2 through the public address). Any failure after the switch restores the sockets automatically. The switch cuts the open connections once (about a second); Tailscale clients reconnect.

## Rollback

```bash
ssh root@116.118.3.46 'cd /root/mesh-lab-haproxy-p13f && ./rollback.sh'          # sockets back, haproxy stopped
ssh root@116.118.3.46 'cd /root/mesh-lab-haproxy-p13f && ./rollback.sh --purge'  # and remove its two files
```

`mesh-bootstrap` with `-proxy-protocol-from` behind the old forwarders refuses every connection from 10.99.77.1 (no header): remove the flag on rollback (unset `BOOT_PROXY_PROTOCOL_FROM`).

## Verification after apply (by the operator)

- `ss -ltnp '( sport = :8443 or sport = :8444 or sport = :8445 )'` → haproxy; `ss -ltnp '( sport = :80 or sport = :443 )'` → the MeshGuard HAProxy as before.
- `curl https://c1.cp.gta5web.online:8443/health`, `curl https://c2.cp.gta5web.online:8445/health` → 200; `/metrics` → 404 on both (AGENTS rule 9).
- Bootstrap E2E case through `https://boot.cp.gta5web.online:8444`; on mc-cp the bootstrap log/metrics show the client's public address in the per-address bucket (`mesh_bootstrap_requests_total`, `mesh_bootstrap_proxy_refused_total` on the operator listener).
- `journalctl -u mesh-lab-haproxy` (tcplog lines carry client addresses: they stay on the VPS journal, like the forwarders' connection logs).

## Not in this change

- PROXY v2 for the cluster listeners (8443/8445): `/ts2021`, `/key`, `/verify` per-IP limits are a later ticket; their bytes are unchanged here.
- Edge rate limiting in HAProxy itself (stick tables): possible later as defence in depth; not part of the approved change.
