#!/usr/bin/env bash
# P02 real-client E2E: one Noise key per tailnet, tailnet chosen by Host.
#
# Asserts the TARGET behaviour of P02. Against today's single-tailnet baseline
# most tests are expected to FAIL (RED). See README.md in this directory.
#
# Runs from an operator workstation (Linux, WSL or Git Bash) that can reach:
#   - the public control URL over HTTPS (curl),
#   - mc-cp over SSH (headscale CLI: pre-auth keys, node list/delete),
#   - mc-ci over SSH (Docker with real tailscale/tailscale images).
#
# Usage: run.sh [--fast]
#   --fast   only the HTTP/TLS tests K01-K09 (curl; no Tailscale client,
#            no mc-ci, nothing created on mc-cp)
#   default  everything
#   ONLY="K01 R02" restricts either mode to the listed IDs.
#   JOBS=N   max tests running at once (default 4).
#
# Independent tests run concurrently. Every client scenario uses its own
# uniquely named p02-* containers, volumes, nodes and keys, so they share no
# state. Each test writes its own redacted log under logs/; the summary,
# results.tsv and results.json keep a fixed order.
#
# Secrets: pre-auth keys are created with a short expiry, stored only in a
# mode-700 temp dir (files mode 600), piped to `tailscale up --auth-key=file:/dev/stdin`
# and never printed. Logs written to the artifact dir are redacted.
#
# Exit code: 0 if no FAIL, 1 if any FAIL, 2 on harness/setup error or if
# cleanup cannot prove that everything this run created is gone.
#
# shellcheck disable=SC2015,SC2018,SC2019,SC2029
# (info only: `a && b || c` is used where c is the intended fallback; tr a-z
# on ASCII hostnames; remote commands are built locally on purpose.)

set -u -o pipefail

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }
MODE=full
for arg in "$@"; do
  case $arg in
    --fast) MODE=fast ;;
    -h | --help) usage; exit 0 ;;
    *) echo "unknown argument: $arg (see --help)" >&2; exit 2 ;;
  esac
done

# ----------------------------------------------------------------- config ---
SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
BASE_DOMAIN=${BASE_DOMAIN:-cp.gta5web.online}
PORT=${PORT:-8443}
HOST_A=${HOST_A:-a.$BASE_DOMAIN}
HOST_B=${HOST_B:-b.$BASE_DOMAIN}
HOST_UNKNOWN=${HOST_UNKNOWN:-zz.$BASE_DOMAIN}
PUBLIC_IP=${PUBLIC_IP:-116.118.3.46}
CAPVER=${CAPVER:-116}
IMAGE_REPO=${IMAGE_REPO:-tailscale/tailscale}
IMAGE_NEW=${IMAGE_NEW:-v1.104.7}
IMAGE_MIN=${IMAGE_MIN:-v1.84.3}
HEADSCALE=${HEADSCALE:-sudo -u headscale /usr/local/bin/headscale}
# Admin CLI per tailnet. With one runtime per tailnet (P02 serve-tailnets) each
# has its own database, so node and key IDs are only unique per tailnet; the
# harness tags them "A:<id>" / "B:<id>". Equal values = single-tailnet baseline.
HS_A=${HS_A:-$HEADSCALE}
HS_B=${HS_B:-$HEADSCALE}
# Commands that mint a pre-auth key for tailnet A / tailnet B (JSON on stdout).
# Baseline has one tailnet, so both default to the same user. Once P02/P03
# exist, point KEYCMD_B at tailnet B.
KEYCMD_A=${KEYCMD_A:-$HS_A preauthkeys create --user lab-test --expiration 30m -o json}
KEYCMD_B=${KEYCMD_B:-$HS_B preauthkeys create --user lab-test --expiration 30m -o json}
KEEP=${KEEP:-0}        # 1 = skip cleanup (debugging only)
ONLY=${ONLY:-}         # space-separated test IDs to run, empty = all
JOBS=${JOBS:-4}        # max concurrent tests
PY=${PY:-python3}
case $JOBS in '' | *[!0-9]* | 0) echo "JOBS must be a positive integer" >&2; exit 2 ;; esac
# R01 is a negative test (a leaked peer would show up asynchronously), so it
# keeps a fixed observation window after both clients are up. It stops early
# only if a leak is seen.
R01_OBSERVE_S=5
CLEANUP_RETRY_S=90     # cleanup keeps retrying stragglers (after an interrupt) this long

# Windows Python writes CRLF; strip CR so IDs are clean on Git Bash.
py() { "$PY" "$@" | tr -d '\r'; }

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || echo "$SCRIPT_DIR")
GITSHA=$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo nogit)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
ART_DIR=${ART_DIR:-$REPO_ROOT/artifacts/$GITSHA/P02/e2e-$RUN_ID}
LABEL=p02-e2e=$RUN_ID

URL_A="https://$HOST_A:$PORT"
URL_B="https://$HOST_B:$PORT"

FAST_IDS="K01 K02 K03 K04 K05 K06 K07 K08 K09"
want() {
  if [ "$MODE" = fast ]; then case " $FAST_IDS " in *" $1 "*) ;; *) return 1 ;; esac; fi
  [ -z "$ONLY" ] && return 0
  case " $ONLY " in *" $1 "*) return 0 ;; esac
  return 1
}
any_want() { local t; for t in "$@"; do want "$t" && return 0; done; return 1; }
NEED_NODES=0; any_want R01 R02 R03 && NEED_NODES=1   # creates nodes/keys on mc-cp
NEED_CI=0; any_want K10 R01 R02 R03 && NEED_CI=1     # uses Docker on mc-ci

umask 077
TMPD=$(mktemp -d)
chmod 700 "$TMPD"
# ControlMaster sockets: short path (sun_path limit), removed on exit.
CTL_DIR=$(mktemp -d "${TMPDIR:-/tmp}/p02ssh.XXXXXX")
chmod 700 "$CTL_DIR"
RES_DIR=$TMPD/res
mkdir -p "$ART_DIR/logs" "$RES_DIR"
: >"$TMPD/keyids"; : >"$TMPD/volumes"
exec 3>&1   # console; tests running in the background report progress here

