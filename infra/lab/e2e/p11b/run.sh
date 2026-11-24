#!/usr/bin/env bash
# P11b real-client E2E: cluster c1 as an active and a standby process on one
# PostgreSQL database and one Noise key, behind HAProxy with a /ready health
# check.
#
#   client -> VPS 116.118.3.46:8443 -> wg-mesh -> mc-cp 10.99.77.2:8443 (HAProxy,
#   TCP, TLS passthrough) -> the process whose /ready is 200:
#     mc-cp  192.168.1.241:8443   or   mc-c1b 192.168.1.245:8443
#
# HAProxy and both cluster processes run as transient units (no automatic
# restart) only during the run; the P02 POC on mc-cp is stopped and restored,
# as in the P05 harness. Database: e2e_p11b<run> on mc-db, dropped at cleanup.
#
# Cases (ticket tickets/P11b.md):
#   H01 standby serves nothing (/ready 503, /ts2021 503, direct)
#   F01 kill -9 of the active: the standby takes the lease (epoch+1); clients
#       come back through the same URL without login, same node ID/IP;
#       ping restored; a new node is seen; no double registration
#   F02 SIGSTOP of the active past the TTL: the standby takes over; after
#       SIGCONT the old process serves nothing (no MapResponse, metric
#       unchanged), is standby, and its lease epoch is stale
#   F03 the active loses PostgreSQL (nft drop on its host) while clients
#       still reach it: it stops serving before the standby is ready
#   F04 both processes start at once after the lease expired: one active
#   I01 a second tailnet b never sees a node of a, through every failover
#   L01 cluster_leases epochs only grow, one holder per epoch (audit)
#
# Usage: BIN=<linux/amd64 headscale binary> infra/lab/e2e/p11b/run.sh
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
B_SSH=${B_SSH:-mesh@192.168.1.245}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
PG_SSH=${PG_SSH:-mesh@192.168.1.243}
PG_LISTEN=${PG_LISTEN:-192.168.1.243:6432}
MC_DB_ENV=${MC_DB_ENV:-$HOME/.mesh-lab/mc-db.env}
HOST=c1.cp.gta5web.online
URL=https://$HOST:8443
POC_HEALTH=https://a.cp.gta5web.online:8443/health
CP_IP=192.168.1.241
B_IP=192.168.1.245
CLUSTER_ID=mc-cp-c1
IMAGE=${IMAGE:-tailscale/tailscale:v1.104.7}
BIN=${BIN:?BIN= linux/amd64 cluster binary}
KEEP=${KEEP:-0}
TTL=10  # tuning.cluster_lease.ttl default (seconds)
LABEL=mesh-e2e=p11b
PY=${PY:-python}

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_p11b$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/P11b/e2e-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

