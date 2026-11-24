# mc-c1b: standby process of cluster c1 (P11b, owner-approved 2026-11-25).
# Same binary, same Noise key and the same PostgreSQL as the c1 process on
# mc-cp; serves only while it holds the cluster lease. mc-cp's HAProxy sends
# c1 traffic (from the VPS tunnel) to whichever of mc-cp / mc-c1b answers
# /ready. No public port of its own. Reuses the mc-c2 cloud-init template
# (same packages, hostname templated).
variable "mc_c1b" {
  description = "Standby process of cluster c1 (P11b)."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7706
    ipv4      = "192.168.1.245"
    mac       = "BC:24:11:77:02:45"
    cores     = 4
    memory_mb = 6144
    disk_gb   = 30
  }

  validation {
    condition     = var.mc_c1b.vm_id >= 7700 && var.mc_c1b.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

resource "proxmox_virtual_environment_file" "mc_c1b_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-c1b-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-c2.yaml.tftpl", {
      hostname       = "mc-c1b"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "mc_c1b" {
  name        = "mc-c1b"
  description = "mesh-controlplane: standby process of cluster c1 (P11b), behind mc-cp HAProxy. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "control-plane"]

  node_name = var.pve_node
  vm_id     = var.mc_c1b.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_c1b.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_c1b.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_c1b.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_c1b_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_c1b.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_c1b.mac
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

output "mc_c1b" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_c1b.name
    vm_id = proxmox_virtual_environment_vm.mc_c1b.vm_id
    ipv4  = var.mc_c1b.ipv4
  }
}
