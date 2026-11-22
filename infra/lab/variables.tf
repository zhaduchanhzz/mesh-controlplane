variable "pve_node" {
  description = "Proxmox node name."
  type        = string
  default     = "hanh"
}

variable "pve_ssh_address" {
  description = "Address the provider uses to SSH into the node (snippet upload only)."
  type        = string
}

variable "lab_pool" {
  description = "Pool every lab VM must belong to (AGENTS.md rule 3-4)."
  type        = string
  default     = "meshcontrol-lab"
}

variable "ssh_public_key_file" {
  description = "Public key installed for the lab admin user. The private key never enters the repo."
  type        = string
}

variable "vm_datastore" {
  description = "Datastore for VM disks and cloud-init drives."
  type        = string
  default     = "nvme01"
}

variable "network" {
  description = "Stage-1 underlay: the home LAN on vmbr0 (static addresses .240-.249 reserved for the lab)."
  type = object({
    bridge  = string
    prefix  = number
    gateway = string
    dns     = list(string)
  })
  default = {
    bridge  = "vmbr0"
    prefix  = 24
    gateway = "192.168.1.1"
    dns     = ["1.1.1.1", "1.0.0.1"]
  }
}

variable "mc_ci" {
  description = "Build/test/CI runner VM."
  type = object({
    vm_id     = number
    ipv4      = string
    mac       = string
    cores     = number
    memory_mb = number
    disk_gb   = number
  })
  default = {
    vm_id     = 7701
    ipv4      = "192.168.1.240"
    mac       = "BC:24:11:77:02:40"
    cores     = 8
    memory_mb = 24576
    disk_gb   = 120
  }

  validation {
    condition     = var.mc_ci.vm_id >= 7700 && var.mc_ci.vm_id <= 7799
    error_message = "Lab VMIDs must stay within 7700-7799 (AGENTS.md rule 3)."
  }
}
