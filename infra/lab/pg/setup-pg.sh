#!/usr/bin/env bash
# PostgreSQL 16 + Patroni node of the lab HA cluster (P13a, ADR 0011).
# Run as root on mc-pg-01 (7708, .247) and mc-pg-02 (7709, .248), after
# setup-etcd.sh has run on all three etcd members:
#
#   sudo SECRETS=gen  bash setup-pg.sh   first node: create /etc/mc-pg secrets
#   sudo SECRETS=keep bash setup-pg.sh   default: secrets must already exist
#                                        (copied from the first node, see
#                                        deploy.sh; never through argv or logs)
#
# Idempotent. Installs and pins: PGDG postgresql-16, patroni, pgbackrest (stanza:
# setup-pgbackrest.sh, P13c), chrony (replaces systemd-timesyncd), softdog watchdog,
# nftables host firewall. Patroni bootstraps a FRESH cluster `mc-pg` (initdb
# --data-checksums on the first node to take the leader key; the second
# clones it with pg_basebackup). This is not a migration of `meshlab`.
#
# Secrets (AGENTS.md rule 2): /etc/mc-pg (root, 0700)
#   patroni.env     PATRONI_{SUPERUSER,REPLICATION,REWIND,RESTAPI}_{USERNAME,PASSWORD}
#   headscale.pass  password of the application role `headscale`
# Generated here from /dev/urandom (letters and digits, 32 chars); they are
# never printed. The operator copy lives in %USERPROFILE%\.mesh-lab\mc-pg.env.
set -euo pipefail

PG_MAJOR=16
PG_VERSION=${PG_VERSION:-16.16-1.pgdg24.04+1}
PATRONI_VERSION=${PATRONI_VERSION:-4.1.5-2.pgdg24.04+1}
PGBACKREST_VERSION=${PGBACKREST_VERSION:-2.60.1-1.pgdg24.04+1}
# apt.postgresql.org signing key (ACCC4CF8), full fingerprint pinned.
PGDG_FPR=B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8
SECRETS=${SECRETS:-keep}
HERE=$(cd "$(dirname "$0")" && pwd)

