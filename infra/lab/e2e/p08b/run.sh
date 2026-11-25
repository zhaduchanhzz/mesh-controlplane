#!/usr/bin/env bash
# P08b real-client E2E: cluster c1 on mc-cp (one process, PostgreSQL on
# mc-db) with tailnets a and b, real Tailscale clients on mc-ci, through the
# public endpoint:
#
#   client / laptop -> VPS 116.118.3.46:8443 -> wg-mesh -> mc-cp:8443
#
# The cluster runs as a transient unit only during the run; the P02 POC on
# mc-cp is stopped and restored, as in the P05/P11b harnesses. Database:
# e2e_p08b<run> on mc-db, dropped at cleanup. mesh-placement (operator API,
# mTLS) runs as a transient unit on mc-cp loopback with a throwaway CA made
# on mc-cp under /run (tmpfs), removed at cleanup.
#
# Cases (tickets/P08b.md):
#   C00 clients: a1 a2 a3 in a (a1 advertises 10.88.0.0/24), b1 b2 in b
#   M01 live admin over the unix socket (CLI): approve a1's route -> a2 sees it
#   M02 expire a2 -> a2 must log in again, a1 sees a2 expired
#   M03 delete a3 -> a1 drops a3 (live, no restart)
#   M04 delete an empty user; a user with nodes is refused
#   M05 B's IDs through A are refused; B gets no update frame for any M0x
#       (headscale_tailnet_mapresponses_sent_total{tailnet=b,type=update})
#   K01 API keys: CLI (a, acting as e2e-a) and placement API over mTLS (b),
#       audited; no secret in platform_audit
#   A01 public /api/v2 with a's key lists a's devices only (Bearer, Basic)
#   A02 another tailnet or its device: 404, nothing changes in b
#   A03 policy / settings / device trust writes: 403, revisions unchanged
#   A04 a pre-auth key created through the API enrolls a4 into a; deleting
#       a4 through the API drops it at a1; b gets no update frame
#   A05 revoked (CLI and placement API), garbage, auth key as API key: 401
#   A06 rate limits: a burst of a's key gets 429, b's key right after is 200
#   A07 no /api/v1, no /metrics, no /cluster/v1 on the public listener
#   X01 per-tailnet metrics on the loopback metrics listener: IDs, no slugs
#   S01 no unredacted secret in the artifacts
#
# Usage: BIN=<linux/amd64 headscale> PLBIN=<linux/amd64 mesh-placement> infra/lab/e2e/p08b/run.sh
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
PG_SSH=${PG_SSH:-mesh@192.168.1.243}
PG_LISTEN=${PG_LISTEN:-192.168.1.243:6432}
MC_DB_ENV=${MC_DB_ENV:-$HOME/.mesh-lab/mc-db.env}
HOST=c1.cp.gta5web.online
URL=https://$HOST:8443
POC_HEALTH=https://a.cp.gta5web.online:8443/health
CLUSTER_ID=mc-cp-c1
IMAGE=${IMAGE:-tailscale/tailscale:v1.104.7}
BIN=${BIN:?BIN= linux/amd64 headscale binary}
PLBIN=${PLBIN:?PLBIN= linux/amd64 mesh-placement binary}
KEEP=${KEEP:-0}
LABEL=mesh-e2e=p08b
PY=${PY:-python}
ROUTE=10.88.0.0/24

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_p08b$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/P08b/e2e-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

CFG=/etc/headscale/cluster/config.yaml
UNIT=mesh-c1-e2e
PLUNIT=mesh-placement-e2e
PLDIR=/run/p08b-placement
HSC="sudo -u headscale /usr/local/bin/headscale-cluster -c $CFG cluster --cluster-id $CLUSTER_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
ssh_() { local h=$1; shift; ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=15 "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }
pg_() { ssh_ "$PG_SSH" "$@"; }
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }
now_ms() { py 'import time; print(int(time.time()*1000))' | strip_cr; }
sql() { pg_ "sudo docker exec mc-pg psql -At -U postgres -d $PG_DB -c \"$1\"" | strip_cr; }

FAILS=0
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-4s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------ helpers -----
metrics() { cp_ "curl -s -m 3 http://127.0.0.1:9095/metrics" 2>/dev/null | strip_cr; }
# updates_to TAILNET_ID: update frames written to that tailnet's clients
updates_to() { metrics | awk -v t="tailnet=\"$1\"" 'index($0, "headscale_tailnet_mapresponses_sent_total{") == 1 && index($0, t) && index($0, "type=\"update\"") {s+=$2} END {printf "%d", s}'; }

