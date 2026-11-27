#!/usr/bin/env bash
# P13f (D9a): replace the three lab socket-proxyd forwarders on the VPS with
# the separate HAProxy instance mesh-lab-haproxy. NOT RUN by the agent: the
# owner approves the diff in README.md first (AGENTS rule 3).
#
# Run on the VPS as root, from a copy of this directory:
#   sudo ./apply.sh --check     # preflight only, changes nothing
#   sudo ./apply.sh             # preflight, then apply; rolls back on failure
#
# What it changes: installs /etc/mesh-lab-haproxy/haproxy.cfg and
# /etc/systemd/system/mesh-lab-haproxy.service, stops and disables
# mesh-{cp,boot,c2}-proxy.{socket,service} (their unit files stay as they
# are), starts mesh-lab-haproxy. Nothing else: no package, no firewall rule,
# no WireGuard, MeshGuard HAProxy, stund, postfix or opendkim change. Before
# and after it records the listeners of those services and the nft ruleset
# hash, and fails (rolling back) if any of them changed.
#
# Existing connections through the forwarders are cut at the switch (about a
# second); Tailscale clients reconnect by themselves.
set -euo pipefail

PUBLIC_IP=116.118.3.46
OLD_UNITS=(mesh-cp-proxy mesh-boot-proxy mesh-c2-proxy)
LAB_PORTS=(8443 8444 8445)
HERE=$(cd "$(dirname "$0")" && pwd)
TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP=/root/mesh-lab-haproxy-backup-$TS
CHECK_ONLY=${1:-}

say() { printf '%s apply: %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { say "STOP: $*"; exit 1; }

# The listeners of the services that must not change: TCP 80, 443, 25, 587
# and UDP 3478, 51820, 51821, with their owning process names.
untouched_listeners() {
	{
		ss -Hltnp '( sport = :80 or sport = :443 or sport = :25 or sport = :587 )'
		ss -Hlunp '( sport = :3478 or sport = :51820 or sport = :51821 )'
	} | awk '{print $1, $4, $5, $NF}' | sed -E 's/pid=[0-9]+,//g; s/fd=[0-9]+//g' | sort
}

nft_hash() { nft list ruleset | sha256sum | cut -d' ' -f1; }

lab_listeners() {
	ss -Hltnp '( sport = :8443 or sport = :8444 or sport = :8445 )' | awk '{print $4, $NF}' | sort
}

# ---------------------------------------------------------------- preflight
[ "$(id -u)" = 0 ] || die "run as root"
ip -o -4 addr show | grep -q " $PUBLIC_IP/" || die "this host does not own $PUBLIC_IP: not the lab VPS"
ip link show wg-mesh >/dev/null 2>&1 || die "wg-mesh is down; the backends are behind it"
command -v haproxy >/dev/null || die "no haproxy binary: installing a package is not part of this change; ask the owner"
[ -x /usr/sbin/haproxy ] || die "haproxy is not at /usr/sbin/haproxy (the unit's path); ask the owner"

ver=$(haproxy -v | head -1 | sed -nE 's/^HAProxy version ([0-9]+)\.([0-9]+).*/\1 \2/p')
read -r major minor <<<"${ver:-0 0}"
{ [ "$major" -gt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -ge 0 ]; }; } || die "haproxy $major.$minor is older than 2.0"
haproxy -vv 2>/dev/null | grep -qi 'USE_SYSTEMD=1\|+SYSTEMD' || say "warning: haproxy built without systemd notify support; Type=notify may fail"

for u in "${OLD_UNITS[@]}"; do
	systemctl cat "$u.socket" >/dev/null 2>&1 || die "unit $u.socket not found; the forwarders differ from README.md"
	systemctl cat "$u.service" >/dev/null 2>&1 || die "unit $u.service not found"
done

