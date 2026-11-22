#!/usr/bin/env bash
# Prepare a Linux workstation (WSL Ubuntu or mc-ci) to run OpenTofu against the
# lab Proxmox. Idempotent. Uses sudo only to add one /etc/hosts line.
#
#   MESH_LAB_DIR=/mnt/c/Users/<you>/.mesh-lab bash infra/lab/scripts/runner-setup-wsl.sh
#
# Reads PVE_HOST / PVE_IP from $MESH_LAB_DIR/proxmox.env. Never prints secrets.
set -euo pipefail

TOFU_VERSION="${TOFU_VERSION:-1.13.9}"
BIN_DIR="$HOME/.local/bin"
STATE_DIR="$HOME/.mesh-lab"

if [ -z "${MESH_LAB_DIR:-}" ]; then
	win_home=$(cmd.exe /c 'echo %USERPROFILE%' 2>/dev/null | tr -d '\r')
	MESH_LAB_DIR="$(wslpath "$win_home")/.mesh-lab"
fi
ENV_FILE="$MESH_LAB_DIR/proxmox.env"
[ -r "$ENV_FILE" ] || { echo "missing $ENV_FILE" >&2; exit 1; }

PVE_HOST=$(sed -n 's/^PVE_HOST=//p' "$ENV_FILE")
PVE_IP=$(sed -n 's/^PVE_IP=//p' "$ENV_FILE")
mkdir -p "$BIN_DIR" "$STATE_DIR"

# 1. OpenTofu, verified against the release SHA256SUMS.
if ! "$BIN_DIR/tofu" version 2>/dev/null | grep -q "v$TOFU_VERSION"; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	base="https://github.com/opentofu/opentofu/releases/download/v$TOFU_VERSION"
	pkg="tofu_${TOFU_VERSION}_linux_amd64.tar.gz"
	curl -fsSLo "$tmp/$pkg" "$base/$pkg"
	curl -fsSLo "$tmp/SHA256SUMS" "$base/tofu_${TOFU_VERSION}_SHA256SUMS"
	(cd "$tmp" && grep "  $pkg\$" SHA256SUMS | sha256sum -c -)
	tar -xzf "$tmp/$pkg" -C "$tmp" tofu
	install -m 0755 "$tmp/tofu" "$BIN_DIR/tofu"
fi
"$BIN_DIR/tofu" version | head -1

# 2. The PVE certificate's SAN lists $PVE_HOST but not the current IP, and the
#    owner chose not to regenerate it, so map the name locally.
#    WSL may rewrite /etc/hosts on restart; rerun this script then.
if ! grep -qE "^[0-9.]+[[:space:]]+$PVE_HOST([[:space:]]|\$)" /etc/hosts; then
	echo "$PVE_IP $PVE_HOST" | sudo tee -a /etc/hosts >/dev/null
fi
getent hosts "$PVE_HOST"

# 3. CA bundle: system roots (registry/provider downloads) + PVE root CA.
cat /etc/ssl/certs/ca-certificates.crt "$MESH_LAB_DIR/pve-root-ca.pem" >"$STATE_DIR/ca-bundle.pem"
echo "CA bundle: $STATE_DIR/ca-bundle.pem"

# 4. TLS check against the PVE API (expects 401 without credentials).
code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$STATE_DIR/ca-bundle.pem" "https://$PVE_HOST:8006/api2/json/version")
echo "PVE API TLS verified, HTTP $code"
