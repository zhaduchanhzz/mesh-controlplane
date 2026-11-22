#!/usr/bin/env bash
# P05 real-client E2E: one cluster process, one Noise key and one hostname for
# several tailnets; the tailnet of a client comes from its auth key (ADR 0007).
#
# Path: tailscale clients (docker on mc-ci) -> https://c1.cp.gta5web.online:8443
# -> VPS 116.118.3.46 -> WireGuard -> mc-cp:8443 (headscale-cluster.service).
# The P02 POC (headscale-tailnets.service) holds :8443 normally; this harness
# stops it, runs the cluster on a fresh E2E database, and restores the POC.
#
# Usage: run.sh            (BIN=path to a linux/amd64 headscale binary to install;
#                           default: build one from the current checkout)
#        KEEP=1 run.sh     (leave clients and cluster running, for debugging)
#
# Secrets: pre-auth keys are minted on mc-cp, kept only in a mode-700 temp dir,
# piped to `tailscale up --auth-key=file:/dev/stdin`, never printed. Logs in
# the artifact dir are redacted.
#
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029

set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
HOST=${HOST:-c1.cp.gta5web.online}
OTHER_HOST=${OTHER_HOST:-a.cp.gta5web.online}
PORT=${PORT:-8443}
URL="https://$HOST:$PORT"
CLUSTER_ID=${CLUSTER_ID:-mc-cp-c1}
IMAGE=${IMAGE:-tailscale/tailscale:v1.104.7}
CAPVER=${CAPVER:-116}
KEEP=${KEEP:-0}
OBSERVE_S=${OBSERVE_S:-5}
LABEL=mesh-e2e=p05

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
ART_DIR=$REPO/artifacts/$SHA/P05/e2e-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"

HSC="sudo -u headscale /usr/local/bin/headscale-cluster -c /etc/headscale/cluster/config.yaml cluster --cluster-id $CLUSTER_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
oneline() { tr '\n' ' ' | cut -c1-160; }
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
ssh_() { local h=$1; shift; ssh -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }

FAILS=0
record() { # ID STATUS REASON
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-4s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------- cleanup ---
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving clients and cluster running"; return; }
  log "cleanup"
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\"" || CLEAN_OK=0
  # The glob must expand as root: the mesh user cannot list the directory.
  cp_ "sudo systemctl stop headscale-cluster; sudo sh -c 'rm -f /var/lib/headscale/cluster-e2e/db.sqlite*'; \
       sudo sh -c '! ls /var/lib/headscale/cluster-e2e/db.sqlite* 2>/dev/null'; ok=\$?; \
       sudo systemctl start headscale-tailnets; exit \$ok" || { log "E2E database not removed"; CLEAN_OK=0; }
  local i code=000
  for i in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "https://$OTHER_HOST:$PORT/health" || true)
    [ "$code" = 200 ] && break
    sleep 1
  done
  [ "$code" = 200 ] && log "P02 POC restored (/health 200 on $OTHER_HOST)" || { log "P02 POC NOT healthy after restore ($code)"; CLEAN_OK=0; }
  rm -rf "$TMPD"
}
trap 'cleanup; exit 2' INT TERM

