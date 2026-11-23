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
# P08 (C03): the listener answers only its own hostname (421 otherwise).
#
# P09a (S01, S03, S16, S18): B under allow then deny-all while A keeps a
# wildcard; whole-cluster restart without re-login; key revocation and tenant
# delete/recreate.
#
# P10 scenarios (L01, F01-F03) change tailnets while the cluster runs: a live
# tailnet, an owner_epoch bump, suspend/resume and a move to another cluster.
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
# P11: set BOOT_BIN to a mesh-bootstrap binary to run the bootstrap cases
# (B01-B06). It serves on mc-cp loopback only (no firewall change); the
# onboarding requests run on mc-cp with curl --resolve.
BOOT_BIN=${BOOT_BIN:-}
BOOT_HOST=${BOOT_HOST:-boot.cp.gta5web.online}
BOOT_PORT=${BOOT_PORT:-8444}

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
  [ -n "$BOOT_BIN" ] && cp_ "sudo systemctl stop mesh-bootstrap-e2e 2>/dev/null; sudo rm -f /usr/local/bin/mesh-bootstrap; true"
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
  # P10: new devices enroll only into tailnets of a registered cluster.
  cp_ "$HSC register --class shared --max-devices 5000 >/dev/null" || return 1
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
# wait_peer NAME PEER TIMEOUT_S: NAME's netmap lists PEER
wait_peer() {
  local i
  for i in $(seq 1 "$3"); do case " $(peers "$1") " in *" $2 "*) return 0 ;; esac; sleep 1; done
  return 1
}
mint() { # SLUG USER KEYFILE
  cp_ "$HSC tailnet authkey --slug $1 --user $2 --reusable --expiration 30m" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])' >"$3" && chmod 600 "$3" && [ -s "$3" ]
}
# Read the journal first: grep -q would SIGPIPE journalctl and, with
# pipefail, fail the pipeline even on a match.
cluster_log_has() {
  local out; out=$(cp_ "sudo journalctl -u headscale-cluster --since -${2}s --no-pager" 2>/dev/null)
  grep -q "$1" <<<"$out"
}
drop_ts() { ci_ "docker rm -f $1 >/dev/null 2>&1; docker volume rm $1 >/dev/null 2>&1; true"; }
ALLOW_ALL='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}'
DENY_ALL='{"grants":[]}'
# set_policy SLUG POLICY: store a policy as the operator; the running cluster
# reloads it when policy_revision moves (P10).
set_policy() {
  local pf=/var/lib/headscale/cluster-e2e/policy.hujson
  printf '%s\n' "$2" | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $HSC tailnet policy --slug $1 --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"
}
# Hostnames by tailnet: A = p05-a*, p05-d-a (R04); B = p05-b*, p05-d-b (R04).
has_a_host() { grep -qE '(^| )(p05-a[0-9]+|p05-d-a)( |$)' <<<"$1"; }
has_b_host() { grep -qE '(^| )(p05-b[0-9]+|p05-d-b)( |$)' <<<"$1"; }
health_code() { curl -s -o /dev/null -w '%{http_code}' -m 5 "$URL/health" || true; }

