# mc-c2: second cluster process (lab stage 2a, owner-approved 2026-11-24). Shares
# PostgreSQL on mc-db with mc-cp (c1); reached from the Internet through
# 116.118.3.46:8445 -> WireGuard -> mc-cp:8445 -> LAN -> this VM:8443
# (infra/lab/stage2a-public-path.md).
variable "mc_c2" {
  description = "Second cluster process VM (stage 2a)."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7705
    ipv4      = "192.168.1.244"
    mac       = "BC:24:11:77:02:44"
    cores     = 4
    memory_mb = 6144
    disk_gb   = 30
  }

  validation {
    condition     = var.mc_c2.vm_id >= 7700 && var.mc_c2.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

resource "proxmox_virtual_environment_file" "mc_c2_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-c2-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-c2.yaml.tftpl", {
      hostname       = "mc-c2"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "mc_c2" {
  name        = "mc-c2"
  description = "mesh-controlplane: cluster c2 (stage 2a), behind mc-cp and the VPS tunnel. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "control-plane"]

  node_name = var.pve_node
  vm_id     = var.mc_c2.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_c2.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_c2.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_c2.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_c2_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_c2.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_c2.mac
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

output "mc_c2" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_c2.name
    vm_id = proxmox_virtual_environment_vm.mc_c2.vm_id
    ipv4  = var.mc_c2.ipv4
  }
}
