#!/usr/bin/env bash
# Stage 2a: install cluster c2 on mc-c2 (VMID 7705, 192.168.1.244). Re-runnable.
#
# - binary: the cluster binary from mc-cp (same build as c1) unless BIN= names
#   a local file; sha256 is checked on both ends;
# - TLS: the wildcard *.cp.gta5web.online cert/key copied from mc-cp over SSH
#   pipes (never on a command line, never in a log); renewals on mc-cp must be
#   copied again before the cert expires (2027-02-20);
# - DB: the long-lived `meshlab` database on mc-db behind PgBouncer
#   (192.168.1.243:6432); the headscale role password comes from
#   %USERPROFILE%\.mesh-lab\mc-db.env through stdin into a 0640 file;
# - Noise key: created by the cluster on first start, own key for c2 (ADR 0005:
#   one Noise key per cluster);
# - cluster id `mc-c2` (the P05 harness uses `mc-cp-c2` for an unregistered
#   cluster, so that name is avoided), registered ACTIVE with endpoint
#   https://c2.cp.gta5web.online:8445 (VPS :8445 -> wg-mesh -> mc-cp:8445 -> here);
# - nftables on mc-c2: 8443 only from the lab range .240-.249 (mc-cp relays from
#   .241), SSH from the LAN, everything else inbound dropped.
#
# Usage: infra/lab/c2/setup-c2.sh            (from Git Bash or WSL)
#        BIN=path/to/headscale-cluster infra/lab/c2/setup-c2.sh
set -euo pipefail

LAB_DIR=${MESH_LAB_DIR:-$HOME/.mesh-lab}
KEY=${MESH_LAB_KEY:-$LAB_DIR/mesh-lab}
DB_ENV=${MC_DB_ENV:-$LAB_DIR/mc-db.env}
CP=192.168.1.241
C2=192.168.1.244
CLUSTER_ID=mc-c2
ENDPOINT=https://c2.cp.gta5web.online:8445
HSC="sudo -u headscale /usr/local/bin/headscale-cluster -c /etc/headscale/cluster/config.yaml cluster --cluster-id $CLUSTER_ID"
CR=$(printf '\r')

