#!/usr/bin/env bash
# Two-cluster real-client E2E (lab stage 2a): cluster c1 on mc-cp and cluster
# c2 on mc-c2 share one PostgreSQL database (via PgBouncer on mc-db), each
# with its own Noise key and its own public hostname:
#   c1: https://c1.cp.gta5web.online:8443  (VPS -> wg-mesh -> mc-cp)
#   c2: https://c2.cp.gta5web.online:8445  (VPS -> wg-mesh -> mc-cp relay -> mc-c2)
# Bootstrap (P11) runs on mc-cp and is reached at https://boot.cp.gta5web.online:8444.
#
# Tailnets: a and c owned by c1 (mc-cp-c1), b owned by c2 (mc-c2).
# Cases (IDs in evidence/E2E-two-cluster.md):
#   N01 two clusters, two Noise keys, each listener serves only its hostname
#   N02 3 tailnets / 2 clusters: peers, per-tailnet IPAM, data plane
#   N03 F07 register: a key presented to the cluster that does not own its
#       tailnet is refused, no node anywhere
#   N04 F07 map: a registered node of a, retargeted to c2 (same keys), gets no
#       netmap, no node is created, a is unchanged
#   N05 F06 bootstrap with two clusters: each key gets its owner's URL; the
#       returned URL enrolls a real client into the right cluster
#   N06 restart of c2 while c1 serves: c1 clients unaffected, c2 clients back
#       without re-login
#   N07 move a from c1 to c2: c1 retires a and refuses it, c2 loads it,
#       bootstrap answers c2; clients re-login at c2; b and c unaffected
#   N08 stale client: a node still pointed at c1 after the move gets no update
#   N09 artifacts hold no secret
#
# The run gets its own database e2e_n2c<run> on mc-db (dropped at cleanup).
# mc-c2 is switched to it for the run and restored to `meshlab` afterwards;
# mc-cp runs the cluster instead of the P02 POC and restores the POC.
#
# Usage: BIN=headscale-cluster BOOT_BIN=mesh-bootstrap run.sh   (linux/amd64)
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
C2_SSH=${C2_SSH:-mesh@192.168.1.244}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
PG_SSH=${PG_SSH:-mesh@192.168.1.243}
PG_LISTEN=${PG_LISTEN:-192.168.1.243:6432}
MC_DB_ENV=${MC_DB_ENV:-$HOME/.mesh-lab/mc-db.env}
C1_ID=mc-cp-c1
C2_ID=mc-c2
C1_URL=https://c1.cp.gta5web.online:8443
C2_URL=https://c2.cp.gta5web.online:8445
POC_HEALTH=https://a.cp.gta5web.online:8443/health
BOOT_HOST=boot.cp.gta5web.online
BOOT_URL=https://$BOOT_HOST:8444
BOOT_LISTEN=10.99.77.2:8444
IMAGE=${IMAGE:-tailscale/tailscale:v1.104.7}
BIN=${BIN:?BIN= linux/amd64 cluster binary}
BOOT_BIN=${BOOT_BIN:?BOOT_BIN= linux/amd64 mesh-bootstrap binary}
KEEP=${KEEP:-0}
LABEL=mesh-e2e=n2c
PY=${PY:-python}

REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_n2c$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/two-cluster/e2e-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