# --------------------------------------------------------------- tests ---
t_http() {
  local k1 k2 code
  k1=$(curl -s -m 10 "$URL/key?v=$CAPVER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["publicKey"])' 2>/dev/null)
  k2=$(curl -s -m 10 "$URL/key?v=$CAPVER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["publicKey"])' 2>/dev/null)
  printf 'cluster noise key: %s
' "${k1:0:13}…" >>"$ART_DIR/env.txt"
  # One key for the cluster; that every tenant uses it is shown by R01/R04
  # (tailnets A and B register through the same URL). Since P08 the listener
  # answers no other hostname, so Host independence is no longer probed here.
  if [ -n "$k1" ] && [ "$k1" = "$k2" ]; then record C01 PASS "one Noise key for the cluster at $HOST"
  else record C01 FAIL "key fetches differ or empty: ${k1:0:13} / ${k2:0:13}"; fi

  # C03 (P08, S11c): only the cluster's own hostname is served. The other
  # hostname reaches the same listener (wildcard cert, --connect-to); a raw IP
  # sends no SNI. -k only where the certificate cannot match by construction.
  local ip=${VPS_IP:-116.118.3.46} c3="" c
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 "$URL/health"); [ "$c" = 200 ] || c3="$c3 own-host=$c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 --connect-to "$OTHER_HOST:$PORT:$HOST:$PORT" "https://$OTHER_HOST:$PORT/health")
  [ "$c" = 421 ] || c3="$c3 other-host=$c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 --connect-to "$OTHER_HOST:$PORT:$HOST:$PORT" "https://$OTHER_HOST:$PORT/key?v=$CAPVER")
  [ "$c" = 421 ] || c3="$c3 other-host-key=$c"
  c=$(curl -sk -o /dev/null -w '%{http_code}' -m 10 "https://$ip:$PORT/health"); [ "$c" = 421 ] || c3="$c3 raw-ip=$c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Host: $OTHER_HOST:$PORT" "$URL/health"); [ "$c" = 421 ] || c3="$c3 sni-own/host-other=$c"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 --connect-to "$OTHER_HOST:$PORT:$HOST:$PORT" -H "Host: $HOST:$PORT" "https://$OTHER_HOST:$PORT/health")
  [ "$c" = 421 ] || c3="$c3 sni-other/host-own=$c"
  # C04 (P08, S14/AGENTS rule 9): cluster metrics listener (127.0.0.1:9095 in
  # infra/lab/mc-cp/cluster/config.yaml) serves /metrics to loopback only and
  # nothing else; it is not reachable from the LAN.
  local c4="" m d l
  m=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:9095/metrics")
  d=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:9095/debug/")
  l=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "http://${CP_SSH#*@}:9095/metrics" || true)
  [ "$m" = 200 ] || c4="$c4 loopback-metrics=$m"
  [ "$d" = 404 ] || c4="$c4 loopback-debug=$d"
  case $l in 000) ;; *) c4="$c4 lan-metrics=$l" ;; esac
  [ -z "$c4" ] && record C04 PASS "metrics on loopback only, /metrics only (debug 404, LAN unreachable)"     || record C04 FAIL "unexpected:$c4"

  [ -z "$c3" ] && record C03 PASS "only $HOST is served: other host, raw IP and SNI != Host get 421"     || record C03 FAIL "unexpected:$c3"

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