CFG=/etc/headscale/cluster/config.yaml
UNIT=mesh-c1-e2e
HSC="sudo -u headscale /usr/local/bin/headscale-cluster -c $CFG cluster --cluster-id $CLUSTER_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
ssh_() { local h=$1; shift; ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
b_() { ssh_ "$B_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }
pg_() { ssh_ "$PG_SSH" "$@"; }
on() { if [ "$1" = cp ]; then shift; cp_ "$@"; else shift; b_ "$@"; fi; }
ipof() { [ "$1" = cp ] && echo $CP_IP || echo $B_IP; }
other() { [ "$1" = cp ] && echo b || echo cp; }
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }
now_ms() { py 'import time; print(int(time.time()*1000))' | strip_cr; }

FAILS=0
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-4s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------ helpers -----
# direct NODE PATH: status of GET PATH straight to a process (SNI and Host of
# the cluster), bypassing HAProxy; 000 when it does not answer. Sent from
# mc-cp: its nft accepts 8443 only from the VPS tunnel (and itself), and
# mc-c1b accepts 8443 from the lab range.
direct() {
  local c; c=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve '$HOST:8443:$(ipof "$1")' 'https://$HOST:8443$2'" 2>/dev/null)
  echo "${c:-000}"
}
direct_upgrade() {
  local c; c=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve '$HOST:8443:$(ipof "$1")' -X POST \
    -H 'Upgrade: tailscale-control-protocol' -H 'Connection: upgrade' 'https://$HOST:8443/ts2021'" 2>/dev/null)
  echo "${c:-000}"
}
public_ready() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$URL/ready" || true; }
wait_direct_ready() { # NODE TIMEOUT_S -> 0 when ready
  local end=$(( $(date +%s) + $2 ))
  while [ "$(date +%s)" -lt "$end" ]; do [ "$(direct "$1" /ready)" = 200 ] && return 0; sleep 0.2; done
  return 1
}
lease_row() { pg_ "sudo docker exec mc-pg psql -At -U postgres -d $PG_DB -c \"SELECT holder||' '||epoch||' '||(expires_at > clock_timestamp()) FROM cluster_leases WHERE cluster_id='$CLUSTER_ID'\""; }
lease_epoch() { lease_row | awk '{print $2}'; }
start_proc() { # NODE: transient unit, no restart (kill -9 must stay dead)
  on "$1" "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    /usr/local/bin/headscale-cluster -c $CFG cluster serve --cluster-id $CLUSTER_ID"
}
stop_proc() { on "$1" "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; true"; }
proc_log() { on "$1" "sudo journalctl -u $UNIT --since -${2}s --no-pager" 2>/dev/null; }
map_sent() { # NODE: map frames (updates + keepalives) this process wrote to clients (metrics listener, loopback)
  on "$1" "curl -s -m 3 http://127.0.0.1:9095/metrics" 2>/dev/null \
    | awk '/^headscale_mapresponse_sent_total\{status="ok"/ {s+=$2} END {printf "%d", s}'
}

# clients (docker on mc-ci)
start_ts() {
  ci_ "docker run -d --name $1 --hostname $1 --label $LABEL --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL $IMAGE \
    tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done"
}
ts_up() { # NAME KEYFILE
  ci_ "docker exec -i $1 timeout 45 tailscale up --login-server=$URL --auth-key=file:/dev/stdin --hostname=$1" \
    <"$2" 2>&1 | redact >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
peers() { field "$1" "' '.join(sorted(p['HostName'] for p in (d.get('Peer') or {}).values()))"; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
state() { field "$1" "d['BackendState']"; }
nodekey() { field "$1" "d['Self']['PublicKey'][:16]"; }
wait_peers() { local i; for i in $(seq 1 "$3"); do [ "$(peers "$1")" = "$2" ] && return 0; sleep 1; done; return 1; }
ping_ok() { ci_ "docker exec $1 tailscale ping -c 3 --timeout 5s $2" >>"$ART_DIR/logs/$1-ping.log" 2>&1; }
needs_login() { ci_ "docker logs $1 2>&1" | grep -c 'NeedsLogin' || true; }
save_logs() { ci_ "docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }
nodes_json() { cp_ "$HSC tailnet nodes --slug $1" 2>/dev/null; }
node_ids() { nodes_json "$1" | py "import json; print(' '.join(f\"{n['hostname']}={n['id']}/{n['ipv4']}\" for n in sorted(json.load(sys.stdin), key=lambda n: n['hostname'])))" | strip_cr; }
node_count() { nodes_json "$1" | py "import json; print(len(json.load(sys.stdin)))" | strip_cr; }

# ------------------------------------------------------------ cleanup -----
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\"" || { log "clients not removed"; CLEAN_OK=0; }
  for n in cp b; do on $n "sudo nft delete table inet p11b_e2e 2>/dev/null; true"; stop_proc $n; done
  cp_ "sudo systemctl stop mesh-c1-ha 2>/dev/null; sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo rm -f /etc/haproxy/mesh-c1.cfg; true"
  # mc-c1b keeps no secret of c1 after the run.
  b_ "sudo rm -f /etc/headscale/cluster/pg.pass /var/lib/headscale/cluster/noise_private.key; \
      ! sudo test -e /var/lib/headscale/cluster/noise_private.key" || { log "c1 key not removed on mc-c1b"; CLEAN_OK=0; }
  cp_ "sudo rm -f /etc/headscale/cluster/pg.pass; ! sudo test -e /etc/headscale/cluster/pg.pass && sudo systemctl start headscale-tailnets" \
    || { log "mc-cp not restored"; CLEAN_OK=0; }
  [[ $PG_DB =~ ^e2e_[0-9a-z]+$ ]] && pg_ "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $PG_DB && \
    ! sudo docker exec mc-pg psql -At -U postgres -c \"SELECT 1 FROM pg_database WHERE datname = '$PG_DB'\" | grep -q 1" \
    && log "dropped database $PG_DB" || { log "database $PG_DB not dropped"; CLEAN_OK=0; }
  local i p=000
  for i in $(seq 1 30); do p=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$POC_HEALTH" || true); [ "$p" = 200 ] && break; sleep 1; done
  [ "$p" = 200 ] && log "P02 POC restored on mc-cp" || { log "POC not healthy ($p)"; CLEAN_OK=0; }
  rm -rf "$TMPD"
}
trap 'cleanup; exit 2' INT TERM

# -------------------------------------------------------------- setup -----
setup() {
  local pass
  pass=$(grep -E '^MC_DB_APP_PASSWORD=' "$MC_DB_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || return 1
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"
  log "database $PG_DB"
  pg_ "sudo docker exec mc-pg createdb -U postgres -O headscale $PG_DB" || return 1

  log "config (c1 on $PG_DB; mc-cp listens on its LAN address behind HAProxy)"
  awk -v host="${PG_LISTEN%:*}" -v port="${PG_LISTEN##*:}" -v db="$PG_DB" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    host: " host "\n    port: " port \
      "\n    name: " db "\n    user: headscale\n    pass: file:/etc/headscale/cluster/pg.pass\n    ssl: disable"; skip = 1; next }
    skip && /^[^ ]/ { skip = 0 }
    !skip { print }' "$REPO/infra/lab/mc-cp/cluster/config.yaml" >"$TMPD/c1.yaml"
  sed "s#^listen_addr: .*#listen_addr: $CP_IP:8443#" "$TMPD/c1.yaml" >"$TMPD/c1-cp.yaml"
  sed "s#^listen_addr: .*#listen_addr: 0.0.0.0:8443#" "$TMPD/c1.yaml" >"$TMPD/c1-b.yaml"
  grep -q "^listen_addr: $CP_IP:8443" "$TMPD/c1-cp.yaml" || return 1

  log "binary to mc-cp and mc-c1b"
  for n in cp b; do
    scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$BIN" "$([ $n = cp ] && echo $CP_SSH || echo $B_SSH):/tmp/p11b-hs" || return 1
    on $n "sudo install -m 0755 /tmp/p11b-hs /usr/local/bin/headscale-cluster && rm -f /tmp/p11b-hs" || return 1
  done
  [ "$(cp_ 'sha256sum /usr/local/bin/headscale-cluster' | cut -c1-64)" = "$(b_ 'sha256sum /usr/local/bin/headscale-cluster' | cut -c1-64)" ] || return 1

  log "mc-cp: stop POC, config, pg.pass"
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; \
       sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && \
       sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster && \
       sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  cp_ "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1-cp.yaml" || return 1
  # The cluster's Noise key exists on mc-cp (created on first c1 start); make
  # sure, then copy it to mc-c1b through pipes only.
  cp_ "sudo test -s /var/lib/headscale/cluster/noise_private.key" || return 1

  log "mc-c1b: user, dirs, TLS, Noise key of c1, config, pg.pass, nft"
  b_ "set -e
    id headscale >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/headscale --shell /usr/sbin/nologin headscale
    sudo install -d -o root -g headscale -m 0750 /etc/headscale /etc/headscale/tls /etc/headscale/cluster
    sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster" || return 1
  cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | b_ 'sudo install -o root -g headscale -m 0644 /dev/stdin /etc/headscale/tls/fullchain.pem' || return 1
  cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | b_ 'sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/tls/privkey.pem' || return 1
  cp_ 'sudo cat /var/lib/headscale/cluster/noise_private.key' | b_ 'sudo install -o headscale -g headscale -m 0600 /dev/stdin /var/lib/headscale/cluster/noise_private.key' || return 1
  [ "$(cp_ 'sudo sha256sum /var/lib/headscale/cluster/noise_private.key' | cut -c1-64)" = \
    "$(b_ 'sudo sha256sum /var/lib/headscale/cluster/noise_private.key' | cut -c1-64)" ] || { log "noise key copy mismatch"; return 1; }
  b_ "sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  b_ "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1-b.yaml" || return 1
  b_ "sudo tee /etc/nftables.conf >/dev/null && sudo nft -c -f /etc/nftables.conf && sudo nft -f /etc/nftables.conf && sudo systemctl enable nftables >/dev/null 2>&1" <<'EOF' || return 1
#!/usr/sbin/nft -f
# mc-c1b (P11b): SSH from the LAN, the c1 port from the lab range (HAProxy on
# mc-cp 192.168.1.241); everything else inbound dropped.
table inet mc_c1b
delete table inet mc_c1b
table inet mc_c1b {
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

  log "HAProxy on mc-cp ($URL -> /ready)"
  cp_ "command -v haproxy >/dev/null || { sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q haproxy >/dev/null && sudo systemctl disable --now haproxy >/dev/null 2>&1; }; command -v haproxy" >/dev/null || return 1
  cp_ "sudo tee /etc/haproxy/mesh-c1.cfg >/dev/null && sudo haproxy -c -q -f /etc/haproxy/mesh-c1.cfg" <<EOF || return 1
# P11b E2E only (infra/lab/e2e/p11b/run.sh): c1 traffic from the VPS tunnel
# to whichever process answers /ready 200. TCP, TLS passthrough.
global
  maxconn 4096
defaults
  mode tcp
  timeout connect 3s
  timeout client 1h
  timeout server 1h
  timeout tunnel 1h
frontend c1
  bind 10.99.77.2:8443
  default_backend c1
backend c1
  option httpchk
  http-check send meth GET uri /ready ver HTTP/1.1 hdr Host $HOST
  http-check expect status 200
  default-server inter 500ms fall 2 rise 2 check check-ssl check-sni $HOST verify required ca-file /etc/ssl/certs/ca-certificates.crt on-marked-down shutdown-sessions
  server mc-cp $CP_IP:8443
  server mc-c1b $B_IP:8443
EOF
  cp_ "sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo systemd-run --quiet --unit=mesh-c1-ha /usr/sbin/haproxy -Ws -f /etc/haproxy/mesh-c1.cfg" || return 1

  log "registry and tailnets a, b (owner $CLUSTER_ID)"
  cp_ "$HSC register --class shared --max-devices 5000 >/dev/null" || return 1
  local s pf=/var/lib/headscale/cluster/p11b-policy.hujson
  for s in a b; do cp_ "$HSC tailnet create --slug $s >/dev/null && $HSC tailnet status --slug $s --set ACTIVE" || return 1; done
  printf '%s\n' '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' \
    | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $HSC tailnet policy --slug a --file $pf && $HSC tailnet policy --slug b --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc" || return 1
  for s in a b; do
    cp_ "$HSC tailnet authkey --slug $s --user e2e-$s --reusable --expiration 90m" \
      | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s" || return 1
    chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  done

  log "start mc-cp process first (active), then mc-c1b (standby)"
  start_proc cp || return 1
  wait_direct_ready cp 30 || { log "mc-cp process not ready"; return 1; }
  start_proc b || return 1
  local i; for i in $(seq 1 30); do [ "$(direct b /health)" = 200 ] && break; sleep 1; done
  [ "$(direct b /health)" = 200 ] || { log "mc-c1b process not listening"; return 1; }
  for i in $(seq 1 30); do [ "$(public_ready)" = 200 ] && break; sleep 1; done
  [ "$(public_ready)" = 200 ] || { log "public /ready not 200 through HAProxy"; return 1; }
  ci_ "docker image inspect $IMAGE >/dev/null 2>&1 || docker pull -q $IMAGE >/dev/null" || return 1
}

# --------------------------------------------------------------- cases ----
ACTIVE=cp
t_h01() {
  local p=""
  [ "$(direct cp /ready)" = 200 ] || p="$p; mc-cp /ready $(direct cp /ready)"
  [ "$(direct b /ready)" = 503 ] || p="$p; standby /ready $(direct b /ready)"
  [ "$(direct b /health)" = 200 ] || p="$p; standby /health $(direct b /health)"
  [ "$(direct_upgrade b)" = 503 ] || p="$p; standby /ts2021 $(direct_upgrade b)"
  [ "$(direct b /metrics)" = 404 ] || p="$p; standby /metrics on the cluster port"
  local row; row=$(lease_row); printf 'H01 lease %s\n' "$row" >>"$ART_DIR/logs/lease.txt"
  [ "$(awk '{print $2}' <<<"$row")" = 1 ] || p="$p; lease [$row]"
  [ -z "$p" ] && record H01 PASS "active ready (lease epoch 1); standby /health 200, /ready 503, /ts2021 503, no /metrics" || record H01 FAIL "${p#; }"
}

t_clients() {
  local p="" r
  for c in p11b-a1 p11b-a2 p11b-b1; do start_ts $c || p="$p; start $c"; done
  r=0; ts_up p11b-a1 "$TMPD/key-a" || r=$?; [ $r = 0 ] || p="$p; a1 up $r"
  r=0; ts_up p11b-a2 "$TMPD/key-a" || r=$?; [ $r = 0 ] || p="$p; a2 up $r"
  r=0; ts_up p11b-b1 "$TMPD/key-b" || r=$?; [ $r = 0 ] || p="$p; b1 up $r"
  wait_peers p11b-a1 p11b-a2 30 || p="$p; a1 peers [$(peers p11b-a1)]"
  ping_ok p11b-a1 p11b-a2 || p="$p; a1 cannot ping a2"
  [ -z "$(peers p11b-b1)" ] || p="$p; b1 sees [$(peers p11b-b1)]"
  [ -z "$p" ] && record C00 PASS "a1<->a2 through HAProxy on the active, ping ok; b1 alone" || record C00 FAIL "${p#; }"
}

check_b_isolated() { # CASE
  case " $(peers p11b-b1) " in *p11b-a*) echo "b1 sees an a node in $1";; esac
}

t_f01() {
  local p="" ids_before ip1 nk1 e0 t0 t1 t2 t3 na nl1 nl2
  ids_before=$(node_ids a); ip1=$(selfip p11b-a1); nk1=$(nodekey p11b-a1); e0=$(lease_epoch); na=$(node_count a)
  # Every client passes NeedsLogin once before its first login: count after.
  nl1=$(needs_login p11b-a1); nl2=$(needs_login p11b-a2)
  t0=$(now_ms)
  cp_ "sudo systemctl kill --kill-whom=main -s KILL $UNIT" || { record F01 FAIL "kill"; return; }
  wait_direct_ready b 40 || p="$p; standby not ready in 40s"
  t1=$(now_ms)
  local i; for i in $(seq 1 60); do [ "$(state p11b-a1)" = Running ] && ping_ok p11b-a1 p11b-a2 && break; sleep 1; done
  t2=$(now_ms)
  ping_ok p11b-a1 p11b-a2 || p="$p; a1 cannot ping a2 after failover"
  start_ts p11b-a3 >/dev/null; local r=0; ts_up p11b-a3 "$TMPD/key-a" || r=$?
  wait_peers p11b-a1 "p11b-a2 p11b-a3" 60 || p="$p; a1 does not see a3 [$(peers p11b-a1)]"
  t3=$(now_ms)
  [ "$(lease_epoch)" = $((e0 + 1)) ] || p="$p; lease epoch $e0 -> $(lease_epoch)"
  [ "$(selfip p11b-a1)" = "$ip1" ] && [ "$(nodekey p11b-a1)" = "$nk1" ] || p="$p; a1 IP/key changed"
  local ids_after; ids_after=$(node_ids a)
  for kv in $ids_before; do grep -qw -- "$kv" <<<"$ids_after" || p="$p; node $kv changed"; done
  [ "$(node_count a)" = $((na + 1)) ] || p="$p; a nodes $na -> $(node_count a) (want +1: a3)"
  [ "$(needs_login p11b-a1)" = "$nl1" ] && [ "$(needs_login p11b-a2)" = "$nl2" ] || p="$p; NeedsLogin after the failover"
  local iso; iso=$(check_b_isolated F01); [ -z "$iso" ] || p="$p; $iso"
  printf 'F01 kill->standby ready %d ms; ->a1 Running+ping %d ms; ->a1 sees new a3 %d ms\n' \
    $((t1 - t0)) $((t2 - t0)) $((t3 - t0)) >>"$ART_DIR/timings.txt"
  printf 'F01 nodes before [%s] after [%s]\n' "$ids_before" "$ids_after" >>"$ART_DIR/logs/nodes.txt"
  [ -z "$p" ] && record F01 PASS "kill -9 active: standby ready in $((t1 - t0)) ms (lease epoch $((e0 + 1))), a1 Running+ping in $((t2 - t0)) ms without login (same node ID/IP/key), a3 seen in $((t3 - t0)) ms; b isolated" \
    || record F01 FAIL "${p#; }"
  ACTIVE=b
}

t_f02() {
  local p="" e0 t0 t1 sent0 sent1 since
  # mc-cp back as standby.
  start_proc cp; sleep 3
  [ "$(direct cp /ready)" = 503 ] || p="$p; restarted mc-cp is not standby ($(direct cp /ready))"
  e0=$(lease_epoch)
  sent0=$(map_sent b)
  t0=$(now_ms)
  b_ "sudo systemctl kill --kill-whom=main -s STOP $UNIT" || { record F02 FAIL "SIGSTOP"; return; }
  wait_direct_ready cp $((TTL + 20)) || p="$p; mc-cp did not take over"
  t1=$(now_ms)
  sleep 3
  since=$(date +%s)
  b_ "sudo systemctl kill --kill-whom=main -s CONT $UNIT"
  sleep 12
  sent1=$(map_sent b)
  [ "$(direct b /ready)" = 503 ] || p="$p; resumed process /ready $(direct b /ready)"
  [ "$(direct_upgrade b)" = 503 ] || p="$p; resumed process /ts2021 $(direct_upgrade b)"
  [ "${sent0:-0}" -gt 0 ] || p="$p; active sent no MapResponse before SIGSTOP (metric not read?)"
  [ -n "$sent0" ] && [ "$sent0" = "$sent1" ] || p="$p; resumed process sent MapResponses ($sent0 -> $sent1)"
  proc_log b $(( $(date +%s) - since + 2 )) | redact >"$ART_DIR/logs/F02-resumed.log"
  grep -qE 'self-fence deadline|lease lost' "$ART_DIR/logs/F02-resumed.log" || p="$p; no lease-end log after resume"
  [ "$(lease_epoch)" = $((e0 + 1)) ] || p="$p; lease epoch $e0 -> $(lease_epoch)"
  local i; for i in $(seq 1 60); do [ "$(state p11b-a1)" = Running ] && ping_ok p11b-a1 p11b-a2 && break; sleep 1; done
  ping_ok p11b-a1 p11b-a2 || p="$p; a1 cannot ping a2 on the new active"
  start_ts p11b-a4 >/dev/null; local r=0; ts_up p11b-a4 "$TMPD/key-a" || r=$?
  wait_peers p11b-a1 "p11b-a2 p11b-a3 p11b-a4" 60 || p="$p; a1 does not see a4 [$(peers p11b-a1)]"
  local iso; iso=$(check_b_isolated F02); [ -z "$iso" ] || p="$p; $iso"
  printf 'F02 SIGSTOP->mc-cp ready %d ms; MapResponses of the resumed process %s -> %s\n' $((t1 - t0)) "$sent0" "$sent1" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record F02 PASS "SIGSTOP active past TTL: other process ready in $((t1 - t0)) ms (epoch $((e0 + 1))); after SIGCONT the old one sent no MapResponse ($sent0 -> $sent1), /ready 503, /ts2021 503, logged its lease end; clients on the new active see a4" \
    || record F02 FAIL "${p#; }"
  ACTIVE=cp
}

t_f03() {
  local p="" e0 tself tready
  # Active: mc-cp; standby: mc-c1b (it resumed as standby in F02).
  [ "$(direct b /ready)" = 503 ] || p="$p; mc-c1b not standby before F03"
  e0=$(lease_epoch)
  # Drop mc-cp -> PostgreSQL (PgBouncer) only; clients still reach mc-cp.
  # The cut and the polling of both /ready run in one command on mc-cp
  # (its clock, ~100 ms resolution): "<ms active stopped> <ms standby ready>
  # <MapResponses of mc-cp when it stopped>", relative to the cut.
  local out sent_stop sent_end
  out=$(cp_ "sudo nft -f - <<'EOF' || exit 1
table inet p11b_e2e {
  chain output { type filter hook output priority filter; policy accept; ip daddr ${PG_LISTEN%:*} tcp dport ${PG_LISTEN##*:} drop; }
}
EOF
t0=\$(date +%s%3N); end=\$((t0 + $(( (TTL + 30) * 1000 )))); ts=-; tr=-; sent=-
rd() { curl -s -o /dev/null -w '%{http_code}' -m 1 --resolve '$HOST:8443:'\$1 'https://$HOST:8443/ready'; }
while [ \$(date +%s%3N) -lt \$end ]; do
  now=\$(date +%s%3N)
  if [ \$ts = - ] && [ \"\$(rd $CP_IP)\" != 200 ]; then ts=\$((now - t0)); sent=\$(curl -s -m 2 http://127.0.0.1:9095/metrics | awk '/^headscale_mapresponse_sent_total\\{status=\"ok\"/ {s+=\$2} END {printf \"%d\", s}'); fi
  if [ \$tr = - ] && [ \"\$(rd $B_IP)\" = 200 ]; then tr=\$((now - t0)); fi
  [ \$ts != - ] && [ \$tr != - ] && break
  sleep 0.05
done
echo \$ts \$tr \$sent") || { cp_ "sudo nft delete table inet p11b_e2e" 2>/dev/null; record F03 FAIL "nft/poll"; return; }
  read -r tself tready sent_stop <<<"$(strip_cr <<<"$out")"
  sleep 3
  sent_end=$(map_sent cp)
  cp_ "sudo nft delete table inet p11b_e2e"
  [ "$tself" != - ] || p="$p; partitioned active never stopped serving"
  [ "$tready" != - ] || p="$p; standby never became ready"
  if [ "$tself" != - ] && [ "$tready" != - ] && [ "$tready" -lt "$tself" ]; then p="$p; standby ready $((tself - tready)) ms BEFORE the old active stopped (overlap)"; fi
  [ "$sent_stop" != - ] && [ "${sent_stop:-0}" -gt 0 ] || p="$p; active sent no MapResponse before the cut (metric not read?)"
  [ "$sent_stop" != - ] && [ "$sent_stop" = "$sent_end" ] || p="$p; fenced active sent MapResponses after it stopped ($sent_stop -> $sent_end)"
  [ "$(lease_epoch)" = $((e0 + 1)) ] || p="$p; lease epoch $e0 -> $(lease_epoch)"
  local i; for i in $(seq 1 60); do [ "$(state p11b-a1)" = Running ] && ping_ok p11b-a1 p11b-a2 && break; sleep 1; done
  ping_ok p11b-a1 p11b-a2 || p="$p; a1 cannot ping a2 after F03"
  sleep 5
  [ "$(direct cp /ready)" = 503 ] || p="$p; mc-cp /ready $(direct cp /ready) after the partition healed (want standby)"
  local iso; iso=$(check_b_isolated F03); [ -z "$iso" ] || p="$p; $iso"
  printf 'F03 partition->active stops %s ms; ->standby ready %s ms (mc-cp clock); MapResponses of the fenced active %s -> %s\n' "$tself" "$tready" "$sent_stop" "$sent_end" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record F03 PASS "active cut from PostgreSQL: stopped serving at $tself ms, standby ready at $tready ms (no overlap, epoch $((e0 + 1))); fenced active sent no MapResponse after ($sent_stop -> $sent_end); old active standby after heal; clients ok" \
    || record F03 FAIL "${p#; }"
  ACTIVE=b
}

t_f04() {
  local p="" e0 ready
  e0=$(lease_epoch)
  stop_proc cp; b_ "sudo systemctl kill --kill-whom=main -s KILL $UNIT"; stop_proc b
  sleep $((TTL + 2))
  # Start both at once.
  start_proc cp & start_proc b & wait
  wait_direct_ready cp 20 || wait_direct_ready b 20 || p="$p; nobody ready"
  local i ok=1
  for i in $(seq 1 20); do
    ready=0
    [ "$(direct cp /ready)" = 200 ] && ready=$((ready + 1))
    [ "$(direct b /ready)" = 200 ] && ready=$((ready + 1))
    [ $ready = 1 ] || { ok=0; p="$p; $ready ready at sample $i"; break; }
    sleep 0.5
  done
  [ "$(lease_epoch)" = $((e0 + 1)) ] || p="$p; lease epoch $e0 -> $(lease_epoch) (want +1: one winner)"
  for i in $(seq 1 60); do [ "$(state p11b-a1)" = Running ] && ping_ok p11b-a1 p11b-a2 && break; sleep 1; done
  ping_ok p11b-a1 p11b-a2 || p="$p; a1 cannot ping a2 after F04"
  [ -z "$p" ] && record F04 PASS "both processes started at once after expiry: exactly one ready over 10 s, lease epoch +1; clients back" \
    || record F04 FAIL "${p#; }"
}

t_l01() {
  local p="" log
  local since=$(( $(date +%s) - T0 + 5 ))
  log=$( { proc_log cp $since; proc_log b $since; } | grep -aE 'cluster lease acquired|cluster lease ended' | redact)
  printf '%s\n' "$log" >"$ART_DIR/logs/lease-events.log"
  # Every acquired epoch appears once, epochs strictly increase in time.
  py 'import re,sys
ev=[]
for l in open(sys.argv[1], encoding="utf-8", errors="replace"):
    e=re.search(r"lease_epoch=(\d+)", l)
    t=re.search(r"(\d{4}-\d\d-\d\dT[0-9:.]+Z)", l)
    if "cluster lease acquired" in l and e and t:
        ev.append((t.group(1), int(e.group(1))))
ev.sort()
eps=[e for _,e in ev]
ok = eps == sorted(set(eps)) and len(eps) == len(set(eps))
print("ok" if ok else "bad", eps)' "$ART_DIR/logs/lease-events.log" | strip_cr >"$TMPD/l01" || true
  grep -q '^ok' "$TMPD/l01" || p="epochs not strictly increasing or reused: $(cat "$TMPD/l01")"
  [ -z "$p" ] && record L01 PASS "lease epochs acquired, in time order: $(cut -d' ' -f2- "$TMPD/l01")" || record L01 FAIL "$p"
}

t_i01() {
  local p=""
  [ -z "$(peers p11b-b1)" ] || p="b1 sees [$(peers p11b-b1)]"
  grep -E 'p11b-a[0-9]' <<<"$(peers p11b-b1)" && p="$p; a host in b"
  [ -z "$p" ] && record I01 PASS "b1 saw no node of a after F01-F04" || record I01 FAIL "$p"
}

t_secrets() {
  local hits
  hits=$(grep -rlE 'hskey-auth-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
  [ -z "$hits" ] && record S01 PASS "no auth key, private key or full machine/node key in the artifacts" || record S01 FAIL "unredacted in: $hits"
}

# ----------------------------------------------------------------- main ---
T0=$(date +%s)
log "P11b E2E $RUN_ID at $SHA -> $URL (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "image: $IMAGE"; echo "commit: $SHA"; echo "database: $PG_DB"; echo "binary: $(sha256sum "$BIN" | cut -c1-16)"; } >>"$ART_DIR/env.txt"
t_h01
t_clients
t_f01
t_f02
t_f03
t_f04
t_i01
t_l01
for n in cp b; do proc_log $n $(( $(date +%s) - T0 + 5 )) | redact >"$ART_DIR/logs/$n-process.log"; done
lease_row >>"$ART_DIR/logs/lease.txt"
t_secrets
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
