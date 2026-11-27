#!/usr/bin/env bash
# etcd member for the Patroni DCS of the lab PostgreSQL HA cluster (P13a,
# ADR 0011 §A): three members, mc-pg-01 (.247), mc-pg-02 (.248) and mc-db
# (.243, witness: no Patroni PostgreSQL there). Losing one member keeps quorum.
#
#   sudo bash setup-etcd.sh            on each of the three hosts
#
# Idempotent. Upstream etcd release binary (SHA-256 checked against the
# release's SHA256SUMS), unit `mc-etcd.service`, user `etcd`, data in
# /var/lib/mc-etcd. A re-run on an existing member only rewrites the unit and
# the env file; the --initial-cluster* settings are ignored by etcd once the
# data directory exists.
#
# Access: plain HTTP on the lab LAN, restricted by nftables to the three
# members (+ localhost). On the PG nodes the full host firewall comes from
# setup-pg.sh; on mc-db (Docker owns iptables there) this script loads a
# separate table `inet mc_etcd` that only touches tcp/2379-2380, persisted
# by `mc-etcd-nft.service`. No TLS/auth on etcd yet: see
# evidence/P13a-ha-postgres.md "Known limitations".
#
# Resource bounds (mc-db also runs the long-lived mc-pg/mc-pgb containers):
# MemoryMax 512M, quota 2 GiB, periodic compaction. Patroni writes a few
# small keys every loop_wait (5 s), so IO is negligible.
set -euo pipefail

ETCD_VER=${ETCD_VER:-v3.6.17}
TOKEN=mc-pg-etcd
declare -A MEMBERS=(
	[mc-pg-01]=192.168.1.247
	[mc-pg-02]=192.168.1.248
	[mc-db]=192.168.1.243
)
ORDER=(mc-pg-01 mc-pg-02 mc-db)

name=${ETCD_NAME_OVERRIDE:-$(hostname -s)}
myip=${MEMBERS[$name]:-}
[ -n "$myip" ] || { echo "host $name is not an etcd member (${ORDER[*]})" >&2; exit 1; }
ip -4 -o addr show | grep -q " $myip/" || { echo "$myip is not configured on $name" >&2; exit 1; }

umask 022

# --- binary ------------------------------------------------------------------
if ! /usr/local/bin/etcd --version 2>/dev/null | grep -q "etcd Version: ${ETCD_VER#v}$"; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	base=https://github.com/etcd-io/etcd/releases/download/$ETCD_VER
	tgz=etcd-$ETCD_VER-linux-amd64.tar.gz
	curl -fsSL --retry 3 -o "$tmp/$tgz" "$base/$tgz"
	curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
	(cd "$tmp" && grep " $tgz\$" SHA256SUMS | sha256sum -c --quiet -)
	tar -xzf "$tmp/$tgz" -C "$tmp"
	install -m 0755 "$tmp/etcd-$ETCD_VER-linux-amd64/etcd" "$tmp/etcd-$ETCD_VER-linux-amd64/etcdctl" \
		"$tmp/etcd-$ETCD_VER-linux-amd64/etcdutl" /usr/local/bin/
fi

id etcd >/dev/null 2>&1 || useradd --system --home-dir /var/lib/mc-etcd --shell /usr/sbin/nologin etcd
install -d -m 0700 -o etcd -g etcd /var/lib/mc-etcd
install -d -m 0755 /etc/mc-etcd

initial=""
for m in "${ORDER[@]}"; do initial+="${initial:+,}$m=http://${MEMBERS[$m]}:2380"; done

