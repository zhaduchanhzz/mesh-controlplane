#!/usr/bin/env bash
# PgBouncer + local HAProxy on a PG node of the lab HA cluster (P13b,
# ADR 0011 §D, ADR 0006 §3-4). Run as root on mc-pg-01 and mc-pg-02 after
# setup-pg.sh (Patroni running, /etc/mc-pg/headscale.pass present):
#
#   sudo bash setup-pgbouncer.sh
#
# Files expected next to this script (deploy.sh pushes them):
#   haproxy-pg.cfg  pgbouncer.ini.tmpl  pgbouncer-clusters.ini
#
# Idempotent: unchanged files restart nothing; a changed PgBouncer config is
# applied with a reload (RELOAD keeps client connections), a changed
# HAProxy config with a reload (new process, old sessions drain).
#
# Path of a client write: client -> <node>:6432 PgBouncer (transaction pool)
# -> 127.0.0.1:5000 HAProxy -> current Patroni primary :5432.
#
# Secrets (AGENTS.md rule 2): /etc/pgbouncer/userlist.txt (postgres, 0600)
# holds role headscale's password, built from /etc/mc-pg/headscale.pass on
# the node; never printed, never on a command line.
set -euo pipefail

PGBOUNCER_VERSION=${PGBOUNCER_VERSION:-1.26.1-1.pgdg24.04+1}
HAPROXY_VERSION=${HAPROXY_VERSION:-2.8.30-0ubuntu0.24.04.2}
HERE=$(cd "$(dirname "$0")" && pwd)

declare -A NODES=([mc-pg-01]=192.168.1.247 [mc-pg-02]=192.168.1.248)
name=$(hostname -s)
myip=${NODES[$name]:-}
[ -n "$myip" ] || { echo "host $name is not a PG node" >&2; exit 1; }
[ -s /etc/mc-pg/headscale.pass ] || { echo "/etc/mc-pg/headscale.pass missing (setup-pg.sh first)" >&2; exit 1; }
for f in haproxy-pg.cfg pgbouncer.ini.tmpl pgbouncer-clusters.ini; do
	[ -s "$HERE/$f" ] || { echo "$HERE/$f missing" >&2; exit 1; }
done
export DEBIAN_FRONTEND=noninteractive

# --- packages (PGDG repository from setup-pg.sh; HAProxy from Ubuntu) ----------
want="pgbouncer=$PGBOUNCER_VERSION haproxy=$HAPROXY_VERSION"
need=0
for p in $want; do
	[ "$(dpkg-query -W -f='${Version}' "${p%%=*}" 2>/dev/null || true)" = "${p#*=}" ] || need=1
done
if [ "$need" = 1 ]; then
	apt-get update -qq
	apt-mark unhold pgbouncer haproxy >/dev/null 2>&1 || true
	# shellcheck disable=SC2086
	apt-get install -y -qq --no-install-recommends $want >/dev/null
fi
apt-mark hold pgbouncer haproxy >/dev/null
# socat: HAProxy's stats socket (this script's report, check-pg.sh).
dpkg -s socat >/dev/null 2>&1 || apt-get install -y -qq --no-install-recommends socat >/dev/null

# --- HAProxy -----------------------------------------------------------------------
hap_changed=0
haproxy -c -q -f "$HERE/haproxy-pg.cfg"
if ! cmp -s "$HERE/haproxy-pg.cfg" /etc/haproxy/haproxy.cfg; then
	install -m 0644 "$HERE/haproxy-pg.cfg" /etc/haproxy/haproxy.cfg
	hap_changed=1
fi
systemctl enable -q haproxy.service
if ! systemctl is-active -q haproxy.service; then
	systemctl start haproxy.service
elif [ "$hap_changed" = 1 ]; then
	systemctl reload haproxy.service
fi

