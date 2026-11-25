# Stage-2 VMs (04-proxmox-lab.md §3-4). Disabled unless stage2_enabled = true.
#
# NIC decision: every stage-2 VM has exactly ONE NIC, on its lab VNet. The
# relay does NOT get a vmbr0 NIC: that would expose it to the home LAN and
# give a dual-homed path around the A<->B isolation. Clients reach the relay
# through the host (DERP tcp/443-449, STUN udp/3478 only, see
# stage2-host-firewall.nft); everything reaches the Internet by SNAT (cluster
# public URLs via the VPS, packages, public DNS from var.stage2_dns).

locals {
  stage2_vms = var.stage2_enabled ? var.stage2_vms : {}

  stage2_total_memory_mb = sum(concat([0], [for v in values(var.stage2_vms) : v.memory_mb]))
}

resource "proxmox_virtual_environment_file" "stage2_user_data" {
  for_each = local.stage2_vms

  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "${each.key}-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-stage2.yaml.tftpl", {
      hostname       = each.key
      role           = each.value.role
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "stage2" {
  for_each = local.stage2_vms

  name        = each.key
  description = "mesh-controlplane: ${each.value.role}. Managed by OpenTofu (infra/lab, stage 2)."
  tags        = ["mesh-lab", "stage2", each.value.role]

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
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = each.value.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.stage2_user_data[each.key].id

    ip_config {
      ipv4 {
        address = "${each.value.ipv4}/${split("/", var.stage2_vnets[each.value.net].cidr)[1]}"
        gateway = var.stage2_vnets[each.value.net].gateway
      }
    }

    dns {
      servers = var.stage2_dns
    }
  }

  network_device {
    bridge      = proxmox_sdn_vnet.stage2[each.value.net].id
    mac_address = each.value.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  # The VNet bridge exists on the host only after the SDN config is applied.
  depends_on = [proxmox_sdn_applier.stage2]

  lifecycle {
    # Lab VMs are never replaced implicitly; a replace needs human approval.
    prevent_destroy = true

    precondition {
      condition     = contains(keys(var.stage2_vnets), each.value.net)
      error_message = "stage2_vms[${each.key}].net must be a key of stage2_vnets."
    }

    precondition {
      condition = (
        cidrhost(var.stage2_vnets[each.value.net].cidr, tonumber(split(".", each.value.ipv4)[3])) == each.value.ipv4
        && each.value.ipv4 != var.stage2_vnets[each.value.net].gateway
      )
      error_message = "stage2_vms[${each.key}].ipv4 must be a host address inside its VNet subnet and not the gateway."
    }

    precondition {
      condition     = local.stage2_total_memory_mb <= var.stage2_memory_budget_mb
      error_message = "Sum of stage2_vms memory (${local.stage2_total_memory_mb} MiB) exceeds stage2_memory_budget_mb (${var.stage2_memory_budget_mb} MiB)."
    }
  }
}

# Hostnames, IDs and addresses only; never secrets (04-proxmox-lab.md §7).
output "stage2_vms" {
  value = {
    for k, v in local.stage2_vms : k => {
      vm_id = proxmox_virtual_environment_vm.stage2[k].vm_id
      role  = v.role
      vnet  = var.stage2_vnets[v.net].id
      ipv4  = v.ipv4
    }
  }
}

output "stage2_vnets" {
  value = { for k, v in local.stage2_vnets : k => { id = v.id, cidr = v.cidr, gateway = v.gateway } }
}