cat >/etc/mc-etcd/etcd.env.new <<EOF
# Managed by infra/lab/pg/setup-etcd.sh (no secrets).
ETCD_NAME=$name
ETCD_DATA_DIR=/var/lib/mc-etcd/data
ETCD_LISTEN_PEER_URLS=http://$myip:2380
ETCD_LISTEN_CLIENT_URLS=http://$myip:2379,http://127.0.0.1:2379
ETCD_ADVERTISE_CLIENT_URLS=http://$myip:2379
ETCD_INITIAL_ADVERTISE_PEER_URLS=http://$myip:2380
ETCD_INITIAL_CLUSTER=$initial
ETCD_INITIAL_CLUSTER_STATE=new
ETCD_INITIAL_CLUSTER_TOKEN=$TOKEN
ETCD_AUTO_COMPACTION_MODE=periodic
ETCD_AUTO_COMPACTION_RETENTION=1h
ETCD_QUOTA_BACKEND_BYTES=2147483648
ETCD_HEARTBEAT_INTERVAL=100
ETCD_ELECTION_TIMEOUT=1000
ETCD_LOG_LEVEL=warn
EOF
changed=0
cmp -s /etc/mc-etcd/etcd.env.new /etc/mc-etcd/etcd.env || { mv /etc/mc-etcd/etcd.env.new /etc/mc-etcd/etcd.env; changed=1; }
rm -f /etc/mc-etcd/etcd.env.new

cat >/etc/systemd/system/mc-etcd.service.new <<'EOF'
# Managed by infra/lab/pg/setup-etcd.sh
[Unit]
Description=etcd member of the mc-pg Patroni DCS (P13a, ADR 0011)
After=network-online.target mc-etcd-nft.service nftables.service
Wants=network-online.target

[Service]
Type=notify
User=etcd
Group=etcd
EnvironmentFile=/etc/mc-etcd/etcd.env
ExecStart=/usr/local/bin/etcd
Restart=on-failure
RestartSec=2
LimitNOFILE=65536
MemoryHigh=384M
MemoryMax=512M
Nice=-5
IOSchedulingClass=best-effort
IOSchedulingPriority=2

[Install]
WantedBy=multi-user.target
EOF
cmp -s /etc/systemd/system/mc-etcd.service.new /etc/systemd/system/mc-etcd.service ||
	{ mv /etc/systemd/system/mc-etcd.service.new /etc/systemd/system/mc-etcd.service; changed=1; }
rm -f /etc/systemd/system/mc-etcd.service.new

# --- firewall on hosts without the full mc-pg firewall (mc-db) -----------------
if [ "$name" = mc-db ]; then
	peers=""
	for m in "${ORDER[@]}"; do peers+="${peers:+, }${MEMBERS[$m]}"; done
	cat >/etc/mc-etcd/mc-etcd.nft <<EOF
#!/usr/sbin/nft -f
# Managed by infra/lab/pg/setup-etcd.sh. Only tcp/2379-2380; every other
# packet is left to the existing (Docker/iptables-nft) rules.
table inet mc_etcd
delete table inet mc_etcd
table inet mc_etcd {
	chain input {
		type filter hook input priority filter - 5; policy accept;
		iif "lo" accept
		tcp dport { 2379, 2380 } ip saddr { $peers } accept
		tcp dport { 2379, 2380 } drop
	}
}
EOF
	cat >/etc/systemd/system/mc-etcd-nft.service <<'EOF'
# Managed by infra/lab/pg/setup-etcd.sh
[Unit]
Description=nftables table inet mc_etcd (etcd ports, members only)
Before=mc-etcd.service
After=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/mc-etcd/mc-etcd.nft
ExecStop=/usr/sbin/nft delete table inet mc_etcd

[Install]
WantedBy=multi-user.target
EOF
	nft -c -f /etc/mc-etcd/mc-etcd.nft
	systemctl daemon-reload
	systemctl enable -q mc-etcd-nft.service
	systemctl restart mc-etcd-nft.service
fi

systemctl daemon-reload
systemctl enable -q mc-etcd.service
if [ "$changed" = 1 ] || ! systemctl is-active -q mc-etcd.service; then
	# The first member blocks in "notify" until a quorum exists: do not wait.
	systemctl restart --no-block mc-etcd.service
fi
echo "etcd $(/usr/local/bin/etcd --version | awk '/etcd Version/{print $3}') member $name ($myip), unit mc-etcd.service"
