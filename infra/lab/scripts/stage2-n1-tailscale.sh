#!/bin/bash
# Tailscale static builds on the four stage-2 clients, side by side, not running.
# v1.84.3 is no longer served as a static tarball (404 on pkgs.tailscale.com
# /stable, 2026-11-25): it was taken from the tailscale/tailscale:v1.84.3
# image on mc-ci (docker cp /usr/local/bin/tailscale{,d}) and piped to each
# client into /opt/tailscale/1.84.3.
set -u
K=(-i "$HOME/.mesh-lab/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
PX="ProxyCommand=ssh -i $HOME/.mesh-lab/mesh-lab -o IdentitiesOnly=yes -o BatchMode=yes mesh@192.168.1.240 nc -q0 %h %p"
for ip in 10.77.20.11 10.77.20.12 10.77.30.11 10.77.30.12; do
  echo "== $ip"
  ssh "${K[@]}" -o "$PX" "mesh@$ip" 'set -e
    for v in 1.104.7 1.84.3; do
      d=/opt/tailscale/$v
      if [ -x $d/tailscaled ]; then echo "$v present"; continue; fi
      t=$(mktemp -d); cd $t
      curl -fsSL -o ts.tgz "https://pkgs.tailscale.com/stable/tailscale_${v}_amd64.tgz"
      curl -fsSL -o ts.sha "https://pkgs.tailscale.com/stable/tailscale_${v}_amd64.tgz.sha256"
      echo "$(cat ts.sha)  ts.tgz" | sha256sum -c --quiet
      tar -xzf ts.tgz
      sudo install -d $d
      sudo install -m 0755 tailscale_${v}_amd64/tailscale tailscale_${v}_amd64/tailscaled $d/
      cd /; rm -rf $t
      echo "$v installed: $($d/tailscale version | head -1)"
    done
    pgrep -x tailscaled >/dev/null && echo "tailscaled RUNNING" || echo "no tailscaled running"'
done