# ------------------------------------------------- S01/S03, S16 (P09a) ---
# S01: inside B, b1 <-> b4 under an allow policy (peers + data plane), and
# neither tailnet sees the other.
# S03: A keeps its wildcard policy, B switches to {"grants":[]}: B's nodes
# lose each other, A is unaffected and still sees nothing of B. B's allow
# policy is restored afterwards for the P10 cases.
t_scope() {
  local r=0 problems="" pb1 pb4 pa ib4 i
  start_ts p05-b4 && ts_auth p05-b4 up "$TMPD/key-b" p05-b4 || r=$?
  if [ $r != 0 ]; then
    record S01 FAIL "b4 registration rc=$r"; record S03 FAIL "not run: S01 setup failed"; return
  fi
  wait_peer p05-b1 p05-b4 30 || problems="$problems; b1 does not see b4"
  wait_peer p05-b4 p05-b1 30 || problems="$problems; b4 does not see b1"
  sleep "$OBSERVE_S"
  pb1=$(peers p05-b1); pb4=$(peers p05-b4); pa="$(peers p05-a1) $(peers p05-a2)"
  has_a_host "$pb1 $pb4" && problems="$problems; B sees A: [$pb1 | $pb4]"
  has_b_host "$pa" && problems="$problems; A sees B: [$pa]"
  ib4=$(selfip p05-b4)
  ci_ "docker exec p05-b1 tailscale ping -c 5 --timeout 5s $ib4" >"$ART_DIR/logs/S01-ping.log" 2>&1 \
    || problems="$problems; b1 cannot ping b4"
  [ -z "$problems" ] && record S01 PASS "B: b1<->b4 peers and ping under allow; no A host in B, no B host in A" \
    || record S01 FAIL "${problems#; }"

  problems=""
  if ! set_policy b "$DENY_ALL"; then record S03 FAIL "could not store the deny policy of B"; return; fi
  for i in $(seq 1 30); do pb1=$(peers p05-b1); pb4=$(peers p05-b4); [ -z "$pb1$pb4" ] && break; sleep 1; done
  [ -z "$pb1$pb4" ] || problems="$problems; B still sees peers under deny: [$pb1 | $pb4]"
  pa=$(peers p05-a1)
  case " $pa " in *" p05-a2 "*) ;; *) problems="$problems; a1 lost a2 [$pa]" ;; esac
  has_b_host "$pa $(peers p05-a2)" && problems="$problems; A sees B under A's wildcard"
  if ci_ "docker exec p05-b1 tailscale ping -c 2 --timeout 3s $ib4" >"$ART_DIR/logs/S03-ping.log" 2>&1; then
    problems="$problems; b1 still pings b4 under deny"
  fi
  [ -z "$problems" ] && record S03 PASS "A wildcard, B grants=[]: B nodes see and reach no peer (${i}s); A unaffected, sees no B host" \
    || record S03 FAIL "${problems#; }"

  set_policy b "$ALLOW_ALL" && wait_peer p05-b1 p05-b4 30 || log "warning: B allow policy not visible on b1 within 30s"
}

# S16: restart the whole cluster process (same DB, same Noise key). Clients
# reconnect by themselves: same node key, IP and node ID, no new node, no
# cross-tailnet peer. A node that joins after the restart is seen by a1,
# which proves a1's map stream is served by the new process.
t_restart() {
  local problems="" before after code i c r=0
  local who="p05-a1 p05-a2 p05-b1 p05-b4"
  snap() {
    for c in $who; do printf '%s key=%s ip=%s\n' $c "$(nodekey $c)" "$(selfip $c)"; done
    for c in a b; do
      server_nodes $c | python3 -c 'import json,sys; [print(n["id"], n["hostname"], n["ipv4"], n["node_key"]) for n in sorted(json.load(sys.stdin), key=lambda n: n["id"])]'
    done
  }
  before=$(snap)
  local t0; t0=$(date +%s)
  if ! cp_ "sudo systemctl restart headscale-cluster"; then record S16 FAIL "systemctl restart failed"; return; fi
  for i in $(seq 1 30); do code=$(health_code); [ "$code" = 200 ] && break; sleep 1; done
  [ "$code" = 200 ] || problems="$problems; /health $code after restart"
  for c in $who; do wait_state $c Running 60 || problems="$problems; $c not Running"; done
  after=$(snap)
  if [ "$before" != "$after" ]; then
    problems="$problems; identity changed (logs/S16-diff.txt)"
    diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >"$ART_DIR/logs/S16-diff.txt"
  fi
  start_ts p05-a4 && ts_auth p05-a4 up "$TMPD/key-a" p05-a4 || r=$?
  [ $r = 0 ] || problems="$problems; a4 registration after restart rc=$r"
  wait_peer p05-a1 p05-a4 45 || problems="$problems; a1 does not see a4 (stream not re-established)"
  wait_peer p05-b1 p05-b4 30 || problems="$problems; b1 lost b4"
  has_b_host "$(peers p05-a1)" && problems="$problems; A sees B after restart"
  has_a_host "$(peers p05-b1)" && problems="$problems; B sees A after restart"
  printf '%s\n' "$before" | wc -l >"$ART_DIR/logs/S16-identities-count.txt"
  [ -z "$problems" ] && record S16 PASS "cluster restart: 4 clients back without re-login, same node key/IP/NodeID, no new node; a1 sees a4 joined after ($(( $(date +%s) - t0 ))s)" \
    || record S16 FAIL "${problems#; }"
  save_logs p05-a4; save_logs p05-b4
}