# ------------------------------------------------------------------- ssh ---
SSHOPTS=(-o BatchMode=yes -o ConnectTimeout=10 -i "$SSH_KEY")
CP_MUX=(); CI_MUX=()
# cp_ssh never reads stdin (-n), so it is safe inside `while read` loops.
cp_ssh() { ssh -n "${SSHOPTS[@]}" "${CP_MUX[@]}" "$CP_SSH" "$@"; }
ci_ssh() { ssh "${SSHOPTS[@]}" "${CI_MUX[@]}" "$CI_SSH" "$@"; }

# mux_open HOST -> prints "on" after starting a ControlMaster that works.
# Git Bash's msys OpenSSH starts a master, but its sessions fail and fall back
# to a new connection, so a probe decides; "off" = plain connections.
mux_open() {
  local h=$1 path="$CTL_DIR/%C" err
  ssh "${SSHOPTS[@]}" -o ControlMaster=yes -o ControlPath="$path" -o ControlPersist=900 \
    -fN "$h" </dev/null >/dev/null 2>&1 3>&- || { echo off; return; }
  if err=$(ssh -n "${SSHOPTS[@]}" -o ControlMaster=no -o ControlPath="$path" "$h" true 2>&1) && [ -z "$err" ]; then
    echo on
  else
    ssh -o ControlPath="$path" -O exit "$h" >/dev/null 2>&1
    echo off
  fi
}
mux_close() {
  local h
  for h in "$CP_SSH" "$CI_SSH"; do
    ssh -o ControlPath="$CTL_DIR/%C" -O check "$h" >/dev/null 2>&1 &&
      ssh -o ControlPath="$CTL_DIR/%C" -O exit "$h" >/dev/null 2>&1
  done
  rm -rf "$CTL_DIR"
}

# ---------------------------------------------------------------- helpers ---
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
now() { local t=${EPOCHREALTIME:-}; if [ -n "$t" ]; then printf '%s' "${t/,/.}"; else date +%s; fi; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }
elapsed_ge() { awk -v e="$(since "$1")" -v t="$2" 'BEGIN { exit !(e >= t) }'; } # T0 SECONDS
T_START=$(now)
SLOT=00; JOB_T0=$T_START

# record ID MAP STATUS REASON  (appends to this test's result slot)
record() {
  local id=$1 map=$2 st=$3 reason=$4 dur
  dur=$(since "$JOB_T0")
  printf '%s\t%s\t%s\t%s\t%s\n' "$id" "$map" "$st" "$reason" "$dur" >>"$RES_DIR/$SLOT"
  printf '%-5s %-12s %-12s %6ss  %s\n' "$st" "$id" "$map" "$dur" "$reason" >&3
}

# Redact anything that might be a credential; shorten public keys.
redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g' \
    -e 's/\b([0-9a-f]{8})[0-9a-f]{56}\b/\1…/g'
}

short() { printf '%s' "${1:0:20}"; }

# poll TIMEOUT_S INTERVAL_S CMD... -> 0 as soon as CMD succeeds, 1 after TIMEOUT_S
poll() {
  local t=$1 iv=$2 t0; shift 2; t0=$(now)
  while :; do
    "$@" && return 0
    elapsed_ge "$t0" "$t" && return 1
    sleep "$iv"
  done
}

# ---- job runner: at most $JOBS tests at once, each in its own process group
JOB_PGIDS=()
# launch SLOT NAME FUNC [ARGS...]
launch() {
  local slot=$1 name=$2; shift 2
  while [ "$(jobs -rp | wc -l)" -ge "$JOBS" ]; do wait -n 2>/dev/null || true; done
  set -m   # own process group, so an interrupt can kill the whole test tree
  (
    set +m
    SLOT=$slot; JOB_T0=$(now)
    log "start $name"
    "$@"; rc=$?
    [ -s "$RES_DIR/$SLOT" ] || record "$name" "-" FAIL "harness: test ended (rc=$rc) without a result, see logs/$slot-$name.log"
    log "end $name rc=$rc ($(since "$JOB_T0")s)"
  ) </dev/null 2>&1 | redact >"$ART_DIR/logs/$slot-$name.log" &
  JOB_PGIDS+=("$(jobs -p %+)")
  set +m
}
stop_jobs() {
  local pg
  for pg in "${JOB_PGIDS[@]}"; do kill -TERM -- "-$pg" 2>/dev/null; done
  wait 2>/dev/null
}

# fetch_key URL [curl args...] -> sets FK_CODE, FK_KEY ("" if none)
fetch_key() {
  local url=$1; shift
  local out
  out=$(curl -sS -m 15 -w '\n%{http_code}' "$@" "$url" 2>/dev/null) || true
  FK_CODE=$(printf '%s' "$out" | tail -n1)
  FK_KEY=$(printf '%s' "$out" | sed '$d' | grep -oE '"publicKey":"mkey:[0-9a-f]{64}"' | cut -d'"' -f4 || true)
  [ -n "$FK_CODE" ] || FK_CODE=000
}

# refused CODE -> 0 for a fail-closed answer: HTTP >=400, or no HTTP answer at
# all (000, e.g. TLS refused). Valid because K01 first proves the path works.
refused() { [ "$1" = 000 ] || { [ "$1" -ge 400 ] 2>/dev/null && [ "$1" != 101 ]; }; }
# rejected CODE KEY -> refused and no key leaked
rejected() { refused "$1" && [ -z "$2" ]; }

# upgrade_code URL [curl args...] -> HTTP status of a ts2021 upgrade attempt
# (bogus handshake: a correct server must refuse BEFORE switching protocols
# when Host/SNI do not identify a tailnet).
upgrade_code() {
  local url=$1; shift
  curl -sS -m 10 -o /dev/null -w '%{http_code}' --http1.1 -X POST \
    -H 'Upgrade: tailscale-control-protocol' -H 'Connection: upgrade' \
    -H 'X-Tailscale-Handshake: AAAA' "$@" "$url" 2>/dev/null || true
}

