# Stage 2: isolated SDN underlays + service/client VMs (04-proxmox-lab.md §3-4).
# Everything here is disabled by default, so a plan with default variables is
# identical to the stage-1 plan. Enable with: -var stage2_enabled=true

variable "stage2_enabled" {
  description = "Create the stage-2 SDN underlays and VMs. SDN is a host network change: human approval required (AGENTS.md rule 3)."
  type        = bool
  default     = false
}

variable "stage2_sdn_zone" {
  description = "Proxmox SDN simple zone that holds all lab VNets."
  type        = string
  default     = "mclab"

  # pve-network parse_sdn_zone_id: ^[a-z][a-z0-9]*[a-z0-9]$ and at most 8 chars.
  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.stage2_sdn_zone))
    error_message = "SDN zone IDs must be 2-8 chars, lowercase letter first, then [a-z0-9]."
  }
}

variable "stage2_sdn_dhcp" {
  description = <<-EOT
    Enable PVE SDN DHCP (dnsmasq) on the zone and a DHCP range on each subnet.
    Off by default: every lab VM gets a static address through cloud-init.
    Turning it on also needs `dnsmasq` installed on the PVE host (host change,
    human approval).
  EOT
  type        = bool
  default     = false
}

variable "stage2_vnets" {
  description = "Lab underlays. Keys are referenced by stage2_vms[*].net."
  type = map(object({
    id      = string # VNet ID == Linux bridge name on the host
    alias   = string
    cidr    = string
    gateway = string # host-side address of the VNet bridge
    snat    = bool   # masquerade egress through the host uplink
    # Used only when stage2_sdn_dhcp = true; kept clear of the static .1x-.3x hosts.
    dhcp_start = string
    dhcp_end   = string
  }))
  default = {
    client_a = {
      id         = "mccla"
      alias      = "mesh-lab client-a underlay"
      cidr       = "10.77.20.0/24"
      gateway    = "10.77.20.1"
      snat       = true
      dhcp_start = "10.77.20.100"
      dhcp_end   = "10.77.20.199"
    }
    client_b = {
      id         = "mcclb"
      alias      = "mesh-lab client-b underlay"
      cidr       = "10.77.30.0/24"
      gateway    = "10.77.30.1"
      snat       = true
      dhcp_start = "10.77.30.100"
      dhcp_end   = "10.77.30.199"
    }
    services = {
      id         = "mcsvc"
      alias      = "mesh-lab services (DERP relay)"
      cidr       = "10.77.40.0/24"
      gateway    = "10.77.40.1"
      snat       = true
      dhcp_start = "10.77.40.100"
      dhcp_end   = "10.77.40.199"
    }
  }

  # pve-network parse_sdn_vnet_id: same rule as zones, at most 8 chars
  # (the VNet ID becomes a host bridge name).
  validation {
    condition     = alltrue([for v in values(var.stage2_vnets) : can(regex("^[a-z][a-z0-9]{1,7}$", v.id))])
    error_message = "SDN VNet IDs must be 2-8 chars, lowercase letter first, then [a-z0-9]."
  }

  # Never collide with host bridges outside the lab (vmbr0, OPNsense's lan10).
  validation {
    condition     = alltrue([for v in values(var.stage2_vnets) : startswith(v.id, "mc")])
    error_message = "Lab VNet IDs must start with \"mc\" so they cannot shadow vmbr0, lan10 or other host bridges."
  }

  validation {
    condition     = alltrue([for v in values(var.stage2_vnets) : can(cidrhost(v.cidr, 0)) && startswith(v.cidr, "10.77.")])
    error_message = "Lab subnets must be valid CIDRs inside 10.77.0.0/16."
  }

  validation {
    condition     = length(distinct([for v in values(var.stage2_vnets) : v.id])) == length(var.stage2_vnets)
    error_message = "VNet IDs must be unique."
  }
}

variable "stage2_vms" {
  description = "Stage-2 VMs for Gate N1 (P09, owner decision Q1 2026-11-25): the DERP/STUN relay and four Tailscale clients, A and B on separate underlays. Clusters, PostgreSQL and bootstrap stay on the stage-1/2a/2b VMs on vmbr0."
  type = map(object({
    vm_id     = number
    role      = string
    net       = string # key of stage2_vnets
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  }))
  default = {
    mc-relay     = { vm_id = 7714, role = "relay", net = "services", ipv4 = "10.77.40.30", mac = "BC:24:11:77:40:30", cores = 2, memory_mb = 2048, disk_gb = 20 }
    mc-client-a1 = { vm_id = 7715, role = "client", net = "client_a", ipv4 = "10.77.20.11", mac = "BC:24:11:77:20:11", cores = 1, memory_mb = 2048, disk_gb = 12 }
    mc-client-a2 = { vm_id = 7716, role = "client", net = "client_a", ipv4 = "10.77.20.12", mac = "BC:24:11:77:20:12", cores = 1, memory_mb = 2048, disk_gb = 12 }
    mc-client-b1 = { vm_id = 7717, role = "client", net = "client_b", ipv4 = "10.77.30.11", mac = "BC:24:11:77:30:11", cores = 1, memory_mb = 2048, disk_gb = 12 }
    mc-client-b2 = { vm_id = 7718, role = "client", net = "client_b", ipv4 = "10.77.30.12", mac = "BC:24:11:77:30:12", cores = 1, memory_mb = 2048, disk_gb = 12 }
  }

  validation {
    condition     = alltrue([for v in values(var.stage2_vms) : v.vm_id >= 7710 && v.vm_id <= 7719])
    error_message = "Stage-2 VMIDs must stay within 7710-7719 (lab range 7700-7799, AGENTS.md rule 3)."
  }

  validation {
    condition     = length(distinct([for v in values(var.stage2_vms) : v.vm_id])) == length(var.stage2_vms)
    error_message = "Stage-2 VMIDs must be unique."
  }

  validation {
    condition     = length(distinct([for v in values(var.stage2_vms) : upper(v.mac)])) == length(var.stage2_vms)
    error_message = "Stage-2 MAC addresses must be unique."
  }

  validation {
    condition     = alltrue([for k in keys(var.stage2_vms) : startswith(k, "mc-")])
    error_message = "Lab VM names must start with \"mc-\"."
  }
}

variable "stage2_memory_budget_mb" {
  description = "Upper bound on the sum of stage2_vms[*].memory_mb (guest RAM). Default 12 GiB: relay 2 GiB + four 2 GiB clients, with headroom."
  type        = number
  default     = 12288

  validation {
    # Host: 125.7 GiB total; ~84 GiB free with VMs 100-102 and mc-ci (24 GiB) running.
    condition     = var.stage2_memory_budget_mb > 0 && var.stage2_memory_budget_mb <= 81920
    error_message = "stage2_memory_budget_mb must be between 1 and 81920 MiB (keeps host headroom)."
  }
}

variable "stage2_dns" {
  description = "Resolvers of the stage-2 VMs. Public, so the host firewall (stage2-host-firewall.nft) can drop every lab -> home LAN packet without a DNS exception."
  type        = list(string)
  default     = ["1.1.1.1", "9.9.9.9"]
}
