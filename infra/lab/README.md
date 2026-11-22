# infra/lab — Proxmox lab as code

Stage 1 manages one VM, `mc-ci` (VMID 7701, 192.168.1.240 on `vmbr0`): the Linux build, Docker integration and CI runner box. Policy: `AGENTS.md` rule 3–4 and `../../04-proxmox-lab.md` §5–6.

## External client outside NAT (not managed by OpenTofu)

| Field | Value |
|---|---|
| Host | `116.118.3.46` (`vnflashlearner`), public KVM VPS, Ubuntu 24.04.5, 2 vCPU, 1.9 GiB |
| Role | **Real Tailscale client on the Internet only** (P2P / DERP fallback from outside the home NAT). Never a control plane, shard, edge or relay. |
| Access | SSH key `~/.mesh-lab/mesh-vps` (`mesh-vps-test`), root. Password auth is disabled on the host. Host key ED25519 `SHA256:vQeQ2oUNyGXztzOSHwZyu67l2nL7hEEplWndPTOXa2w`. |
| Tailscale | 1.104.7 from pkgs.tailscale.com stable, `apt-mark hold`, **`tailscaled` disabled and stopped** until P09. Server minimum is v1.84 (capver 116). |
| Do not touch | Existing services: HAProxy `mg-l4proxy` (80/443), `mg-stund` (3478/udp), Postfix/OpenDKIM (25/587), WireGuard (51820/udp). `network-access-mesh` is no longer developed, but these services are left as they are. |
| Known gaps | No IPv6. Firewall: MeshGuard nftables table `inet mg_vps` (input policy drop), see "Public test endpoint". |

## Public test endpoint (owner request 2026-11-22)

```
client (Internet / home) --TLS--> 116.118.3.46:8443 (VPS: systemd-socket-proxyd, TCP only)
                                     `- WireGuard wg-mesh 10.99.77.1 <-> 10.99.77.2 (udp/51821, mc-cp dials out)
                                          `-> mc-cp (VMID 7702, 192.168.1.241): control plane :8443
```

| Piece | Where | Notes |
|---|---|---|
| `mc-cp` | `cp.tf`, `cloud-init/mc-cp.yaml.tftpl` | 8 vCPU / 24 GiB / 40 GiB. nftables (`/etc/nftables.conf`, table `mc_cp`): input drop, except tcp/8443 from `wg-mesh` 10.99.77.1 and tcp/22 from 192.168.1.0/24 |
| Tunnel | `/etc/wireguard/wg-mesh.conf` on both ends | Private keys are generated on each host and loaded via `PostUp`; never copied off the host. `AllowedIPs` is /32 on both ends, so there are no other routes. Public keys: VPS `yMlzx095IRiDj9wBbIqqEZ5P4X6bcRseQ0EMLIrHyiU=`, mc-cp `USZJgorbAlsHsJPs1rT8h3wwLhYHECPdiAs5c+6cAxI=` |
| VPS forwarder | `mesh-cp-proxy.socket` / `.service` | Listens on `116.118.3.46:8443` and forwards to `10.99.77.2:8443`. The control plane sees source 10.99.77.1, so per-client-IP limits are not possible yet |
| VPS firewall | `/etc/meshguard/vps-firewall.nft` (pre-existing MeshGuard table `inet mg_vps`, input policy drop) | Owner approved adding `udp dport 51821` and `tcp dport 8443` (Internet-wide). Backup: `vps-firewall.nft.bak-20261122-meshcp`. Rollback: restore the backup, then `nft -f /etc/meshguard/vps-firewall.nft` |
| DNS/TLS | Cloudflare zone `gta5web.online`, **DNS only** (not proxied) | Cloudflare Proxy/Tunnel cannot carry `/ts2021` (`docs/ref/integration/reverse-proxy.md:172`). **Correction (P02 E2E, 2026-11-22):** the client still tries `http://<host>:80/ts2021` first even with `:8443` in the ControlURL: `ts2021/client.go:137-142` sets `httpPort=""`, and `controlhttp/client.go:253` turns "" into the default "80". Observed with v1.84.3 and v1.104.7. On this lab those port-80 attempts reach the unrelated MeshGuard HAProxy (empty reply), so the client falls back to 8443; only metadata (Host, an encrypted Noise initiation) leaks there. The P10 edge must serve or explicitly refuse `/ts2021` on port 80. Cert: Let's Encrypt DNS-01 |

Earlier note "nftables has no rules" was wrong: the VPS has the MeshGuard firewall above.

Open for P09: the lab control plane sits behind the home NAT. This client needs a reachable control URL (port-forward, public edge, or a tunnel) and a DERP region it can reach. Each of these exposes something to the Internet, so it needs owner approval (AGENTS.md rule 3).

## Layout

| File | Purpose |
|---|---|
| `versions.tf` | OpenTofu ≥ 1.13, `bpg/proxmox` pinned (see `.terraform.lock.hcl`) |
| `providers.tf` | Credentials from env only; TLS verified; SSH used only for snippet upload |
| `variables.tf` | Node, pool, network, `mc_ci` sizing; VMID validated to 7700–7799 |
| `image.tf` | Ubuntu 24.04 cloud image, dated release + SHA-256, in `local:import` |
| `cloud-init.tf`, `cloud-init/` | User-data snippet: `mesh` user (key only), qemu-guest-agent, Docker |
| `vm.tf` | `mc-ci`, `prevent_destroy = true` |
| `scripts/pve_api.py` | Read-only-by-default PVE API client (`inventory`, `get`, `--write post/put`) |
| `scripts/runner-setup-wsl.sh` | Installs OpenTofu (checksum-verified), hosts mapping, CA bundle |
| `scripts/tofu.sh` | Runs `tofu` with credentials from `~/.mesh-lab/proxmox.env` |
| `scripts/check-plan.py` | Self-apply gate: exit 0 only for create/update inside the lab scope |
| `LAB00-operator.md` | One-time host preparation and the optional least-privilege token |