# clients (docker on mc-ci)
start_ts() {
  ci_ "docker run -d --name $1 --hostname $1 --label $LABEL --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL $IMAGE \
    tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done"
}
ts_up() { # NAME KEYFILE [extra flags]
  local n=$1 k=$2; shift 2
  ci_ "docker exec -i $n timeout 60 tailscale up --login-server=$URL --auth-key=file:/dev/stdin --hostname=$n $*" \
    <"$k" 2>&1 | redact >>"$ART_DIR/logs/$n.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
peers() { field "$1" "' '.join(sorted(p['HostName'] for p in (d.get('Peer') or {}).values()))"; }
state() { field "$1" "d['BackendState']"; }
peer_routes() { field "$1" "' '.join(r for p in (d.get('Peer') or {}).values() if p['HostName']=='$2' for r in (p.get('PrimaryRoutes') or []))"; }
peer_expired() { field "$1" "any(p.get('Expired') for p in (d.get('Peer') or {}).values() if p['HostName']=='$2')"; }
wait_for() { # TIMEOUT_S 'CONDITION' -> elapsed ms, or status 1 (the condition is re-evaluated)
  local end=$(( $(date +%s) + $1 )) t0; t0=$(now_ms)
  while [ "$(date +%s)" -lt "$end" ]; do eval "$2" && { echo $(( $(now_ms) - t0 )); return 0; }; sleep 0.3; done
  return 1
}
has_peer() { case " $(peers "$1") " in *" $2 "*) return 0;; esac; return 1; }
save_logs() { ci_ "docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }

# live admin CLI (unix socket); JSON on stdout
adm() { cp_ "$HSC tailnet --slug $*" 2>>"$ART_DIR/logs/admin.err"; }
node_id() { adm "$1 node list" | py "import json; print(next(str(n['id']) for n in json.load(sys.stdin)['nodes'] if n['hostname']=='$2'))" 2>/dev/null | strip_cr; }

# public /api/v2: api METHOD PATH KEYFILE [BODY] -> status; body in $TMPD/body
hdr() { # KEYFILE -> header file
  local h="$1.hdr"
  [ -s "$h" ] || { printf 'Authorization: Bearer %s\n' "$(cat "$1")" >"$h"; chmod 600 "$h"; }
  echo "$h"
}
api() {
  local m=$1 p=$2 k=$3 b=${4:-} h
  h=$(hdr "$k")
  if [ -n "$b" ]; then
    curl -s -m 15 -o "$TMPD/body" -w '%{http_code}' -X "$m" -H @"$h" -H 'Content-Type: application/json' --data "$b" "$URL$p" || echo 000
  else
    curl -s -m 15 -o "$TMPD/body" -w '%{http_code}' -X "$m" -H @"$h" "$URL$p" || echo 000
  fi
}
api_log() { printf '%s %s -> %s %s\n' "$1" "$2" "$3" "$(redact <"$TMPD/body" | head -c 300)" >>"$ART_DIR/logs/api.log"; }

# placement API over mTLS on mc-cp loopback: pl METHOD PATH [IDEMPOTENCY-KEY] [BODY]
pl() {
  local ik=${3:-} b=${4:-}
  cp_ "sudo curl -s -m 10 --cacert $PLDIR/ca.crt --cert $PLDIR/op.crt --key $PLDIR/op.key -X $1 \
    ${ik:+-H 'Idempotency-Key: $ik'} ${b:+-H 'Content-Type: application/json' --data '$b'} https://127.0.0.1:9443$2"
}

# ------------------------------------------------------------ cleanup -----
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\"" || { log "clients not removed"; CLEAN_OK=0; }
  cp_ "sudo journalctl -u $UNIT --since -2h --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/cluster.journal.log" || true
  cp_ "for u in $UNIT $PLUNIT; do sudo systemctl stop \$u 2>/dev/null; sudo systemctl reset-failed \$u 2>/dev/null; done; \
       sudo rm -rf $PLDIR; ! sudo test -e $PLDIR" || { log "placement material not removed"; CLEAN_OK=0; }
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

  log "config (c1 on $PG_DB)"
  awk -v host="${PG_LISTEN%:*}" -v port="${PG_LISTEN##*:}" -v db="$PG_DB" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    host: " host "\n    port: " port \
      "\n    name: " db "\n    user: headscale\n    pass: file:/etc/headscale/cluster/pg.pass\n    ssl: disable"; skip = 1; next }
    skip && /^[^ ]/ { skip = 0 }
    !skip { print }' "$REPO/infra/lab/mc-cp/cluster/config.yaml" >"$TMPD/c1.yaml"
  grep -q '^unix_socket: /var/run/headscale-cluster/cluster.sock' "$TMPD/c1.yaml" || return 1

  log "binaries to mc-cp"
  local src dst
  for dst in headscale-cluster mesh-placement; do
    src=$BIN; [ $dst = mesh-placement ] && src=$PLBIN
    gzip -c "$src" | cp_ "gunzip -c >/tmp/p08b-bin && sudo install -m 0755 /tmp/p08b-bin /usr/local/bin/$dst && rm -f /tmp/p08b-bin" || return 1
  done
  log "headscale-cluster $(cp_ 'sha256sum /usr/local/bin/headscale-cluster' | cut -c1-16)…"

  log "mc-cp: stop POC, config, pg.pass"
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; \
       sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && \
       sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster && \
       sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  cp_ "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1.yaml" || return 1

  # P12 (owner decision 2026-11-25): tailnets are created only on a
  # registered ACTIVE cluster whose process holds its lease: register, start
  # the cluster, then create the tailnets live (loaded by its reconcile).
  log "registry: cluster $CLUSTER_ID"
  cp_ "$HSC register --class shared --max-devices 5000 >/dev/null" || return 1

  log "cluster process (transient unit, admin socket in /run/headscale-cluster)"
  local since; since=$(cp_ 'date +%s') || return 1
  cp_ "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    /usr/local/bin/headscale-cluster -c $CFG cluster serve --cluster-id $CLUSTER_ID" || return 1
  local i
  for i in $(seq 1 40); do [ "$(curl -s -o /dev/null -w '%{http_code}' -m 3 "$URL/ready")" = 200 ] && break; sleep 1; done
  [ "$(curl -s -o /dev/null -w '%{http_code}' -m 3 "$URL/ready")" = 200 ] || { log "public /ready not 200"; return 1; }

  log "tailnets a, b (owner $CLUSTER_ID), created while the cluster runs"
  local s pf=/var/lib/headscale/cluster/p08b-policy.hujson
  for s in a b; do cp_ "$HSC tailnet create --slug $s >/dev/null && $HSC tailnet status --slug $s --set ACTIVE >/dev/null" || return 1; done
  printf '%s\n' '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' \
    | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $HSC tailnet policy --slug a --file $pf >/dev/null && $HSC tailnet policy --slug b --file $pf >/dev/null; rc=\$?; sudo rm -f $pf; exit \$rc" || return 1
  for s in a b; do
    cp_ "$HSC tailnet authkey --slug $s --user e2e-$s --reusable --expiration 90m" \
      | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s" || return 1
    chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  done
  TA=$(sql "SELECT id FROM tailnets WHERE slug='a'"); TB=$(sql "SELECT id FROM tailnets WHERE slug='b'")
  [[ $TA =~ ^[0-9a-f-]{36}$ && $TB =~ ^[0-9a-f-]{36}$ ]] || return 1
  log "tailnet a=$TA b=$TB"
  local ok=0 jl
  for i in $(seq 1 30); do
    jl=$(cp_ "sudo journalctl -u $UNIT --since @$since --no-pager" 2>/dev/null | grep 'loaded tailnet runtime')
    grep -q 'slug=a' <<<"$jl" && grep -q 'slug=b' <<<"$jl" && { ok=1; break; }
    sleep 1
  done
  [ $ok = 1 ] || { log "tailnets a, b not loaded"; return 1; }

  log "mesh-placement on mc-cp loopback (throwaway CA under $PLDIR)"
  cp_ "set -e; sudo install -d -o root -g headscale -m 0751 $PLDIR; cd $PLDIR
    sudo openssl ecparam -name prime256v1 -genkey -noout -out ca.key 2>/dev/null
    sudo openssl req -x509 -new -key ca.key -subj /CN=p08b-e2e-ca -days 1 -out ca.crt 2>/dev/null
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
    sudo systemd-run --quiet --unit=$PLUNIT --uid=headscale --gid=headscale /usr/local/bin/mesh-placement \
      -config $CFG -listen 127.0.0.1:9443 -tls-cert $PLDIR/srv.crt -tls-key $PLDIR/srv.key \
      -client-ca $PLDIR/ca.crt -operators spiffe://mesh/operator/e2e" || return 1
  for i in $(seq 1 20); do pl GET /v1/clusters >/dev/null 2>&1 && break; sleep 0.5; done
  pl GET /v1/clusters | grep -q "$CLUSTER_ID" || { log "placement API not answering"; return 1; }

  ci_ "docker image inspect $IMAGE >/dev/null 2>&1 || docker pull -q $IMAGE >/dev/null" || return 1
}

