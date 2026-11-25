#!/usr/bin/env bash
# P12 real-client E2E: two clusters, each an active and a standby process, on
# one PostgreSQL database; tailnets placed by mesh-placement; bootstrap
# returns the owner cluster; restart and failover on both clusters; a move
# between clusters by hand-over (ADR 0010).
#
#   c1 (mc-cp-c1): https://c1.cp.gta5web.online:8443
#      VPS -> wg-mesh -> mc-cp 10.99.77.2:8443 (HAProxy mesh-c1-ha, transient)
#      -> mc-cp 192.168.1.241:8443 | mc-c1b 192.168.1.245:8443
#   c2 (mc-c2):    https://c2.cp.gta5web.online:8445
#      VPS -> wg-mesh -> mc-cp 10.99.77.2:8445 (HAProxy mesh-c2-ha, long-lived)
#      -> mc-c2 192.168.1.244:8443 | mc-c2b 192.168.1.246:8443
#   bootstrap: https://boot.cp.gta5web.online:8444 (mc-cp, transient)
#   mesh-placement: 127.0.0.1:9443 on mc-cp, mTLS with a throwaway CA (transient)
#
# Isolation from the long-lived lab: the run's binary is installed as
# /usr/local/bin/headscale-cluster-p12 and its config as
# /etc/headscale/cluster/config-p12.yaml on every host (the long-lived files
# are not modified); processes run as transient units $UNIT. The long-lived
# c2 (meshlab) and the P02 POC on mc-cp are stopped for the run and
# restarted at cleanup. Database e2e_p12<run> on mc-db, dropped at cleanup.
#
# Cases (tickets/P12.md, step 7):
#   P01 3 tailnets created through mesh-placement spread over both clusters;
#       each owner loads its tailnets; bootstrap returns the owner's URL;
#       reconcile report empty
#   P02 2 clients per tailnet (one alone) enrolled at the URL bootstrap
#       returned: peers only within the tailnet, ping, per-tailnet IPAM
#   P03 F07: a key presented to the cluster that does not own its tailnet is
#       refused, no node anywhere
#   P04/P05 restart of c1 / c2 (both processes) while the other serves: the
#       other's clients keep pinging; clients come back without login
#   P06/P07 F01 kill -9 of the active of c1 / c2: the standby takes over;
#       clients back without login; the other cluster's lease and clients
#       untouched
#   P08/P09 F02 SIGSTOP of the active of c1 / c2 past the TTL: the other
#       process takes over; after SIGCONT the old one serves nothing
#   P10 move of a tailnet c1 -> c2 by hand-over: bootstrap MOVING then c2;
#       c1 handed over before c2 loaded (logs); epoch + 1; clients re-login
#       at c2; the old key refused at c1; other tailnets unaffected
#   P11 audit: owner_epoch per tailnet, lease epochs strictly increasing per
#       cluster, one live holder per cluster; reconcile report empty
#   P12 artifacts hold no secret
#
# Usage: BIN=<headscale> PLBIN=<mesh-placement> BOOT_BIN=<mesh-bootstrap> run.sh
#        (linux/amd64; IMAGE=tailscale/tailscale:vX to change the client)
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
C1B_SSH=${C1B_SSH:-mesh@192.168.1.245}
C2_SSH=${C2_SSH:-mesh@192.168.1.244}
C2B_SSH=${C2B_SSH:-mesh@192.168.1.246}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
PG_SSH=${PG_SSH:-mesh@192.168.1.243}
PG_LISTEN=${PG_LISTEN:-192.168.1.243:6432}
MC_DB_ENV=${MC_DB_ENV:-$HOME/.mesh-lab/mc-db.env}
C1_ID=mc-cp-c1
C2_ID=mc-c2
HOST1=c1.cp.gta5web.online
HOST2=c2.cp.gta5web.online
URL1=https://$HOST1:8443
URL2=https://$HOST2:8445
POC_HEALTH=https://a.cp.gta5web.online:8443/health
BOOT_HOST=boot.cp.gta5web.online
BOOT_URL=https://$BOOT_HOST:8444
BOOT_LISTEN=10.99.77.2:8444
IMAGE=${IMAGE:-tailscale/tailscale:v1.104.7}
BIN=${BIN:?BIN= linux/amd64 cluster binary}
PLBIN=${PLBIN:?PLBIN= linux/amd64 mesh-placement binary}
BOOT_BIN=${BOOT_BIN:?BOOT_BIN= linux/amd64 mesh-bootstrap binary}
KEEP=${KEEP:-0}
TTL=10
LABEL=mesh-e2e=p12
PY=${PY:-python}

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_p12$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/P12/e2e-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

HS=/usr/local/bin/headscale-cluster-p12
CFG=/etc/headscale/cluster/config-p12.yaml
UNIT=mesh-p12-e2e
PLUNIT=mesh-placement-p12
BOOTUNIT=mesh-bootstrap-p12
PLDIR=/run/p12-placement
CLI1="sudo -u headscale $HS -c $CFG cluster --cluster-id $C1_ID"
CLI2="sudo -u headscale $HS -c $CFG cluster --cluster-id $C2_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
ssh_() { local h=$1; shift; ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }
pg_() { ssh_ "$PG_SSH" "$@"; }
sshof() { case $1 in cp) echo "$CP_SSH";; c1b) echo "$C1B_SSH";; c2) echo "$C2_SSH";; c2b) echo "$C2B_SSH";; esac; }
on() { local n=$1; shift; ssh_ "$(sshof "$n")" "$@"; }
ipof() { case $1 in cp) echo 192.168.1.241;; c1b) echo 192.168.1.245;; c2) echo 192.168.1.244;; c2b) echo 192.168.1.246;; esac; }
clusterof() { case $1 in cp|c1b) echo c1;; *) echo c2;; esac; }
hostof() { [ "$1" = c1 ] && echo $HOST1 || echo $HOST2; }
urlof() { [ "$1" = c1 ] && echo $URL1 || echo $URL2; }
idof() { [ "$1" = c1 ] && echo $C1_ID || echo $C2_ID; }
nodesof() { [ "$1" = c1 ] && echo "cp c1b" || echo "c2 c2b"; }
otherc() { [ "$1" = c1 ] && echo c2 || echo c1; }
cli() { local c=$1; shift; if [ "$c" = c1 ] && true; then cp_ "$CLI1 $*"; else on c2 "$CLI2 $*"; fi; }
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

