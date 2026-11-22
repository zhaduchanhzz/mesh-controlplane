#!/usr/bin/env bash
# Install Nix on a GitHub-hosted Linux runner with the Determinate Systems
# installer, pinned to NIX_INSTALLER_VERSION and verified against
# NIX_INSTALLER_SHA256 (both set in the workflow env). No FlakeHub login,
# no diagnostics upload, no tokens.
set -euo pipefail

: "${NIX_INSTALLER_VERSION:?}"
: "${NIX_INSTALLER_SHA256:?}"

# The pinned checksum is for the x86_64-linux asset only.
if [ "$(uname -sm)" != "Linux x86_64" ]; then
	echo "unsupported platform $(uname -sm)" >&2
	exit 1
fi

dir=$(mktemp -d)
bin="$dir/nix-installer"
curl -fsSL --proto '=https' --tlsv1.2 -o "$bin" \
	"https://github.com/DeterminateSystems/nix-installer/releases/download/${NIX_INSTALLER_VERSION}/nix-installer-x86_64-linux"
echo "${NIX_INSTALLER_SHA256}  $bin" | sha256sum -c -
chmod +x "$bin"

sudo env NIX_INSTALLER_DIAGNOSTIC_ENDPOINT="" \
	"$bin" install linux --no-confirm

echo "/nix/var/nix/profiles/default/bin" >>"$GITHUB_PATH"
/nix/var/nix/profiles/default/bin/nix --version