ws_code() {
  local url=$1; shift
  curl -sS -m 10 -o /dev/null -w '%{http_code}' --http1.1 \
    -H 'Upgrade: websocket' -H 'Connection: Upgrade' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Sec-WebSocket-Protocol: ts2021' \
    "$@" "$url?X-Tailscale-Handshake=AAAA" 2>/dev/null || true
}

# ---- server side (read-only except own keys/nodes) ----
# nodes_json -> merged node list of tailnet A and B; "id" is tagged "A:<n>"/"B:<n>".
nodes_json() {
  local a b
  a=$(cp_ssh "$HS_A nodes list -o json") || return 1
  if [ "$HS_A" = "$HS_B" ]; then b="[]"; else b=$(cp_ssh "$HS_B nodes list -o json") || return 1; fi
  py -c '
import json,sys
out=[]
for tag,raw in (("A",sys.argv[1]),("B",sys.argv[2])):
    for n in (json.loads(raw or "[]") or []):
        n["id"]=tag+":"+str(n["id"]); out.append(n)
print(json.dumps(out))' "$a" "$b"
}
# hs_of TAGGED_ID -> admin CLI of that tailnet; node_delete TAGGED_ID
hs_of() { case $1 in B:*) printf '%s' "$HS_B" ;; *) printf '%s' "$HS_A" ;; esac; }
node_delete() { cp_ssh "$(hs_of "$1") nodes delete -i ${1#*:} --force"; }

# node_by_name NAME FIELD -> value for the node with given name ("" if none)
node_by_name() {
  nodes_json | py -c '
import json,sys
name,field=sys.argv[1],sys.argv[2]
for n in json.load(sys.stdin) or []:
    if n.get("name")==name or n.get("givenName")==name:
        print(n.get(field,"")); break' "$1" "$2"
}

# count_machine MKEY -> number of nodes with that machine key
count_machine() {
  nodes_json | py -c '
import json,sys
print(sum(1 for n in (json.load(sys.stdin) or []) if n.get("machineKey")==sys.argv[1]))' "$1"
}

# mint_key A|B -> path of a mode-600 JSON file holding a fresh pre-auth key
mint_key() {
  local which=$1 cmd f
  [ "$which" = A ] && cmd=$KEYCMD_A || cmd=$KEYCMD_B
  f=$(mktemp "$TMPD/key-$which-XXXXXX")
  chmod 600 "$f"
  if ! cp_ssh "$cmd" >"$f"; then log "mint_key $which failed"; return 1; fi
  py -c 'import json,sys;print(sys.argv[2]+":"+str(json.load(open(sys.argv[1]))["id"]))' "$f" "$which" >>"$TMPD/keyids"
  printf '%s' "$f"
}
# key_secret FILE -> key on stdout (only ever piped, never printed)
key_secret() { "$PY" -c 'import json,sys;sys.stdout.write(json.load(open(sys.argv[1]))["key"])' "$1"; }

# ---- client side (mc-ci docker) ----
# start_ts NAME IMAGE_TAG: create the container and, in the same SSH round
# trip, poll until tailscaled answers. As before, a slow daemon is not an
# error here; the following steps assert the state.
start_ts() {
  local n=$1 tag=$2
  echo "$n" >>"$TMPD/volumes"
  ci_ssh "docker run -d --name $n --hostname $n --label $LABEL -e TS_DEBUG_NOISE_DIAL=1 \
    --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun -v $n:/var/lib/tailscale \
    $IMAGE_REPO:$tag tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null || exit 1
    for i in \$(seq 1 60); do
      docker exec $n tailscale status --json >/dev/null 2>&1 && exit 0
      docker exec $n tailscale status 2>&1 | grep -q 'Logged out\|NeedsLogin' && exit 0
      sleep 0.5
    done
    exit 0"
}
# ts_auth NAME up|login URL KEYFILE HOSTNAME -> exit code of tailscale
ts_auth() {
  local n=$1 verb=$2 url=$3 kf=$4 hn=$5
  key_secret "$kf" | ci_ssh "docker exec -i $n timeout 60 tailscale $verb --login-server=$url --auth-key=file:/dev/stdin --hostname=$hn" \
    2>&1 | redact >>"$ART_DIR/$n.cli.log"
  return "${PIPESTATUS[1]}"
}
ts_json() { ci_ssh "docker exec $1 tailscale status --json" 2>/dev/null; }
ts_field() { # NAME JQ
  ci_ssh "docker exec $1 tailscale status --json 2>/dev/null | jq -r '$2'" 2>/dev/null
}
is_state() { [ "$(ts_field "$1" .BackendState)" = "$2" ]; }
wait_state() { poll "$3" 0.5 is_state "$1" "$2"; } # NAME STATE TIMEOUT_S
current_profile() { ci_ssh "docker exec $1 tailscale switch --list" 2>/dev/null | awk '/\*$/ {print $1}'; }
save_logs() { ci_ssh "docker logs $1 2>&1" </dev/null | redact >"$ART_DIR/$1.tailscaled.log" 2>/dev/null || true; }

