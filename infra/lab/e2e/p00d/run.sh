#!/usr/bin/env bash
# P00d M06 real-client E2E: with tuning.incremental_netmap on (INCREMENTAL=1)
# or off, a revoked peer must leave the netmap of real Tailscale clients
# within BOUND_S seconds, every time, for:
#   revoke  a node deleted by the operator
#   userdel a node deleted, then its user destroyed (S5: users with nodes
#           cannot be destroyed, so the node goes first)
#   policy  a policy change that blocks a peer
# and, as a control, a joining node must appear (join).
#
# Removals must come by the delta/policy path, not by the reconciliation or
# the periodic full map: both run every hour during the test (§5.2).
#
# P00d works on the single-tailnet runtime (ticket "Ngoài phạm vi"); the
# cluster process has no operator path to delete nodes or users. So this
# runs `headscale serve` on mc-cp, from a fresh database, as a transient
# systemd unit on the port and public path the P05 cluster uses
# (VPS 116.118.3.46 -> WireGuard -> mc-cp:8443), with the P02 POC stopped
# for the run and restored after.
#
# Usage: BIN=path/to/linux-amd64/headscale [INCREMENTAL=0|1] [ROUNDS=5] run.sh
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
IMAGE=${IMAGE:-tailscale/tailscale:v1.104.7}
INCREMENTAL=${INCREMENTAL:-1}
ROUNDS=${ROUNDS:-5}
BOUND_S=${BOUND_S:-10}
KEEP=${KEEP:-0}
# python3, or python where only that exists (Windows runners).
PY=${PY:-$(python3 -c 1 >/dev/null 2>&1 && echo python3 || echo python)}
LABEL=mesh-e2e=p00d
UNIT=headscale-p00d-e2e
CONF=/etc/headscale/p00d-e2e/config.yaml
STATE=/var/lib/headscale/p00d-e2e
HSB=/usr/local/bin/headscale-p00d
HS="sudo -u headscale $HSB -c $CONF"

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
ART_DIR=$REPO/artifacts/$SHA/P00d/m06-inc$INCREMENTAL-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
ssh_() { local h=$1; shift; ssh -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }

