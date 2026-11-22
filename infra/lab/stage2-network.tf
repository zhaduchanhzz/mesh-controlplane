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
# Topology: one simple zone, four VNets. A simple zone is a set of host-local
# bridges with no physical uplink; the host owns each subnet's gateway (.1) and
# routes between all VNets of the zone. SNAT masquerades egress out of the
# host's default-route interface (vmbr0). Nothing attaches to lan10.
#
# !!! ISOLATION IS NOT ENFORCED BY THIS FILE !!!
# Because the host routes between VNets, client-a (10.77.20.0/24) and client-b
# (10.77.30.0/24) CAN reach each other through the host once this is applied.
# The "VPN isolation" E2E tests are invalid until A<->B is blocked.
#
# TODO(human-approval, AGENTS.md rule 3): PVE firewall rules - NOT IMPLEMENTED.
#   These are host firewall changes; an agent must not write or apply them
#   without an approved ticket and a reviewed diff. Proposed policy:
#     - DROP forward 10.77.20.0/24 <-> 10.77.30.0/24 (both directions).
#     - client_a/client_b -> services: allow only edge 10.77.40.12 tcp/443 and
#       relay 10.77.40.30 tcp/443 + udp/3478; drop the rest of 10.77.40.0/24
#       (registry and shards are not client-facing).
#     - mgmt 10.77.10.0/24 -> all lab VNets: allow tcp/22 (+ ICMP).
#     - all lab VNets -> host gateway addresses (10.77.x.1): drop tcp/8006 and
#       tcp/22, so lab VMs cannot reach the PVE API or host SSH.
#     - lab VNets -> 192.168.1.0/24 (home LAN): drop, except as approved.
#   Then run the LAB06 negative test: ping/TCP from client-a to client-b fails.

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
  dhcp_dns_server = var.stage2_sdn_dhcp ? var.network.dns[0] : null

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