# --------------------------------------------------------------- cases ----
t_clients() {
  local p="" r c
  for c in p08b-a1 p08b-a2 p08b-a3 p08b-b1 p08b-b2; do start_ts $c || p="$p; start $c"; done
  r=0; ts_up p08b-a1 "$TMPD/key-a" --advertise-routes=$ROUTE || r=$?; [ $r = 0 ] || p="$p; a1 up $r"
  for c in p08b-a2 p08b-a3; do r=0; ts_up $c "$TMPD/key-a" || r=$?; [ $r = 0 ] || p="$p; $c up $r"; done
  for c in p08b-b1 p08b-b2; do r=0; ts_up $c "$TMPD/key-b" || r=$?; [ $r = 0 ] || p="$p; $c up $r"; done
  wait_for 40 '[ "$(peers p08b-a1)" = "p08b-a2 p08b-a3" ]' >/dev/null || p="$p; a1 peers [$(peers p08b-a1)]"
  wait_for 40 '[ "$(peers p08b-b1)" = "p08b-b2" ]' >/dev/null || p="$p; b1 peers [$(peers p08b-b1)]"
  [ -z "$p" ] && record C00 PASS "a1 a2 a3 mesh in a (a1 advertises $ROUTE), b1 b2 in b" || record C00 FAIL "${p#; }"
}

