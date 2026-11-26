# mc-load: second load-generator box for P14f F5 (owner decision 2026-11-27:
# no-ceiling capacity runs; extra load VM in the lab pool, create only).
# capload clients run here (and on mc-ci) so the load generator does not cap
# the measured limit of the server on mc-bench. Stopped unless a run needs it
# (host RAM time-sharing, P14 decision).
variable "mc_load" {
  description = "Load-generator VM (capload)."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7710
    ipv4      = "192.168.1.249"
    mac       = "BC:24:11:77:02:49"
    cores     = 12
    memory_mb = 8192
    disk_gb   = 40
  }

  validation {
    condition     = var.mc_load.vm_id >= 7700 && var.mc_load.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

resource "proxmox_virtual_environment_file" "mc_load_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-load-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-ci.yaml.tftpl", {
      hostname       = "mc-load"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }

  # Only read at VM creation.
  lifecycle {
    ignore_changes = [source_raw]
  }
}

resource "proxmox_virtual_environment_vm" "mc_load" {
  name        = "mc-load"
  description = "mesh-controlplane: P14f capload load generator. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "bench", "load"]

  node_name = var.pve_node
  vm_id     = var.mc_load.vm_id
  pool_id   = var.lab_pool
  on_boot   = false
  # Stopped unless a capacity run needs it: `-var mc_load_running=true`.
  started = var.mc_load_running

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_load.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_load.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_load.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_load_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_load.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_load.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    prevent_destroy = true
    # The image a disk was created from and the cloud-init snippet only act
    # at creation, and PVE does not report them back.
    ignore_changes = [disk[0].import_from, initialization[0].user_data_file_id]
  }
}

output "mc_load" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_load.name
    vm_id = proxmox_virtual_environment_vm.mc_load.vm_id
    ipv4  = var.mc_load.ipv4
  }
}

variable "mc_load_running" {
  description = "Start mc-load (7710). Off by default to free host RAM; set true only while a capacity run needs a second load host."
  type        = bool
  default     = false
}
