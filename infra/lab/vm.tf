resource "proxmox_virtual_environment_vm" "mc_ci" {
  name        = "mc-ci"
  description = "mesh-controlplane: Go build, Docker integration tests, CI runner. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "ci"]

  node_name = var.pve_node
  vm_id     = var.mc_ci.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_ci.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_ci.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_ci.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_ci_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_ci.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_ci.mac
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
