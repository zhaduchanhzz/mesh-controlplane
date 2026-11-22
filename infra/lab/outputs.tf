# Hostnames, IDs and addresses only; never secrets (04-proxmox-lab.md §7).
output "mc_ci" {
  value = {
    name  = proxmox_virtual_environment_vm.mc_ci.name
    vm_id = proxmox_virtual_environment_vm.mc_ci.vm_id
    ipv4  = var.mc_ci.ipv4
    ssh   = "ssh -i <mesh-lab key> mesh@${var.mc_ci.ipv4}"
  }
}