# ------------------------------------------------------------------ P10 ---
t_p10() {
  local pf=/var/lib/headscale/cluster-e2e/policy.hujson i t0

  # L01: a tailnet created and activated while the cluster runs is served
  # without a restart; its policy, written by the operator, is reloaded live.
  t0=$(date +%s)
  if cp_ "$HSC tailnet create --slug d >/dev/null && $HSC tailnet status --slug d --set ACTIVE"; then
    for i in $(seq 1 20); do mint d e2e-d "$TMPD/key-d" 2>/dev/null && break; sleep 1; done
    start_ts p05-d1 && start_ts p05-d2
    local r1=0 r2=0
    ts_auth p05-d1 up "$TMPD/key-d" p05-d1 || r1=$?
    ts_auth p05-d2 up "$TMPD/key-d" p05-d2 || r2=$?
    local deny; deny=$(peers p05-d1)
    printf '%s\n' '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' \
      | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $HSC tailnet policy --slug d --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc" \
      || r1=97
    if [ $r1 = 0 ] && [ $r2 = 0 ] && [ -z "$deny" ] && wait_peer p05-d1 p05-d2 20 && [ "$(peers p05-d1)" = p05-d2 ]; then
      record L01 PASS "live tailnet served without restart: deny-all first, operator policy reloaded ($(( $(date +%s) - t0 ))s)"
    else
      record L01 FAIL "rc d1=$r1 d2=$r2, peers before policy=[$deny], after=[$(peers p05-d1)]"
    fi
  else
    record L01 FAIL "could not create/activate tailnet d"
  fi

  # F01: owner_epoch bump for this cluster: the runtime is reloaded, the
  # clients reconnect on their own (same node key, no new node) and see a
  # node that joined after the reload.
  local nk1 nodes_before nodes_after r=0
  nk1=$(nodekey p05-a1); nodes_before=$(server_nodes a | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
  t0=$(date +%s)
  if cp_ "$HSC tailnet reassign --slug a --to $CLUSTER_ID >/dev/null"; then
    sleep 4
    start_ts p05-a3 && ts_auth p05-a3 up "$TMPD/key-a" p05-a3 || r=$?
    nodes_after=$(server_nodes a | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
    if [ $r = 0 ] && wait_peer p05-a1 p05-a3 45 && [ "$(nodekey p05-a1)" = "$nk1" ] \
      && [ "$nodes_after" = $((nodes_before + 1)) ] && cluster_log_has 'retiring tailnet runtime' 120; then
      record F01 PASS "epoch bump: runtime reloaded, a1 reconnected without re-login and sees a3 ($(( $(date +%s) - t0 ))s)"
    else
      record F01 FAIL "rc=$r a1 peers=[$(peers p05-a1)] node key same=$([ "$(nodekey p05-a1)" = "$nk1" ] && echo yes || echo no) nodes $nodes_before->$nodes_after"
    fi
  else
    record F01 FAIL "reassign failed"
  fi

  # F02: suspend B: its keys stop registering; resume: b1 comes back by
  # itself and sees a node that joined after the resume.
  # Fresh devices for every refusal check: a refused tailscaled keeps its
  # auth key and registers later if the key becomes valid again.
  start_ts p05-y1 >/dev/null
  local rs=0 rr=0
  if cp_ "$HSC tailnet status --slug b --set SUSPENDED"; then
    sleep 4
    ts_auth p05-y1 up "$TMPD/key-b" p05-y-susp || rs=$?
    local refused=no; [ $rs != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out" && refused=yes
    save_logs p05-y1; drop_ts p05-y1
    cp_ "$HSC tailnet status --slug b --set ACTIVE"
    sleep 4
    start_ts p05-b2 && ts_auth p05-b2 up "$TMPD/key-b" p05-b2 || rr=$?
    if [ $refused = yes ] && [ $rr = 0 ] && wait_peer p05-b1 p05-b2 45; then
      record F02 PASS "suspended B refused new nodes; after resume b1 reconnected and sees b2"
    else
      record F02 FAIL "refused while suspended=$refused (rc=$rs), resume rc=$rr, b1 peers=[$(peers p05-b1)]"
    fi
  else
    record F02 FAIL "could not suspend b"
  fi

  # F03: move A to another cluster: this cluster stops serving it (new
  # registrations refused); B and D are unaffected.
  local rm=0
  if cp_ "$HSC tailnet reassign --slug a --to mc-cp-c2 >/dev/null"; then
    sleep 4
    start_ts p05-y2 >/dev/null
    ts_auth p05-y2 up "$TMPD/key-a" p05-y-moved || rm=$?
    local refused=no; [ $rm != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out" && refused=yes
    save_logs p05-y2; drop_ts p05-y2
    local rb=0; start_ts p05-b3 && ts_auth p05-b3 up "$TMPD/key-b" p05-b3 || rb=$?
    if [ $refused = yes ] && [ $rb = 0 ] && wait_peer p05-b1 p05-b3 30 && cluster_log_has 'retiring tailnet runtime' 60; then
      record F03 PASS "A moved away: refused here, runtime retired; B still serves new nodes"
    else
      record F03 FAIL "A refused=$refused (rc=$rm), B rc=$rb, b1 peers=[$(peers p05-b1)]"
    fi
  else
    record F03 FAIL "reassign to another cluster failed"
  fi
  local c; for c in p05-d1 p05-d2 p05-a3 p05-b2 p05-b3; do save_logs $c; done
}

# ---------------------------------------------------------- S18 (P09a) ---
# S18: revoke a key; delete a tenant (DELETING, terminal) and create a new
# one. Old keys and old nodes never come back: a revoked key or a key of the
# deleted tenant registers nothing, DELETING cannot be undone, the slug
# cannot be reused, and the deleted tenant's node, restarted, is not served
# and does not appear in the new tenant.
t_lifecycle() {
  local problems="" kid i r=0 n st
  if ! cp_ "$HSC tailnet create --slug e >/dev/null && $HSC tailnet status --slug e --set ACTIVE"; then
    record S18 FAIL "could not create/activate tailnet e"; return
  fi
  for i in $(seq 1 20); do mint e e2e-e "$TMPD/key-e" 2>/dev/null && break; sleep 1; done
  kid=$(cp_ "$HSC tailnet authkey --slug e --user e2e-e --expiration 30m" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); open(sys.argv[1],"w").write(d["key"]+"\n"); print(d["id"])' "$TMPD/key-e-rev")
  chmod 600 "$TMPD/key-e-rev"
  [ -n "$kid" ] && cp_ "$HSC tailnet authkey-revoke --slug e --id $kid" || problems="$problems; revoke failed (id=[$kid])"

  # (1) revoked key: refused, no node (fresh device).
  start_ts p05-y3 >/dev/null; r=0
  ts_auth p05-y3 up "$TMPD/key-e-rev" p05-y-revoked || r=$?
  n=$(server_field e p05-y-revoked id)
  { [ $r != 0 ] && [ -z "$n" ]; } || problems="$problems; revoked key registered (rc=$r node=[$n])"
  printf 'revoked key: %s\n' "$(oneline <"$TMPD/last.out")" >>"$ART_DIR/logs/S18.txt"
  save_logs p05-y3; drop_ts p05-y3

  # A live node of e, then delete e.
  r=0; start_ts p05-e1 && ts_auth p05-e1 up "$TMPD/key-e" p05-e1 || r=$?
  [ $r = 0 ] || problems="$problems; e1 registration rc=$r"
  cp_ "$HSC tailnet status --slug e --set DELETING" || problems="$problems; could not set DELETING"
  sleep 4
  local jl; jl=$(cp_ "sudo journalctl -u headscale-cluster --since -60s --no-pager" 2>/dev/null | grep 'retiring tailnet runtime')
  grep -qE 'slug[=":]+e([" ,}]|$)' <<<"$jl" || problems="$problems; runtime of e not retired"

  # (2) DELETING is terminal and the slug is not reusable.
  cp_ "$HSC tailnet status --slug e --set ACTIVE" >/dev/null 2>"$TMPD/reactivate.err" \
    && problems="$problems; DELETING -> ACTIVE accepted"
  cp_ "$HSC tailnet create --slug e" >/dev/null 2>&1 && problems="$problems; slug e created again"

  # (3) the deleted tenant's key registers nothing (fresh device).
  start_ts p05-y4 >/dev/null; r=0
  ts_auth p05-y4 up "$TMPD/key-e" p05-y-deleted || r=$?
  { [ $r != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out"; } || problems="$problems; key of deleted e not refused (rc=$r)"
  save_logs p05-y4; drop_ts p05-y4

  # (4) a new tenant e2; e's node, restarted, does not come back anywhere.
  cp_ "$HSC tailnet create --slug e2 >/dev/null && $HSC tailnet status --slug e2 --set ACTIVE" || problems="$problems; could not create e2"
  for i in $(seq 1 20); do mint e2 e2e-e2 "$TMPD/key-e2" 2>/dev/null && break; sleep 1; done
  # The oracle is what the server serves, not the client's BackendState:
  # tailscaled >= 1.104 caches the netmap on disk and reports Running from
  # that cache after a restart without any answer from control (seen on
  # 876b60f: "loaded netmap from disk cache; 0 peers", state Running, every
  # request refused). Served = a netmap fetched from control after restart.
  local rts el
  rts=$(ci_ "date -u +%Y-%m-%dT%H:%M:%SZ")
  ci_ "docker restart p05-e1 >/dev/null" || problems="$problems; could not restart e1"
  r=0; start_ts p05-e2 && ts_auth p05-e2 up "$TMPD/key-e2" p05-e2 || r=$?
  [ $r = 0 ] || problems="$problems; e2 registration rc=$r"
  sleep 15
  st=$(state p05-e1)
  el=$(ci_ "docker logs --since $rts p05-e1 2>&1")
  grep -q 'control-netmap .*cached=false' <<<"$el" && problems="$problems; deleted tenant's node e1 got a netmap from control after restart"
  grep -qE 'fetch failed 401|only registers nodes with an auth key|invalid pre auth key' <<<"$el"     || problems="$problems; no refusal of e1 seen after restart"
  [ "$(peers p05-e1)" = "" ] || problems="$problems; e1 lists peers after restart"
  n=$(server_nodes e2 | python3 -c 'import json,sys; print(" ".join(sorted(x["hostname"] for x in json.load(sys.stdin))))')
  [ "$n" = p05-e2 ] || problems="$problems; nodes of e2 = [$n]"
  case " $(peers p05-e2) " in *p05-e1*) problems="$problems; e2 sees e1" ;; esac
  printf 'e1 state after restart: %s\ne2 nodes: %s\n' "$st" "$n" >>"$ART_DIR/logs/S18.txt"
  save_logs p05-e1; save_logs p05-e2
  [ -z "$problems" ] && record S18 PASS "revoked key and key of deleted tenant refused; DELETING terminal, slug not reusable; deleted node not served after restart (no netmap from control, refused; client state=$st from its disk cache); new tenant e2 has only its own node" \
    || record S18 FAIL "${problems#; }"
}

# ------------------------------------------------------------- P11 -------
# boot_req KEYFILE [TENANT] [KEYID_OVERRIDE]: POST /v1/bootstrap on mc-cp
# loopback with the key's identifier and SHA-256 proof (the secret never
# leaves this host); prints "<status> <body>". Bodies are not logged with
# the proof.
boot_req() {
  local body
  # One python step builds the body: the proof is computed over the secret
  # read from the key file, so no shell variable ever holds the secret, and
  # no line ending of the host's python can leak into it.
  body=$(python3 -c 'import hashlib,json,sys
k=open(sys.argv[1]).read().strip()
i,s=k.split("hskey-auth-",1)[1].split("-",1)
if sys.argv[3]: i=sys.argv[3]
d={"credential":{"kind":"authkey_proof","key_id":i,"proof":hashlib.sha256(s.encode()).hexdigest()}}
if sys.argv[2]: d["tenant"]=sys.argv[2]
sys.stdout.write(json.dumps(d))' "$1" "${2:-}" "${3:-}")
  body=${body%$'\r'}
  printf %s "$body" | cp_ "curl -s -m 5 --resolve $BOOT_HOST:$BOOT_PORT:127.0.0.1 -o /tmp/boot.out -w '%{http_code} ' \
    -H 'Content-Type: application/json' --data-binary @- https://$BOOT_HOST:$BOOT_PORT/v1/bootstrap; cat /tmp/boot.out; rm -f /tmp/boot.out"
}

t_bootstrap() {
  local problems="" r code url first="" k
  scp -q -i "$SSH_KEY" -o BatchMode=yes "$BOOT_BIN" "$CP_SSH:/tmp/mesh-bootstrap" || { record B01 FAIL "could not copy mesh-bootstrap"; return; }
  cp_ "set -e
    sudo install -m 0755 /tmp/mesh-bootstrap /usr/local/bin/mesh-bootstrap && rm -f /tmp/mesh-bootstrap
    sudo systemctl stop mesh-bootstrap-e2e 2>/dev/null || true
    sudo systemd-run --quiet --unit=mesh-bootstrap-e2e --uid=headscale --gid=headscale \
      /usr/local/bin/mesh-bootstrap -config /etc/headscale/cluster/config.yaml -listen 127.0.0.1:$BOOT_PORT \
      -tls-cert /etc/headscale/tls/fullchain.pem -tls-key /etc/headscale/tls/privkey.pem -hostname $BOOT_HOST" \
    || { record B01 FAIL "could not start mesh-bootstrap"; return; }
  for k in $(seq 1 20); do
    code=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve $BOOT_HOST:$BOOT_PORT:127.0.0.1 https://$BOOT_HOST:$BOOT_PORT/health" || true)
    [ "$code" = 200 ] && break
    sleep 1
  done
  cp_ "sudo journalctl -u mesh-bootstrap-e2e --no-pager -n 20" 2>&1 | redact >"$ART_DIR/logs/bootstrap-start.log"
  [ "$code" = 200 ] || { record B01 FAIL "bootstrap not healthy ($code)"; return; }

  # B01 (F06): a valid key of ACTIVE tailnet b gets this cluster's URL, only.
  # (a was moved to the unregistered mc-cp-c2 by F03: see B06.)
  r=$(boot_req "$TMPD/key-b" b)
  printf 'B01 %s\n' "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
  code=${r%% *}
  url=$(python3 -c 'import json,sys; d=json.loads(sys.argv[1].split(" ",1)[1]); print(d["control_url"] if set(d)=={"control_url","issued_at"} else "EXTRA:"+",".join(sorted(d)))' "$r" 2>/dev/null)
  { [ "$code" = 200 ] && [ "$url" = "$URL" ]; } && record B01 PASS "valid key of b -> $url (control_url and issued_at only)" \
    || record B01 FAIL "got [$r]"

  # B02 (I15, I08): refusals are identical: suspended tenant c, key of a
  # naming tenant b, key of b naming a, an unknown key identifier.
  problems=""
  for k in "$TMPD/key-c|" "$TMPD/key-a|b" "$TMPD/key-b|a" "$TMPD/key-b||000000000000"; do
    r=$(boot_req "${k%%|*}" "$(cut -d'|' -f2 <<<"$k")" "$(cut -d'|' -f3 <<<"$k")")
    printf 'B02 %s\n' "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
    [ "${r%% *}" = 404 ] || problems="$problems; not 404 [$r]"
    [ -z "$first" ] && first=$r
    [ "$r" = "$first" ] || problems="$problems; differs [$r] vs [$first]"
    grep -q "cp.gta5web" <<<"$r" && problems="$problems; leaks a URL"
  done
  [ -z "$problems" ] && record B02 PASS "suspended, unknown key, key of a naming b, key of b naming a: one identical 404" \
    || record B02 FAIL "${problems#; }"

  # B03: onboarding with the returned URL enrolls a real client into b.
  start_ts p05-boot1 >/dev/null
  r=0; ci_ "docker exec -i p05-boot1 timeout 45 tailscale up --login-server=$url --auth-key=file:/dev/stdin --hostname=p05-boot1" \
    <"$TMPD/key-b" >>"$ART_DIR/logs/p05-boot1.cli.log" 2>&1 || r=$?
  k=$(server_field b p05-boot1 id)
  { [ "$r" = 0 ] && wait_state p05-boot1 Running 20 && [ -n "$k" ]; } \
    && record B03 PASS "tailscale up --login-server=<bootstrap URL> enrolled p05-boot1 into b" \
    || record B03 FAIL "rc=$r node=[$k] state=$(state p05-boot1)"
  save_logs p05-boot1

  # B04 (I09, rule 9): no debug/metrics/openapi; another Host -> 421.
  problems=""
  for k in /metrics /debug/pprof/ /v1/openapi /; do
    code=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve $BOOT_HOST:$BOOT_PORT:127.0.0.1 https://$BOOT_HOST:$BOOT_PORT$k")
    [ "$code" = 404 ] || problems="$problems; $k=$code"
  done
  code=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve $BOOT_HOST:$BOOT_PORT:127.0.0.1 -H 'Host: $HOST' https://$BOOT_HOST:$BOOT_PORT/health")
  [ "$code" = 421 ] || problems="$problems; Host $HOST=$code"
  [ -z "$problems" ] && record B04 PASS "only /v1/bootstrap and /health; debug/metrics/openapi 404; foreign Host 421" \
    || record B04 FAIL "${problems#; }"

  # B06 (F06): a, moved by F03 to mc-cp-c2 (not registered), is 503 to a
  # valid key holder and never gets this cluster's URL.
  r=$(boot_req "$TMPD/key-a" a)
  printf 'B06 %s\n' "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
  { [ "${r%% *}" = 503 ] && ! grep -q "cp.gta5web" <<<"$r"; } \
    && record B06 PASS "moved tenant a (owner cluster not registered): 503 to the key holder, no URL" \
    || record B06 FAIL "got [$r]"

  cp_ "sudo journalctl -u mesh-bootstrap-e2e --no-pager" 2>&1 | redact >"$ART_DIR/logs/bootstrap.log"
  grep -qE '[0-9a-f]{64}' "$ART_DIR/logs/bootstrap.log" && record B05 FAIL "bootstrap log holds a 64-hex value (proof?)" \
    || record B05 PASS "bootstrap log holds no proof or key"
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
t_scope
t_restart
t_p10
t_lifecycle
[ -n "$BOOT_BIN" ] && t_bootstrap
for c in p05-a1 p05-a2 p05-b1 p05-b4; do save_logs $c; done
cp_ "sudo journalctl -u headscale-cluster --since -$(( $(date +%s) - T0 + 5 ))s --no-pager" 2>&1 | redact >"$ART_DIR/logs/cluster.log"
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
