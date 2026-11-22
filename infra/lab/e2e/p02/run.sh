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
# Secrets: pre-auth keys are created with a short expiry, stored only in a
# mode-700 temp dir (files mode 600), piped to `tailscale up --auth-key=file:/dev/stdin`
# and never printed. Logs written to the artifact dir are redacted.
#
# Exit code: 0 if no FAIL, 1 if any FAIL, 2 on harness/setup error.

set -u -o pipefail

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
PY=${PY:-python3}
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

SSHOPTS=(-o BatchMode=yes -o ConnectTimeout=10 -i "$SSH_KEY")
# cp_ssh never reads stdin (-n), so it is safe inside `while read` loops.
cp_ssh() { ssh -n "${SSHOPTS[@]}" "$CP_SSH" "$@"; }
ci_ssh() { ssh "${SSHOPTS[@]}" "$CI_SSH" "$@"; }

umask 077
TMPD=$(mktemp -d)
chmod 700 "$TMPD"
mkdir -p "$ART_DIR"
: >"$TMPD/keyids"; : >"$TMPD/volumes"

# ---------------------------------------------------------------- helpers ---
RESULTS=()
FAILS=0
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# record ID MAP STATUS REASON
record() {
  local id=$1 map=$2 st=$3 reason=$4
  RESULTS+=("$id|$map|$st|$reason")
  [ "$st" = FAIL ] && FAILS=$((FAILS + 1))
  printf '%-5s %-4s %-24s %s\n' "$st" "$id" "$map" "$reason"
  printf '%s\t%s\t%s\t%s\n' "$id" "$map" "$st" "$reason" >>"$ART_DIR/results.tsv"
}

want() { [ -z "$ONLY" ] && return 0; case " $ONLY " in *" $1 "*) return 0 ;; esac; return 1; }

# Redact anything that might be a credential; shorten public keys.
redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g' \
    -e 's/\b([0-9a-f]{8})[0-9a-f]{56}\b/\1…/g'
}

short() { printf '%s' "${1:0:20}"; }

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