B_BASE=0
b_quiet() { # CASE: no update frame to b since B_BASE (after a settle)
  sleep 3
  local now; now=$(updates_to "$TB")
  printf '%s b updates %s -> %s\n' "$1" "$B_BASE" "$now" >>"$ART_DIR/logs/b-frames.txt"
  [ "$now" = "$B_BASE" ]
}

t_m01() {
  local p="" id1 out ms
  sleep 3; B_BASE=$(updates_to "$TB")
  out=$(adm "a routes list"); printf '%s\n' "$out" >"$ART_DIR/logs/m01-routes-before.json"
  grep -q "$ROUTE" <<<"$out" || p="$p; $ROUTE not announced in the listing"
  id1=$(node_id a p08b-a1)
  [ -n "$id1" ] || p="$p; a1 has no ID"
  adm "a routes approve --id $id1 --routes $ROUTE" >"$ART_DIR/logs/m01-approve.json" || p="$p; approve failed"
  ms=$(wait_for 20 '[ "$(peer_routes p08b-a2 p08b-a1)" = "$ROUTE" ]') || p="$p; a2 never saw $ROUTE as a1's route"
  b_quiet M01 || p="$p; b got update frames"
  [ -z "$p" ] && record M01 PASS "route approved over the admin socket; a2 sees it as a1's primary route after ${ms}ms; b: 0 update frames" || record M01 FAIL "${p#; }"
}

t_m02() {
  local p="" id2 ms
  id2=$(node_id a p08b-a2)
  adm "a node expire --id $id2" >"$ART_DIR/logs/m02-expire.json" || p="$p; expire failed"
  ms=$(wait_for 30 '[ "$(peer_expired p08b-a1 p08b-a2)" = True ] || ! has_peer p08b-a1 p08b-a2') || p="$p; a1 never saw a2 expired"
  wait_for 30 '[ "$(state p08b-a2)" = NeedsLogin ]' >/dev/null || p="$p; a2 state $(state p08b-a2)"
  b_quiet M02 || p="$p; b got update frames"
  [ -z "$p" ] && record M02 PASS "a2 expired live: a1 sees it after ${ms}ms, a2 NeedsLogin; b: 0 update frames" || record M02 FAIL "${p#; }"
}