SSH=(ssh -i "$KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
cp_() { "${SSH[@]}" "mesh@$CP" "$@"; }
c2_() { "${SSH[@]}" "mesh@$C2" "$@"; }
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

c2_ 'test -e /var/lib/mesh-lab-bootstrap-done' || { log "mc-c2 cloud-init not finished"; exit 1; }

log "user, directories"
c2_ 'set -e
  id headscale >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/headscale --shell /usr/sbin/nologin headscale
  sudo install -d -o root -g headscale -m 0750 /etc/headscale /etc/headscale/tls /etc/headscale/cluster
  sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster'

log "binary"
if [ -n "${BIN:-}" ]; then
  want=$(sha256sum "$BIN" | cut -d' ' -f1)
  c2_ 'sudo tee /usr/local/bin/headscale-cluster.new >/dev/null' <"$BIN"
else
  want=$(cp_ 'sha256sum /usr/local/bin/headscale-cluster' | cut -d' ' -f1)
  cp_ 'cat /usr/local/bin/headscale-cluster' | c2_ 'sudo tee /usr/local/bin/headscale-cluster.new >/dev/null'
fi
got=$(c2_ 'sha256sum /usr/local/bin/headscale-cluster.new' | cut -d' ' -f1)
[ "$want" = "$got" ] || { log "binary sha256 mismatch"; exit 1; }
c2_ 'sudo chmod 0755 /usr/local/bin/headscale-cluster.new && sudo mv /usr/local/bin/headscale-cluster.new /usr/local/bin/headscale-cluster'
log "binary sha256 ${got:0:16}"

log "TLS (wildcard from mc-cp)"
cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | c2_ 'sudo tee /etc/headscale/tls/fullchain.pem >/dev/null'
cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | c2_ 'sudo tee /etc/headscale/tls/privkey.pem >/dev/null'
c2_ 'set -e
  sudo chown root:headscale /etc/headscale/tls/fullchain.pem /etc/headscale/tls/privkey.pem
  sudo chmod 0644 /etc/headscale/tls/fullchain.pem; sudo chmod 0640 /etc/headscale/tls/privkey.pem'
[ "$(cp_ 'sudo sha256sum /etc/headscale/tls/privkey.pem' | cut -d' ' -f1)" = "$(c2_ 'sudo sha256sum /etc/headscale/tls/privkey.pem' | cut -d' ' -f1)" ] \
  || { log "TLS key copy mismatch"; exit 1; }

log "DB password (stdin only)"
app_pass=$(grep -E '^MC_DB_APP_PASSWORD=' "$DB_ENV" | head -1 | cut -d= -f2-)
app_pass=${app_pass%"$CR"}
[ -n "$app_pass" ] || { log "MC_DB_APP_PASSWORD missing in $DB_ENV"; exit 1; }
printf '%s' "$app_pass" | c2_ 'sudo tee /etc/headscale/cluster/pg.pass >/dev/null
  sudo chown root:headscale /etc/headscale/cluster/pg.pass; sudo chmod 0640 /etc/headscale/cluster/pg.pass'
unset app_pass

log "config"
c2_ 'sudo tee /etc/headscale/cluster/config.yaml >/dev/null; sudo chown root:headscale /etc/headscale/cluster/config.yaml; sudo chmod 0640 /etc/headscale/cluster/config.yaml' <<'EOF'
# Stage 2a cluster c2 on mc-c2 (infra/lab/c2/setup-c2.sh). One Noise key and
# one listener for every tailnet this cluster owns (ADR 0005, ADR 0007).
# Public path: VPS 116.118.3.46:8445 -> wg-mesh -> mc-cp 10.99.77.2:8445 ->
# LAN -> mc-c2:8443.
server_url: https://c2.cp.gta5web.online:8445
listen_addr: 0.0.0.0:8443
# Not served by the cluster listener (AGENTS rule 9); loopback only.
metrics_listen_addr: 127.0.0.1:9095
unix_socket: /var/run/headscale-cluster/cluster.sock
unix_socket_permission: "0770"
disable_check_updates: true

tls:
  cert_path: /etc/headscale/tls/fullchain.pem
  key_path: /etc/headscale/tls/privkey.pem

noise:
  private_key_path: /var/lib/headscale/cluster/noise_private.key

prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential

# Each tailnet keeps its own policy in the database.
policy:
  mode: database

derp:
  server:
    enabled: false
  urls:
    - https://controlplane.tailscale.com/derpmap/default
  auto_update_enabled: true

# Long-lived lab database (stage 2), shared with every cluster of the lab.
database:
  type: postgres
  postgres:
    host: 192.168.1.243
    port: 6432
    name: meshlab
    user: headscale
    pass: file:/etc/headscale/cluster/pg.pass
    ssl: disable

dns:
  magic_dns: true
  base_domain: c2.tailnet.internal
  override_local_dns: false

log:
  level: info
EOF

log "systemd unit"
c2_ 'sudo tee /etc/systemd/system/headscale-cluster.service >/dev/null' <<'EOF'
[Unit]
Description=mesh-controlplane stage 2a cluster c2: many tailnets, one Noise key (mc-c2)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=headscale
Group=headscale
ExecStart=/usr/local/bin/headscale-cluster -c /etc/headscale/cluster/config.yaml cluster serve --cluster-id mc-c2
Restart=on-failure
RestartSec=5
RestartPreventExitStatus=78
RuntimeDirectory=headscale-cluster
RuntimeDirectoryMode=0770
StateDirectory=headscale headscale/cluster
StateDirectoryMode=0750
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
ReadWritePaths=/var/lib/headscale /var/run/headscale-cluster
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
EOF

log "nftables (mc-c2 only)"
c2_ 'sudo tee /etc/nftables.conf.new >/dev/null' <<'EOF'
#!/usr/sbin/nft -f
# mc-c2 (stage 2a): inbound only SSH from the LAN and the cluster port from the
# lab range (mc-cp relays the public path from 192.168.1.241).
table inet mc_c2
delete table inet mc_c2
table inet mc_c2 {
  chain input {
    type filter hook input priority filter; policy drop;
    iif "lo" accept
    ct state established,related accept
    ct state invalid drop
    ip protocol icmp accept
    meta l4proto ipv6-icmp accept
    ip saddr 192.168.1.0/24 tcp dport 22 accept
    ip saddr 192.168.1.240-192.168.1.249 tcp dport 8443 accept
  }
}
EOF
c2_ 'set -e
  sudo nft -c -f /etc/nftables.conf.new
  test -e /etc/nftables.conf.before-stage2a || sudo cp -p /etc/nftables.conf /etc/nftables.conf.before-stage2a
  sudo mv /etc/nftables.conf.new /etc/nftables.conf
  sudo nft -f /etc/nftables.conf
  sudo systemctl enable nftables >/dev/null 2>&1'

log "register $CLUSTER_ID (ACTIVE, $ENDPOINT)"
c2_ "sudo systemctl daemon-reload"
if out=$(c2_ "$HSC register --endpoint $ENDPOINT" 2>&1); then
  printf '%s\n' "$out"
elif printf '%s' "$out" | grep -qiE 'exist|duplicate'; then
  log "already registered"
else
  printf '%s\n' "$out" | tail -5; exit 1
fi

log "start"
c2_ 'sudo systemctl enable --now headscale-cluster && sleep 3 && sudo systemctl restart headscale-cluster'
for _ in $(seq 1 30); do
  code=$(cp_ "curl -s -o /dev/null -w '%{http_code}' --max-time 3 --resolve c2.cp.gta5web.online:8443:$C2 https://c2.cp.gta5web.online:8443/health" || true)
  [ "$code" = 200 ] && break
  sleep 2
done
log "health from mc-cp over LAN: ${code:-none}"
c2_ 'sudo journalctl -u headscale-cluster -b --no-pager -n 15' | grep -viE 'pass|secret|key=' || true
[ "$code" = 200 ]
