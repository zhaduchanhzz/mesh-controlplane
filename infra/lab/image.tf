# Ubuntu 24.04 cloud image, pinned to a dated release and verified by SHA-256.
# Stored in local:import (owner-approved 2026-11-22). Bump both values together.
locals {
  ubuntu_release = "release-20261017"
  ubuntu_sha256  = "04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94"
}

resource "proxmox_download_file" "ubuntu_2404" {
  node_name    = var.pve_node
  datastore_id = "local"
  content_type = "import"

  url = "https://cloud-images.ubuntu.com/releases/noble/${local.ubuntu_release}/ubuntu-24.04-server-cloudimg-amd64.img"
  # The .img is qcow2; the extension tells PVE which format to import.
  file_name          = "mc-ubuntu-2404-${local.ubuntu_release}.qcow2"
  checksum           = local.ubuntu_sha256
  checksum_algorithm = "sha256"
  overwrite          = false

  # Only used for the download; not read back by `tofu import` (state
  # rebuilt 2026-11-24), where they would force a re-download.
  lifecycle {
    ignore_changes = [url, checksum, checksum_algorithm]
  }
}
