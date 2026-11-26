#!/usr/bin/env bash
# P13f: undo apply.sh on the VPS. Run as root:
#   sudo ./rollback.sh            # back to the socket-proxyd forwarders
#   sudo ./rollback.sh --purge    # and remove the mesh-lab-haproxy files
#
# Stops and disables mesh-lab-haproxy, re-enables and starts
# mesh-{cp,boot,c2}-proxy.socket (their unit files were never modified), and
# checks that the lab ports are held by them again. Nothing else is touched.
set -euo pipefail

OLD_UNITS=(mesh-cp-proxy mesh-boot-proxy mesh-c2-proxy)
LAB_PORTS=(8443 8444 8445)
PURGE=${1:-}

say() { printf '%s rollback: %s\n' "$(date -u +%H:%M:%S)" "$*"; }

[ "$(id -u)" = 0 ] || { say "run as root"; exit 1; }

systemctl disable --now mesh-lab-haproxy.service 2>/dev/null || true

for u in "${OLD_UNITS[@]}"; do
	systemctl enable --now "$u.socket"
done

sleep 1
fail=0

for p in "${LAB_PORTS[@]}"; do
	owner=$(ss -Hltnp "( sport = :$p )" | awk '{print $NF}')
	case "$owner" in
	*systemd*) say "port $p: $owner" ;;
	*) say "port $p NOT restored: '${owner:-nothing}'"; fail=1 ;;
	esac
done

if [ "$PURGE" = --purge ]; then
	rm -f /etc/systemd/system/mesh-lab-haproxy.service
	rm -rf /etc/mesh-lab-haproxy
	systemctl daemon-reload
	say "mesh-lab-haproxy files removed"
fi

[ "$fail" = 0 ] && say "forwarders restored" || { say "check the socket units by hand"; exit 1; }