# --------------------------------------------------------------- setup ---
setup() {
  local bin=${BIN:-}
  if [ -z "$bin" ]; then
    bin=$TMPD/headscale-cluster
    log "building linux/amd64 binary at $SHA"
    (cd "$REPO" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o "$bin" ./cmd/headscale) || return 1
  fi
  log "installing binary, config and unit on mc-cp"
  scp -q -i "$SSH_KEY" -o BatchMode=yes "$bin" "$REPO/infra/lab/mc-cp/cluster/config.yaml" \
    "$REPO/infra/lab/mc-cp/headscale-cluster.service" "$CP_SSH:/tmp/" || return 1
  cp_ "set -e
    sudo install -m 0755 /tmp/$(basename "$bin") /usr/local/bin/headscale-cluster
    sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster
    sudo install -o root -g headscale -m 0640 /tmp/config.yaml /etc/headscale/cluster/config.yaml
    sudo install -m 0644 /tmp/headscale-cluster.service /etc/systemd/system/headscale-cluster.service
    sudo systemctl daemon-reload
    sudo systemctl stop headscale-tailnets headscale-cluster
    sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale/cluster /var/lib/headscale/cluster-e2e
    sudo sh -c 'rm -f /var/lib/headscale/cluster-e2e/db.sqlite*'
    sudo sh -c '! ls /var/lib/headscale/cluster-e2e/db.sqlite* 2>/dev/null'
    rm -f /tmp/$(basename "$bin") /tmp/config.yaml /tmp/headscale-cluster.service" || return 1

  log "seeding tailnets a, b (ACTIVE) and c (SUSPENDED) while the cluster is stopped"
  local pol='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}'
  local s pf=/var/lib/headscale/cluster-e2e/policy.hujson
  for s in a b c; do
    cp_ "$HSC tailnet create --slug $s >/dev/null && $HSC tailnet status --slug $s --set ACTIVE" || return 1
  done
  # Written where the headscale user can read it; removed after use.
  printf '%s\n' "$pol" | cp_ "sudo rm -f /tmp/p05-policy.hujson; sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf" || return 1
  for s in a b; do
    cp_ "$HSC tailnet policy --slug $s --file $pf" || return 1
  done
  cp_ "sudo rm -f $pf"
  for s in a b c; do
    cp_ "$HSC tailnet authkey --slug $s --user e2e-$s --reusable --expiration 30m" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])' >"$TMPD/key-$s" || return 1
    chmod 600 "$TMPD/key-$s"
    [ -s "$TMPD/key-$s" ] || return 1
  done
  cp_ "$HSC tailnet status --slug c --set SUSPENDED" || return 1
  printf 'hskey-auth-AAAAAAAAAAAA-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB\n' >"$TMPD/key-bogus"

  log "starting cluster"
  cp_ "sudo systemctl start headscale-cluster" || return 1
  local i code=000
  for i in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$URL/health" || true)
    [ "$code" = 200 ] && break
    sleep 1
  done
  cp_ "sudo journalctl -u headscale-cluster -b --no-pager -n 30" 2>&1 | redact >"$ART_DIR/logs/cluster-start.log"
  [ "$code" = 200 ] || { log "cluster not healthy ($code)"; return 1; }
  ci_ "docker image inspect $IMAGE >/dev/null 2>&1 || docker pull -q $IMAGE >/dev/null" || return 1
}

# ------------------------------------------------------------- clients ---
start_ts() { # NAME
  ci_ "docker run -d --name $1 --hostname $1 --label $LABEL \
    --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL \
    $IMAGE tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; \
      [ \$? -le 1 ] && exit 0; sleep 0.5; done; exit 0"
}
ts_auth() { # NAME up|login KEYFILE HOSTNAME -> tailscale exit code
  ci_ "docker exec -i $1 timeout 45 tailscale $2 --login-server=$URL --auth-key=file:/dev/stdin --hostname=$4" \
    <"$3" 2>&1 | redact | tee "$TMPD/last.out" >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | python3 -c "import json,sys; d=json.load(sys.stdin); print($2)" 2>/dev/null; }