# ------------------------------------------------------------ processes ---
# direct NODE PATH: GET straight to one process (its cluster's SNI/Host),
# from mc-cp (mc-c1b, mc-c2, mc-c2b accept 8443 from the lab range).
direct() {
  local c h; h=$(hostof "$(clusterof "$1")")
  c=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve '$h:8443:$(ipof "$1")' 'https://$h:8443$2'" 2>/dev/null)
  echo "${c:-000}"
}
public_ready() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$(urlof "$1")/ready" || true; }
wait_direct_ready() { local end=$(( $(date +%s) + $2 )); while [ "$(date +%s)" -lt "$end" ]; do [ "$(direct "$1" /ready)" = 200 ] && return 0; sleep 0.2; done; return 1; }
wait_public_ready() { local i; for i in $(seq 1 "$2"); do [ "$(public_ready "$1")" = 200 ] && return 0; sleep 1; done; return 1; }
start_proc() { # NODE: transient unit, no restart
  on "$1" "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    $HS -c $CFG cluster serve --cluster-id $(idof "$(clusterof "$1")")"
}
stop_proc() { on "$1" "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; true"; }
proc_log() { on "$1" "sudo journalctl -u $UNIT --since -${2}s --no-pager -o short-iso-precise" 2>/dev/null; }
map_sent() { on "$1" "curl -s -m 3 http://127.0.0.1:9095/metrics" 2>/dev/null | awk '/^headscale_mapresponse_sent_total\{status="ok"/ {s+=$2} END {printf "%d", s}'; }
active_of() { local n; for n in $(nodesof "$1"); do [ "$(direct "$n" /ready)" = 200 ] && { echo "$n"; return; }; done; echo none; }
standby_of() { local a n; a=$(active_of "$1"); for n in $(nodesof "$1"); do [ "$n" != "$a" ] && echo "$n"; done; }
psql_() { pg_ "sudo docker exec mc-pg psql -At -U postgres -d $PG_DB -c \"$1\""; }
lease_row() { psql_ "SELECT holder||' '||epoch||' '||(expires_at > clock_timestamp()) FROM cluster_leases WHERE cluster_id='$(idof "$1")'"; }
lease_epoch() { lease_row "$1" | awk '{print $2}'; }

