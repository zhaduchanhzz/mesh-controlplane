# mc-db: the lab's long-lived PostgreSQL + PgBouncer (ADR 0005 shared
# PostgreSQL, ADR 0006 PgBouncer), owner-approved 2026-11-24.
#
# A root of its own, with its own state: the infra/lab state lives on another
# machine, and planning infra/lab without it would try to create mc-ci, mc-cp
# and mc-bench again. This root only manages mc-db and its cloud-init snippet,
# and uses the Ubuntu image infra/lab downloaded (by file id, not as a
# resource, so this root never re-downloads or deletes it).
#
#   bash infra/lab/scripts/tofu.sh -chdir=db plan -out=db.tfplan
#   tofu show -json db.tfplan | python3 scripts/check-plan.py
#
# PostgreSQL/PgBouncer themselves are installed by setup-db.sh, not here.

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

variable "mc_db" {
  description = "Lab database VM."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7704
    ipv4      = "192.168.1.243"
    mac       = "BC:24:11:77:02:43"
    cores     = 4
    memory_mb = 8192
    disk_gb   = 60
  }

  validation {
    condition     = var.mc_db.vm_id >= 7700 && var.mc_db.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

locals {
  # Downloaded by infra/lab (image.tf); same release pin.
  ubuntu_image = "local:import/mc-ubuntu-2404-release-20261017.qcow2"
  network = {
    bridge  = "vmbr0"
    prefix  = 24
    gateway = "192.168.1.1"
    dns     = ["1.1.1.1", "1.0.0.1"]
  }
}

resource "proxmox_virtual_environment_file" "mc_db_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-db-user-data.yaml"
    data = templatefile("${path.module}/../cloud-init/mc-ci.yaml.tftpl", {
      hostname       = "mc-db"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "mc_db" {
  name        = "mc-db"
  description = "mesh-controlplane: lab PostgreSQL 16 + PgBouncer (ADR 0005/0006). Managed by OpenTofu (infra/lab/db)."
  tags        = ["mesh-lab", "db"]

  node_name = var.pve_node
  vm_id     = var.mc_db.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_db.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_db.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = local.ubuntu_image
    interface    = "scsi0"
    size         = var.mc_db.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_db_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_db.ipv4}/${local.network.prefix}"
        gateway = local.network.gateway
      }
    }

    dns {
      servers = local.network.dns
    }
  }

  network_device {
    bridge      = local.network.bridge
    mac_address = var.mc_db.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    # The lab database is never replaced implicitly; a replace needs human approval.
    prevent_destroy = true
  }
}

output "mc_db" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_db.name
    vm_id = proxmox_virtual_environment_vm.mc_db.vm_id
    ipv4  = var.mc_db.ipv4
  }
}