t_m03() {
  local p="" id3 ms rows
  id3=$(node_id a p08b-a3)
  adm "a node delete --id $id3" >"$ART_DIR/logs/m03-delete.json" || p="$p; delete failed"
  ms=$(wait_for 20 '! has_peer p08b-a1 p08b-a3') || p="$p; a1 peers [$(peers p08b-a1)]"
  rows=$(sql "SELECT count(*) FROM nodes WHERE id = $id3 AND deleted_at IS NULL")
  [ "$rows" = 0 ] || p="$p; node row of a3 still there"
  b_quiet M03 || p="$p; b got update frames"
  [ -z "$p" ] && record M03 PASS "a3 deleted live: a1 drops it after ${ms}ms, row gone; b: 0 update frames" || record M03 FAIL "${p#; }"
}

t_m04() {
  local p="" out
  cp_ "$HSC tailnet authkey --slug a --user spare --expiration 5m >/dev/null" || p="$p; could not create user spare"
  adm "a user delete --name spare" >"$ART_DIR/logs/m04-spare.json" || p="$p; deleting spare failed"
  [ "$(sql "SELECT count(*) FROM users WHERE tailnet_id='$TA' AND name='spare' AND deleted_at IS NULL")" = 0 ] || p="$p; spare still there"
  out=$(adm "a user delete --name e2e-a" 2>&1) && p="$p; e2e-a (with nodes) was deleted"
  [ "$(sql "SELECT count(*) FROM users WHERE tailnet_id='$TA' AND name='e2e-a' AND deleted_at IS NULL")" = 1 ] || p="$p; e2e-a gone"
  b_quiet M04 || p="$p; b got update frames"
  [ -z "$p" ] && record M04 PASS "empty user deleted; user with nodes refused (409); b: 0 update frames" || record M04 FAIL "${p#; }"
}

t_m05() {
  local p="" idb1 out
  idb1=$(node_id b p08b-b1)
  for op in "node delete --id $idb1" "node expire --id $idb1" "routes approve --id $idb1 --routes 10.99.0.0/24" "user delete --name e2e-b"; do
    out=$(adm "a $op" 2>&1) && p="$p; '$op' of b through a succeeded"
    grep -q "404\|409" <<<"$(tail -1 "$ART_DIR/logs/admin.err")" || p="$p; '$op' not 404"
  done
  [ "$(peers p08b-b1)" = "p08b-b2" ] || p="$p; b1 peers [$(peers p08b-b1)]"
  [ "$(state p08b-b1)" = Running ] || p="$p; b1 state $(state p08b-b1)"
  [ -z "$(cp_ "sudo ls -l /run/headscale-cluster/cluster.sock" | grep -v '^srwxrwx---')" ] || p="$p; socket mode"
  b_quiet M05 || p="$p; b got update frames"
  [ -z "$p" ] && record M05 PASS "b's node/user through a: 404, b unchanged; socket srwxrwx--- headscale; b: 0 update frames over M01-M05" || record M05 FAIL "${p#; }"
}

t_k01() {
  local p="" r kid
  cp_ "$HSC tailnet apikey create --slug a --user e2e-a --expiration 2h" \
    | py 'import json; d=json.load(sys.stdin); print(d["key"]); print(d["id"], file=sys.stderr)' 2>"$TMPD/kid-a" | strip_cr >"$TMPD/api-a"
  chmod 600 "$TMPD/api-a"; [ -s "$TMPD/api-a" ] || p="$p; CLI mint failed"
  r=$(pl POST "/v1/tailnets/$TB/apikeys" p08b-k1 '{"expires_in_seconds":7200,"user":"e2e-b"}')
  py 'import json; print(json.loads(sys.argv[1])["key"])' "$r" | strip_cr >"$TMPD/api-b"; chmod 600 "$TMPD/api-b"
  [ -s "$TMPD/api-b" ] || p="$p; placement mint failed"
  kid=$(py 'import json; print(json.loads(sys.argv[1])["id"])' "$r" | strip_cr); echo "$kid" >"$TMPD/kid-b"
  r=$(pl POST "/v1/tailnets/$TB/apikeys" p08b-k1 '{"expires_in_seconds":7200,"user":"e2e-b"}')
  py 'import json; d=json.loads(sys.argv[1]); assert "key" not in d and d.get("replayed") is True' "$r" 2>/dev/null || p="$p; placement replay showed a key"
  r=$(pl POST "/v1/tailnets/$TB/apikeys" "" '{"expires_in_seconds":7200}')
  grep -q '"status":422' <<<"$r" || p="$p; no Idempotency-Key accepted"
  local audit; audit=$(sql "SELECT actor||' '||action||' '||outcome FROM platform_audit WHERE action LIKE '%APIKey' ORDER BY id")
  printf '%s\n' "$audit" >"$ART_DIR/logs/k01-audit.txt"
  grep -q "cli:$CLUSTER_ID CreateAPIKey ok" <<<"$audit" || p="$p; CLI mint not audited"
  grep -q "spiffe://mesh/operator/e2e CreateAPIKey ok" <<<"$audit" || p="$p; placement mint not audited"
  grep -q "spiffe://mesh/operator/e2e CreateAPIKey replay" <<<"$audit" || p="$p; replay not audited"
  local leak; leak=$(sql "SELECT count(*) FROM platform_audit WHERE detail LIKE '%hskey-api-%' OR target LIKE '%hskey-api-%'")
  [ "$leak" = 0 ] || p="$p; key text in the audit"
  [ -z "$p" ] && record K01 PASS "a: CLI (actor cli:$CLUSTER_ID, user e2e-a); b: placement API mTLS (actor spiffe://mesh/operator/e2e), replay without key, no key without Idempotency-Key; audited, no key in audit" || record K01 FAIL "${p#; }"
}