peers() { field "$1" "' '.join(sorted(p['HostName'] for p in (d.get('Peer') or {}).values()))"; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
state() { field "$1" "d['BackendState']"; }
nodekey() { field "$1" "d['Self']['PublicKey'][:16]"; }
wait_state() { local i; for i in $(seq 1 "$3"); do [ "$(state "$1")" = "$2" ] && return 0; sleep 1; done; return 1; }
server_nodes() { cp_ "$HSC tailnet nodes --slug $1" 2>/dev/null; } # JSON
server_field() { # SLUG HOSTNAME FIELD
  server_nodes "$1" | python3 -c "import json,sys
for n in json.load(sys.stdin):
    if n['hostname']=='$2': print(n['$3'])"
}
save_logs() { ci_ "docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }

# --------------------------------------------------------------- tests ---
t_http() {
  local k1 k2 code
  k1=$(curl -s -m 10 "$URL/key?v=$CAPVER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["publicKey"])' 2>/dev/null)
  # Same listener reached under another hostname: the key is per cluster.
  k2=$(curl -s -m 10 --connect-to "$OTHER_HOST:$PORT:$HOST:$PORT" "https://$OTHER_HOST:$PORT/key?v=$CAPVER" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["publicKey"])' 2>/dev/null)
  printf 'cluster noise key: %s\n' "${k1:0:13}…" >>"$ART_DIR/env.txt"
  if [ -n "$k1" ] && [ "$k1" = "$k2" ]; then record C01 PASS "one Noise key for the cluster, independent of Host"
  else record C01 FAIL "key c1=${k1:0:13} other=${k2:0:13}"; fi
  local bad="" p
  for p in /api/v1/node /api/v2/tailnet/-/devices /metrics /debug/ /swagger /register/xyz /oidc/callback; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 "$URL$p")
    case $code in 2??) bad="$bad $p=$code" ;; esac
  done
  [ -z "$bad" ] && record C02 PASS "no admin API, metrics, debug or web-auth route on the public listener" \
    || record C02 FAIL "served:$bad"
}

t_isolation() {
  local ra=0 ra2=0 rb=0
  start_ts p05-a1 && start_ts p05-a2 && start_ts p05-b1 || { record R01 FAIL "client setup failed"; return; }
  ts_auth p05-a1 up "$TMPD/key-a" p05-a1 || ra=$?
  ts_auth p05-a2 up "$TMPD/key-a" p05-a2 || ra2=$?
  ts_auth p05-b1 up "$TMPD/key-b" p05-b1 || rb=$?
  if [ $ra != 0 ] || [ $ra2 != 0 ] || [ $rb != 0 ]; then
    record R01 FAIL "registration failed (a1=$ra a2=$ra2 b1=$rb)"; return
  fi
  # a1 must learn a2 (positive), then watch for any cross-tenant peer.
  local i pa1 pa2 pb1 leak=""
  for i in $(seq 1 20); do pa1=$(peers p05-a1); [ "$pa1" = p05-a2 ] && break; sleep 1; done
  for i in $(seq 1 "$OBSERVE_S"); do
    pa1=$(peers p05-a1); pa2=$(peers p05-a2); pb1=$(peers p05-b1)
    case " $pa1 $pa2 " in *p05-b1*) leak="A sees b1" ;; esac
    [ -n "$pb1" ] && leak="$leak B sees [$pb1]"
    [ -n "$leak" ] && break
    sleep 1
  done
  if [ -n "$leak" ]; then record R01 FAIL "cross-tailnet peer: $leak"
  elif [ "$pa1" = p05-a2 ] && [ "$pa2" = p05-a1 ]; then record R01 PASS "A: a1<->a2 only; B: b1 sees no peer (${OBSERVE_S}s window)"
  else record R01 FAIL "A peers incomplete: a1=[$pa1] a2=[$pa2]"; fi

  local ia ib; ia=$(selfip p05-a1); ib=$(selfip p05-b1)
  if [ -n "$ia" ] && [ "$ia" = "$ib" ]; then record R02 PASS "per-tailnet IPAM: first node of A and of B both got $ia"
  else record R02 FAIL "a1=$ia b1=$ib (expected the same first address)"; fi

  local ia2; ia2=$(selfip p05-a2)
  if ci_ "docker exec p05-a1 tailscale ping -c 5 --timeout 5s $ia2" >"$ART_DIR/logs/R03-ping.log" 2>&1; then
    record R03 PASS "data plane inside A: a1 pings a2"
  else record R03 FAIL "a1 cannot ping a2 (see logs/R03-ping.log)"; fi
}