for p in "${LAB_PORTS[@]}"; do
	owner=$(ss -Hltnp "( sport = :$p )" | awk '{print $NF}')
	case "$owner" in
	*systemd*|*socket-proxyd*|"") ;;
	*) die "port $p is held by $owner, not a socket unit" ;;
	esac
done

systemctl is-active --quiet mesh-lab-haproxy.service && die "mesh-lab-haproxy is already running"

haproxy -c -q -f "$HERE/haproxy.cfg" || die "haproxy.cfg does not validate"
systemd-analyze verify "$HERE/mesh-lab-haproxy.service" >/dev/null 2>&1 || say "warning: systemd-analyze verify reported issues (see: systemd-analyze verify $HERE/mesh-lab-haproxy.service)"

say "preflight ok: haproxy $major.$minor, units ${OLD_UNITS[*]}, lab listeners:"
lab_listeners | sed 's/^/  /'

if [ "$CHECK_ONLY" = --check ]; then
	say "check only: nothing changed"
	exit 0
fi

# ---------------------------------------------------------------- backup
mkdir -p "$BACKUP"
chmod 0700 "$BACKUP"
for u in "${OLD_UNITS[@]}"; do
	for k in socket service; do
		f=$(systemctl show -P FragmentPath "$u.$k")
		[ -n "$f" ] && cp -a "$f" "$BACKUP/"
		printf '%s.%s %s %s\n' "$u" "$k" "$(systemctl is-enabled "$u.$k" 2>/dev/null || true)" \
			"$(systemctl is-active "$u.$k" 2>/dev/null || true)" >>"$BACKUP/units.state"
	done
done
untouched_listeners >"$BACKUP/untouched-listeners.before"
nft_hash >"$BACKUP/nft.sha256.before"
lab_listeners >"$BACKUP/lab-listeners.before"
say "backup in $BACKUP"

rollback() {
	say "rolling back"
	systemctl disable --now mesh-lab-haproxy.service >/dev/null 2>&1 || true
	for u in "${OLD_UNITS[@]}"; do
		systemctl enable --now "$u.socket" || true
	done
	lab_listeners | sed 's/^/  /'
	die "apply failed; forwarders restored (backup $BACKUP)"
}

# ---------------------------------------------------------------- install
install -d -m 0755 /etc/mesh-lab-haproxy
install -m 0644 "$HERE/haproxy.cfg" /etc/mesh-lab-haproxy/haproxy.cfg
install -m 0644 "$HERE/mesh-lab-haproxy.service" /etc/systemd/system/mesh-lab-haproxy.service
systemctl daemon-reload

# ---------------------------------------------------------------- switch
for u in "${OLD_UNITS[@]}"; do
	systemctl disable --now "$u.socket"
	systemctl stop "$u.service" 2>/dev/null || true
done

systemctl enable --now mesh-lab-haproxy.service || rollback

# ---------------------------------------------------------------- verify
sleep 2
systemctl is-active --quiet mesh-lab-haproxy.service || rollback

for p in "${LAB_PORTS[@]}"; do
	ss -Hltnp "( sport = :$p )" | grep -q haproxy || { say "port $p not held by haproxy"; rollback; }
done

[ "$(nft_hash)" = "$(cat "$BACKUP/nft.sha256.before")" ] || { say "nft ruleset changed"; rollback; }
untouched_listeners >"$BACKUP/untouched-listeners.after"
diff -u "$BACKUP/untouched-listeners.before" "$BACKUP/untouched-listeners.after" ||
	{ say "a listener of another service changed"; rollback; }

# The clusters answer through the new path (bootstrap runs only during E2E).
for hp in c1.cp.gta5web.online:8443 c2.cp.gta5web.online:8445; do
	code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 --resolve "$hp:$PUBLIC_IP" "https://$hp/health" || true)
	say "https://$hp/health -> $code"
done

lab_listeners >"$BACKUP/lab-listeners.after"
say "applied: lab ports 8443-8445 served by mesh-lab-haproxy; rollback: $HERE/rollback.sh"