t_a01() {
  local p="" c names
  c=$(api GET /api/v2/tailnet/-/devices "$TMPD/api-a"); api_log GET /tailnet/-/devices "$c"
  [ "$c" = 200 ] || p="$p; devices $c"
  names=$(py 'import json; print(" ".join(sorted(d["hostname"] for d in json.load(open(sys.argv[1]))["devices"])))' "$TMPD/body" | strip_cr)
  [ "$names" = "p08b-a1 p08b-a2" ] || p="$p; devices [$names]"
  c=$(api GET "/api/v2/tailnet/$TA/devices" "$TMPD/api-a"); [ "$c" = 200 ] || p="$p; own ID $c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 15 -u "$(cat "$TMPD/api-a"):" "$URL/api/v2/tailnet/-/devices"); [ "$c" = 200 ] || p="$p; basic $c"
  [ -z "$p" ] && record A01 PASS "a's key: devices [p08b-a1 p08b-a2] for '-' and a's ID; Bearer and Basic" || record A01 FAIL "${p#; }"
}

t_a02() {
  local p="" c idb1 before
  idb1=$(node_id b p08b-b1); before=$(updates_to "$TB")
  for x in "GET /api/v2/tailnet/$TB/devices" "GET /api/v2/tailnet/$TB/keys" "GET /api/v2/tailnet/$TB/users" \
           "GET /api/v2/device/$idb1" "POST /api/v2/device/$idb1/expire" "DELETE /api/v2/device/$idb1" \
           "POST /api/v2/device/$idb1/authorized" "POST /api/v2/tailnet/$TB/acl"; do
    if [ "${x% *}" = GET ]; then c=$(api GET "${x#* }" "$TMPD/api-a"); else c=$(api "${x% *}" "${x#* }" "$TMPD/api-a" '{}'); fi
    api_log "${x% *}" "${x#* }" "$c"
    [ "$c" = 404 ] || p="$p; ${x}: $c"
  done
  [ "$(peers p08b-b1)" = "p08b-b2" ] && [ "$(state p08b-b1)" = Running ] || p="$p; b1 changed"
  B_BASE=$before; b_quiet A02 || p="$p; b got update frames"
  [ -z "$p" ] && record A02 PASS "8 requests naming b or b1 with a's key: all 404; b1 unchanged, 0 update frames to b" || record A02 FAIL "${p#; }"
}

