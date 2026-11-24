# mc-cp: test control plane (owner request 2026-11-22). Clients reach it from the
# Internet through 116.118.3.46:8443 -> WireGuard -> this VM. See README "Public test endpoint".
variable "mc_cp" {
  description = "Test control-plane VM."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7702
    ipv4      = "192.168.1.241"
    mac       = "BC:24:11:77:02:41"
    cores     = 4
    memory_mb = 8192
    disk_gb   = 40
  }

  validation {
    condition     = var.mc_cp.vm_id >= 7700 && var.mc_cp.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

resource "proxmox_virtual_environment_file" "mc_cp_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-cp-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-cp.yaml.tftpl", {
      hostname       = "mc-cp"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }

  # Only read at VM creation; PVE does not return the content on import, and
  # the key here changed (lab access rebuilt 2026-11-23) after the VM was made.
  lifecycle {
    ignore_changes = [source_raw]
  }
}

resource "proxmox_virtual_environment_vm" "mc_cp" {
  name        = "mc-cp"
  description = "mesh-controlplane: test control plane behind the VPS tunnel. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "control-plane"]

  node_name = var.pve_node
  vm_id     = var.mc_cp.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_cp.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_cp.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_cp.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_cp_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_cp.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_cp.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    prevent_destroy = true
    # The image a disk was created from and the cloud-init snippet only act
    # at creation, and PVE does not report them back: after a `tofu import`
    # (2026-11-24, state rebuilt on this machine) they would read as a
    # change and force a replace of a running VM.
    ignore_changes = [disk[0].import_from, initialization[0].user_data_file_id]
  }
}

output "mc_cp" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_cp.name
    vm_id = proxmox_virtual_environment_vm.mc_cp.vm_id
    ipv4  = var.mc_cp.ipv4
  }
}
