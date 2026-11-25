# Stage 2 underlays as Proxmox SDN (04-proxmox-lab.md §3).
#
# HOST NETWORK CHANGE: every resource in this file changes /etc/network on the
# PVE node. scripts/check-plan.py never lets an agent self-apply them; a human
# reviews the plan first (AGENTS.md rule 3, 04-proxmox-lab.md §5).
#
# Host prerequisites (human, outside OpenTofu):
#   - /etc/network/interfaces must contain `source /etc/network/interfaces.d/*`
#     (default on PVE >= 8).
#   - IPv4 forwarding is required for routing between VNets and for SNAT.
#   - Only if stage2_sdn_dhcp = true: `apt install dnsmasq` and
#     `systemctl disable --now dnsmasq` (PVE runs per-zone instances).
#
# Topology (Gate N1, P09, owner decision Q1 2026-11-25): one simple zone,
# three VNets: client A (10.77.20.0/24), client B (10.77.30.0/24) and services
# (10.77.40.0/24, the DERP/STUN relay). A simple zone is a set of host-local
# bridges with no physical uplink; the host owns each subnet's gateway (.1),
# routes between the VNets and SNATs egress out of vmbr0. Nothing attaches to
# lan10. There is no management VNet: mc-ci (192.168.1.240, vmbr0) reaches the
# lab VMs through the host (route 10.77.0.0/16 via the PVE host).
#
# ISOLATION is enforced by the host nftables table in
# infra/lab/stage2-host-firewall.nft, applied by a human (AGENTS.md rule 3;
# runbook infra/lab/stage2-n1.md). Without it the host routes A <-> B and the
# "VPN isolation" E2E results are invalid: the N1 harness checks it first.

locals {
  stage2_vnets = var.stage2_enabled ? var.stage2_vnets : {}
}

# Applies pending SDN config after objects are removed (destroy ordering).
resource "proxmox_sdn_applier" "stage2_finalizer" {
  count = var.stage2_enabled ? 1 : 0
}

resource "proxmox_sdn_zone_simple" "stage2" {
  count = var.stage2_enabled ? 1 : 0

  id    = var.stage2_sdn_zone
  nodes = [var.pve_node]

  # PVE IPAM is required by the dnsmasq DHCP backend.
  ipam = "pve"
  dhcp = var.stage2_sdn_dhcp ? "dnsmasq" : null

  depends_on = [proxmox_sdn_applier.stage2_finalizer]
}

resource "proxmox_sdn_vnet" "stage2" {
  for_each = local.stage2_vnets

  id    = each.value.id
  zone  = proxmox_sdn_zone_simple.stage2[0].id
  alias = each.value.alias

  # isolate_ports stays off: a1<->a2 (and b1<->b2) must reach each other on
  # their own underlay for the direct P2P tests. Isolation is between VNets.
  isolate_ports = false
  vlan_aware    = false

  depends_on = [proxmox_sdn_applier.stage2_finalizer]
}

resource "proxmox_sdn_subnet" "stage2" {
  for_each = local.stage2_vnets

  cidr    = each.value.cidr
  vnet    = proxmox_sdn_vnet.stage2[each.key].id
  gateway = each.value.gateway
  snat    = each.value.snat

  # Always set: bpg/proxmox 0.123.1 cannot handle an unknown (conditional)
  # dhcp_range object at validate time. The range is inert unless the zone
  # has dhcp = "dnsmasq" (stage2_sdn_dhcp = true).
  dhcp_range = {
    start_address = each.value.dhcp_start
    end_address   = each.value.dhcp_end
  }
  dhcp_dns_server = var.stage2_sdn_dhcp ? var.stage2_dns[0] : null

  lifecycle {
    precondition {
      condition     = cidrhost(each.value.cidr, 1) == each.value.gateway
      error_message = "Each lab subnet's gateway must be its .1 host address."
    }
  }

  depends_on = [proxmox_sdn_applier.stage2_finalizer]
}

# Commits the pending SDN config (equivalent to "Apply" in the PVE SDN UI).
resource "proxmox_sdn_applier" "stage2" {
  count = var.stage2_enabled ? 1 : 0

  lifecycle {
    replace_triggered_by = [
      proxmox_sdn_zone_simple.stage2,
      proxmox_sdn_vnet.stage2,
      proxmox_sdn_subnet.stage2,
    ]
  }

  depends_on = [
    proxmox_sdn_zone_simple.stage2,
    proxmox_sdn_vnet.stage2,
    proxmox_sdn_subnet.stage2,
  ]
}