declare -A NODES=([mc-pg-01]=192.168.1.247 [mc-pg-02]=192.168.1.248)
name=$(hostname -s)
myip=${NODES[$name]:-}
[ -n "$myip" ] || { echo "host $name is not a PG node" >&2; exit 1; }
ip -4 -o addr show | grep -q " $myip/" || { echo "$myip is not configured on $name" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive

# --- secrets ---------------------------------------------------------------------
install -d -m 0700 -o root -g root /etc/mc-pg
# Bounded input: `tr </dev/urandom | head` would die of SIGPIPE under pipefail.
gen() {
	local s
	s=$(head -c 4096 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')
	[ "${#s}" -ge 32 ] || return 1
	printf '%s' "${s:0:32}"
}
if [ ! -s /etc/mc-pg/patroni.env ] || [ ! -s /etc/mc-pg/headscale.pass ]; then
	[ "$SECRETS" = gen ] || { echo "/etc/mc-pg secrets missing (SECRETS=gen on the first node, then copy)" >&2; exit 1; }
	(
		umask 077
		{
			echo "PATRONI_SUPERUSER_USERNAME=postgres"
			echo "PATRONI_SUPERUSER_PASSWORD=$(gen)"
			echo "PATRONI_REPLICATION_USERNAME=replicator"
			echo "PATRONI_REPLICATION_PASSWORD=$(gen)"
			echo "PATRONI_REWIND_USERNAME=rewinder"
			echo "PATRONI_REWIND_PASSWORD=$(gen)"
			echo "PATRONI_RESTAPI_USERNAME=patroni"
			echo "PATRONI_RESTAPI_PASSWORD=$(gen)"
		} >/etc/mc-pg/patroni.env
		gen >/etc/mc-pg/headscale.pass
	)
fi
chmod 0600 /etc/mc-pg/patroni.env /etc/mc-pg/headscale.pass
grep -qE '^PATRONI_SUPERUSER_PASSWORD=[A-Za-z0-9]{24,}$' /etc/mc-pg/patroni.env || { echo "patroni.env malformed" >&2; exit 1; }

# --- time: chrony (ADR 0011 §C, L6) ------------------------------------------------
if ! dpkg -s chrony >/dev/null 2>&1; then
	apt-get update -qq
	apt-get install -y -qq chrony >/dev/null
fi
systemctl enable -q --now chrony

# --- watchdog: softdog ------------------------------------------------------------
# Ubuntu blacklists watchdog drivers for alias autoloading; loading by name
# (modules-load.d / modprobe) is not affected.
echo softdog >/etc/modules-load.d/softdog.conf
modprobe softdog
cat >/etc/udev/rules.d/60-mc-pg-watchdog.rules <<'EOF'
# Patroni (user postgres) owns the watchdog (P13a, ADR 0011 §A).
KERNEL=="watchdog", OWNER="postgres", GROUP="postgres", MODE="0600"
EOF

# --- PGDG repository ---------------------------------------------------------------
keyring=/etc/apt/keyrings/pgdg.gpg
if [ ! -s "$keyring" ]; then
	install -d -m 0755 /etc/apt/keyrings
	tmp=$(mktemp -d)
	curl -fsSL --retry 3 -o "$tmp/pgdg.asc" https://www.postgresql.org/media/keys/ACCC4CF8.asc
	got=$(gpg --show-keys --with-colons "$tmp/pgdg.asc" | awk -F: '/^fpr:/{print $10; exit}')
	[ "$got" = "$PGDG_FPR" ] || { echo "PGDG key fingerprint mismatch: $got" >&2; rm -rf "$tmp"; exit 1; }
	gpg --dearmor <"$tmp/pgdg.asc" >"$keyring"
	chmod 0644 "$keyring"
	rm -rf "$tmp"
fi
echo "deb [signed-by=$keyring] https://apt.postgresql.org/pub/repos/apt noble-pgdg main" >/etc/apt/sources.list.d/pgdg.list

# Patroni owns the data directory: no Debian "main" cluster.
install -d -m 0755 /etc/postgresql-common
echo "create_main_cluster = false" >/etc/postgresql-common/createcluster.conf

want="postgresql-$PG_MAJOR=$PG_VERSION postgresql-client-$PG_MAJOR=$PG_VERSION patroni=$PATRONI_VERSION pgbackrest=$PGBACKREST_VERSION"
need=0
for p in $want; do
	[ "$(dpkg-query -W -f='${Version}' "${p%%=*}" 2>/dev/null || true)" = "${p#*=}" ] || need=1
done
if [ "$need" = 1 ]; then
	apt-get update -qq
	apt-mark unhold postgresql-$PG_MAJOR postgresql-client-$PG_MAJOR patroni pgbackrest >/dev/null 2>&1 || true
	# shellcheck disable=SC2086
	apt-get install -y -qq --no-install-recommends $want python3-psycopg2 >/dev/null
fi
apt-mark hold postgresql-$PG_MAJOR postgresql-client-$PG_MAJOR patroni pgbackrest >/dev/null
# The packaged units must not start a second PostgreSQL/Patroni.
systemctl disable -q --now postgresql.service 2>/dev/null || true
systemctl disable -q --now patroni.service 2>/dev/null || true
udevadm control --reload
udevadm trigger --name-match=watchdog || true
[ "$(stat -c %U /dev/watchdog)" = postgres ] || { echo "/dev/watchdog is not owned by postgres" >&2; exit 1; }

# --- host firewall (no Docker on these VMs: nftables owns the ruleset) -----------
# The first ruleset with `ct state` rules loads conntrack: TCP streams opened
# before that (etcd peers, started by setup-etcd.sh) are picked up mid-stream,
# their packets classified invalid and dropped, and etcd keeps a silent dead
# stream (seen 2026-11-26: member stuck at an old raft term). Restart the local
# etcd member once in that case; the other two keep quorum.
fresh_ct=0
[ -d /proc/sys/net/netfilter ] && [ -e /proc/sys/net/netfilter/nf_conntrack_count ] || fresh_ct=1
nft -c -f "$HERE/mc-pg.nft"
systemctl enable -q nftables.service
if ! cmp -s "$HERE/mc-pg.nft" /etc/nftables.conf || ! nft list table inet mc_pg >/dev/null 2>&1; then
	install -m 0644 "$HERE/mc-pg.nft" /etc/nftables.conf
	systemctl restart nftables.service
fi
if [ "$fresh_ct" = 1 ] && systemctl is-active -q mc-etcd.service; then
	systemctl restart mc-etcd.service
fi

# --- Patroni ----------------------------------------------------------------------
install -d -m 0750 -o postgres -g postgres /etc/patroni /var/lib/postgresql/$PG_MAJOR
sed -e "s/@NAME@/$name/g" -e "s/@IP@/$myip/g" "$HERE/patroni.yml.tmpl" >/etc/patroni/mc-pg.yml.new
chown root:postgres /etc/patroni/mc-pg.yml.new
chmod 0640 /etc/patroni/mc-pg.yml.new
changed=0
cmp -s /etc/patroni/mc-pg.yml.new /etc/patroni/mc-pg.yml || { mv /etc/patroni/mc-pg.yml.new /etc/patroni/mc-pg.yml; changed=1; }
rm -f /etc/patroni/mc-pg.yml.new
# On a re-run the running Patroni holds 8008/5432, and the validator reports
# exactly that; any other finding fails the run.
if ! vout=$(set -a; . /etc/mc-pg/patroni.env; set +a; patroni --validate-config /etc/patroni/mc-pg.yml 2>&1); then
	if ! systemctl is-active -q mc-patroni.service || grep -v -E "Port (8008|5432) is already in use" <<<"$vout" | grep -q .; then
		echo "$vout" >&2
		exit 1
	fi
fi

cat >/etc/systemd/system/mc-patroni.service.new <<'EOF'
# Managed by infra/lab/pg/setup-pg.sh (P13a, ADR 0011)
[Unit]
Description=Patroni for the mc-pg PostgreSQL HA cluster
After=network-online.target mc-etcd.service nftables.service chrony.service
Wants=network-online.target

[Service]
Type=simple
User=postgres
Group=postgres
EnvironmentFile=/etc/mc-pg/patroni.env
ExecStart=/usr/bin/patroni /etc/patroni/mc-pg.yml
ExecReload=/bin/kill -s HUP $MAINPID
# Patroni stops PostgreSQL itself; do not kill the postmaster with it.
KillMode=process
TimeoutStopSec=60
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
cmp -s /etc/systemd/system/mc-patroni.service.new /etc/systemd/system/mc-patroni.service ||
	{ mv /etc/systemd/system/mc-patroni.service.new /etc/systemd/system/mc-patroni.service; changed=1; }
rm -f /etc/systemd/system/mc-patroni.service.new

# patronictl with the REST API credentials from patroni.env (root only).
cat >/usr/local/sbin/mc-patronictl <<'EOF'
#!/usr/bin/env bash
# patronictl for the mc-pg cluster; reads credentials from /etc/mc-pg (root).
set -euo pipefail
set -a; . /etc/mc-pg/patroni.env; set +a
exec /usr/bin/patronictl -c /etc/patroni/mc-pg.yml "$@"
EOF
chmod 0750 /usr/local/sbin/mc-patronictl

systemctl daemon-reload
systemctl enable -q mc-patroni.service
if ! systemctl is-active -q mc-patroni.service; then
	systemctl start mc-patroni.service
elif [ "$changed" = 1 ]; then
	systemctl reload mc-patroni.service
fi

# --- wait for a running PostgreSQL under Patroni -------------------------------
for _ in $(seq 1 120); do
	st=$(curl -fsS http://127.0.0.1:8008/patroni 2>/dev/null | jq -r '.state // empty' || true)
	[ "$st" = running ] || [ "$st" = streaming ] && break
	sleep 2
done
role=$(curl -fsS http://127.0.0.1:8008/patroni | jq -r .role)

# --- application role (leader only; replicated to the standby) ------------------
if [ "$role" = primary ] || [ "$role" = master ]; then
	{
		printf "\\set app '%s'\n" "$(cat /etc/mc-pg/headscale.pass)"
		cat <<'SQL'
SET client_min_messages = warning;
SELECT format('CREATE ROLE headscale LOGIN PASSWORD %L', :'app')
  WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'headscale') \gexec
ALTER ROLE headscale PASSWORD :'app';
ALTER ROLE headscale SET idle_in_transaction_session_timeout = '5s';
SQL
	} | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -q -X -d postgres >/dev/null
fi

echo "$name: postgres $(runuser -u postgres -- /usr/lib/postgresql/$PG_MAJOR/bin/postgres --version | awk '{print $3}'), patroni $(dpkg-query -W -f='${Version}' patroni), role $role, state $(curl -fsS http://127.0.0.1:8008/patroni | jq -r .state)"