FAILS=0
record() { # ID STATUS REASON
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-8s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------- cleanup ---
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving clients and server running"; return; }
  log "cleanup"
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\"" || CLEAN_OK=0
  cp_ "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; \
       sudo rm -rf $STATE /etc/headscale/p00d-e2e $HSB; \
       sudo sh -c '! ls $STATE 2>/dev/null'; ok=\$?; \
       sudo systemctl start headscale-tailnets; exit \$ok" || { log "E2E state not removed"; CLEAN_OK=0; }
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
write_config() {
  cat <<EOF
server_url: $URL
listen_addr: 0.0.0.0:$PORT
metrics_listen_addr: 127.0.0.1:9096
unix_socket: /run/headscale-p00d/hs.sock
unix_socket_permission: "0770"
disable_check_updates: true
tls:
  cert_path: /etc/headscale/tls/fullchain.pem
  key_path: /etc/headscale/tls/privkey.pem
noise:
  private_key_path: $STATE/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
policy:
  mode: database
derp:
  server:
    enabled: false
  urls:
    - https://controlplane.tailscale.com/derpmap/default
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: $STATE/db.sqlite
dns:
  magic_dns: true
  base_domain: m06.tailnet.internal
  override_local_dns: false
log:
  level: info
tuning:
  incremental_netmap:
    enabled: $([ "$INCREMENTAL" = 1 ] && echo true || echo false)
    reconcile_interval: 1h
    full_map_interval: 1h
EOF
}

setup() {
  [ -n "${BIN:-}" ] || { log "BIN is required"; return 1; }
  log "installing binary and config on mc-cp (incremental=$INCREMENTAL)"
  write_config >"$TMPD/config.yaml"
  cp "$TMPD/config.yaml" "$ART_DIR/config.yaml"
  scp -q -i "$SSH_KEY" -o BatchMode=yes "$BIN" "$CP_SSH:/tmp/headscale-p00d" || return 1
  scp -q -i "$SSH_KEY" -o BatchMode=yes "$TMPD/config.yaml" "$CP_SSH:/tmp/p00d-config.yaml" || return 1
  cp_ "set -e
    sudo install -m 0755 /tmp/headscale-p00d $HSB
    sudo install -d -o root -g headscale -m 0750 /etc/headscale/p00d-e2e
    sudo install -o root -g headscale -m 0640 /tmp/p00d-config.yaml $CONF
    rm -f /tmp/headscale-p00d /tmp/p00d-config.yaml
    sudo systemctl stop headscale-tailnets headscale-cluster
    sudo rm -rf $STATE
    sudo install -d -o headscale -g headscale -m 0750 $STATE
    sudo systemd-run --unit=$UNIT --uid=headscale --gid=headscale \
      -p RuntimeDirectory=headscale-p00d -p RuntimeDirectoryMode=0770 \
      -p NoNewPrivileges=yes -p ProtectSystem=strict -p ProtectHome=yes \
      -p ReadWritePaths=$STATE \
      $HSB -c $CONF serve" || return 1
  local i code=000
  for i in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$URL/health" || true)
    [ "$code" = 200 ] && break
    sleep 1
  done
  [ "$code" = 200 ] || { log "server not healthy ($code)"; cp_ "sudo journalctl -u $UNIT --no-pager -n 30" 2>&1 | redact >&2; return 1; }
  ci_ "docker image inspect $IMAGE >/dev/null 2>&1 || docker pull -q $IMAGE >/dev/null" || return 1
  set_policy "$ALLOW" || return 1
  for u in umain ux; do cp_ "$HS users create $u >/dev/null" || return 1; done
}

ALLOW='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}'
# Only umain's own nodes reach each other: ux's node leaves umain's view.
BLOCK='{"acls":[{"action":"accept","src":["umain@"],"dst":["umain@:*"]}]}'

set_policy() {
  local pf=$STATE/policy.hujson
  printf '%s\n' "$1" | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $HS policy set -f $pf >/dev/null; rc=\$?; sudo rm -f $pf; exit \$rc"
}
# JSON is parsed on the Linux hosts: python3 on a Windows runner keeps a CR
# on its output.
mint() { # USER KEYFILE
  cp_ "$HS preauthkeys create --user $1 --reusable --expiration 30m -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"key\"])'" \
    >"$2" && chmod 600 "$2" && [ -s "$2" ]
}
node_id() { # HOSTNAME
  cp_ "$HS nodes list -o json | python3 -c 'import json,sys
for n in json.load(sys.stdin) or []:
    if \"$1\" in (n.get(\"name\"), n.get(\"given_name\"), n.get(\"givenName\")): print(n[\"id\"])'"
}
user_id() { # NAME
  cp_ "$HS users list -o json | python3 -c 'import json,sys
for u in json.load(sys.stdin) or []:
    if u.get(\"name\") == \"$1\": print(u[\"id\"])'"
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
up() { # NAME KEYFILE
  start_ts "$1" || return 1
  ci_ "docker exec -i $1 timeout 45 tailscale up --login-server=$URL --auth-key=file:/dev/stdin --hostname=$1" \
    <"$2" 2>&1 | redact >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
drop_ts() { ci_ "docker rm -f $1 >/dev/null 2>&1; docker volume rm $1 >/dev/null 2>&1; true"; }
peers() {
  ci_ "docker exec $1 tailscale status --json 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(\" \".join(sorted(p[\"HostName\"] for p in (d.get(\"Peer\") or {}).values())))' 2>/dev/null"
}
has_peer() { case " $(peers "$1") " in *" $2 "*) return 0 ;; esac; return 1; }

# until_ T0 NAME PEER present|absent LIMIT_S: seconds from T0 until NAME's
# netmap shows PEER present/absent; "timeout" past LIMIT_S.
until_() {
  local t0=$1 name=$2 peer=$3 want=$4 limit=$5 now
  while :; do
    if has_peer "$name" "$peer"; then [ "$want" = present ] && break; else [ "$want" = absent ] && break; fi
    now=$(date +%s.%N)
    awk -v a="$now" -v b="$t0" -v l="$limit" 'BEGIN { exit !(a - b > l) }' && { echo timeout; return 1; }
    sleep 0.2
  done
  awk -v a="$(date +%s.%N)" -v b="$t0" 'BEGIN { printf "%.2f", a - b }'
}

sample() { # SCENARIO ROUND SECONDS
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$INCREMENTAL" "$3" >>"$ART_DIR/m06.tsv"
  log "  $1 round $2: $3 s"
}

# --------------------------------------------------------------- tests ---
run_rounds() {
  mint umain "$TMPD/key-main" && mint ux "$TMPD/key-x" || { record SETUP FAIL "minting keys"; return; }
  up m06-m1 "$TMPD/key-main" && up m06-m2 "$TMPD/key-main" && up m06-x "$TMPD/key-x" \
    || { record SETUP FAIL "base clients did not come up"; return; }
  [ "$(until_ "$(date +%s.%N)" m06-m1 m06-x present 60)" != timeout ] \
    && [ "$(until_ "$(date +%s.%N)" m06-m1 m06-m2 present 60)" != timeout ] \
    || { record SETUP FAIL "base mesh not formed: m1 sees [$(peers m06-m1)]"; return; }

  local i t0 s uid
  for i in $(seq 1 "$ROUNDS"); do
    log "round $i"

    # join (control): a new node of umain appears on m1.
    t0=$(date +%s.%N)
    up m06-j$i "$TMPD/key-main" || { record "join-$i" FAIL "tailscale up"; continue; }
    s=$(until_ "$t0" m06-m1 m06-j$i present 60); sample join "$i" "$s"

    # revoke: the operator deletes j$i.
    local jid; jid=$(node_id m06-j$i)
    [ -n "$jid" ] || { record "revoke-$i" FAIL "node id not found"; continue; }
    cp_ "$HS nodes delete --identifier $jid --force >/dev/null" || { record "revoke-$i" FAIL "delete failed"; continue; }
    t0=$(date +%s.%N)
    s=$(until_ "$t0" m06-m1 m06-j$i absent $((BOUND_S + 20))); sample revoke "$i" "$s"
    s=$(until_ "$t0" m06-m2 m06-j$i absent $((BOUND_S + 20))); sample revoke-m2 "$i" "$s"
    drop_ts m06-j$i

    # userdel: a node of a fresh user, deleted, then the user destroyed.
    cp_ "$HS users create u$i >/dev/null" && mint "u$i" "$TMPD/key-u" \
      || { record "userdel-$i" FAIL "user/key"; continue; }
    up m06-d$i "$TMPD/key-u" || { record "userdel-$i" FAIL "tailscale up"; continue; }
    [ "$(until_ "$(date +%s.%N)" m06-m1 m06-d$i present 60)" != timeout ] || { record "userdel-$i" FAIL "d$i never visible"; continue; }
    local did; did=$(node_id m06-d$i); uid=$(user_id "u$i")
    cp_ "$HS nodes delete --identifier $did --force >/dev/null && $HS users destroy --identifier $uid --force >/dev/null" \
      || { record "userdel-$i" FAIL "delete node/user failed"; continue; }
    t0=$(date +%s.%N)
    s=$(until_ "$t0" m06-m1 m06-d$i absent $((BOUND_S + 20))); sample userdel "$i" "$s"
    drop_ts m06-d$i

    # policy: a policy that blocks ux; then restore.
    set_policy "$BLOCK" || { record "policy-$i" FAIL "policy set"; continue; }
    t0=$(date +%s.%N)
    s=$(until_ "$t0" m06-m1 m06-x absent $((BOUND_S + 20))); sample policy "$i" "$s"
    set_policy "$ALLOW"
    t0=$(date +%s.%N)
    s=$(until_ "$t0" m06-m1 m06-x present 60); sample policy-restore "$i" "$s"
  done

  # Verdicts: every removal within BOUND_S, every time.
  local sc
  for sc in revoke revoke-m2 userdel policy; do
    "$PY" - "$ART_DIR/m06.tsv" "$sc" "$BOUND_S" "$ROUNDS" >"$TMPD/verdict" <<'PY'
import sys, statistics
path, sc, bound, rounds = sys.argv[1], sys.argv[2], float(sys.argv[3]), int(sys.argv[4])
vals = [l.split('\t')[3].strip() for l in open(path) if l.split('\t')[0] == sc]
bad = [v for v in vals if v == 'timeout' or float(v) > bound]
nums = sorted(float(v) for v in vals if v != 'timeout')
p50 = statistics.median(nums) if nums else float('nan')
mx = max(nums) if nums else float('nan')
ok = len(vals) >= rounds and not bad
print(('PASS' if ok else 'FAIL') + f'\tn={len(vals)} p50={p50:.2f}s max={mx:.2f}s bound={bound:.0f}s' + (f' over={bad}' if bad else ''))
PY
    # A verdict that did not come out (no interpreter, bad data) is a FAIL.
    local st; st=$(cut -f1 "$TMPD/verdict" | tr -d '')
    case $st in PASS | FAIL) ;; *) st=FAIL ;; esac
    record "$sc" "$st" "$(cut -f2 "$TMPD/verdict" | tr -d '')"
  done
}

check_server() {
  local m; m=$(cp_ "curl -s http://127.0.0.1:9096/metrics" | grep -E '^headscale_nodestore_(snapshot_builds_total|netmap_divergence_total|netmap_reconciles_total)')
  printf '%s\n' "$m" >"$ART_DIR/metrics.txt"
  cp_ "sudo journalctl -u $UNIT --no-pager" 2>&1 | redact >"$ART_DIR/logs/server.log"
  if [ "$INCREMENTAL" = 1 ]; then
    grep -q 'peers="incremental"' <<<"$m" && record PATH PASS "incremental builds ran: $(grep 'peers="incremental"' <<<"$m" | awk '{print $2}')" \
      || record PATH FAIL "no incremental build recorded"
  else
    grep -q 'peers="incremental"' <<<"$m" && record PATH FAIL "incremental build with the switch off" \
      || record PATH PASS "no incremental build (switch off)"
  fi
  grep -q 'diverged' "$ART_DIR/logs/server.log" && record DIVERGE FAIL "server logged a divergence" \
    || record DIVERGE PASS "no divergence logged"
}

# ---------------------------------------------------------------- main ---
T0=$(date +%s)
log "P00d M06 $RUN_ID at $SHA -> $URL incremental=$INCREMENTAL rounds=$ROUNDS bound=${BOUND_S}s (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "image: $IMAGE"; echo "commit: $SHA"; echo "incremental: $INCREMENTAL"; echo "bin_sha256: $(sha256sum "$BIN" | cut -c1-16)"; } >>"$ART_DIR/env.txt"
run_rounds
check_server
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
