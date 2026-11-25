# mc-c2b: standby process of cluster c2 (P12, owner decision Q8a,
# 2026-11-25). Same binary, Noise key and PostgreSQL as the c2 process on
# mc-c2; serves only while it holds c2's cluster lease. A front door on
# mc-cp (P12, diff reviewed separately) sends c2 traffic to whichever of
# mc-c2 / mc-c2b answers /ready. No public port of its own. Same sizing and
# cloud-init template as mc-c1b.
variable "mc_c2b" {
  description = "Standby process of cluster c2 (P12)."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7707
    ipv4      = "192.168.1.246"
    mac       = "BC:24:11:77:02:46"
    cores     = 4
    memory_mb = 6144
    disk_gb   = 30
  }

  validation {
    condition     = var.mc_c2b.vm_id >= 7700 && var.mc_c2b.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

resource "proxmox_virtual_environment_file" "mc_c2b_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-c2b-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-c2.yaml.tftpl", {
      hostname       = "mc-c2b"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "mc_c2b" {
  name        = "mc-c2b"
  description = "mesh-controlplane: standby process of cluster c2 (P12), behind the c2 front door on mc-cp. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "control-plane"]

  node_name = var.pve_node
  vm_id     = var.mc_c2b.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_c2b.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_c2b.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_c2b.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_c2b_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_c2b.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_c2b.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    prevent_destroy = true
    # As for mc-c1b: the image and the cloud-init snippet only act at
    # creation and PVE does not report them back.
    ignore_changes = [disk[0].import_from, initialization[0].user_data_file_id]
  }
}

output "mc_c2b" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_c2b.name
    vm_id = proxmox_virtual_environment_vm.mc_c2b.vm_id
    ipv4  = var.mc_c2b.ipv4
  }
}