CFG=/etc/headscale/cluster/config.yaml
HSC1="sudo -u headscale /usr/local/bin/headscale-cluster -c $CFG cluster --cluster-id $C1_ID"
HSC2="sudo -u headscale /usr/local/bin/headscale-cluster -c $CFG cluster --cluster-id $C2_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
ssh_() { local h=$1; shift; ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
c2_() { ssh_ "$C2_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }
pg_() { ssh_ "$PG_SSH" "$@"; }
# Python on this host: LF output only (a CR once made a check pass by mistake).
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }

FAILS=0
record() { # ID STATUS REASON
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-4s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------- clients ---
ts_run_args() { echo "--name $1 --hostname $1 --label $LABEL --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
  --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL"; }
wait_daemon() { ci_ "for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done; exit 0"; }
start_ts() { ci_ "docker run -d $(ts_run_args "$1") $IMAGE tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null" && wait_daemon "$1"; }
# ts_up NAME URL KEYFILE HOSTNAME [EXTRA] -> tailscale exit code
ts_up() {
  ci_ "docker exec -i $1 timeout 45 tailscale up --login-server=$2 --auth-key=file:/dev/stdin --hostname=$4 ${5:-}" \
    <"$3" 2>&1 | redact | tee "$TMPD/last.out" >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
peers() { field "$1" "' '.join(sorted(p['HostName'] for p in (d.get('Peer') or {}).values()))"; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
state() { field "$1" "d['BackendState']"; }
nodekey() { field "$1" "d['Self']['PublicKey'][:16]"; }
wait_state() { local i; for i in $(seq 1 "$3"); do [ "$(state "$1")" = "$2" ] && return 0; sleep 1; done; return 1; }
wait_peers() { # NAME "SORTED PEERS" TIMEOUT
  local i; for i in $(seq 1 "$3"); do [ "$(peers "$1")" = "$2" ] && return 0; sleep 1; done; return 1
}
ping_ok() { ci_ "docker exec $1 tailscale ping -c 3 --timeout 5s $2" >>"$ART_DIR/logs/$1-ping.log" 2>&1; }
save_logs() { ci_ "docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }
drop_ts() { ci_ "docker rm -f $1 >/dev/null 2>&1; docker volume rm $1 >/dev/null 2>&1; true"; }

# Operator view. Tenant commands are fenced to the owner cluster's CLI
# ("tailnet is owned by another cluster"), so each runs where its tailnet lives.
OWNER_a=c1; OWNER_b=c2; OWNER_c=c1
owner_cli() { # SLUG ARGS...: "cluster tailnet ARGS --slug SLUG" on the owner's host
  local s=$1 o; shift
  o=$(eval "echo \$OWNER_$s")
  if [ "$o" = c1 ]; then cp_ "$HSC1 tailnet $* --slug $s"; else c2_ "$HSC2 tailnet $* --slug $s"; fi
}
nodes_json() { owner_cli "$1" nodes 2>/dev/null; }
node_count() { nodes_json "$1" | py "import json; print(len(json.load(sys.stdin)))" | strip_cr; }
node_field() { # SLUG HOSTNAME FIELD
  nodes_json "$1" | py "import json
for n in json.load(sys.stdin):
    if n['hostname']==sys.argv[1]: print(n[sys.argv[2]])" "$2" "$3" | strip_cr
}
counts() { echo "a=$(node_count a) b=$(node_count b) c=$(node_count c)"; }
# log_has c1|c2 PATTERN SINCE_S
log_has() {
  local out; out=$(ssh_ "$([ "$1" = c1 ] && echo "$CP_SSH" || echo "$C2_SSH")" \
    "sudo journalctl -u headscale-cluster --since -${3}s --no-pager" 2>/dev/null)
  grep -q "$2" <<<"$out"
}
health() { curl -s -o /dev/null -w '%{http_code}' -m 5 "$1/health" || true; }
noise_key() { curl -s -m 5 "$1/key?v=116" | py "import json; print(json.load(sys.stdin).get('publicKey',''))" 2>/dev/null | strip_cr; }

# boot_req KEYFILE [TENANT]: POST /v1/bootstrap with the key's id and SHA-256
# proof (the secret never leaves this host); prints "<status> <body>".
boot_req() {
  local body out=$TMPD/boot.out code
  body=$(py 'import hashlib,json
k=open(sys.argv[1]).read().strip()
i,s=k.split("hskey-auth-",1)[1].split("-",1)
d={"credential":{"kind":"authkey_proof","key_id":i,"proof":hashlib.sha256(s.encode()).hexdigest()}}
if sys.argv[2]: d["tenant"]=sys.argv[2]
sys.stdout.write(json.dumps(d))' "$1" "${2:-}" | strip_cr)
  code=$(printf %s "$body" | curl -s -m 10 -o "$out" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary @- "$BOOT_URL/v1/bootstrap")
  printf '%s %s' "$code" "$(cat "$out" 2>/dev/null)"
  rm -f "$out"
}
boot_url() { py 'import json; d=json.loads(sys.argv[1].split(" ",1)[1]); print(d.get("control_url",""))' "$1" 2>/dev/null | strip_cr; }

# ------------------------------------------------------------- cleanup ---
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; rm -f ~/n2c-retarget.py; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\"" || { log "clients not removed"; CLEAN_OK=0; }
  cp_ "sudo systemctl stop mesh-bootstrap-e2e headscale-cluster 2>/dev/null; sudo rm -f /usr/local/bin/mesh-bootstrap /etc/headscale/cluster/pg.pass; \
       ! sudo test -e /etc/headscale/cluster/pg.pass && sudo systemctl start headscale-tailnets" \
    || { log "mc-cp not restored"; CLEAN_OK=0; }
  # mc-c2 back on the long-lived meshlab database.
  c2_ "set -e; sudo systemctl stop headscale-cluster
       if sudo test -e $CFG.n2c-saved; then sudo mv $CFG.n2c-saved $CFG; fi
       sudo grep -q '^    name: meshlab$' $CFG
       sudo systemctl start headscale-cluster" || { log "mc-c2 config not restored"; CLEAN_OK=0; }
  [[ $PG_DB =~ ^e2e_[0-9a-z]+$ ]] && pg_ "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $PG_DB && \
    ! sudo docker exec mc-pg psql -At -U postgres -c \"SELECT 1 FROM pg_database WHERE datname = '$PG_DB'\" | grep -q 1" \
    && log "dropped database $PG_DB" || { log "database $PG_DB not dropped"; CLEAN_OK=0; }
  local i p=000 q=000
  for i in $(seq 1 30); do
    p=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$POC_HEALTH" || true); q=$(health "$C2_URL")
    [ "$p" = 200 ] && [ "$q" = 200 ] && break; sleep 1
  done
  [ "$p" = 200 ] && log "P02 POC restored on mc-cp" || { log "POC not healthy ($p)"; CLEAN_OK=0; }
  [ "$q" = 200 ] && c2_ "sudo journalctl -u headscale-cluster --since -60s --no-pager" | grep -q 'meshlab' \
    && log "c2 back on meshlab (/health 200)" || { log "c2 not healthy on meshlab ($q)"; CLEAN_OK=0; }
  rm -rf "$TMPD"
}
trap 'cleanup; exit 2' INT TERM

# --------------------------------------------------------------- setup ---
setup() {
  log "database $PG_DB on mc-db"
  local pass
  pass=$(grep -E '^MC_DB_APP_PASSWORD=' "$MC_DB_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || return 1
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"
  pg_ "sudo docker exec mc-pg createdb -U postgres -O headscale $PG_DB" || return 1

  log "c1 on mc-cp (binary, config on $PG_DB, unit)"
  awk -v host="${PG_LISTEN%:*}" -v port="${PG_LISTEN##*:}" -v db="$PG_DB" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    host: " host "\n    port: " port \
      "\n    name: " db "\n    user: headscale\n    pass: file:/etc/headscale/cluster/pg.pass\n    ssl: disable"; skip = 1; next }
    skip && /^[^ ]/ { skip = 0 }
    !skip { print }' "$REPO/infra/lab/mc-cp/cluster/config.yaml" >"$TMPD/c1.yaml"
  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$BIN" "$CP_SSH:/tmp/n2c-hs" || return 1
  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$TMPD/c1.yaml" "$REPO/infra/lab/mc-cp/headscale-cluster.service" "$CP_SSH:/tmp/" || return 1
  cp_ "sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  cp_ "set -e
    sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null || true
    sudo install -m 0755 /tmp/n2c-hs /usr/local/bin/headscale-cluster
    sudo install -o root -g headscale -m 0640 /tmp/c1.yaml $CFG
    sudo install -m 0644 /tmp/headscale-cluster.service /etc/systemd/system/headscale-cluster.service
    sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale/cluster /var/lib/headscale/cluster-e2e
    sudo systemctl daemon-reload; rm -f /tmp/n2c-hs /tmp/c1.yaml /tmp/headscale-cluster.service" || return 1

  log "c2 on mc-c2 (binary, config switched to $PG_DB)"
  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$BIN" "$C2_SSH:/tmp/n2c-hs" || return 1
  c2_ "set -e
    sudo systemctl stop headscale-cluster
    sudo install -m 0755 /tmp/n2c-hs /usr/local/bin/headscale-cluster; rm -f /tmp/n2c-hs
    sudo test -e $CFG.n2c-saved || sudo cp -p $CFG $CFG.n2c-saved
    sudo sed -i 's/^    name: .*/    name: $PG_DB/' $CFG
    sudo grep -q '^    name: $PG_DB\$' $CFG" || return 1

  # P13d (ADR 0013): cluster processes, mesh-placement and the admin CLI never
  # migrate; the operator step creates the schema of the fresh database.
  log "schema: migrate step"
  cp_ "$HSC1 migrate" >"$ART_DIR/logs/migrate.json" || { log "migrate step failed"; return 1; }
  # P12 (owner decision 2026-11-25): tailnets are created only on a
  # registered ACTIVE cluster whose process holds its lease: register and
  # start both clusters, then create the tailnets live.
  log "registry: register both clusters"
  cp_ "$HSC1 register --class shared --max-devices 5000 >/dev/null" || return 1
  c2_ "$HSC2 register --class shared --max-devices 5000 >/dev/null" || return 1

  local since1 since2
  since1=$(cp_ 'date +%s') && since2=$(c2_ 'date +%s') || return 1
  log "starting both clusters"
  cp_ "sudo systemctl start headscale-cluster" && c2_ "sudo systemctl start headscale-cluster" || return 1
  local i h1=000 h2=000
  for i in $(seq 1 40); do h1=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$C1_URL/ready" || true); h2=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$C2_URL/ready" || true); [ "$h1$h2" = 200200 ] && break; sleep 1; done
  cp_ "sudo journalctl -u headscale-cluster -b --no-pager -n 30" 2>&1 | redact >"$ART_DIR/logs/c1-start.log"
  c2_ "sudo journalctl -u headscale-cluster --since -120s --no-pager" 2>&1 | redact >"$ART_DIR/logs/c2-start.log"
  [ "$h1$h2" = 200200 ] || { log "clusters not healthy (c1=$h1 c2=$h2)"; return 1; }

  log "tailnets a, c (c1) and b (c2), created while the clusters run"
  local s pf1=/var/lib/headscale/cluster-e2e/n2c-policy.hujson pf2=/var/lib/headscale/cluster/n2c-policy.hujson
  local pol='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}'
  for s in a c; do cp_ "$HSC1 tailnet create --slug $s >/dev/null && $HSC1 tailnet status --slug $s --set ACTIVE" || return 1; done
  c2_ "$HSC2 tailnet create --slug b >/dev/null && $HSC2 tailnet status --slug b --set ACTIVE" || return 1
  printf '%s\n' "$pol" | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf1 && $HSC1 tailnet policy --slug a --file $pf1 && $HSC1 tailnet policy --slug c --file $pf1; rc=\$?; sudo rm -f $pf1; exit \$rc" || return 1
  printf '%s\n' "$pol" | c2_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf2 && $HSC2 tailnet policy --slug b --file $pf2; rc=\$?; sudo rm -f $pf2; exit \$rc" || return 1
  for s in a b c; do
    owner_cli $s authkey --user e2e-$s --reusable --expiration 90m \
      | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s" || return 1
    chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  done

  local ok=0 j1 j2
  for i in $(seq 1 30); do
    j1=$(cp_ "sudo journalctl -u headscale-cluster --since @$since1 --no-pager" 2>/dev/null | grep 'loaded tailnet runtime')
    j2=$(c2_ "sudo journalctl -u headscale-cluster --since @$since2 --no-pager" 2>/dev/null | grep 'loaded tailnet runtime')
    grep -q 'slug=a' <<<"$j1" && grep -q 'slug=c' <<<"$j1" && grep -q 'slug=b' <<<"$j2" && { ok=1; break; }
    sleep 1
  done
  [ $ok = 1 ] || { log "tailnets not loaded (c1: a, c; c2: b)"; return 1; }

  log "bootstrap on mc-cp ($BOOT_URL)"
  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$BOOT_BIN" "$CP_SSH:/tmp/mesh-bootstrap" || return 1
  cp_ "set -e; sudo install -m 0755 /tmp/mesh-bootstrap /usr/local/bin/mesh-bootstrap; rm -f /tmp/mesh-bootstrap
    sudo systemd-run --quiet --unit=mesh-bootstrap-e2e --uid=headscale --gid=headscale \
      /usr/local/bin/mesh-bootstrap -config $CFG -listen $BOOT_LISTEN \
      -tls-cert /etc/headscale/tls/fullchain.pem -tls-key /etc/headscale/tls/privkey.pem -hostname $BOOT_HOST" || return 1
  for i in $(seq 1 20); do [ "$(health "$BOOT_URL")" = 200 ] && break; sleep 1; done
  [ "$(health "$BOOT_URL")" = 200 ] || { log "bootstrap not healthy"; return 1; }

  scp -q -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "$REPO/infra/lab/e2e/twocluster/retarget-state.py" "$CI_SSH:n2c-retarget.py" || return 1
  ci_ "docker image inspect $IMAGE >/dev/null 2>&1 || docker pull -q $IMAGE >/dev/null" || return 1
}

# --------------------------------------------------------------- cases ---
t_n01() {
  local k1 k2 problems=""
  k1=$(noise_key "$C1_URL"); k2=$(noise_key "$C2_URL")
  printf 'c1 %s\nc2 %s\n' "${k1:0:13}" "${k2:0:13}" >"$ART_DIR/logs/noise-keys.txt"
  { [ -n "$k1" ] && [ -n "$k2" ] && [ "$k1" != "$k2" ]; } || problems="$problems; noise keys c1=${k1:0:13} c2=${k2:0:13}"
  local c
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H "Host: c1.cp.gta5web.online" "$C2_URL/health"); [ "$c" = 421 ] || problems="$problems; c2 answered c1's Host ($c)"
  c=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H "Host: c2.cp.gta5web.online" "$C1_URL/health"); [ "$c" = 421 ] || problems="$problems; c1 answered c2's Host ($c)"
  for u in "$C1_URL" "$C2_URL"; do
    c=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$u/metrics"); [ "$c" = 404 ] || problems="$problems; $u/metrics=$c"
  done
  [ -z "$problems" ] && record N01 PASS "two clusters, two Noise keys (${k1:5:8}… / ${k2:5:8}…); each listener 421s the other's Host; no /metrics" \
    || record N01 FAIL "${problems#; }"
}

t_n02() {
  local r problems=""
  start_ts n2-a1 && start_ts n2-a2 && start_ts n2-b1 && start_ts n2-b2 && start_ts n2-c1 || { record N02 FAIL "client setup"; return; }
  for x in "n2-a1 $C1_URL a" "n2-a2 $C1_URL a" "n2-b1 $C2_URL b" "n2-b2 $C2_URL b" "n2-c1 $C1_URL c"; do
    set -- $x; r=0; ts_up "$1" "$2" "$TMPD/key-$3" "$1" || r=$?
    [ $r = 0 ] || problems="$problems; $1 up rc=$r"
  done
  wait_peers n2-a1 n2-a2 30; wait_peers n2-b1 n2-b2 30; sleep 5
  local pa1 pa2 pb1 pb2 pc1
  pa1=$(peers n2-a1); pa2=$(peers n2-a2); pb1=$(peers n2-b1); pb2=$(peers n2-b2); pc1=$(peers n2-c1)
  printf 'a1=[%s] a2=[%s] b1=[%s] b2=[%s] c1=[%s]\n' "$pa1" "$pa2" "$pb1" "$pb2" "$pc1" >"$ART_DIR/logs/N02-peers.txt"
  [ "$pa1" = n2-a2 ] && [ "$pa2" = n2-a1 ] || problems="$problems; a peers a1=[$pa1] a2=[$pa2]"
  [ "$pb1" = n2-b2 ] && [ "$pb2" = n2-b1 ] || problems="$problems; b peers b1=[$pb1] b2=[$pb2]"
  [ -z "$pc1" ] || problems="$problems; c1 sees [$pc1]"
  local ia ib ic; ia=$(selfip n2-a1); ib=$(selfip n2-b1); ic=$(selfip n2-c1)
  { [ -n "$ia" ] && [ "$ia" = "$ib" ] && [ "$ia" = "$ic" ]; } || problems="$problems; first IPs a=$ia b=$ib c=$ic"
  ping_ok n2-a1 n2-a2 || problems="$problems; a1 cannot ping a2"
  ping_ok n2-b1 n2-b2 || problems="$problems; b1 cannot ping b2"
  [ "$(counts)" = "a=2 b=2 c=1" ] || problems="$problems; nodes $(counts)"
  [ -z "$problems" ] && record N02 PASS "a (c1): a1<->a2; b (c2): b1<->b2; c (c1): alone; first IP $ia in each tailnet; pings ok in a and b" \
    || record N02 FAIL "${problems#; }"
}

t_n03() {
  local problems="" before after r x
  before=$(counts)
  for x in "n2-x1 $C1_URL b" "n2-x2 $C2_URL a" "n2-x3 $C2_URL c"; do
    set -- $x; start_ts "$1" >/dev/null; r=0; ts_up "$1" "$2" "$TMPD/key-$3" "$1" || r=$?
    { [ $r != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out"; } || problems="$problems; key of $3 at $2: rc=$r [$(tr '\n' ' ' <"$TMPD/last.out" | cut -c1-120)]"
    save_logs "$1"; drop_ts "$1"
  done
  after=$(counts)
  [ "$before" = "$after" ] || problems="$problems; nodes $before -> $after"
  [ -z "$problems" ] && record N03 PASS "key of b at c1, keys of a and c at c2: 'invalid pre auth key', no node created ($after)" \
    || record N03 FAIL "${problems#; }"
}

t_n04() {
  local problems="" before after n nk_before nk_after
  before=$(counts); nk_before=$(node_field a n2-a1 node_key)
  ci_ "docker create $(ts_run_args n2-xa) $IMAGE tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null" || { record N04 FAIL "create"; return; }
  n=$(ci_ "set -e; umask 077; f=\$(mktemp); docker cp n2-a1:/var/lib/tailscale/tailscaled.state \$f; \
    python3 ~/n2c-retarget.py \$f $C1_URL $C2_URL; docker cp \$f n2-xa:/var/lib/tailscale/tailscaled.state; rm -f \$f" 2>/dev/null | tail -1)
  [ "${n:-0}" -ge 1 ] 2>/dev/null || { record N04 FAIL "state not retargeted ($n)"; drop_ts n2-xa; return; }
  ci_ "docker start n2-xa >/dev/null" && wait_daemon n2-xa
  sleep 25
  local st pe
  st=$(state n2-xa); pe=$(peers n2-xa)
  ci_ "docker exec n2-xa tailscale status 2>&1" | redact >"$ART_DIR/logs/N04-status.txt"
  after=$(counts); nk_after=$(node_field a n2-a1 node_key)
  [ "$st" != Running ] || problems="$problems; state Running at c2"
  [ -z "$pe" ] || problems="$problems; peers at c2 [$pe]"
  [ "$before" = "$after" ] || problems="$problems; nodes $before -> $after"
  [ "$nk_before" = "$nk_after" ] || problems="$problems; a1 node key changed in DB"
  grep -q '/register/' "$ART_DIR/logs/N04-status.txt" && problems="$problems; c2 handed out an auth URL"
  wait_peers n2-a1 n2-a2 5 || problems="$problems; a1 lost a2 ($(peers n2-a1))"
  save_logs n2-xa; drop_ts n2-xa
  [ -z "$problems" ] && record N04 PASS "registered node of a retargeted to c2 (same machine/node keys): state=$st, no peers, no node created ($after), a unchanged" \
    || record N04 FAIL "${problems#; }"
}

t_n05() {
  local problems="" r u x
  for x in "a $C1_URL" "b $C2_URL" "c $C1_URL"; do
    set -- $x; r=$(boot_req "$TMPD/key-$1" "$1"); printf 'N05 %s %s\n' "$1" "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
    u=$(boot_url "$r"); [ "${r%% *}" = 200 ] && [ "$u" = "$2" ] || problems="$problems; key of $1 -> [$r]"
  done
  r=$(boot_req "$TMPD/key-a" b); printf 'N05 a-as-b %s\n' "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
  [ "${r%% *}" = 404 ] || problems="$problems; key of a naming b -> [$r]"
  r=$(boot_req "$TMPD/key-b" b); u=$(boot_url "$r")
  start_ts n2-b3 >/dev/null; local rc=0; ts_up n2-b3 "$u" "$TMPD/key-b" n2-b3 || rc=$?
  { [ $rc = 0 ] && wait_peers n2-b3 "n2-b1 n2-b2" 30 && [ -n "$(node_field b n2-b3 id)" ]; } || problems="$problems; enroll via $u rc=$rc peers=[$(peers n2-b3)]"
  [ -z "$problems" ] && record N05 PASS "bootstrap: a, c -> $C1_URL; b -> $C2_URL; key of a naming b 404; b3 enrolled via the returned URL into b (sees b1 b2)" \
    || record N05 FAIL "${problems#; }"
}

t_n06() {
  local problems="" nk ib nb t0
  nk=$(nodekey n2-b1); ib=$(selfip n2-b1); nb=$(node_count b)
  c2_ "sudo systemctl stop headscale-cluster" || { record N06 FAIL "stop c2"; return; }
  t0=$(date +%s); sleep 5
  [ "$(health "$C2_URL")" != 200 ] || problems="$problems; c2 still healthy while stopped"
  [ "$(state n2-a1)" = Running ] || problems="$problems; a1 state $(state n2-a1) while c2 down"
  ping_ok n2-a1 n2-a2 || problems="$problems; a1 cannot ping a2 while c2 down"
  start_ts n2-c2 >/dev/null; local r=0; ts_up n2-c2 "$C1_URL" "$TMPD/key-c" n2-c2 || r=$?
  { [ $r = 0 ] && wait_peers n2-c1 n2-c2 30; } || problems="$problems; c1 did not enroll c2 while c2 cluster down (rc=$r)"
  c2_ "sudo systemctl start headscale-cluster" || problems="$problems; start c2"
  local i; for i in $(seq 1 30); do [ "$(health "$C2_URL")" = 200 ] && break; sleep 1; done
  wait_state n2-b1 Running 60 && wait_peers n2-b1 "n2-b2 n2-b3" 60 || problems="$problems; b1 not back (state=$(state n2-b1) peers=[$(peers n2-b1)])"
  [ "$(nodekey n2-b1)" = "$nk" ] && [ "$(selfip n2-b1)" = "$ib" ] || problems="$problems; b1 key/IP changed"
  [ "$(node_count b)" = "$nb" ] || problems="$problems; b nodes $nb -> $(node_count b)"
  ping_ok n2-b1 n2-b2 || problems="$problems; b1 cannot ping b2 after restart"
  [ -z "$problems" ] && record N06 PASS "c2 down: c1 kept serving (a1 Running, a1->a2 ping, new node c2 enrolled at c1); c2 back: b1 same key/IP, no re-login, sees b2 b3 ($(( $(date +%s) - t0 ))s)" \
    || record N06 FAIL "${problems#; }"
}

t_n07_n08() {
  local problems="" stale="" t0 r u id1_before ip1_before na_before
  id1_before=$(node_field a n2-a1 id); ip1_before=$(selfip n2-a1); na_before=$(node_count a)
  local pb1; pb1=$(peers n2-b1); local pc; pc=$(peers n2-c1)
  t0=$(date +%s)
  owner_cli a reassign --to $C2_ID >"$ART_DIR/logs/N07-reassign.json" 2>&1 || { record N07 FAIL "reassign"; record N08 FAIL "not run"; return; }
  OWNER_a=c2
  local i ok1=0 ok2=0
  for i in $(seq 1 20); do
    log_has c1 'retiring tailnet runtime' 60 && ok1=1
    log_has c2 'slug=a' 60 && log_has c2 'loaded tailnet runtime' 60 && ok2=1
    [ $ok1$ok2 = 11 ] && break; sleep 1
  done
  [ $ok1 = 1 ] || problems="$problems; c1 did not retire a"
  [ $ok2 = 1 ] || problems="$problems; c2 did not load a"
  # c1 refuses a; bootstrap now answers c2.
  start_ts n2-y1 >/dev/null; r=0; ts_up n2-y1 "$C1_URL" "$TMPD/key-a" n2-y1 || r=$?
  { [ $r != 0 ] && grep -q 'invalid pre auth key' "$TMPD/last.out"; } || problems="$problems; c1 still accepts a (rc=$r)"
  save_logs n2-y1; drop_ts n2-y1
  r=$(boot_req "$TMPD/key-a" a); printf 'N07 a %s\n' "$r" >>"$ART_DIR/logs/bootstrap-responses.txt"
  u=$(boot_url "$r"); [ "$u" = "$C2_URL" ] || problems="$problems; bootstrap for a -> [$r]"
  # New node of a at c2.
  start_ts n2-a3 >/dev/null; r=0; ts_up n2-a3 "$C2_URL" "$TMPD/key-a" n2-a3 || r=$?
  [ $r = 0 ] || problems="$problems; a3 at c2 rc=$r"
  # N08: a2, still pointed at c1, must not learn about a3.
  sleep 15
  case " $(peers n2-a2) " in *" n2-a3 "*) stale="a2 (on c1) learned a3 after the move" ;; esac
  printf 'a2 peers after move (pointed at c1): [%s] state=%s\n' "$(peers n2-a2)" "$(state n2-a2)" >"$ART_DIR/logs/N08-a2.txt"
  # Re-login of a1 and a2 at c2 (ADR 0005: clients of a moved tailnet log in again).
  for n in n2-a1 n2-a2; do r=0; ts_up "$n" "$C2_URL" "$TMPD/key-a" "$n" --force-reauth || r=$?; [ $r = 0 ] || problems="$problems; $n re-login rc=$r"; done
  wait_peers n2-a1 "n2-a2 n2-a3" 45 || problems="$problems; a1 at c2 peers=[$(peers n2-a1)]"
  wait_peers n2-a2 "n2-a1 n2-a3" 30 || problems="$problems; a2 at c2 peers=[$(peers n2-a2)]"
  ping_ok n2-a1 n2-a3 || problems="$problems; a1 cannot ping a3"
  local id1_after ip1_after; id1_after=$(node_field a n2-a1 id); ip1_after=$(selfip n2-a1)
  printf 'a1 node id %s -> %s, IP %s -> %s; a nodes %s -> %s\n' "$id1_before" "$id1_after" "$ip1_before" "$ip1_after" "$na_before" "$(node_count a)" >"$ART_DIR/logs/N07-relogin.txt"
  # b (c2) and c (c1) unaffected.
  [ "$(peers n2-b1)" = "$pb1" ] || problems="$problems; b1 peers [$pb1] -> [$(peers n2-b1)]"
  local pa_leak; pa_leak=$(peers n2-b1; peers n2-c1; peers n2-c2)
  grep -qE 'n2-a[0-9]' <<<"$pa_leak" && problems="$problems; a host seen from b or c"
  [ "$(state n2-c1)" = Running ] && [ "$(peers n2-c1)" = n2-c2 ] || problems="$problems; c1 client state=$(state n2-c1) peers=[$(peers n2-c1)]"
  [ -z "$problems" ] && record N07 PASS "a moved c1 -> c2: c1 retired and refuses a, c2 loaded it, bootstrap -> $C2_URL; a1 a2 re-logged at c2 and see a3 (a1 id $id1_before->$id1_after, IP $ip1_before->$ip1_after); b and c unaffected ($(( $(date +%s) - t0 ))s)" \
    || record N07 FAIL "${problems#; }"
  [ -z "$stale" ] && record N08 PASS "a2 still pointed at c1 after the move got no update (did not learn a3 in 15s)" \
    || record N08 FAIL "$stale"
}

t_n09() {
  local hits
  hits=$(grep -rlE 'hskey-auth-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
  [ -z "$hits" ] && record N09 PASS "no auth key, private key or full machine/node key in the artifacts" \
    || record N09 FAIL "unredacted in: $(echo "$hits" | tr '\n' ' ')"
}

# ---------------------------------------------------------------- main ---
T0=$(date +%s)
log "two-cluster E2E $RUN_ID at $SHA: c1 $C1_URL, c2 $C2_URL (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "image: $IMAGE"; echo "commit: $SHA"; echo "database: $PG_DB (mc-db via PgBouncer $PG_LISTEN)"
  echo "cluster binary: $(sha256sum "$BIN" | cut -c1-16)"; echo "bootstrap binary: $(sha256sum "$BOOT_BIN" | cut -c1-16)"; } >>"$ART_DIR/env.txt"
t_n01
t_n02
t_n03
t_n04
t_n05
t_n06
t_n07_n08
for c in n2-a1 n2-a2 n2-a3 n2-b1 n2-b2 n2-b3 n2-c1 n2-c2; do save_logs $c; done
cp_ "sudo journalctl -u headscale-cluster --since -$(( $(date +%s) - T0 + 5 ))s --no-pager" 2>&1 | redact >"$ART_DIR/logs/c1.log"
c2_ "sudo journalctl -u headscale-cluster --since -$(( $(date +%s) - T0 + 5 ))s --no-pager" 2>&1 | redact >"$ART_DIR/logs/c2.log"
cp_ "sudo journalctl -u mesh-bootstrap-e2e --no-pager" 2>&1 | redact >"$ART_DIR/logs/bootstrap.log"
t_n09
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
