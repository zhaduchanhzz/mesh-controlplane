# cloud-init user-data uploaded to local:snippets (owner-approved 2026-11-22).
# Contains only a public key; no secrets.
resource "proxmox_virtual_environment_file" "mc_ci_user_data" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "snippets"

  source_raw {
    file_name = "mc-ci-user-data.yaml"
    data = templatefile("${path.module}/cloud-init/mc-ci.yaml.tftpl", {
      hostname       = "mc-ci"
      ssh_public_key = trimspace(file(var.ssh_public_key_file))
    })
  }

  # Only read at VM creation; PVE does not return the content on import, and
  # the key here changed (lab access rebuilt 2026-11-23) after the VM was made.
  lifecycle {
    ignore_changes = [source_raw]
  }
}