# ---------------------------------------------------------------- results ---
RESULTS_DONE=0
# assemble_results -> results.tsv (stable order: slot, then record order)
assemble_results() {
  [ "$RESULTS_DONE" = 1 ] && return
  RESULTS_DONE=1
  local f
  : >"$ART_DIR/results.tsv"
  for f in "$RES_DIR"/*; do [ -f "$f" ] && cat "$f" >>"$ART_DIR/results.tsv"; done
}
# write_json EXIT_CODE CLEANUP_OK -> results.json
write_json() {
  py -c '
import json,sys
rows=[]
for line in open(sys.argv[1],encoding="utf-8"):
    f=line.rstrip("\n").split("\t")
    if len(f)<5: continue
    rows.append({"id":f[0],"mapping":f[1],"status":f[2],"reason":f[3],"duration_s":float(f[4])})
doc={"run_id":sys.argv[2],"repo":sys.argv[3],"mode":sys.argv[4],"only":sys.argv[5],"jobs":int(sys.argv[6]),
     "wall_time_s":float(sys.argv[7]),"tests_wall_time_s":float(sys.argv[8] or 0),
     "exit_code":int(sys.argv[9]),"cleanup_ok":sys.argv[10]=="1",
     "counts":{s:sum(1 for r in rows if r["status"]==s) for s in ("PASS","FAIL","SKIP")},
     "tests":rows}
with open(sys.argv[11],"w",encoding="utf-8") as out:
    json.dump(doc,out,indent=2,ensure_ascii=False)
    out.write("\n")' "$ART_DIR/results.tsv" "$RUN_ID" "$GITSHA" "$MODE" "$ONLY" "$JOBS" \
    "$(since "$T_START")" "${TESTS_WALL:-}" "$1" "$2" "$ART_DIR/results.json" ||
    log "WARNING: could not write results.json"
}

# ---------------------------------------------------------------- cleanup ---
PRE_NODE_IDS=""
TESTS_WALL=""

valid_tagged() { [[ $1 =~ ^[AB]:[0-9]+$ ]]; }
# run_tagged node|key TAGGED_IDS...: delete nodes, or expire+delete keys;
# one SSH call per tailnet, both tailnets at once. Only well-formed
# "A:<n>"/"B:<n>" IDs are used.
run_tagged() {
  local kind=$1 tag id cmd h; shift
  for tag in A B; do
    cmd=""; h=$(hs_of "$tag:")
    for id in "$@"; do
      valid_tagged "$id" || continue
      [ "${id%%:*}" = "$tag" ] || continue
      id=${id#*:}
      if [ "$kind" = node ]; then
        cmd="$cmd $h nodes delete -i $id --force >/dev/null 2>&1 && echo 'deleted node $tag:$id';"
      else
        cmd="$cmd $h preauthkeys expire -i $id --force >/dev/null 2>&1; $h preauthkeys delete -i $id --force >/dev/null 2>&1 && echo 'expired+deleted key $tag:$id';"
      fi
    done
    [ -n "$cmd" ] && { cp_ssh "$cmd" 2>/dev/null | while read -r l; do log "$l"; done; } &
  done
  wait
  return 0
}
# our_new_nodes -> "p02 <tagged id>" or "other <tagged id>" per node that was
# not present before the run. Only "p02" ones (named p02-*) may be deleted.
our_new_nodes() {
  nodes_json | py -c '
import json,sys
pre=set(sys.argv[1].split(","))
for n in json.load(sys.stdin) or []:
    if str(n["id"]) in pre: continue
    p02=n.get("name","").startswith("p02-") or n.get("givenName","").startswith("p02-")
    print(("p02 " if p02 else "other ")+n["id"])' "$PRE_NODE_IDS"
}
# our_key_ids -> tagged IDs of every key this run minted (ID list + key files,
# so a key whose ID line was lost to an interrupt is still found)
our_key_ids() {
  {
    cat "$TMPD/keyids"
    local f
    for f in "$TMPD"/key-A-* "$TMPD"/key-B-*; do
      [ -s "$f" ] || continue
      py -c '
import json,sys
try: print(sys.argv[2]+":"+str(json.load(open(sys.argv[1]))["id"]))
except Exception: pass' "$f" "$(basename "$f" | cut -d- -f2)"
    done
  } | grep -E '^[AB]:[0-9]+$' | sort -u
}
# our_volumes -> p02-* volume names this run created
our_volumes() { grep -E '^p02-[a-z0-9-]+$' "$TMPD/volumes" | sort -u; }

cleanup_pass() {
  local ids names name vols
  if [ "$NEED_CI" = 1 ]; then
    ids=$(ci_ssh "docker ps -aq --filter label=$LABEL" </dev/null 2>/dev/null | tr -d '\r' | grep -E '^[0-9a-f]+$' | tr '\n' ' ')
    if [ -n "${ids// /}" ]; then
      names=$(ci_ssh "docker inspect -f '{{.Name}}' $ids" </dev/null 2>/dev/null | tr -d '/\r')
      for name in $names; do
        [[ $name =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] && save_logs "$name" &
      done
      wait
      ci_ssh "docker rm -f $ids >/dev/null" </dev/null 2>/dev/null || true
    fi
    vols=$(our_volumes | tr '\n' ' ')
    if [ -n "${vols// /}" ]; then ci_ssh "docker volume rm $vols >/dev/null 2>&1" </dev/null || true; fi
  fi
  if [ "$NEED_NODES" = 1 ]; then
    # shellcheck disable=SC2046 # IDs are validated, one word each
    [ -n "$PRE_NODE_IDS" ] && run_tagged node $(our_new_nodes | awk '$1 == "p02" { print $2 }')
    # shellcheck disable=SC2046
    run_tagged key $(our_key_ids)
  fi
}
# verify_clean -> prints what this run created and is still present ("" = clean)
# The checks run concurrently; each writes "<what>:<ids>," to its own file.
verify_clean() {
  local v=$TMPD/verify keys tag
  rm -rf "$v"; mkdir -p "$v"
  if [ "$NEED_CI" = 1 ]; then
    {
      local x all
      x=$(ci_ssh "docker ps -aq --filter label=$LABEL" </dev/null 2>/dev/null) || x="unverifiable"
      [ -n "$x" ] && printf 'containers:%s ' "$(printf '%s' "$x" | tr -d '\r' | tr '\n' ',')"
      if [ -n "$(our_volumes)" ]; then
        if all=$(ci_ssh "docker volume ls -q" </dev/null 2>/dev/null); then
          x=$(printf '%s\n' "$all" | tr -d '\r' | grep -xF -f <(our_volumes) || true)
        else x="unverifiable"; fi
        [ -n "$x" ] && printf 'volumes:%s ' "$(printf '%s' "$x" | tr '\n' ',')"
      fi
    } >"$v/ci" &
  fi
  if [ "$NEED_NODES" = 1 ]; then
    if [ -n "$PRE_NODE_IDS" ]; then
      {
        local x ours other
        x=$(our_new_nodes) || { printf 'nodes:unverifiable '; exit 0; }
        # Ours = new and named p02-*; other new nodes are only reported.
        ours=$(printf '%s\n' "$x" | awk '$1 == "p02" { print $2 }' | tr '\n' ',')
        other=$(printf '%s\n' "$x" | awk '$1 == "other" { print $2 }' | tr '\n' ' ')
        [ -n "$other" ] && log "note: nodes not named p02-* appeared during the run (not ours, left alone): $other"
        [ -n "$ours" ] && printf 'nodes:%s ' "$ours"
      } >"$v/nodes" &
    fi
    keys=$(our_key_ids)
    for tag in A B; do
      {
        local ids x
        ids=$(printf '%s\n' "$keys" | grep "^$tag:" | cut -d: -f2 | tr '\n' ',')
        [ -n "$ids" ] || exit 0
        # The list holds key material: it only flows into python's stdin and
        # is reduced to IDs; it is never stored or printed.
        x=$(cp_ssh "$(hs_of "$tag:") preauthkeys list -o json" 2>/dev/null | py -c '
import json,sys
ours=set(i for i in sys.argv[1].split(",") if i)
for k in json.load(sys.stdin) or []:
    if str(k.get("id")) in ours: print(sys.argv[2]+":"+str(k.get("id")))' "$ids" "$tag") || x="unverifiable"
        [ -n "$x" ] && printf 'keys:%s ' "$(printf '%s' "$x" | tr '\n' ',')"
      } >"$v/keys-$tag" &
    done
  fi
  wait
  cat "$v"/* 2>/dev/null | sed 's/ $//'
}

cleanup() {
  local rc=$? left="" clean=1 t0
  trap '' INT TERM   # a second Ctrl-C must not abort the cleanup
  stop_jobs
  [ -n "$TESTS_WALL" ] || TESTS_WALL=$(since "$T_START")
  assemble_results
  if [ "$KEEP" = 1 ]; then
    log "KEEP=1: leaving containers/nodes; temp dir $TMPD (contains keys!) must be removed by hand"
    write_json "$rc" 0
    mux_close
    exit "$rc"
  fi
  if [ "$NEED_CI" = 1 ] || [ "$NEED_NODES" = 1 ]; then
    t0=$(now)
    log "cleanup: containers/volumes with label $LABEL, new p02-* nodes, own pre-auth keys"
    while :; do
      cleanup_pass
      left=$(verify_clean)
      [ -z "$left" ] && break
      if elapsed_ge "$t0" "$CLEANUP_RETRY_S"; then
        log "ERROR: cleanup incomplete, still present: $left (remove by hand)"
        clean=0; [ "$rc" = 0 ] && rc=2
        break
      fi
      log "cleanup: still present: $left; retrying"
      sleep 3
    done
    [ "$clean" = 1 ] && log "cleanup: verified, nothing created by this run is left ($(since "$t0")s)"
  fi
  write_json "$rc" "$clean"
  mux_close
  rm -rf "$TMPD"
  log "wall time $(since "$T_START")s, exit $rc"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 2' INT TERM

# ------------------------------------------------------------------ setup ---
log "P02 E2E run $RUN_ID ($MODE, JOBS=$JOBS), repo $GITSHA, artifacts $ART_DIR"
command -v curl >/dev/null && command -v "$PY" >/dev/null || { log "need curl and $PY"; exit 2; }
if [ "$NEED_NODES" = 1 ] || [ "$NEED_CI" = 1 ]; then
  # Both hosts probed at once. ControlPath %C differs per host.
  [ "$NEED_NODES" = 1 ] && { mux_open "$CP_SSH" >"$TMPD/mux-cp" & }
  [ "$NEED_CI" = 1 ] && { mux_open "$CI_SSH" >"$TMPD/mux-ci" & }
  wait
  [ "$(cat "$TMPD/mux-cp" 2>/dev/null)" = on ] && CP_MUX=(-o ControlMaster=no -o ControlPath="$CTL_DIR/%C")
  [ "$(cat "$TMPD/mux-ci" 2>/dev/null)" = on ] && CI_MUX=(-o ControlMaster=no -o ControlPath="$CTL_DIR/%C")
  log "ssh multiplexing: mc-cp=$([ ${#CP_MUX[@]} -gt 0 ] && echo on || echo off) mc-ci=$([ ${#CI_MUX[@]} -gt 0 ] && echo on || echo off)"
fi

# Independent setup steps run concurrently.
SETUP_PIDS=()
(fetch_key "$URL_A/key?v=$CAPVER"; printf '%s %s\n' "$FK_CODE" "$FK_KEY" >"$TMPD/ka") & SETUP_PIDS+=($!)
(fetch_key "$URL_B/key?v=$CAPVER"; printf '%s %s\n' "$FK_CODE" "$FK_KEY" >"$TMPD/kb") & SETUP_PIDS+=($!)
{
  echo "run_id=$RUN_ID repo=$GITSHA mode=$MODE jobs=$JOBS only=${ONLY:-all}"
  echo "server: $(cp_ssh "$HEADSCALE version" 2>/dev/null | tr '\n' ' ')"
  echo "client images: $IMAGE_REPO:$IMAGE_NEW $IMAGE_REPO:$IMAGE_MIN"
  echo "curl: $(curl --version | head -1)"
  echo "hosts: A=$HOST_A B=$HOST_B unknown=$HOST_UNKNOWN port=$PORT ip=$PUBLIC_IP"
} 2>/dev/null | redact >"$ART_DIR/env.txt" & SETUP_PIDS+=($!)
PRE_PID=""; IMG_PID=""
if [ "$NEED_NODES" = 1 ]; then
  (nodes_json | py -c 'import json,sys;print(",".join(str(n["id"]) for n in json.load(sys.stdin) or []))' >"$TMPD/pre") &
  PRE_PID=$!
fi
if [ "$NEED_CI" = 1 ]; then
  # Each image at most once per run, and only if it is not there yet.
  ci_ssh "for t in $IMAGE_NEW $IMAGE_MIN; do docker image inspect $IMAGE_REPO:\$t >/dev/null 2>&1 || docker pull -q $IMAGE_REPO:\$t >/dev/null || { echo \"cannot get image $IMAGE_REPO:\$t\" >&2; exit 1; }; done" </dev/null &
  IMG_PID=$!
fi
wait "${SETUP_PIDS[@]}"
if [ -n "$PRE_PID" ]; then
  wait "$PRE_PID" || { log "cannot list nodes on mc-cp"; exit 2; }
  PRE_NODE_IDS=$(cat "$TMPD/pre")
  [ -n "$PRE_NODE_IDS" ] || PRE_NODE_IDS="-"
fi
if [ -n "$IMG_PID" ]; then wait "$IMG_PID" || { log "cannot get client images on mc-ci"; exit 2; }; fi
CA=000; KA=""; CB=000; KB=""
read -r CA KA <"$TMPD/ka" || true
read -r CB KB <"$TMPD/kb" || true
echo "A $CA $(short "$KA")  B $CB $(short "$KB")" >"$ART_DIR/keys.txt"
DISCRIMINATING=0
[ -n "$KA" ] && [ -n "$KB" ] && [ "$KA" != "$KB" ] && DISCRIMINATING=1
log "setup done ($(since "$T_START")s)"
echo "STATUS ID           MAP          TIME    REASON"

# ======================================================= HTTP-level tests ===
t_K01() {
  if [ "$CA" != 200 ] || [ "$CB" != 200 ] || [ -z "$KA" ] || [ -z "$KB" ]; then
    record K01 "S11,N2,I01" FAIL "/key not served: A=$CA B=$CB"
  elif [ "$KA" = "$KB" ]; then
    record K01 "S11,N2,I01" FAIL "A and B return the SAME Noise key $(short "$KA")"
  else
    record K01 "S11,N2,I01" PASS "A=$(short "$KA") B=$(short "$KB")"
  fi
}

t_K02() {
  fetch_key "https://$HOST_UNKNOWN:$PORT/key?v=$CAPVER"
  if rejected "$FK_CODE" "$FK_KEY"; then record K02 "S11,N3" PASS "unknown Host -> $FK_CODE, no key"
  else record K02 "S11,N3" FAIL "unknown Host $HOST_UNKNOWN -> $FK_CODE key=$(short "$FK_KEY")"; fi
}

t_K03() {
  local r1 r2 ok
  # Raw IP URL: no SNI, Host = IP:port. -k only because no cert can cover the IP.
  fetch_key "https://$PUBLIC_IP:$PORT/key?v=$CAPVER" -k
  r1="ip-url:$FK_CODE"; ok=1; rejected "$FK_CODE" "$FK_KEY" || ok=0
  # SNI = A, Host header = raw IP.
  fetch_key "$URL_A/key?v=$CAPVER" -H "Host: $PUBLIC_IP:$PORT"
  r2="sniA+hostIP:$FK_CODE"; rejected "$FK_CODE" "$FK_KEY" || ok=0
  if [ $ok = 1 ]; then record K03 "S11,N3" PASS "$r1 $r2, no key"
  else record K03 "S11,N3" FAIL "raw-IP Host got a key ($r1 $r2)"; fi
}

t_K04() {
  # Host variants with SNI = A. All must map to tailnet A's key.
  local bad="" n=0 h
  for h in "$HOST_A:$PORT" "$HOST_A" "$(printf '%s' "$HOST_A" | tr a-z A-Z):$PORT" "$HOST_A.:$PORT"; do
    n=$((n + 1))
    fetch_key "$URL_A/key?v=$CAPVER" -H "Host: $h"
    { [ "$FK_CODE" = 200 ] && [ "$FK_KEY" = "$KA" ]; } || bad="$bad [$h -> $FK_CODE $(short "$FK_KEY")]"
  done
  if [ -n "$bad" ]; then record K04 "N4" FAIL "variant not normalised to A:$bad"
  elif [ $DISCRIMINATING = 0 ]; then record K04 "N4" SKIP "$n/$n variants give A's key, but A==B so the check cannot discriminate (single tailnet)"
  else record K04 "N4" PASS "$n/$n Host variants -> A's key"; fi
}

t_K05() {
  local c1 k1 c2 k2
  # SNI = A, Host = B on /key.
  fetch_key "$URL_A/key?v=$CAPVER" -H "Host: $HOST_B:$PORT"
  c1=$FK_CODE; k1=$FK_KEY
  fetch_key "$URL_B/key?v=$CAPVER" -H "Host: $HOST_A:$PORT"
  c2=$FK_CODE; k2=$FK_KEY
  if rejected "$c1" "$k1" && rejected "$c2" "$k2"; then record K05 "S11,N1" PASS "SNI/Host mismatch -> $c1/$c2"
  else record K05 "S11,N1" FAIL "SNI A+Host B -> $c1 $(short "$k1"); SNI B+Host A -> $c2 $(short "$k2")"; fi
}

t_K06() {
  local c; c=$(upgrade_code "$URL_A/ts2021" -H "Host: $HOST_B:$PORT")
  if refused "${c:-000}"; then record K06 "S11,N1" PASS "/ts2021 SNI A+Host B -> $c"
  else record K06 "S11,N1" FAIL "/ts2021 SNI A+Host B upgraded ($c), not refused before handshake"; fi
}

t_K07() {
  local c1 c2
  c1=$(upgrade_code "https://$HOST_UNKNOWN:$PORT/ts2021")
  c2=$(upgrade_code "https://$PUBLIC_IP:$PORT/ts2021" -k)
  if refused "${c1:-000}" && refused "${c2:-000}"; then
    record K07 "S11,N3" PASS "/ts2021 unknown=$c1 ip=$c2"
  else record K07 "S11,N3" FAIL "/ts2021 upgraded for unknown Host ($c1) / raw IP ($c2)"; fi
}

t_K08() {
  local c; c=$(ws_code "$URL_A/ts2021" -H "Host: $HOST_B:$PORT")
  if refused "${c:-000}"; then record K08 "S11,N9" PASS "WebSocket SNI A+Host B -> $c"
  else record K08 "S11,N9" FAIL "WebSocket /ts2021 SNI A+Host B upgraded ($c)"; fi
}

t_K09() {
  fetch_key "$URL_A/key?v=$CAPVER" -H "X-Tailnet-ID: $HOST_B" -H "X-Forwarded-Host: $HOST_B" -H "Forwarded: host=$HOST_B"
  if [ "$FK_CODE" != 200 ] || [ "$FK_KEY" != "$KA" ]; then record K09 "S11" FAIL "forged headers changed result: $FK_CODE $(short "$FK_KEY")"
  elif [ $DISCRIMINATING = 0 ]; then record K09 "S11" SKIP "forged headers ignored, but A==B so a switch to B is undetectable"
  else record K09 "S11" PASS "forged X-Tailnet-ID/X-Forwarded-Host ignored, key stays A"; fi
}

t_K10() {
  # Real client Noise handshake per host (positive control) + port-80-first
  # observation. Both hosts are probed at once, each in its own --rm container.
  local ok=1 info="" h out p80 gk
  for h in "$HOST_A" "$HOST_B"; do
    ci_ssh "docker run --rm --label $LABEL $IMAGE_REPO:$IMAGE_NEW timeout 60 tailscale debug ts2021 --host $h:$PORT --verbose 2>&1" \
      </dev/null | grep -v bootstrapDNS | redact >"$ART_DIR/K10-debug-ts2021-$h.log" &
  done
  wait
  for h in "$HOST_A" "$HOST_B"; do
    out=$(cat "$ART_DIR/K10-debug-ts2021-$h.log")
    printf '%s' "$out" | grep -q 'did noise handshake' || ok=0
    p80=no; printf '%s' "$out" | grep -qE 'Dial\("tcp", "[^"]+:80"\)' && p80=yes
    gk=$(printf '%s' "$out" | grep -oE 'got public key: mkey:[0-9a-f]{8}' | sed 's/got public key: //')
    info="$info $h:key=$gk,port80-tried=$p80"
  done
  if [ $ok = 1 ]; then record K10 "P02-POC1" PASS "$IMAGE_NEW handshake OK on A and B;$info"
  else record K10 "P02-POC1" FAIL "$IMAGE_NEW debug ts2021 handshake failed;$info"; fi
  record K10m "P02-POC1" SKIP "$IMAGE_MIN 'debug ts2021' cannot dial host:port (resolves \"host:8443\" as a name); v1.84 handshake is covered by R02"
}

# Not possible against this baseline / without a custom client.
t_static_skips() {
  want K11 && record K11 "S11,N2" SKIP "Host B + initiation encrypted to key A needs a custom Noise client; also A==B on baseline"
  want N5  && record N5  "N5,I01" SKIP "inner :authority=B inside a Noise session A needs a custom client"
  want N7  && record N7  "N7"     SKIP "map request with tailnet-A node key over a B session needs a custom client and two tailnets"
  want N10 && record N10 "N10"    SKIP "port 80 on $PUBLIC_IP is not the control plane (HAProxy, empty reply); no HTTP-proxy path in this lab"
  want S10w && record S10w "S10"  SKIP "web/OIDC approval and cookies: no OIDC configured on the lab server"
  return 0
}

# ======================================================= real-client tests ===
# sees NAME PEER_HOSTNAME -> True/False: does NAME list PEER_HOSTNAME as a peer
sees() {
  ts_json "$1" | py -c 'import json,sys;d=json.load(sys.stdin);print(any(p.get("HostName")==sys.argv[1] for p in (d.get("Peer") or {}).values()))' "$2" 2>/dev/null
}

t_R01() {
  # A client registered via A must not see a peer registered via B (and vice versa).
  local na=p02-r01-a nb=p02-r01-b fa fb ra=0 rb=0 pa pb see_ab see_ba t0
  if ! { fa=$(mint_key A) && fb=$(mint_key B) && start_ts $na "$IMAGE_NEW" && start_ts $nb "$IMAGE_NEW"; }; then
    record R01 "S11,N7,I01" FAIL "setup failed"
    return
  fi
  ts_auth $na up "$URL_A" "$fa" $na & pa=$!
  ts_auth $nb up "$URL_B" "$fb" $nb & pb=$!
  wait $pa || ra=$?
  wait $pb || rb=$?
  # Negative check: observe for R01_OBSERVE_S seconds (was a fixed "sleep 5"
  # and one sample). Stops early only if a cross-tailnet peer shows up.
  t0=$(now)
  while :; do
    see_ab=$(sees $na $nb); see_ba=$(sees $nb $na)
    { [ "$see_ab" = True ] || [ "$see_ba" = True ]; } && break
    elapsed_ge "$t0" "$R01_OBSERVE_S" && break
    sleep 1
  done
  if [ $ra != 0 ] || [ $rb != 0 ]; then record R01 "S11,N7,I01" FAIL "registration failed (A rc=$ra, B rc=$rb)"
  elif [ "$see_ab" = False ] && [ "$see_ba" = False ]; then record R01 "S11,N7,I01" PASS "A and B netmaps are disjoint"
  else record R01 "S11,N7,I01" FAIL "cross-tailnet peer visible: A sees B=$see_ab, B sees A=$see_ba"; fi
}

# R02: one device, profile A -> B -> A (+ restart), same machine key.
r02() {
  local tag=$1 n=p02-r02-${1//./-} fa fb pa pb nka nkb mk cnt problems="" ra=0 rb=0
  fa=$(mint_key A) && fb=$(mint_key B) && start_ts "$n" "$tag" || { record "R02/$tag" "S05,I03" FAIL "setup failed"; return; }
  ts_auth "$n" up "$URL_A" "$fa" "$n-a" || ra=$?
  pa=$(current_profile "$n"); nka=$(ts_field "$n" .Self.PublicKey)
  ts_auth "$n" login "$URL_B" "$fb" "$n-b" || rb=$?
  pb=$(current_profile "$n"); nkb=$(ts_field "$n" .Self.PublicKey)
  if [ $ra != 0 ] || [ $rb != 0 ]; then record "R02/$tag" "S05,I03" FAIL "login failed (A rc=$ra, B rc=$rb)"; return; fi
  local nprof; nprof=$(ci_ssh "docker exec $n tailscale switch --list" | tail -n +2 | grep -c .)
  [ "$nprof" -ge 2 ] && [ "$pa" != "$pb" ] || problems="$problems; client has $nprof profile(s)"
  [ "$nka" != "$nkb" ] || problems="$problems; same node key in A and B"
  # Server: two independent nodes sharing this device's machine key.
  mk=$(node_by_name "$n-b" machineKey)
  [ -n "$mk" ] || mk=$(node_by_name "$n-a" machineKey)
  cnt=$(count_machine "$mk")
  local ida idb; ida=$(node_by_name "$n-a" id); idb=$(node_by_name "$n-b" id)
  [ "${cnt:-0}" -ge 2 ] && [ -n "$ida" ] && [ -n "$idb" ] && [ "$ida" != "$idb" ] \
    || problems="$problems; server has $cnt node(s) for this machine key (A id=${ida:-none}, B id=${idb:-none})"
  # Switch back to A: must resume the old identity without re-auth.
  ci_ssh "docker exec $n tailscale switch $pa" >/dev/null 2>&1
  if wait_state "$n" Running 30 && [ "$(ts_field "$n" .Self.PublicKey)" = "$nka" ]; then :; else
    problems="$problems; back on A: state=$(ts_field "$n" .BackendState), node key $( [ "$(ts_field "$n" .Self.PublicKey)" = "$nka" ] && echo same || echo changed/none)"
  fi
  # Restart the device: must stay on A with A's identity. No fixed sleep:
  # wait_state polls the restarted daemon until it is Running.
  ci_ssh "docker restart $n >/dev/null"
  if wait_state "$n" Running 30 && [ "$(current_profile "$n")" = "$pa" ] && [ "$(ts_field "$n" .Self.PublicKey)" = "$nka" ]; then :; else
    problems="$problems; after restart: profile=$(current_profile "$n") state=$(ts_field "$n" .BackendState)"
  fi
  # Each profile dials its own host (Noise key fetched per ControlURL).
  local lg; lg=$(ci_ssh "docker logs $n 2>&1" | grep 'trying noise dial' | redact)
  printf '%s\n' "$lg" >"$ART_DIR/$n.noise-dials.log"
  printf '%s' "$lg" | grep -q "$HOST_A" && printf '%s' "$lg" | grep -q "$HOST_B" || problems="$problems; noise dials did not use both hosts"
  # N8: deleting node A must not affect node B (only meaningful with two nodes).
  if [ "$tag" = "$IMAGE_NEW" ]; then
    if [ -n "$ida" ] && [ -n "$idb" ] && [ "$ida" != "$idb" ]; then
      node_delete "$ida" >/dev/null 2>&1
      if [ -n "$(node_by_name "$n-b" id)" ]; then record N8 "N8" PASS "deleting A's node left B's node (same machine key)"
      else record N8 "N8" FAIL "deleting A's node removed B's node"; fi
    else
      record N8 "N8" SKIP "server collapsed A and B into one node (see R02/$tag), nothing independent to delete"
    fi
  fi
  if [ -z "$problems" ]; then record "R02/$tag" "S05,I03" PASS "profiles $pa/$pb, 2 nodes, A resumed after switch+restart"
  else record "R02/$tag" "S05,I03" FAIL "${problems#; }"; fi
}

t_R03() {
  # Pre-auth key minted for tailnet A presented at B must not register anywhere.
  local n=p02-r03 fa rc=0 id
  if ! { fa=$(mint_key A) && start_ts $n "$IMAGE_NEW"; }; then
    record R03 "S10,N6" FAIL "setup failed"
    return
  fi
  ts_auth $n up "$URL_B" "$fa" $n || rc=$?
  id=$(node_by_name $n id)
  if [ $rc != 0 ] && [ -z "$id" ]; then record R03 "S10,N6" PASS "A's key refused at B (rc=$rc), no node created"
  else record R03 "S10,N6" FAIL "A's key registered at B (rc=$rc, node id=${id:-none}, state=$(ts_field $n .BackendState))"; fi
}

# ------------------------------------------------------------------- run ---
# Slots fix the report order: K01..K09, K10, static SKIPs, R01, R02 (min, new), R03.
# Launch order: the quick K tests first, then the longest client scenarios.
for i in 1 2 3 4 5 6 7 8 9; do want "K0$i" && launch "0$i" "K0$i" "t_K0$i"; done
want R02 && launch 14 "R02-$IMAGE_NEW" r02 "$IMAGE_NEW"
want R02 && launch 13 "R02-$IMAGE_MIN" r02 "$IMAGE_MIN"
want R03 && launch 15 R03 t_R03
want R01 && launch 12 R01 t_R01
want K10 && launch 10 K10 t_K10
SLOT=11; JOB_T0=$(now); t_static_skips
wait
TESTS_WALL=$(since "$T_START")

# ---------------------------------------------------------------- summary ---
assemble_results
FAILS=$(awk -F'\t' '$3 == "FAIL"' "$ART_DIR/results.tsv" | wc -l | tr -d ' ')
echo
echo "== summary ($RUN_ID, $MODE, JOBS=$JOBS, tests ${TESTS_WALL}s) =="
while IFS=$'\t' read -r id map st _ dur; do
  printf '%-5s %-13s %-12s %6ss\n' "$st" "$id" "$map" "$dur"
done <"$ART_DIR/results.tsv"
echo "FAIL=$FAILS  artifacts: $ART_DIR"
[ "$FAILS" = 0 ] && exit 0 || exit 1