# node_field NAME FIELD -> value for the node with given name ("" if none)
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
start_ts() { # NAME IMAGE_TAG
  local n=$1 tag=$2
  echo "$n" >>"$TMPD/volumes"
  ci_ssh "docker run -d --name $n --hostname $n --label $LABEL -e TS_DEBUG_NOISE_DIAL=1 \
    --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun -v $n:/var/lib/tailscale \
    $IMAGE_REPO:$tag tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null" || return 1
  for _ in $(seq 1 20); do
    ci_ssh "docker exec $n tailscale status --json >/dev/null 2>&1 || docker exec $n tailscale status 2>&1 | grep -q 'Logged out\|NeedsLogin'" && return 0
    sleep 1
  done
  return 0
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
wait_state() { # NAME STATE SECONDS
  local s
  for _ in $(seq 1 "$3"); do
    s=$(ts_field "$1" .BackendState)
    [ "$s" = "$2" ] && return 0
    sleep 1
  done
  return 1
}
current_profile() { ci_ssh "docker exec $1 tailscale switch --list" 2>/dev/null | awk '/\*$/ {print $1}'; }
save_logs() { ci_ssh "docker logs $1 2>&1" | redact >"$ART_DIR/$1.tailscaled.log" 2>/dev/null || true; }

# ---------------------------------------------------------------- cleanup ---
PRE_NODE_IDS=""
cleanup() {
  local rc=$?
  if [ "$KEEP" = 1 ]; then
    log "KEEP=1: leaving containers/nodes; temp dir $TMPD (contains keys!) must be removed by hand"
    exit $rc
  fi
  log "cleanup: containers/volumes with label $LABEL"
  local ids
  ids=$(ci_ssh "docker ps -aq --filter label=$LABEL" 2>/dev/null | tr '\n' ' ')
  for c in $ids; do
    local name
    name=$(ci_ssh "docker inspect -f '{{.Name}}' $c" 2>/dev/null | tr -d '/')
    [ -n "$name" ] && save_logs "$name"
  done
  [ -n "$ids" ] && ci_ssh "docker rm -f $ids >/dev/null" || true
  for v in $(sort -u "$TMPD/volumes"); do
    case $v in p02-*) ci_ssh "docker volume rm $v >/dev/null 2>&1" || true ;; esac
  done
  log "cleanup: nodes created by this run (name p02-*, not present before)"
  if [ -n "$PRE_NODE_IDS" ]; then
    nodes_json | py -c '
import json,sys
pre=set(sys.argv[1].split(","))
for n in json.load(sys.stdin) or []:
    if str(n["id"]) not in pre and (n.get("name","").startswith("p02-") or n.get("givenName","").startswith("p02-")):
        print(n["id"])' "$PRE_NODE_IDS" | while read -r id; do
      node_delete "$id" >/dev/null 2>&1 && log "deleted node $id"
    done
  fi
  log "cleanup: expire+delete own pre-auth keys"
  while read -r k; do
    [ -n "$k" ] || continue
    local h; h=$(hs_of "$k"); k=${k#*:}
    cp_ssh "$h preauthkeys expire -i $k --force >/dev/null 2>&1; $h preauthkeys delete -i $k --force >/dev/null 2>&1" || true
  done <"$TMPD/keyids"
  local left
  left=$(nodes_json | py -c '
import json,sys
pre=set(sys.argv[1].split(","))
print(" ".join(str(n["id"]) for n in json.load(sys.stdin) or [] if str(n["id"]) not in pre))' "$PRE_NODE_IDS")
  [ -z "$left" ] || log "WARNING: nodes not present before the run still exist: $left (check by hand)"
  rm -rf "$TMPD"
  exit $rc
}
trap cleanup EXIT
trap 'exit 2' INT TERM

# ------------------------------------------------------------------ setup ---
log "P02 E2E run $RUN_ID, repo $GITSHA, artifacts $ART_DIR"
command -v curl >/dev/null && command -v "$PY" >/dev/null || { log "need curl and $PY"; exit 2; }
PRE_NODE_IDS=$(nodes_json | py -c 'import json,sys;print(",".join(str(n["id"]) for n in json.load(sys.stdin) or []))') \
  || { log "cannot list nodes on mc-cp"; exit 2; }
[ -n "$PRE_NODE_IDS" ] || PRE_NODE_IDS="-"
{
  echo "run_id=$RUN_ID repo=$GITSHA"
  echo "server: $(cp_ssh "$HEADSCALE version" 2>/dev/null | tr '\n' ' ')"
  echo "client images: $IMAGE_REPO:$IMAGE_NEW $IMAGE_REPO:$IMAGE_MIN"
  echo "curl: $(curl --version | head -1)"
  echo "hosts: A=$HOST_A B=$HOST_B unknown=$HOST_UNKNOWN port=$PORT ip=$PUBLIC_IP"
} | redact >"$ART_DIR/env.txt"
for t in "$IMAGE_NEW" "$IMAGE_MIN"; do
  ci_ssh "docker image inspect $IMAGE_REPO:$t >/dev/null 2>&1 || docker pull -q $IMAGE_REPO:$t >/dev/null" \
    || { log "cannot get image $IMAGE_REPO:$t"; exit 2; }
done
: >"$ART_DIR/results.tsv"
echo "STATUS ID   MAP                      REASON"

# ======================================================= HTTP-level tests ===
fetch_key "$URL_A/key?v=$CAPVER"; KA=$FK_KEY; CA=$FK_CODE
fetch_key "$URL_B/key?v=$CAPVER"; KB=$FK_KEY; CB=$FK_CODE
echo "A $CA $(short "$KA")  B $CB $(short "$KB")" >"$ART_DIR/keys.txt"
DISCRIMINATING=0
[ -n "$KA" ] && [ -n "$KB" ] && [ "$KA" != "$KB" ] && DISCRIMINATING=1

if want K01; then
  if [ "$CA" != 200 ] || [ "$CB" != 200 ] || [ -z "$KA" ] || [ -z "$KB" ]; then
    record K01 "S11,N2,I01" FAIL "/key not served: A=$CA B=$CB"
  elif [ "$KA" = "$KB" ]; then
    record K01 "S11,N2,I01" FAIL "A and B return the SAME Noise key $(short "$KA")"
  else
    record K01 "S11,N2,I01" PASS "A=$(short "$KA") B=$(short "$KB")"
  fi
fi

if want K02; then
  fetch_key "https://$HOST_UNKNOWN:$PORT/key?v=$CAPVER"
  if rejected "$FK_CODE" "$FK_KEY"; then record K02 "S11,N3" PASS "unknown Host -> $FK_CODE, no key"
  else record K02 "S11,N3" FAIL "unknown Host $HOST_UNKNOWN -> $FK_CODE key=$(short "$FK_KEY")"; fi
fi

if want K03; then
  # Raw IP URL: no SNI, Host = IP:port. -k only because no cert can cover the IP.
  fetch_key "https://$PUBLIC_IP:$PORT/key?v=$CAPVER" -k
  r1="ip-url:$FK_CODE"; ok=1; rejected "$FK_CODE" "$FK_KEY" || ok=0
  # SNI = A, Host header = raw IP.
  fetch_key "$URL_A/key?v=$CAPVER" -H "Host: $PUBLIC_IP:$PORT"
  r2="sniA+hostIP:$FK_CODE"; rejected "$FK_CODE" "$FK_KEY" || ok=0
  if [ $ok = 1 ]; then record K03 "S11,N3" PASS "$r1 $r2, no key"
  else record K03 "S11,N3" FAIL "raw-IP Host got a key ($r1 $r2)"; fi
fi

if want K04; then
  # Host variants with SNI = A. All must map to tailnet A's key.
  bad=""; n=0
  for h in "$HOST_A:$PORT" "$HOST_A" "$(printf '%s' "$HOST_A" | tr a-z A-Z):$PORT" "$HOST_A.:$PORT"; do
    n=$((n + 1))
    fetch_key "$URL_A/key?v=$CAPVER" -H "Host: $h"
    { [ "$FK_CODE" = 200 ] && [ "$FK_KEY" = "$KA" ]; } || bad="$bad [$h -> $FK_CODE $(short "$FK_KEY")]"
  done
  if [ -n "$bad" ]; then record K04 "N4" FAIL "variant not normalised to A:$bad"
  elif [ $DISCRIMINATING = 0 ]; then record K04 "N4" SKIP "$n/$n variants give A's key, but A==B so the check cannot discriminate (single tailnet)"
  else record K04 "N4" PASS "$n/$n Host variants -> A's key"; fi
fi

if want K05; then
  # SNI = A, Host = B on /key.
  fetch_key "$URL_A/key?v=$CAPVER" -H "Host: $HOST_B:$PORT"
  c1=$FK_CODE; k1=$FK_KEY
  fetch_key "$URL_B/key?v=$CAPVER" -H "Host: $HOST_A:$PORT"
  c2=$FK_CODE; k2=$FK_KEY
  if rejected "$c1" "$k1" && rejected "$c2" "$k2"; then record K05 "S11,N1" PASS "SNI/Host mismatch -> $c1/$c2"
  else record K05 "S11,N1" FAIL "SNI A+Host B -> $c1 $(short "$k1"); SNI B+Host A -> $c2 $(short "$k2")"; fi
fi

if want K06; then
  c=$(upgrade_code "$URL_A/ts2021" -H "Host: $HOST_B:$PORT")
  if refused "${c:-000}"; then record K06 "S11,N1" PASS "/ts2021 SNI A+Host B -> $c"
  else record K06 "S11,N1" FAIL "/ts2021 SNI A+Host B upgraded ($c), not refused before handshake"; fi
fi

if want K07; then
  c1=$(upgrade_code "https://$HOST_UNKNOWN:$PORT/ts2021")
  c2=$(upgrade_code "https://$PUBLIC_IP:$PORT/ts2021" -k)
  if refused "${c1:-000}" && refused "${c2:-000}"; then
    record K07 "S11,N3" PASS "/ts2021 unknown=$c1 ip=$c2"
  else record K07 "S11,N3" FAIL "/ts2021 upgraded for unknown Host ($c1) / raw IP ($c2)"; fi
fi

if want K08; then
  c=$(ws_code "$URL_A/ts2021" -H "Host: $HOST_B:$PORT")
  if refused "${c:-000}"; then record K08 "S11,N9" PASS "WebSocket SNI A+Host B -> $c"
  else record K08 "S11,N9" FAIL "WebSocket /ts2021 SNI A+Host B upgraded ($c)"; fi
fi

if want K09; then
  fetch_key "$URL_A/key?v=$CAPVER" -H "X-Tailnet-ID: $HOST_B" -H "X-Forwarded-Host: $HOST_B" -H "Forwarded: host=$HOST_B"
  if [ "$FK_CODE" != 200 ] || [ "$FK_KEY" != "$KA" ]; then record K09 "S11" FAIL "forged headers changed result: $FK_CODE $(short "$FK_KEY")"
  elif [ $DISCRIMINATING = 0 ]; then record K09 "S11" SKIP "forged headers ignored, but A==B so a switch to B is undetectable"
  else record K09 "S11" PASS "forged X-Tailnet-ID/X-Forwarded-Host ignored, key stays A"; fi
fi

if want K10; then
  # Real client Noise handshake per host (positive control) + port-80-first observation.
  ok=1; info=""
  for h in "$HOST_A" "$HOST_B"; do
    out=$(ci_ssh "docker run --rm --label $LABEL $IMAGE_REPO:$IMAGE_NEW timeout 60 tailscale debug ts2021 --host $h:$PORT --verbose 2>&1" | grep -v bootstrapDNS | redact)
    printf '%s\n' "$out" >"$ART_DIR/K10-debug-ts2021-$h.log"
    printf '%s' "$out" | grep -q 'did noise handshake' || ok=0
    p80=no; printf '%s' "$out" | grep -qE 'Dial\("tcp", "[^"]+:80"\)' && p80=yes
    gk=$(printf '%s' "$out" | grep -oE 'got public key: mkey:[0-9a-f]{8}' | sed 's/got public key: //')
    info="$info $h:key=$gk,port80-tried=$p80"
  done
  if [ $ok = 1 ]; then record K10 "P02-POC1" PASS "$IMAGE_NEW handshake OK on A and B;$info"
  else record K10 "P02-POC1" FAIL "$IMAGE_NEW debug ts2021 handshake failed;$info"; fi
  record K10m "P02-POC1" SKIP "$IMAGE_MIN 'debug ts2021' cannot dial host:port (resolves \"host:8443\" as a name); v1.84 handshake is covered by R02"
fi

# Not possible against this baseline / without a custom client.
want K11 && record K11 "S11,N2" SKIP "Host B + initiation encrypted to key A needs a custom Noise client; also A==B on baseline"
want N5  && record N5  "N5,I01" SKIP "inner :authority=B inside a Noise session A needs a custom client"
want N7  && record N7  "N7"     SKIP "map request with tailnet-A node key over a B session needs a custom client and two tailnets"
want N10 && record N10 "N10"    SKIP "port 80 on $PUBLIC_IP is not the control plane (HAProxy, empty reply); no HTTP-proxy path in this lab"
want S10w && record S10w "S10"  SKIP "web/OIDC approval and cookies: no OIDC configured on the lab server"

# ======================================================= real-client tests ===
if want R01; then
  # A client registered via A must not see a peer registered via B (and vice versa).
  na=p02-r01-a; nb=p02-r01-b
  if ! { fa=$(mint_key A) && fb=$(mint_key B) && start_ts $na "$IMAGE_NEW" && start_ts $nb "$IMAGE_NEW"; }; then
    record R01 "S11,N7,I01" FAIL "setup failed"
  else
    ra=0; rb=0
    ts_auth $na up "$URL_A" "$fa" $na || ra=$?
    ts_auth $nb up "$URL_B" "$fb" $nb || rb=$?
    sleep 5
    see_ab=$(ts_json $na | py -c 'import json,sys;d=json.load(sys.stdin);print(any(p.get("HostName")==sys.argv[1] for p in (d.get("Peer") or {}).values()))' $nb 2>/dev/null)
    see_ba=$(ts_json $nb | py -c 'import json,sys;d=json.load(sys.stdin);print(any(p.get("HostName")==sys.argv[1] for p in (d.get("Peer") or {}).values()))' $na 2>/dev/null)
    if [ $ra != 0 ] || [ $rb != 0 ]; then record R01 "S11,N7,I01" FAIL "registration failed (A rc=$ra, B rc=$rb)"
    elif [ "$see_ab" = False ] && [ "$see_ba" = False ]; then record R01 "S11,N7,I01" PASS "A and B netmaps are disjoint"
    else record R01 "S11,N7,I01" FAIL "cross-tailnet peer visible: A sees B=$see_ab, B sees A=$see_ba"; fi
  fi
fi

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
  if wait_state "$n" Running 20 && [ "$(ts_field "$n" .Self.PublicKey)" = "$nka" ]; then :; else
    problems="$problems; back on A: state=$(ts_field "$n" .BackendState), node key $( [ "$(ts_field "$n" .Self.PublicKey)" = "$nka" ] && echo same || echo changed/none)"
  fi
  # Restart the device: must stay on A with A's identity.
  ci_ssh "docker restart $n >/dev/null"; sleep 3
  if wait_state "$n" Running 20 && [ "$(current_profile "$n")" = "$pa" ] && [ "$(ts_field "$n" .Self.PublicKey)" = "$nka" ]; then :; else
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
if want R02; then
  r02 "$IMAGE_MIN"
  r02 "$IMAGE_NEW"
fi

if want R03; then
  # Pre-auth key minted for tailnet A presented at B must not register anywhere.
  n=p02-r03
  if ! { fa=$(mint_key A) && start_ts $n "$IMAGE_NEW"; }; then
    record R03 "S10,N6" FAIL "setup failed"
  else
    rc=0; ts_auth $n up "$URL_B" "$fa" $n || rc=$?
    id=$(node_by_name $n id)
    if [ $rc != 0 ] && [ -z "$id" ]; then record R03 "S10,N6" PASS "A's key refused at B (rc=$rc), no node created"
    else record R03 "S10,N6" FAIL "A's key registered at B (rc=$rc, node id=${id:-none}, state=$(ts_field $n .BackendState))"; fi
  fi
fi

# ---------------------------------------------------------------- summary ---
echo
echo "== summary ($RUN_ID) =="
for r in "${RESULTS[@]}"; do IFS='|' read -r id map st _ <<<"$r"; printf '%-5s %-8s %s\n' "$st" "$id" "$map"; done
echo "FAIL=$FAILS  artifacts: $ART_DIR"
[ "$FAILS" = 0 ] && exit 0 || exit 1