t_profiles() {
  local n=p05-d ra=0 rb=0 problems=""
  start_ts $n || { record R04 FAIL "client setup failed"; return; }
  ts_auth $n up "$TMPD/key-a" p05-d-a || ra=$?
  local nka; nka=$(nodekey $n)
  ts_auth $n login "$TMPD/key-b" p05-d-b || rb=$?
  local nkb; nkb=$(nodekey $n)
  [ $ra = 0 ] && [ $rb = 0 ] || { record R04 FAIL "login failed (A rc=$ra, B rc=$rb)"; return; }
  local nprof; nprof=$(ci_ "docker exec $n tailscale switch --list" | tail -n +2 | grep -c .)
  [ "$nprof" -ge 2 ] || problems="$problems; $nprof profile(s)"
  [ "$nka" != "$nkb" ] || problems="$problems; same node key in A and B"
  local mka mkb; mka=$(server_field a p05-d-a machine_key); mkb=$(server_field b p05-d-b machine_key)
  [ -n "$mka" ] && [ "$mka" = "$mkb" ] || problems="$problems; server machine keys A=[$mka] B=[$mkb]"
  [ -z "$(server_field a p05-d-b id)" ] && [ -z "$(server_field b p05-d-a id)" ] || problems="$problems; a node landed in the wrong tailnet"
  local i p
  for i in $(seq 1 15); do p=$(peers $n); [ "$p" = p05-b1 ] && break; sleep 1; done
  [ "$p" = p05-b1 ] || problems="$problems; on B the device sees [$p]"
  local pa; pa=$(ci_ "docker exec $n tailscale switch --list" | awk 'NR>1 && $0 !~ /\*$/ {print $1; exit}')
  ci_ "docker exec $n tailscale switch $pa" >/dev/null 2>&1
  wait_state $n Running 30 || problems="$problems; not Running after switch to A"
  for i in $(seq 1 15); do p=$(peers $n); [ "$p" = "p05-a1 p05-a2" ] && break; sleep 1; done
  [ "$p" = "p05-a1 p05-a2" ] || problems="$problems; back on A the device sees [$p]"
  [ "$(nodekey $n)" = "$nka" ] || problems="$problems; A node key changed after switch"
  save_logs $n
  [ -z "$problems" ] && record R04 PASS "one device, profiles A and B: two nodes, same machine key, each sees only its tailnet" \
    || record R04 FAIL "${problems#; }"
}

t_refusals() {
  local rc=0 n
  start_ts p05-x || { record R05 FAIL "client setup failed"; return; }
  ts_auth p05-x up "$TMPD/key-bogus" p05-x-bogus || rc=$?
  n=$(for s in a b; do server_field $s p05-x-bogus id; done)
  [ $rc != 0 ] && [ -z "$n" ] && grep -q 'invalid pre auth key' "$TMPD/last.out" \
    && record R05 PASS "unknown auth key refused by the server ('invalid pre auth key'), no node created" \
    || record R05 FAIL "rc=$rc node=[$n] output=[$(oneline <"$TMPD/last.out")]"

  rc=0
  ts_auth p05-x up "$TMPD/key-c" p05-x-suspended || rc=$?
  n=$(for s in a b; do server_field $s p05-x-suspended id; done)
  [ $rc != 0 ] && [ -z "$n" ] && grep -q 'invalid pre auth key' "$TMPD/last.out" \
    && record R06 PASS "key of a SUSPENDED tailnet refused with the same message as an unknown key" \
    || record R06 FAIL "rc=$rc node=[$n] output=[$(oneline <"$TMPD/last.out")]"

  local out
  out=$(ci_ "docker exec p05-x timeout 20 tailscale login --login-server=$URL --hostname=p05-x-web" 2>&1 | redact)
  printf '%s\n' "$out" >"$ART_DIR/logs/R07-web.log"
  if printf '%s' "$out" | grep -q '/register/'; then record R07 FAIL "server handed out an interactive auth URL"
  elif printf '%s' "$out" | grep -q 'only registers nodes with an auth key'; then
    record R07 PASS "interactive (web/OIDC) registration refused with a clear message"
  else record R07 FAIL "no auth URL, but no refusal message either: [$(printf '%s' "$out" | oneline)]"; fi
  save_logs p05-x
}

# ---------------------------------------------------------------- main ---
T0=$(date +%s)
log "P05 E2E $RUN_ID at $SHA -> $URL (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "image: $IMAGE"; echo "commit: $SHA"; echo "cluster: $CLUSTER_ID"; } >>"$ART_DIR/env.txt"
t_http
t_isolation
t_profiles
t_refusals
for c in p05-a1 p05-a2 p05-b1; do save_logs $c; done
cp_ "sudo journalctl -u headscale-cluster --since -$(( $(date +%s) - T0 + 5 ))s --no-pager" 2>&1 | redact >"$ART_DIR/logs/cluster.log"
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
