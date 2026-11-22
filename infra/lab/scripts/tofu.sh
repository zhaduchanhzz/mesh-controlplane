#!/usr/bin/env bash
# Run OpenTofu in infra/lab with lab credentials loaded from outside the repo.
#   bash infra/lab/scripts/tofu.sh plan -out=lab.tfplan
# Credentials stay in the environment of this process only.
set -euo pipefail

if [ -z "${MESH_LAB_DIR:-}" ]; then
	win_home=$(cmd.exe /c 'echo %USERPROFILE%' 2>/dev/null | tr -d '\r')
	MESH_LAB_DIR="$(wslpath "$win_home")/.mesh-lab"
fi

set -a
# shellcheck disable=SC1091
. "$MESH_LAB_DIR/proxmox.env"
set +a
unset PVE_CERT # Git-Bash path; Go uses the bundle below.

# The PVE cert's SAN has PVE_HOST but not the current IP. WSL regenerates
# /etc/hosts when it restarts, so re-add the mapping on every run.
if ! grep -qE "^[0-9.]+[[:space:]]+$PVE_HOST([[:space:]]|\$)" /etc/hosts; then
	echo "$PVE_IP $PVE_HOST" | sudo tee -a /etc/hosts >/dev/null
fi

export SSL_CERT_FILE="$HOME/.mesh-lab/ca-bundle.pem"
export TF_VAR_ssh_public_key_file="${TF_VAR_ssh_public_key_file:-$MESH_LAB_DIR/mesh-lab.pub}"
export TF_VAR_pve_ssh_address="${TF_VAR_pve_ssh_address:-$PVE_IP}"

cd "$(dirname "$0")/.."
exec "$HOME/.local/bin/tofu" "$@"