t_a03() {
  local p="" c rev0 rev1 id1
  rev0=$(sql "SELECT policy_revision||'/'||config_revision FROM tailnets WHERE id='$TA'")
  id1=$(node_id a p08b-a1)
  for x in "POST /api/v2/tailnet/-/acl" "PATCH /api/v2/tailnet/-/settings" "POST /api/v2/device/$id1/authorized" \
           "POST /api/v2/device/$id1/key" "POST /api/v2/device/$id1/update-client"; do
    case $x in
      *acl) c=$(api POST /api/v2/tailnet/-/acl "$TMPD/api-a" '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}') ;;
      *settings) c=$(api PATCH /api/v2/tailnet/-/settings "$TMPD/api-a" '{"devicesApprovalOn":true}') ;;
      *authorized) c=$(api POST "${x#* }" "$TMPD/api-a" '{"authorized":true}') ;;
      */key) c=$(api POST "${x#* }" "$TMPD/api-a" '{"keyExpiryDisabled":true}') ;;
      *) c=$(api POST "${x#* }" "$TMPD/api-a") ;;
    esac
    api_log "${x% *}" "${x#* }" "$c"; [ "$c" = 403 ] || p="$p; ${x}: $c"
  done
  c=$(api POST /api/v2/tailnet/-/keys "$TMPD/api-a" '{"keyType":"client","scopes":["devices:core"],"tags":["tag:x"]}'); api_log POST keys-client "$c"
  [ "$c" = 403 ] || p="$p; OAuth client: $c"
  rev1=$(sql "SELECT policy_revision||'/'||config_revision FROM tailnets WHERE id='$TA'")
  [ "$rev0" = "$rev1" ] || p="$p; revisions $rev0 -> $rev1"
  [ "$(api GET /api/v2/tailnet/-/acl "$TMPD/api-a")" = 200 ] || p="$p; policy read refused"
  [ -z "$p" ] && record A03 PASS "acl, settings, authorized, key, update-client, OAuth client: 403; policy/config revision $rev1 unchanged; policy read 200" || record A03 FAIL "${p#; }"
}

t_a04() {
  local p="" c ms id4
  B_BASE=$(updates_to "$TB")
  c=$(api POST /api/v2/tailnet/-/keys "$TMPD/api-a" '{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":false,"preauthorized":true}}}}')
  api_log POST /tailnet/-/keys "$c"; [ "$c" = 200 ] || p="$p; create key $c"
  py 'import json; print(json.load(open(sys.argv[1]))["key"])' "$TMPD/body" | strip_cr >"$TMPD/key-a4"; chmod 600 "$TMPD/key-a4"
  start_ts p08b-a4 && ts_up p08b-a4 "$TMPD/key-a4" || p="$p; a4 could not join with the API key"
  ms=$(wait_for 30 'has_peer p08b-a1 p08b-a4') || p="$p; a1 peers [$(peers p08b-a1)]"
  [ "$(sql "SELECT t.slug FROM nodes n JOIN tailnets t ON t.id=n.tailnet_id WHERE n.hostname='p08b-a4' AND n.deleted_at IS NULL")" = a ] || p="$p; a4 not in a"
  id4=$(node_id a p08b-a4)
  c=$(api DELETE "/api/v2/device/$id4" "$TMPD/api-a"); api_log DELETE "/device/a4" "$c"; [ "$c" = 200 ] || p="$p; delete a4 $c"
  wait_for 20 '! has_peer p08b-a1 p08b-a4' >/dev/null || p="$p; a1 still sees a4"
  b_quiet A04 || p="$p; b got update frames"
  [ -z "$p" ] && record A04 PASS "pre-auth key from the API enrolled a4 into a (a1 saw it after ${ms}ms); API delete dropped it; b: 0 update frames" || record A04 FAIL "${p#; }"
}

t_a05() {
  local p="" c kid
  printf 'hskey-api-AAAAAAAAAAAA-%064d' 0 >"$TMPD/api-garbage"
  for k in garbage; do c=$(api GET /api/v2/tailnet/-/devices "$TMPD/api-$k"); [ "$c" = 401 ] || p="$p; $k $c"; done
  c=$(api GET /api/v2/tailnet/-/devices "$TMPD/key-a"); [ "$c" = 401 ] || p="$p; auth key as API key $c"
  kid=$(cat "$TMPD/kid-a" | strip_cr)
  cp_ "$HSC tailnet apikey revoke --slug a --id $kid" >/dev/null || p="$p; CLI revoke failed"
  c=$(api GET /api/v2/tailnet/-/devices "$TMPD/api-a"); [ "$c" = 401 ] || p="$p; CLI-revoked key $c"
  [ "$(api GET /api/v2/tailnet/-/devices "$TMPD/api-b")" = 200 ] || p="$p; b key before revoke"
  pl POST "/v1/tailnets/$TB/apikeys/$(cat "$TMPD/kid-b")/revoke" p08b-r1 >/dev/null || p="$p; placement revoke failed"
  c=$(api GET /api/v2/tailnet/-/devices "$TMPD/api-b"); [ "$c" = 401 ] || p="$p; placement-revoked key $c"
  [ -z "$p" ] && record A05 PASS "garbage, auth key, key revoked by CLI, key revoked by placement API: 401" || record A05 FAIL "${p#; }"
}