## Secrets (never in this repo)

`%USERPROFILE%\.mesh-lab\` holds `proxmox.env` (root credential), `pve-root-ca.pem`, and the `mesh-lab` SSH keypair. State (`terraform.tfstate*`) and plans (`*.tfplan`) are gitignored.

## Workflow (from WSL Ubuntu)

```bash
export MESH_LAB_DIR=/mnt/c/Users/<you>/.mesh-lab
bash infra/lab/scripts/runner-setup-wsl.sh        # once, or after a WSL reset
S=infra/lab/scripts/tofu.sh
bash $S init && bash $S fmt -check && bash $S validate
bash $S plan -out=lab.tfplan
bash $S show -json lab.tfplan | python3 infra/lab/scripts/check-plan.py
# exit 0  → the agent may: bash $S apply lab.tfplan
# exit 2  → stop; a human reviews the plan (destroy/replace/out of scope)
```

## Known limitations

- The PVE node certificate lists the old IP `192.168.0.101`, not `192.168.1.14`. The client maps `hanh.promox` → `192.168.1.14` in WSL `/etc/hosts`. The host gets its IP by DHCP; a router reservation keeps it stable.
- `192.168.1.240–.249` were free by ping/ARP on 2026-11-22, but they may sit inside the router's DHCP pool. Exclude or reserve them on the router.
- With `root@pam`, scope is enforced by `check-plan.py` and review, not by Proxmox ACLs.

## Stage 2 (draft, disabled by default)

Files: `stage2-variables.tf`, `stage2-network.tf`, `stage2-vms.tf`, `cloud-init/mc-stage2.yaml.tftpl`. With default variables (`stage2_enabled = false`) every stage-2 resource has `count`/`for_each` of zero, so the plan is the same as stage 1.

### Topology

One SDN simple zone `mclab` on node `hanh`. The host owns each `.1` gateway, routes between the VNets and SNATs egress out of `vmbr0`. `lan10` is not touched.

| VNet | Subnet | VMs (VMID, IP) |
|---|---|---|
| `mcmgmt` | 10.77.10.0/24 | none yet (runner `10.77.10.10`, see open decisions) |
| `mccla` | 10.77.20.0/24 | mc-client-a1 7715 `.11`, mc-client-a2 7716 `.12` |
| `mcclb` | 10.77.30.0/24 | mc-client-b1 7717 `.11`, mc-client-b2 7718 `.12` |
| `mcsvc` | 10.77.40.0/24 | mc-placement 7710 `.11`, mc-edge 7711 `.12`, mc-shard-01 7712 `.21`, mc-shard-02 7713 `.22`, mc-relay 7714 `.30` |

Each VM has one NIC on its VNet, a static IP through cloud-init (snippet `mc-<name>-user-data.yaml`), user `mesh` with key-only SSH, and `prevent_destroy`. Edge and relay get no `vmbr0` NIC: that would expose them to the home LAN and give a dual-homed path around the A↔B isolation. Guest RAM is 32 GiB in total by default. The plan fails if the sum of `stage2_vms[*].memory_mb` is more than `stage2_memory_budget_mb`.

SDN zone and VNet IDs are limited to 8 characters, and the provider and PVE allow only `[a-z0-9]` with a letter first. The variables validate this.

### Isolation is not enforced yet

A simple zone routes between all of its VNets, so after an apply client-a **can** reach client-b through the host, and every lab VM can reach the host's `.1` addresses (`:8006`, `:22`). The PVE firewall rules that would block this are written as a **TODO** in `stage2-network.tf` and are not implemented: they are host firewall changes. Until a human approves and applies them, the "VPN isolation" E2E results are not valid.

### Human approval

| Step | Who |
|---|---|
| Host prep: `source /etc/network/interfaces.d/*`, IPv4 forwarding, `dnsmasq` (only if `stage2_sdn_dhcp = true`) | human |
| Any `proxmox_sdn_*` create/update/delete (zone, VNets, subnets, applier) | human. `check-plan.py` always exits 2 |
| PVE firewall rules for A↔B, VNet → host, VNet → home LAN | human (TODO, not in code) |
| Stage-2 VMs and `mc-*` snippets in pool `meshcontrol-lab`, VMID 7710–7719 | agent may self-apply once the SDN exists |
| Any destroy/replace, including setting `stage2_enabled` back to false | human |

### Enable

```bash
bash $S plan -var stage2_enabled=true -out=lab.tfplan
bash $S show -json lab.tfplan | python3 infra/lab/scripts/check-plan.py   # exits 2: SDN needs a human
# After a human reviews and applies the SDN part, later VM-only plans can pass the gate.
```

Notes: every subnet always gets a `dhcp_range` (`.100–.199`). bpg/proxmox 0.123.1 fails `validate` when that attribute is conditional. The range does nothing unless the zone has `dhcp = "dnsmasq"`.
