# mc-pg-01/02: the two PostgreSQL HA VMs of Gate N3 (LAB-P13, ADR 0011,
# owner-approved 2026-11-26: decisions D1 and D7 in tickets/P13.md).
#
#   mc-pg-01  VMID 7708  192.168.1.247  PostgreSQL 16 (Patroni) + etcd
#   mc-pg-02  VMID 7709  192.168.1.248  same, starts as the synchronous replica
#   (mc-db 7704 stays: third etcd member (witness), later pgBackRest repository
#    and monitoring; it is managed by infra/lab/db, not here)
#
# A root of its own with its own state, like infra/lab/db: the infra/lab root
# never reads this directory, so nothing here changes the live lab plan. It
# only creates; it never touches an existing VM. Run from WSL as
# infra/lab/README.md describes (state: keep terraform.tfstate in the main
# checkout's infra/lab/pg/, gitignored, plus a dated copy in
# %USERPROFILE%/.mesh-lab/tofu-state/):
#
#   bash infra/lab/scripts/tofu.sh -chdir=pg init
#   bash infra/lab/scripts/tofu.sh -chdir=pg plan -out=pg.tfplan
#   bash infra/lab/scripts/tofu.sh -chdir=pg show -json pg.tfplan \
#     | python3 infra/lab/scripts/check-plan.py
#
# Software (PostgreSQL 16, Patroni, etcd, chrony, softdog, nftables) is
# installed by setup-pg.sh (P13a); PgBouncer/HAProxy by P13b.

terraform {
  required_version = ">= 1.13.0"

  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "0.123.1"
    }
  }
}

# Credentials come only from the environment (scripts/tofu.sh); TLS is
# verified against the pinned PVE CA (never insecure, AGENTS.md rule 4).
provider "proxmox" {
  insecure = false

  ssh {
    agent    = false
    username = "root"

    node {
      name    = var.pve_node
      address = var.pve_ssh_address
    }
  }
}

variable "pve_node" {
  type    = string
  default = "hanh"
}

variable "pve_ssh_address" {
  type = string
}

variable "lab_pool" {
  type    = string
  default = "meshcontrol-lab"
}

variable "ssh_public_key_file" {
  type = string
}

variable "vm_datastore" {
  type    = string
  default = "nvme01"
}

variable "pg_vms" {
  description = "PostgreSQL HA VMs (tickets/P13.md P13a). 4 GiB each by default: the host has 125.7 GiB and 116 GiB already allocated to running VMs (2026-11-26); resize in place for the P14 benchmark."
  type = map(object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  }))
  default = {
    mc-pg-01 = { vm_id = 7708, ipv4 = "192.168.1.247", mac = "BC:24:11:77:02:47", cores = 4, memory_mb = 4096, disk_gb = 60 }
    mc-pg-02 = { vm_id = 7709, ipv4 = "192.168.1.248", mac = "BC:24:11:77:02:48", cores = 4, memory_mb = 4096, disk_gb = 60 }
  }

  validation {
    condition     = alltrue([for v in values(var.pg_vms) : v.vm_id >= 7700 && v.vm_id <= 7799])
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }

  validation {
    condition     = length(distinct([for v in values(var.pg_vms) : v.vm_id])) == length(var.pg_vms)
    error_message = "VMIDs must be unique."
  }

  validation {
    # .240-.246 are taken (mc-ci, mc-cp, mc-bench, mc-db, mc-c2, mc-c1b, mc-c2b);
    # .249 is kept free for the optional VIP fallback (ADR 0011 §D, decision D3).
    condition     = alltrue([for v in values(var.pg_vms) : contains(["192.168.1.247", "192.168.1.248"], v.ipv4)])
    error_message = "PG VMs use 192.168.1.247 and .248 (the lab's free addresses on vmbr0)."
  }

  validation {
    condition     = sum([for v in values(var.pg_vms) : v.memory_mb]) <= 16384
    error_message = "PG VMs together must stay at or below 16 GiB of guest RAM (host headroom, tickets/P13.md lab check)."
  }
}

locals {
  # Downloaded by infra/lab (image.tf); same release pin as infra/lab/db.
  ubuntu_image = "local:import/mc-ubuntu-2404-release-20261017.qcow2"
  network = {
    bridge  = "vmbr0"
    prefix  = 24
    gateway = "192.168.1.1"
    dns     = ["1.1.1.1", "1.0.0.1"]
  }
}

resource "proxmox_virtual_environment_file" "pg_user_data" {
  for_each = var.pg_vms

  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "${each.key}-user-data.yaml"
    data = templatefile("${path.module}/../cloud-init/mc-pg.yaml.tftpl", {
      hostname       = each.key
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "pg" {
  for_each = var.pg_vms

  name        = each.key
  description = "mesh-controlplane: PostgreSQL 16 HA node (Patroni + etcd, ADR 0011). Managed by OpenTofu (infra/lab/pg)."
  tags        = ["mesh-lab", "db", "pg-ha"]

  node_name = var.pve_node
  vm_id     = each.value.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = each.value.cores
    type  = "host"
  }

  memory {
    dedicated = each.value.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = local.ubuntu_image
    interface    = "scsi0"
    size         = each.value.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.pg_user_data[each.key].id

    ip_config {
      ipv4 {
        address = "${each.value.ipv4}/${local.network.prefix}"
        gateway = local.network.gateway
      }
    }

    dns {
      servers = local.network.dns
    }
  }

  network_device {
    bridge      = local.network.bridge
    mac_address = each.value.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    # Database VMs are never replaced implicitly; a replace needs human approval.
    prevent_destroy = true
    ignore_changes  = [disk[0].import_from, initialization[0].user_data_file_id]
  }
}

# Hostnames, IDs and addresses only; never secrets (04-proxmox-lab.md §7).
output "pg_vms" {
  value = { for k, v in var.pg_vms : k => { vm_id = v.vm_id, ipv4 = v.ipv4 } }
}