t_a06() {
  local p="" codes n200 n429 c
  # Fresh keys: a (limit 10/s burst 20 per key, 20/s burst 40 per tailnet), b.
  cp_ "$HSC tailnet apikey create --slug a --expiration 1h" | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/api-a2"
  cp_ "$HSC tailnet apikey create --slug b --expiration 1h" | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/api-b2"
  chmod 600 "$TMPD/api-a2" "$TMPD/api-b2"
  local h; h=$(hdr "$TMPD/api-a2")
  codes=$(seq 1 60 | xargs -P 30 -I{} curl -s -o /dev/null -w '%{http_code}\n' -m 15 -H @"$h" "$URL/api/v2/tailnet/-/devices" | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  n200=$(grep -o '[0-9]* 200' <<<"$codes" | cut -d' ' -f1); n429=$(grep -o '[0-9]* 429' <<<"$codes" | cut -d' ' -f1)
  [ "${n429:-0}" -gt 0 ] || p="$p; no 429 in [$codes]"
  c=$(api GET /api/v2/tailnet/-/devices "$TMPD/api-b2"); [ "$c" = 200 ] || p="$p; b's key after a's burst: $c"
  [ -z "$p" ] && record A06 PASS "60 parallel requests with one a key: [$codes]; b's key right after: 200" || record A06 FAIL "${p#; }"
}

t_a07() {
  local p="" c
  c=$(api GET /api/v1/node "$TMPD/api-b2"); [ "$c" = 404 ] || p="$p; /api/v1 $c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 "$URL/metrics"); [ "$c" = 404 ] || p="$p; /metrics $c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 "$URL/cluster/v1/tailnet/$TA/nodes"); [ "$c" = 404 ] || p="$p; admin path $c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X POST "$URL/api/v2/oauth/token"); [ "$c" = 401 ] || [ "$c" = 404 ] || p="$p; oauth token $c"
  [ -z "$p" ] && record A07 PASS "public listener: /api/v1 404, /metrics 404, /cluster/v1 404, /api/v2/oauth/token $c (no key)" || record A07 FAIL "${p#; }"
}

t_x01() {
  local p="" m
  m=$(metrics); printf '%s\n' "$m" | grep '^headscale_tailnet_\|^headscale_cluster_api\|^headscale_nodestore_nodes ' >"$ART_DIR/logs/x01-metrics.txt"
  grep -q "headscale_tailnet_nodes{tailnet=\"$TA\"} 2" <<<"$m" || p="$p; tailnet_nodes a"
  grep -q "headscale_tailnet_nodes{tailnet=\"$TB\"} 2" <<<"$m" || p="$p; tailnet_nodes b"
  grep -q "headscale_tailnet_api_requests_total{outcome=\"limited\",tailnet=\"$TA\"}" <<<"$m" || p="$p; no limited counter for a"
  grep -q '^headscale_nodestore_nodes 4' <<<"$m" || p="$p; nodestore_nodes not the sum"
  grep '^headscale_tailnet_' <<<"$m" | grep -q 'tailnet="[ab]"' && p="$p; slug in a label"
  [ -z "$p" ] && record X01 PASS "loopback metrics: tailnet_nodes a=2 b=2, nodestore_nodes 4 (sum), limited counted for a, labels are IDs" || record X01 FAIL "${p#; }"
}

t_s01() {
  local hits
  hits=$(grep -rEo '(tskey|hskey)-[A-Za-z0-9_-]{8,}' "$ART_DIR" | grep -v '<redacted>' | grep -cv 'hskey-api-AAAAAAAAAAAA' || true)
  [ "${hits:-0}" = 0 ] && record S01 PASS "no unredacted key in $(find "$ART_DIR" -type f | wc -l) artifact files" || record S01 FAIL "$hits unredacted keys"
}

# --------------------------------------------------------------- main -----
START=$(date +%s)
log "P08b E2E $RUN_ID (tree $SHA, client image $IMAGE)"
if ! setup; then log "SETUP FAILED"; cleanup; exit 2; fi

t_clients
t_m01; t_m02; t_m03; t_m04; t_m05
t_k01; t_a01; t_a02; t_a03; t_a04; t_a05; t_a06; t_a07
t_x01

cleanup
t_s01
log "done in $(( $(date +%s) - START ))s: FAIL=$FAILS CLEAN_OK=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