# --- PgBouncer -------------------------------------------------------------------
pgb_changed=0
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
chmod 0700 "$stage"
sed -e "s/@IP@/$myip/g" "$HERE/pgbouncer.ini.tmpl" >"$stage/pgbouncer.ini"
cp "$HERE/pgbouncer-clusters.ini" "$stage/pgbouncer-clusters.ini"
cat >"$stage/pg_hba.conf" <<'EOF'
# PgBouncer client authentication (P13b; managed by setup-pgbouncer.sh).
# No `local` line: the admin console is reached as user pgbouncer from the
# process's own uid (postgres), which PgBouncer allows by itself.
# Application role from the lab LAN; nftables narrows 6432 further.
host   all        headscale  192.168.1.0/24    scram-sha-256
EOF
# userlist: letters and digits only (setup-pg.sh generates it so).
(
	umask 077
	p=$(cat /etc/mc-pg/headscale.pass)
	[[ ${#p} -ge 24 && $p =~ ^[A-Za-z0-9]+$ ]] || { echo "headscale.pass malformed" >&2; exit 1; }
	printf '"headscale" "%s"\n' "$p" >"$stage/userlist.txt"
)
for f in pgbouncer.ini pgbouncer-clusters.ini pg_hba.conf userlist.txt; do
	mode=0640
	[ "$f" = userlist.txt ] && mode=0600
	if ! cmp -s "$stage/$f" "/etc/pgbouncer/$f"; then
		install -m "$mode" -o postgres -g postgres "$stage/$f" "/etc/pgbouncer/$f"
		pgb_changed=1
	fi
	chown postgres:postgres "/etc/pgbouncer/$f"
	chmod "$mode" "/etc/pgbouncer/$f"
done

# More file descriptors than the packaged unit's default (max_client_conn
# 2000 plus server connections), and restart after a crash or kill -9.
install -d -m 0755 /etc/systemd/system/pgbouncer.service.d
cat >"$stage/override.conf" <<'EOF'
# Managed by infra/lab/pg/setup-pgbouncer.sh (P13b)
[Unit]
After=haproxy.service

[Service]
LimitNOFILE=8192
Restart=on-failure
RestartSec=1
EOF
unit_changed=0
cmp -s "$stage/override.conf" /etc/systemd/system/pgbouncer.service.d/override.conf ||
	{ install -m 0644 "$stage/override.conf" /etc/systemd/system/pgbouncer.service.d/override.conf; unit_changed=1; }
[ "$unit_changed" = 1 ] && systemctl daemon-reload

systemctl enable -q pgbouncer.service
if ! systemctl is-active -q pgbouncer.service; then
	systemctl start pgbouncer.service
elif [ "$unit_changed" = 1 ]; then
	# listen_addr or limits changed: only a restart applies them.
	systemctl restart pgbouncer.service
elif [ "$pgb_changed" = 1 ]; then
	systemctl reload pgbouncer.service
fi

# --- wait and report ---------------------------------------------------------------
for _ in $(seq 1 30); do
	ss -ltn "sport = :6432" | grep -q "$myip:6432" && ss -ltn "sport = :5000" | grep -q "127.0.0.1:5000" && break
	sleep 1
done
ss -ltn "sport = :6432" | grep -q "$myip:6432" || { echo "pgbouncer not listening on $myip:6432" >&2; exit 1; }
ss -ltn "sport = :5000" | grep -q "127.0.0.1:5000" || { echo "haproxy not listening on 127.0.0.1:5000" >&2; exit 1; }
up=$(echo "show stat" | socat - UNIX-CONNECT:/run/haproxy/admin.sock 2>/dev/null |
	awk -F, '$1=="pg_primary" && $2!="FRONTEND" && $2!="BACKEND" {printf "%s=%s ", $2, $18}' || true)
echo "$name: pgbouncer $(dpkg-query -W -f='${Version}' pgbouncer) on $myip:6432, haproxy $(dpkg-query -W -f='${Version}' haproxy) on 127.0.0.1:5000; backends: ${up:-unknown (socat missing?)}"
