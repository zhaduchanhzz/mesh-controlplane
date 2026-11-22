# mc-bench: isolated benchmark box for P00b (single-process 1k/5k/10k nodes), so
# measurements do not compete with mc-ci tests or the public mc-cp endpoint.
# Sized like a dedicated shard; may later be reused as one.
variable "mc_bench" {
  description = "Benchmark VM."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7703
    ipv4      = "192.168.1.242"
    mac       = "BC:24:11:77:02:42"
    cores     = 8
    memory_mb = 24576
    disk_gb   = 80
  }

  validation {
    condition     = var.mc_bench.vm_id >= 7700 && var.mc_bench.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}

resource "proxmox_virtual_environment_file" "mc_bench_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-bench-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-ci.yaml.tftpl", {
      hostname       = "mc-bench"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }
}

resource "proxmox_virtual_environment_vm" "mc_bench" {
  name        = "mc-bench"
  description = "mesh-controlplane: P00b capacity benchmarks. Managed by OpenTofu (infra/lab)."
  tags        = ["mesh-lab", "bench"]

  node_name = var.pve_node
  vm_id     = var.mc_bench.vm_id
  pool_id   = var.lab_pool
  on_boot   = false

  machine       = "q35"
  scsi_hardware = "virtio-scsi-single"

  agent {
    enabled = true
  }
  stop_on_destroy = true

  cpu {
    cores = var.mc_bench.cores
    type  = "host"
  }

  memory {
    dedicated = var.mc_bench.memory_mb
    floating  = 0
  }

  disk {
    datastore_id = var.vm_datastore
    import_from  = proxmox_download_file.ubuntu_2404.id
    interface    = "scsi0"
    size         = var.mc_bench.disk_gb
    iothread     = true
    discard      = "on"
    ssd          = true
  }

  initialization {
    datastore_id      = var.vm_datastore
    user_data_file_id = proxmox_virtual_environment_file.mc_bench_user_data.id

    ip_config {
      ipv4 {
        address = "${var.mc_bench.ipv4}/${var.network.prefix}"
        gateway = var.network.gateway
      }
    }

    dns {
      servers = var.network.dns
    }
  }

  network_device {
    bridge      = var.network.bridge
    mac_address = var.mc_bench.mac
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    prevent_destroy = true
  }
}

output "mc_bench" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_bench.name
    vm_id = proxmox_virtual_environment_vm.mc_bench.vm_id
    ipv4  = var.mc_bench.ipv4
  }
}