# --------------------------------------------------------------- clients ---
start_ts() {
  ci_ "docker run -d --name $1 --hostname $1 --label $LABEL --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL $IMAGE \
    tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done"
}
ts_up() { # NAME URL KEYFILE [EXTRA]
  ci_ "docker exec -i $1 timeout 45 tailscale up --login-server=$2 --auth-key=file:/dev/stdin --hostname=$1 ${4:-}" \
    <"$3" 2>&1 | redact | tee "$TMPD/last.out" >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
peers() { field "$1" "' '.join(sorted(p['HostName'] for p in (d.get('Peer') or {}).values()))"; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
state() { field "$1" "d['BackendState']"; }
nodekey() { field "$1" "d['Self']['PublicKey'][:16]"; }
wait_peers() { local i; for i in $(seq 1 "$3"); do [ "$(peers "$1")" = "$2" ] && return 0; sleep 1; done; return 1; }
wait_running() { local i; for i in $(seq 1 "$2"); do [ "$(state "$1")" = Running ] && return 0; sleep 1; done; return 1; }
ping_ok() { ci_ "docker exec $1 tailscale ping -c 3 --timeout 5s $2" >>"$ART_DIR/logs/$1-ping.log" 2>&1; }
needs_login() { ci_ "docker logs $1 2>&1" | grep -c 'NeedsLogin' || true; }
save_logs() { ci_ "docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }
drop_ts() { ci_ "docker rm -f $1 >/dev/null 2>&1; docker volume rm $1 >/dev/null 2>&1; true"; }

# Tailnets x, y, z; OWNER_<slug> = c1|c2 (set from placement), ID_<slug>.
owner() { eval "echo \$OWNER_$1"; }
tid() { eval "echo \$ID_$1"; }
nodes_json() { cli "$(owner "$1")" tailnet nodes --slug "$1" 2>/dev/null; }
node_count() { nodes_json "$1" | py "import json; print(len(json.load(sys.stdin)))" | strip_cr; }
counts() { echo "x=$(node_count x) y=$(node_count y) z=$(node_count z)"; }

# placement API over mTLS on mc-cp loopback: pl METHOD PATH [IDEMPOTENCY-KEY] [BODY]
pl() {
  local ik=${3:-} b=${4:-}
  cp_ "sudo curl -s -m 10 --cacert $PLDIR/ca.crt --cert $PLDIR/op.crt --key $PLDIR/op.key -X $1 \
    ${ik:+-H 'Idempotency-Key: $ik'} ${b:+-H 'Content-Type: application/json' --data '$b'} https://127.0.0.1:9443$2"
}
jget() { py "import json; d=json.loads(sys.argv[1]); print($2)" "$1" 2>/dev/null | strip_cr; }
findings() { jget "$(pl GET /v1/reconcile)" "json.dumps(d.get('findings'))"; }

# boot_req KEYFILE [TENANT] -> "<status> <body>" (key id + SHA-256 proof only)
boot_req() {
  local body out=$TMPD/boot.out code
  body=$(py 'import hashlib,json
k=open(sys.argv[1]).read().strip()
i,s=k.split("hskey-auth-",1)[1].split("-",1)
d={"credential":{"kind":"authkey_proof","key_id":i,"proof":hashlib.sha256(s.encode()).hexdigest()}}
if sys.argv[2]: d["tenant"]=sys.argv[2]
sys.stdout.write(json.dumps(d))' "$1" "${2:-}" | strip_cr)
  code=$(printf %s "$body" | curl -s -m 10 -o "$out" -w '%{http_code}' -H 'Content-Type: application/json' --data-binary @- "$BOOT_URL/v1/bootstrap")
  printf '%s %s' "$code" "$(cat "$out" 2>/dev/null)"; rm -f "$out"
}
boot_url() { py 'import json; d=json.loads(sys.argv[1].split(" ",1)[1]); print(d.get("control_url",""))' "$1" 2>/dev/null | strip_cr; }

# ---------------------------------------------------------------- cleanup ---
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\"" || { log "clients not removed"; CLEAN_OK=0; }
  local n
  for n in cp c1b c2 c2b; do
    on $n "sudo systemctl kill -s CONT $UNIT 2>/dev/null; sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; \
      sudo rm -f $CFG $HS; ! sudo test -e $CFG && ! test -e $HS" || { log "$n: run files not removed"; CLEAN_OK=0; }
  done
  cp_ "for u in $PLUNIT $BOOTUNIT mesh-c1-ha; do sudo systemctl stop \$u 2>/dev/null; sudo systemctl reset-failed \$u 2>/dev/null; done; \
       sudo rm -rf $PLDIR /etc/haproxy/mesh-c1-p12.cfg /usr/local/bin/mesh-placement-p12 /usr/local/bin/mesh-bootstrap-p12; ! sudo test -e $PLDIR" \
    || { log "mc-cp placement/bootstrap/HAProxy not removed"; CLEAN_OK=0; }
  # mc-c1b keeps no secret of c1 after the run (as P11b).
  on c1b "sudo rm -f /etc/headscale/cluster/pg.pass /var/lib/headscale/cluster/noise_private.key; \
      ! sudo test -e /var/lib/headscale/cluster/noise_private.key" || { log "c1 key not removed on mc-c1b"; CLEAN_OK=0; }
  cp_ "sudo rm -f /etc/headscale/cluster/pg.pass; ! sudo test -e /etc/headscale/cluster/pg.pass && sudo systemctl start headscale-tailnets" \
    || { log "mc-cp POC not restarted"; CLEAN_OK=0; }
  # Long-lived c2 (meshlab) back on both hosts.
  for n in c2 c2b; do on $n "sudo systemctl start headscale-cluster" || { log "$n: long-lived c2 not started"; CLEAN_OK=0; }; done
  [[ $PG_DB =~ ^e2e_[0-9a-z]+$ ]] && pg_ "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $PG_DB && \
    ! sudo docker exec mc-pg psql -At -U postgres -c \"SELECT 1 FROM pg_database WHERE datname = '$PG_DB'\" | grep -q 1" \
    && log "dropped database $PG_DB" || { log "database $PG_DB not dropped"; CLEAN_OK=0; }
  local i p=000 q=000
  for i in $(seq 1 40); do
    p=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$POC_HEALTH" || true); q=$(public_ready c2)
    [ "$p$q" = 200200 ] && break; sleep 1
  done
  [ "$p" = 200 ] && log "P02 POC restored on mc-cp" || { log "POC not healthy ($p)"; CLEAN_OK=0; }
  [ "$q" = 200 ] && log "long-lived c2 /ready 200 on meshlab" || { log "long-lived c2 /ready $q"; CLEAN_OK=0; }
  rm -rf "$TMPD"
}
trap 'cleanup; exit 2' INT TERM

# ------------------------------------------------------------------ setup ---
setup() {
  local pass i n
  pass=$(grep -E '^MC_DB_APP_PASSWORD=' "$MC_DB_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || return 1
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"
  log "database $PG_DB"
  pg_ "sudo docker exec mc-pg createdb -U postgres -O headscale $PG_DB" || return 1

  log "binary $HS on the four hosts"
  for n in cp c1b c2 c2b; do
    scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$BIN" "$(sshof $n):/tmp/p12-hs" || return 1
    on $n "sudo install -m 0755 /tmp/p12-hs $HS && rm -f /tmp/p12-hs" || return 1
  done

  log "c1 config (e2e database; mc-cp on its LAN address behind HAProxy)"
  awk -v host="${PG_LISTEN%:*}" -v port="${PG_LISTEN##*:}" -v db="$PG_DB" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    host: " host "\n    port: " port \
      "\n    name: " db "\n    user: headscale\n    pass: file:/etc/headscale/cluster/pg.pass\n    ssl: disable"; skip = 1; next }
    skip && /^[^ ]/ { skip = 0 }
    !skip { print }' "$REPO/infra/lab/mc-cp/cluster/config.yaml" >"$TMPD/c1.yaml"
  sed "s#^listen_addr: .*#listen_addr: 192.168.1.241:8443#" "$TMPD/c1.yaml" >"$TMPD/c1-cp.yaml"
  sed "s#^listen_addr: .*#listen_addr: 0.0.0.0:8443#" "$TMPD/c1.yaml" >"$TMPD/c1-b.yaml"
  grep -q "^listen_addr: 192.168.1.241:8443" "$TMPD/c1-cp.yaml" || return 1

  log "mc-cp: stop POC, pg.pass, config"
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; \
       sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && \
       sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster && \
       sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  cp_ "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1-cp.yaml" || return 1
  cp_ "sudo test -s /var/lib/headscale/cluster/noise_private.key" || return 1

  log "mc-c1b: TLS, Noise key of c1, pg.pass, config (pipes only)"
  on c1b "set -e
    id headscale >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/headscale --shell /usr/sbin/nologin headscale
    sudo install -d -o root -g headscale -m 0750 /etc/headscale /etc/headscale/tls /etc/headscale/cluster
    sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster" || return 1
  cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | on c1b 'sudo install -o root -g headscale -m 0644 /dev/stdin /etc/headscale/tls/fullchain.pem' || return 1
  cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | on c1b 'sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/tls/privkey.pem' || return 1
  cp_ 'sudo cat /var/lib/headscale/cluster/noise_private.key' | on c1b 'sudo install -o headscale -g headscale -m 0600 /dev/stdin /var/lib/headscale/cluster/noise_private.key' || return 1
  [ "$(cp_ 'sudo sha256sum /var/lib/headscale/cluster/noise_private.key' | cut -c1-64)" = \
    "$(on c1b 'sudo sha256sum /var/lib/headscale/cluster/noise_private.key' | cut -c1-64)" ] || { log "c1 noise key copy mismatch"; return 1; }
  on c1b "sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  on c1b "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1-b.yaml" || return 1
  on c1b "sudo nft list table inet mc_c1b >/dev/null" || { log "mc-c1b nft table missing (LAB-c1b)"; return 1; }

  log "mc-c2, mc-c2b: stop long-lived c2; run config = theirs on $PG_DB"
  for n in c2 c2b; do
    on $n "set -e; sudo systemctl stop headscale-cluster
      sudo sed 's/^    name: .*/    name: $PG_DB/' /etc/headscale/cluster/config.yaml | sudo install -o root -g headscale -m 0640 /dev/stdin $CFG
      sudo grep -q '^    name: $PG_DB\$' $CFG" || return 1
  done

  log "HAProxy for c1 on mc-cp ($URL1 -> /ready)"
  cp_ "sudo tee /etc/haproxy/mesh-c1-p12.cfg >/dev/null && sudo haproxy -c -q -f /etc/haproxy/mesh-c1-p12.cfg" <<EOF || return 1
# P12 E2E only (infra/lab/e2e/p12/run.sh): c1 traffic to the process whose /ready is 200.
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
  http-check send meth GET uri /ready ver HTTP/1.1 hdr Host $HOST1
  http-check expect status 200
  default-server inter 500ms fall 2 rise 2 check check-ssl check-sni $HOST1 verify required ca-file /etc/ssl/certs/ca-certificates.crt on-marked-down shutdown-sessions
  server mc-cp 192.168.1.241:8443
  server mc-c1b 192.168.1.245:8443
EOF
  cp_ "sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo systemd-run --quiet --unit=mesh-c1-ha /usr/sbin/haproxy -Ws -f /etc/haproxy/mesh-c1-p12.cfg" || return 1

  log "mesh-placement on mc-cp loopback (throwaway CA under $PLDIR)"
  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$PLBIN" "$CP_SSH:/tmp/p12-pl" || return 1
  cp_ "set -e; sudo install -m 0755 /tmp/p12-pl /usr/local/bin/mesh-placement-p12; rm -f /tmp/p12-pl
    sudo install -d -o root -g headscale -m 0751 $PLDIR; cd $PLDIR
    sudo openssl ecparam -name prime256v1 -genkey -noout -out ca.key 2>/dev/null
    sudo openssl req -x509 -new -key ca.key -subj /CN=p12-e2e-ca -days 1 -out ca.crt 2>/dev/null
    sudo openssl ecparam -name prime256v1 -genkey -noout -out srv.key 2>/dev/null
    sudo openssl req -new -key srv.key -subj /CN=localhost -out srv.csr 2>/dev/null
    printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' | sudo tee srv.ext >/dev/null
    sudo openssl x509 -req -in srv.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile srv.ext -out srv.crt 2>/dev/null
    sudo openssl ecparam -name prime256v1 -genkey -noout -out op.key 2>/dev/null
    sudo openssl req -new -key op.key -subj /CN=e2e-operator -out op.csr 2>/dev/null
    printf 'subjectAltName=URI:spiffe://mesh/operator/e2e\nextendedKeyUsage=clientAuth\n' | sudo tee op.ext >/dev/null
    sudo openssl x509 -req -in op.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile op.ext -out op.crt 2>/dev/null
    sudo rm -f ca.key srv.csr op.csr srv.ext op.ext
    sudo chgrp headscale srv.key srv.crt ca.crt; sudo chmod 0640 srv.key; sudo chmod 0600 op.key
    sudo systemctl reset-failed $PLUNIT 2>/dev/null || true
    sudo systemd-run --quiet --unit=$PLUNIT --uid=headscale --gid=headscale /usr/local/bin/mesh-placement-p12 \
      -config $CFG -listen 127.0.0.1:9443 -tls-cert $PLDIR/srv.crt -tls-key $PLDIR/srv.key \
      -client-ca $PLDIR/ca.crt -operators spiffe://mesh/operator/e2e" || return 1
  for i in $(seq 1 30); do pl GET /v1/clusters >/dev/null 2>&1 && break; sleep 0.5; done
  pl GET /v1/clusters | grep -q '"clusters"' || { log "placement API not answering"; return 1; }

  log "register c1 and c2 through mesh-placement (ACTIVE, shared)"
  pl POST /v1/clusters p12-c1 "{\"id\":\"$C1_ID\",\"endpoint_url\":\"$URL1\",\"class\":\"shared\",\"state\":\"ACTIVE\",\"max_devices\":5000}" >/dev/null || return 1
  pl POST /v1/clusters p12-c2 "{\"id\":\"$C2_ID\",\"endpoint_url\":\"$URL2\",\"class\":\"shared\",\"state\":\"ACTIVE\",\"max_devices\":5000}" >/dev/null || return 1

  log "start processes: mc-cp then mc-c1b (c1), mc-c2 then mc-c2b (c2)"
  start_proc cp && wait_direct_ready cp 40 || { log "c1 on mc-cp not ready"; return 1; }
  start_proc c2 && wait_direct_ready c2 40 || { log "c2 on mc-c2 not ready"; return 1; }
  start_proc c1b && start_proc c2b || return 1
  for i in $(seq 1 30); do [ "$(direct c1b /health)$(direct c2b /health)" = 200200 ] && break; sleep 1; done
  [ "$(direct c1b /health)$(direct c2b /health)" = 200200 ] || { log "standby processes not listening"; return 1; }
  wait_public_ready c1 30 && wait_public_ready c2 30 || { log "public /ready (c1=$(public_ready c1) c2=$(public_ready c2))"; return 1; }

  log "bootstrap on mc-cp ($BOOT_URL)"
  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$BOOT_BIN" "$CP_SSH:/tmp/p12-boot" || return 1
  cp_ "set -e; sudo install -m 0755 /tmp/p12-boot /usr/local/bin/mesh-bootstrap-p12; rm -f /tmp/p12-boot
    sudo systemctl reset-failed $BOOTUNIT 2>/dev/null || true
    sudo systemd-run --quiet --unit=$BOOTUNIT --uid=headscale --gid=headscale \
      /usr/local/bin/mesh-bootstrap-p12 -config $CFG -listen $BOOT_LISTEN \
      -tls-cert /etc/headscale/tls/fullchain.pem -tls-key /etc/headscale/tls/privkey.pem -hostname $BOOT_HOST" || return 1
  for i in $(seq 1 20); do [ "$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BOOT_URL/health")" = 200 ] && break; sleep 1; done
  [ "$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BOOT_URL/health")" = 200 ] || { log "bootstrap not healthy"; return 1; }

  ci_ "docker image inspect $IMAGE >/dev/null 2>&1 || docker pull -q $IMAGE >/dev/null" || return 1
}

# ------------------------------------------------------------------ cases ---
t_p01() {
  local p="" s r id o pf=/var/lib/headscale/cluster/p12-policy.hujson pol='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}'
  for s in x y z; do
    r=$(pl POST /v1/tailnets "p12-t-$s" "{\"slug\":\"$s\",\"class\":\"shared\"}")
    printf 'create %s %s\n' "$s" "$r" >>"$ART_DIR/logs/placement.log"
    id=$(jget "$r" "d['id']"); o=$(jget "$r" "d['owner_cluster_id']")
    [ -n "$id" ] || { record P01 FAIL "create $s: $r"; return 1; }
    eval "ID_$s=$id"; if [ "$o" = $C1_ID ]; then eval "OWNER_$s=c1"; else eval "OWNER_$s=c2"; fi
    pl POST "/v1/tailnets/$id/activate" "p12-a-$s" '{"expected_epoch":1}' >>"$ART_DIR/logs/placement.log" || p="$p; activate $s"
  done
  printf 'owners x=%s y=%s z=%s\n' "$OWNER_x" "$OWNER_y" "$OWNER_z" | tee -a "$ART_DIR/logs/placement.log" >/dev/null
  [ "$(printf '%s\n' "$OWNER_x" "$OWNER_y" "$OWNER_z" | sort -u | wc -l)" = 2 ] || p="$p; not spread over both clusters (x=$OWNER_x y=$OWNER_y z=$OWNER_z)"
  for s in x y z; do
    printf '%s\n' "$pol" | if [ "$(owner $s)" = c1 ]; then cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI1 tailnet policy --slug $s --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"
      else on c2 "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI2 tailnet policy --slug $s --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"; fi >/dev/null || p="$p; policy $s"
    cli "$(owner $s)" tailnet authkey --slug $s --user e2e-$s --reusable --expiration 120m \
      | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s"
    chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || p="$p; authkey $s"
  done
  # Each owner's active process loaded its tailnets (live, reconcile <= 2 s).
  local i ok
  for s in x y z; do
    ok=0
    for i in $(seq 1 20); do proc_log "$(active_of "$(owner $s)")" 120 | grep -q "loaded tailnet runtime.*slug=$s" && { ok=1; break; }; sleep 1; done
    [ $ok = 1 ] || p="$p; $s not loaded by its owner $(owner $s)"
    proc_log "$(active_of "$(otherc "$(owner $s)")")" 120 | grep -q "loaded tailnet runtime.*slug=$s" && p="$p; $s loaded by the other cluster"
  done
  for s in x y z; do
    r=$(boot_req "$TMPD/key-$s" "$s"); printf 'P01 %s %s\n' "$s" "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
    [ "$(boot_url "$r")" = "$(urlof "$(owner $s)")" ] || p="$p; bootstrap $s -> [$r]"
  done
  local f; f=$(findings); printf 'P01 reconcile %s\n' "$f" >>"$ART_DIR/logs/reconcile.txt"
  [ "$f" = "[]" ] || p="$p; reconcile findings $f"
  [ -z "$p" ] && record P01 PASS "placement: x->$OWNER_x y->$OWNER_y z->$OWNER_z (spread); owners loaded them live, the other never; bootstrap -> owner URL; reconcile []" \
    || record P01 FAIL "${p#; }"
}

# Clients: x1 x2 (x), y1 y2 (y), z1 (z, alone), each at the URL bootstrap returned.
t_p02() {
  local p="" c s r u
  for c in x1 x2 y1 y2 z1; do
    s=${c:0:1}; start_ts p12-$c || { p="$p; start $c"; continue; }
    u=$(boot_url "$(boot_req "$TMPD/key-$s" "$s")")
    r=0; ts_up p12-$c "$u" "$TMPD/key-$s" || r=$?; [ $r = 0 ] || p="$p; $c up rc=$r"
  done
  wait_peers p12-x1 p12-x2 30 || p="$p; x1 [$(peers p12-x1)]"
  wait_peers p12-y1 p12-y2 30 || p="$p; y1 [$(peers p12-y1)]"
  sleep 3
  [ "$(peers p12-x2)" = p12-x1 ] || p="$p; x2 [$(peers p12-x2)]"
  [ "$(peers p12-y2)" = p12-y1 ] || p="$p; y2 [$(peers p12-y2)]"
  [ -z "$(peers p12-z1)" ] || p="$p; z1 sees [$(peers p12-z1)]"
  ping_ok p12-x1 p12-x2 || p="$p; x1 cannot ping x2"
  ping_ok p12-y1 p12-y2 || p="$p; y1 cannot ping y2"
  [ "$(counts)" = "x=2 y=2 z=1" ] || p="$p; nodes $(counts)"
  [ -z "$p" ] && record P02 PASS "x: x1<->x2 ($OWNER_x), y: y1<->y2 ($OWNER_y), z: z1 alone ($OWNER_z); pings ok; nodes $(counts)" || record P02 FAIL "${p#; }"
}

t_p03() {
  local p="" before after r s n=0
  before=$(counts)
  for s in x y z; do
    n=$((n + 1)); start_ts p12-w$n >/dev/null; r=0
    ts_up p12-w$n "$(urlof "$(otherc "$(owner $s)")")" "$TMPD/key-$s" || r=$?
    { [ $r != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out"; } || p="$p; key of $s at the other cluster: rc=$r"
    save_logs p12-w$n; drop_ts p12-w$n
  done
  after=$(counts); [ "$before" = "$after" ] || p="$p; nodes $before -> $after"
  [ -z "$p" ] && record P03 PASS "each tailnet's key refused at the cluster that does not own it ('invalid pre auth key'), no node ($after)" || record P03 FAIL "${p#; }"
}

# clients_of CLUSTER -> client names whose tailnet that cluster owns (z1 excluded: no peer)
clients_of() { local s; for s in x y; do [ "$(owner $s)" = "$1" ] && echo "p12-${s}1 p12-${s}2"; done; }
first_pair() { set -- $(clients_of "$1"); echo "${1:-} ${2:-}"; }

# restart CLUSTER: both processes stopped, then started (active first).
t_restart() { # CASE CLUSTER
  local id=$1 c=$2 o p="" a b nk ip e_other t0
  o=$(otherc "$c"); set -- $(first_pair "$c"); a=$1; b=$2
  local oa ob; set -- $(first_pair "$o"); oa=${1:-}; ob=${2:-}
  nk=$(nodekey "$a"); ip=$(selfip "$a"); e_other=$(lease_epoch "$o")
  t0=$(date +%s)
  for n in $(nodesof "$c"); do stop_proc "$n"; done
  sleep 3
  [ "$(public_ready "$c")" != 200 ] || p="$p; $c still ready while stopped"
  if [ -n "$oa" ]; then ping_ok "$oa" "$ob" || p="$p; $o clients cannot ping while $c is down"; fi
  set -- $(nodesof "$c"); start_proc "$1"; wait_direct_ready "$1" 40 || p="$p; $1 not ready after restart"; start_proc "$2"
  wait_public_ready "$c" 30 || p="$p; $c public /ready"
  wait_running "$a" 60 && wait_peers "$a" "$b" 60 || p="$p; $a back state=$(state "$a") peers=[$(peers "$a")]"
  ping_ok "$a" "$b" || p="$p; $a cannot ping $b after restart"
  [ "$(nodekey "$a")" = "$nk" ] && [ "$(selfip "$a")" = "$ip" ] || p="$p; $a key/IP changed (login)"
  [ "$(lease_epoch "$o")" = "$e_other" ] || p="$p; $o lease epoch $e_other -> $(lease_epoch "$o")"
  [ -z "$p" ] && record "$id" PASS "restart of $c (both processes): $o kept serving (ping, lease epoch $e_other unchanged); $a back without login, same key/IP ($(( $(date +%s) - t0 ))s)" \
    || record "$id" FAIL "${p#; }"
}

t_f01() { # CASE CLUSTER
  local id=$1 c=$2 o p="" act sb a b e0 eo t0 t1 t2 nl nk
  o=$(otherc "$c"); act=$(active_of "$c"); sb=$(standby_of "$c")
  set -- $(first_pair "$c"); a=$1; b=$2
  local oa ob; set -- $(first_pair "$o"); oa=${1:-}; ob=${2:-}
  e0=$(lease_epoch "$c"); eo=$(lease_row "$o"); nl=$(needs_login "$a"); nk=$(nodekey "$a")
  [ "$act" != none ] && [ -n "$sb" ] || { record "$id" FAIL "no active/standby in $c ($act/$sb)"; return; }
  t0=$(now_ms)
  on "$act" "sudo systemctl kill --kill-whom=main -s KILL $UNIT" || { record "$id" FAIL "kill"; return; }
  wait_direct_ready "$sb" 40 || p="$p; standby $sb not ready in 40s"
  t1=$(now_ms)
  local i; for i in $(seq 1 60); do [ "$(state "$a")" = Running ] && ping_ok "$a" "$b" && break; sleep 1; done
  t2=$(now_ms)
  ping_ok "$a" "$b" || p="$p; $a cannot ping $b after failover"
  [ "$(lease_epoch "$c")" = $((e0 + 1)) ] || p="$p; $c lease epoch $e0 -> $(lease_epoch "$c")"
  [ "$(needs_login "$a")" = "$nl" ] && [ "$(nodekey "$a")" = "$nk" ] || p="$p; $a logged in again"
  [ "$(lease_row "$o")" = "$eo" ] || p="$p; $o lease [$eo] -> [$(lease_row "$o")]"
  if [ -n "$oa" ]; then ping_ok "$oa" "$ob" || p="$p; $o clients cannot ping"; fi
  start_proc "$act"; sleep 3; [ "$(direct "$act" /ready)" = 503 ] || p="$p; restarted $act not standby"
  printf '%s F01 %s: kill->standby ready %d ms; ->client Running+ping %d ms\n' "$id" "$c" $((t1 - t0)) $((t2 - t0)) >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "$id" PASS "F01 $c: kill -9 $act, $sb ready in $((t1 - t0)) ms (epoch $((e0 + 1))), $a back without login in $((t2 - t0)) ms; $o lease and clients untouched" \
    || record "$id" FAIL "${p#; }"
}

t_f02() { # CASE CLUSTER
  local id=$1 c=$2 o p="" act sb a b e0 eo t0 t1 sent0 sent1 since
  o=$(otherc "$c"); act=$(active_of "$c"); sb=$(standby_of "$c")
  set -- $(first_pair "$c"); a=$1; b=$2
  e0=$(lease_epoch "$c"); eo=$(lease_row "$o"); sent0=$(map_sent "$act")
  [ "$act" != none ] && [ -n "$sb" ] || { record "$id" FAIL "no active/standby in $c ($act/$sb)"; return; }
  t0=$(now_ms)
  on "$act" "sudo systemctl kill --kill-whom=main -s STOP $UNIT" || { record "$id" FAIL "SIGSTOP"; return; }
  wait_direct_ready "$sb" $((TTL + 20)) || p="$p; $sb did not take over"
  t1=$(now_ms); sleep 3; since=$(date +%s)
  on "$act" "sudo systemctl kill --kill-whom=main -s CONT $UNIT"
  sleep 12
  sent1=$(map_sent "$act")
  [ "$(direct "$act" /ready)" = 503 ] || p="$p; resumed $act /ready $(direct "$act" /ready)"
  [ "${sent0:-0}" -gt 0 ] || p="$p; $act sent no MapResponse before SIGSTOP"
  [ -n "$sent0" ] && [ "$sent0" = "$sent1" ] || p="$p; resumed $act sent MapResponses ($sent0 -> $sent1)"
  proc_log "$act" $(( $(date +%s) - since + 2 )) | redact >"$ART_DIR/logs/$id-resumed.log"
  grep -qE 'self-fence deadline|lease lost' "$ART_DIR/logs/$id-resumed.log" || p="$p; no lease-end log after resume"
  [ "$(lease_epoch "$c")" = $((e0 + 1)) ] || p="$p; $c lease epoch $e0 -> $(lease_epoch "$c")"
  [ "$(lease_row "$o")" = "$eo" ] || p="$p; $o lease changed"
  local i; for i in $(seq 1 60); do [ "$(state "$a")" = Running ] && ping_ok "$a" "$b" && break; sleep 1; done
  ping_ok "$a" "$b" || p="$p; $a cannot ping $b on the new active"
  printf '%s F02 %s: SIGSTOP->other ready %d ms; resumed MapResponses %s -> %s\n' "$id" "$c" $((t1 - t0)) "$sent0" "$sent1" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "$id" PASS "F02 $c: SIGSTOP $act past TTL, $sb ready in $((t1 - t0)) ms (epoch $((e0 + 1))); resumed $act sent nothing ($sent0 -> $sent1), /ready 503; $o untouched" \
    || record "$id" FAIL "${p#; }"
}

# P10: move a tailnet of c1 to c2 (ADR 0010 hand-over).
t_p10() {
  local p="" s="" r e0 t0 moving=0 i u
  for x in x y; do [ "$(owner $x)" = c1 ] && s=$x; done
  [ -n "$s" ] || { record P10 FAIL "no two-client tailnet on c1 to move"; return; }
  e0=$(jget "$(pl GET "/v1/tailnets/$(tid $s)")" "d['owner_epoch']")
  local since; since=$(date +%s); t0=$(now_ms)
  r=$(pl POST "/v1/tailnets/$(tid $s)/owner" "p12-move-$s" "{\"from_cluster\":\"$C1_ID\",\"from_epoch\":$e0,\"to_cluster\":\"$C2_ID\"}")
  printf 'move %s %s\n' "$s" "$r" >>"$ART_DIR/logs/placement.log"
  [ "$(jget "$r" "d.get('moving_to_cluster_id')")" = $C2_ID ] && [ "$(jget "$r" "d['owner_epoch']")" = "$e0" ] || p="$p; move request [$r]"
  for i in $(seq 1 40); do
    r=$(boot_req "$TMPD/key-$s" "$s"); printf 'P10 %s %s\n' "$(now_ms)" "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
    case "$r" in 503*) moving=1;; 200*) [ "$(boot_url "$r")" = "$URL2" ] && break;; esac
    sleep 0.25
  done
  local t1; t1=$(now_ms)
  [ "$(boot_url "$r")" = "$URL2" ] || p="$p; bootstrap after the move [$r]"
  [ $moving = 1 ] || p="$p; bootstrap never answered MOVING during the hand-over"
  eval "OWNER_$s=c2"
  local row; row=$(jget "$(pl GET "/v1/tailnets/$(tid $s)")" "d['owner_cluster_id']+' '+str(d['owner_epoch'])+' '+str(d.get('moving_to_cluster_id'))")
  [ "$row" = "$C2_ID $((e0 + 1)) None" ] || p="$p; row [$row]"
  # Order: c1 handed over (its log) before c2 loaded (its log), by wall clock.
  local th tl
  sleep 2
  th=$( { for n in cp c1b; do proc_log $n $(( $(date +%s) - since + 5 )); done; } | grep "tailnet handed over" | grep -o '^[0-9T:.+-]*' | head -1)
  tl=$( { for n in c2 c2b; do proc_log $n $(( $(date +%s) - since + 5 )); done; } | grep "loaded tailnet runtime.*slug=$s" | grep -o '^[0-9T:.+-]*' | head -1)
  printf 'P10 handed over at %s (c1), loaded at %s (c2)\n' "$th" "$tl" >>"$ART_DIR/timings.txt"
  [ -n "$th" ] && [ -n "$tl" ] && [[ "$th" < "$tl" ]] || p="$p; order handed-over=[$th] loaded=[$tl]"
  # The key at c1 is refused; clients log in again at c2 (ADR 0005 / P12b).
  start_ts p12-v1 >/dev/null; local rc=0; ts_up p12-v1 "$URL1" "$TMPD/key-$s" || rc=$?
  { [ $rc != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out"; } || p="$p; c1 still accepts $s (rc=$rc)"
  save_logs p12-v1; drop_ts p12-v1
  for c in p12-${s}1 p12-${s}2; do rc=0; ts_up "$c" "$URL2" "$TMPD/key-$s" --force-reauth || rc=$?; [ $rc = 0 ] || p="$p; $c re-login rc=$rc"; done
  wait_peers p12-${s}1 p12-${s}2 45 || p="$p; ${s}1 at c2 [$(peers p12-${s}1)]"
  ping_ok p12-${s}1 p12-${s}2 || p="$p; ${s}1 cannot ping ${s}2 at c2"
  local leak; leak=$(for c in x1 x2 y1 y2 z1; do [ "${c:0:1}" = "$s" ] || peers p12-$c; done)
  grep -q "p12-$s" <<<"$leak" && p="$p; a node of $s seen from another tailnet"
  [ -z "$p" ] && record P10 PASS "move $s c1->c2: request (moving_to, epoch $e0), bootstrap MOVING then $URL2 after $((t1 - t0)) ms; c1 handed over ($th) before c2 loaded ($tl); epoch $((e0 + 1)); c1 refuses $s; clients re-logged at c2; others unaffected" \
    || record P10 FAIL "${p#; }"
}

t_p11() {
  local p="" s f
  for s in x y z; do printf 'P11 %s %s\n' "$s" "$(pl GET "/v1/tailnets/$(tid $s)")" >>"$ART_DIR/logs/placement.log"; done
  local ep; ep=$(psql_ "SELECT slug||'='||owner_cluster_id||'@'||owner_epoch FROM tailnets WHERE slug IN ('x','y','z') ORDER BY slug" | tr '\n' ' ')
  printf 'owners %s\n' "$ep" >>"$ART_DIR/logs/audit.txt"
  for s in x y z; do
    local want=1; [ "$s" = "${MOVED:-}" ] && want=2
    grep -qw "$s=$(idof "$(owner $s)")@$want" <<<"$ep" || p="$p; owner/epoch of $s [$ep]"
  done
  local c since=$(( $(date +%s) - T0 + 5 ))
  for c in c1 c2; do
    { for n in $(nodesof $c); do proc_log $n $since; done; } | grep -aE 'cluster lease acquired' | redact >"$ART_DIR/logs/lease-events-$c.log"
    py 'import re,sys
ev=[]
for l in open(sys.argv[1], encoding="utf-8", errors="replace"):
    e=re.search(r"lease_epoch=(\d+)", l); t=re.match(r"(\S+)", l)
    if e and t: ev.append((t.group(1), int(e.group(1))))
ev.sort(); eps=[e for _,e in ev]
print(("ok" if eps == sorted(set(eps)) else "bad"), eps)' "$ART_DIR/logs/lease-events-$c.log" | strip_cr >"$TMPD/l-$c"
    grep -q '^ok' "$TMPD/l-$c" || p="$p; $c lease epochs $(cat "$TMPD/l-$c")"
    printf '%s %s\n' "$c" "$(lease_row $c)" >>"$ART_DIR/logs/audit.txt"
    case "$(lease_row $c | awk '{print $3}')" in t|true) ;; *) false;; esac || p="$p; $c has no live lease"
  done
  f=$(findings); printf 'P11 reconcile %s\n' "$f" >>"$ART_DIR/logs/reconcile.txt"
  [ "$f" = "[]" ] || p="$p; reconcile findings $f"
  [ -z "$p" ] && record P11 PASS "owners $ep; lease epochs strictly increasing (c1 $(cut -d' ' -f2- "$TMPD/l-c1"), c2 $(cut -d' ' -f2- "$TMPD/l-c2")), one live holder each; reconcile []" \
    || record P11 FAIL "${p#; }"
}

t_p12() {
  local hits
  hits=$(grep -rlE 'hskey-(auth|api)-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}|BEGIN [A-Z ]*PRIVATE KEY' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
  [ -z "$hits" ] && record P12 PASS "no auth key, private key or full machine/node key in the artifacts" || record P12 FAIL "unredacted in: $(echo "$hits" | tr '\n' ' ')"
}

# ------------------------------------------------------------------- main ---
T0=$(date +%s)
log "P12 E2E $RUN_ID at $SHA: c1 $URL1, c2 $URL2, client $IMAGE (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "image: $IMAGE"; echo "commit: $SHA"; echo "database: $PG_DB (mc-db via PgBouncer $PG_LISTEN)"
  echo "cluster binary: $(sha256sum "$BIN" | cut -c1-16)"; echo "mesh-placement: $(sha256sum "$PLBIN" | cut -c1-16)"
  echo "mesh-bootstrap: $(sha256sum "$BOOT_BIN" | cut -c1-16)"; } >>"$ART_DIR/env.txt"
if t_p01; then
  t_p02
  t_p03
  t_restart P04 c1
  t_restart P05 c2
  t_f01 P06 c1
  t_f01 P07 c2
  t_f02 P08 c1
  t_f02 P09 c2
  MOVED=""; for s in x y; do [ "$(owner $s)" = c1 ] && MOVED=$s; done
  t_p10
  t_p11
fi
for n in cp c1b c2 c2b; do proc_log $n $(( $(date +%s) - T0 + 5 )) | redact >"$ART_DIR/logs/$n-process.log"; done
cp_ "sudo journalctl -u $PLUNIT -u $BOOTUNIT --no-pager" 2>&1 | redact >"$ART_DIR/logs/placement-bootstrap.log"
t_p12
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
