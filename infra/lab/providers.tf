# Credentials come only from the environment (PROXMOX_VE_ENDPOINT,
# PROXMOX_VE_USERNAME, PROXMOX_VE_PASSWORD), loaded by scripts/tofu.sh from a
# file outside the repo. TLS is verified against the PVE root CA through
# SSL_CERT_FILE; never set insecure = true (AGENTS.md rule 4).
provider "proxmox" {
  insecure = false

  # SSH is used only to upload the cloud-init snippet (owner-approved).
  ssh {
    agent    = false
    username = "root"

    node {
      name    = var.pve_node
      address = var.pve_ssh_address
    }
  }
}
