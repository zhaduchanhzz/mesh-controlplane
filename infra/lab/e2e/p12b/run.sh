#!/usr/bin/env bash
# P12b real-client E2E: a planned move of a tailnet between two clusters
# (owner decisions Q1-Q7, 2026-11-26), measured.
#
#   c1 (mc-cp-c1): https://c1.cp.gta5web.online:8443
#      VPS -> wg-mesh -> mc-cp 10.99.77.2:8443 (HAProxy mesh-c1-ha, transient) -> mc-cp 192.168.1.241:8443
#   c2 (mc-c2):    https://c2.cp.gta5web.online:8445
#      VPS -> wg-mesh -> mc-cp 10.99.77.2:8445 (HAProxy mesh-c2-ha, long-lived) -> mc-c2 192.168.1.244:8443
#   bootstrap: https://boot.cp.gta5web.online:8444 (mc-cp, transient)
#   mesh-placement: 127.0.0.1:9443 on mc-cp, mTLS with a throwaway CA, scheduler every 2 s
#   clients: docker on mc-ci; re-bootstrap with mesh-rebootstrap on mc-ci
#
# One process per cluster (failover is P12's subject). Same isolation from the
# long-lived lab as P12: run binary /usr/local/bin/headscale-cluster-p12b and
# config /etc/headscale/cluster/config-p12b.yaml; the long-lived c2 (meshlab,
# mc-c2 and mc-c2b) and the P02 POC on mc-cp are stopped for the run and
# restarted at cleanup; database e2e_p12b<run> on mc-db, dropped at cleanup.
#
# Per client image (IMAGES, default v1.104.7 then v1.84.3) one round:
#   M01 tailnet m<r> (placement picks the owner), N docker clients enrolled at
#       the URL bootstrap gives; all Running, N nodes, a pair pings
#   M02 ScheduleMove to the other cluster at now+LEAD with a reason (Q2): every
#       client of m<r> shows the notice (Q1: DisplayMessages / Health), the
#       tailnet's own /api/v2 scheduled-move shows time and reason (Q3); the
#       target cluster is named nowhere; control tailnet o: no notice
#   M03 the scheduler requests the move at scheduled_at, the owner hands over
#       before the target loads (logs), epoch + 1, move HANDED_OVER; bootstrap
#       gives the target URL
#   M04 mesh-rebootstrap (Q5), started before scheduled_at with -from <source
#       URL>, re-logs all N clients, PAR at a time: t50/t90/t100 (Q7); every
#       client back Running at the target with the same node ID and IP (Q4:
#       nodes kept); ping gap of the pair over the whole move; a client not
#       Running gets `tailscale debug restun` (owner decision (a)), counted
#       and named as a manual intervention
#   M05 control tailnet o untouched: owner/epoch, pings, no notice
# Then:
#   M06 audit: one owner per tailnet, reconcile report empty, no move open
#   M07 artifacts hold no secret
#
# Optional client VMs (VM_TARGETS="mesh@10.77.20.11 mesh@10.77.30.11"; Q7's
# "+ 2 client VMs"; default none): a second tailscaled per VM from
# /opt/tailscale/<round 1 version> as transient unit p12b-ts (own state,
# socket, tun p12bts0, port 41642, netfilter off, DNS untouched), enrolled in
# m1; re-bootstrapped by mesh-rebootstrap running on the VM itself (unit
# p12b-rb). Reached through mc-ci (ProxyCommand). Never while another run
# (N1) uses the client VMs.
#
# Usage: BIN=<headscale> PLBIN=<mesh-placement> BOOT_BIN=<mesh-bootstrap> RB_BIN=<mesh-rebootstrap> run.sh
#        (linux/amd64). N=24 PAR=8 LEAD=60 IMAGES="tailscale/tailscale:v1.104.7 tailscale/tailscale:v1.84.3"
#    or: CI_BINDIR=<dir on mc-ci holding headscale, mesh-placement,
#        mesh-bootstrap, mesh-rebootstrap built there> run.sh
#        The lab hosts then fetch the binaries from mc-ci over the LAN
#        (transient unit p12b-binsrv: plain HTTP on mc-ci's LAN address,
#        binaries only, stopped after setup) and check their sha256; nothing
#        large crosses the laptop's route (runs 20261126T041654Z and
#        T042420Z failed copying 81 MB from the laptop over Tailscale).
# Thresholds: none yet (owner Q7: fixed after the first measurement); the
# measurements are recorded, PASS/FAIL covers correctness only.
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
#
# Operating rules (lessons of the N1 and P12 runs):
# - Never edit this file while a run is in progress: bash reads a running
#   script incrementally, an edit changes what the run executes. Edit a copy
#   and swap it in after the run.
# - Start it in the foreground of a background task (not with `&` inside
#   one): an orphaned run keeps going and its cleanup breaks later runs.
# - Every ssh carries keepalives and every long remote command a `timeout`;
#   jobs that must outlive one ssh session run on the remote host as
#   transient systemd units (jobs.sh) and cleanup stops them first.
# shellcheck disable=SC2029
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
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
IMAGES=${IMAGES:-tailscale/tailscale:v1.104.7 tailscale/tailscale:v1.84.3}
N=${N:-24}
PAR=${PAR:-8}
LEAD=${LEAD:-60}
VM_TARGETS=${VM_TARGETS:-}
CI_BINDIR=${CI_BINDIR:-}
CI_LAN=${CI_SSH#*@}
BINPORT=${BINPORT:-18089}
if [ -n "$CI_BINDIR" ]; then
  [[ $CI_BINDIR =~ ^/[A-Za-z0-9/_.-]+$ ]] || { echo "CI_BINDIR: absolute path, [A-Za-z0-9/_.-] only" >&2; exit 2; }
  BIN=- PLBIN=- BOOT_BIN=- RB_BIN=-
else
  BIN=${BIN:?BIN= linux/amd64 cluster binary (or CI_BINDIR=)}
  PLBIN=${PLBIN:?PLBIN= linux/amd64 mesh-placement binary}
  BOOT_BIN=${BOOT_BIN:?BOOT_BIN= linux/amd64 mesh-bootstrap binary}
  RB_BIN=${RB_BIN:?RB_BIN= linux/amd64 mesh-rebootstrap binary}
fi
KEEP=${KEEP:-0}
LABEL=mesh-e2e=p12b
PY=${PY:-python}
# Windows Python reads stdin as cp1252; tailscale JSON is UTF-8 (run at 843e24a:
# every snap came back empty).
export PYTHONUTF8=1 PYTHONIOENCODING=utf-8
REASON="p12b-e2e rebalance"
NOTICE="moves to another control cluster"

for t in $VM_TARGETS; do [[ $t =~ ^mesh@10\.77\.[0-9]+\.[0-9]+$ ]] || { echo "VM_TARGETS: mesh@10.77.x.y only ($t)" >&2; exit 2; }; done
[[ $N =~ ^[0-9]+$ ]] && [ "$N" -ge 2 ] && [[ $PAR =~ ^[0-9]+$ ]] && [ "$PAR" -ge 1 ] || { echo "bad N/PAR" >&2; exit 2; }
# M02 needs ~15 s for its API checks between the last notice poll and scheduled_at.
[[ $LEAD =~ ^[0-9]+$ ]] && [ "$LEAD" -ge 45 ] || { echo "LEAD must be >= 45 s" >&2; exit 2; }
[ -n "$CI_BINDIR" ] || for f in "$BIN" "$PLBIN" "$BOOT_BIN" "$RB_BIN"; do [ -s "$f" ] || { echo "missing binary $f" >&2; exit 2; }; done

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_p12b$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/P12b/e2e-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

HS=/usr/local/bin/headscale-cluster-p12b
CFG=/etc/headscale/cluster/config-p12b.yaml
UNIT=mesh-p12b-e2e
PLUNIT=mesh-placement-p12b
BOOTUNIT=mesh-bootstrap-p12b
PLDIR=/run/p12b-placement
CIDIR=/dev/shm/p12b
# Transient units on mc-ci (jobs.sh): p12b-ping-<slug>, p12b-rb-<slug>.
CIRUN="sudo systemd-run --quiet --collect --uid=mesh --gid=mesh -p SupplementaryGroups=docker -p RuntimeMaxSec=1200"
VMUNIT=p12b-ts
VMDIR=/run/p12b-ts
VMSTATE=/var/lib/p12b-ts
VMSOCK=$VMDIR/tailscaled.sock
VMVER=${IMAGES%% *}; VMVER=${VMVER##*:v} # VMs join round 1: the first image's version
CLI1="sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $C1_ID"
CLI2="sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $C2_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
# Keepalives: the laptop reaches the lab through a Tailscale subnet route; a
# dropped session must fail within a minute, not hang the run (P12 at de8a562
# hung 2 h 48 min on a reset ssh to mc-ci).
SSHO=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
ssh_() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
scp_() { scp -q "${SSHO[@]}" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }
pg_() { ssh_ "$PG_SSH" "$@"; }
sshof() { case $1 in cp) echo "$CP_SSH";; c2) echo "$C2_SSH";; c2b) echo "$C2B_SSH";; esac; }
on() { local n=$1; shift; ssh_ "$(sshof "$n")" "$@"; }
nodeof() { [ "$1" = c1 ] && echo cp || echo c2; }
# Binaries. binsha NAME: sha256 of NAME (headscale, mesh-placement,
# mesh-bootstrap, mesh-rebootstrap), from $TMPD/binsha (filled by binsrv_start
# with CI_BINDIR, else from the local files).
binlocal() { case $1 in headscale) echo "$BIN";; mesh-placement) echo "$PLBIN";; mesh-bootstrap) echo "$BOOT_BIN";; mesh-rebootstrap) echo "$RB_BIN";; esac; }
binsha() {
  if [ -n "$CI_BINDIR" ]; then awk -v n="$1" '$2 == n { print $1 }' "$TMPD/binsha"
  else sha256sum "$(binlocal "$1")" | cut -d' ' -f1; fi
}
binsrv_start() {
  [ -n "$CI_BINDIR" ] || return 0
  ci_ "cd $CI_BINDIR && sha256sum headscale mesh-placement mesh-bootstrap mesh-rebootstrap" | strip_cr >"$TMPD/binsha" || return 1
  [ "$(awk 'length($1) == 64' "$TMPD/binsha" | wc -l)" = 4 ] || { log "CI_BINDIR: missing binaries"; return 1; }
  ci_ "sudo systemctl stop p12b-binsrv 2>/dev/null; sudo systemctl reset-failed p12b-binsrv 2>/dev/null; \
    sudo systemd-run --quiet --collect --unit=p12b-binsrv --uid=mesh --gid=mesh -p RuntimeMaxSec=900 \
      python3 -m http.server $BINPORT --bind $CI_LAN --directory $CI_BINDIR; \
    for i in \$(seq 1 20); do curl -fsS -o /dev/null -m 2 http://$CI_LAN:$BINPORT/ && exit 0; sleep 0.5; done; exit 1" || { log "p12b-binsrv not answering"; return 1; }
}
binsrv_stop() { [ -z "$CI_BINDIR" ] || ci_ "sudo systemctl stop p12b-binsrv 2>/dev/null; sudo systemctl reset-failed p12b-binsrv 2>/dev/null; ! systemctl is-active -q p12b-binsrv"; }
# put NODE NAME DEST: DEST on NODE (cp, c2, c2b) becomes binary NAME, sha256
# checked on NODE. With CI_BINDIR NODE fetches it from mc-ci over the LAN.
put() {
  local n=$1 name=$2 dst=$3 sum; sum=$(binsha "$2")
  [ ${#sum} = 64 ] || return 1
  if [ -n "$CI_BINDIR" ]; then
    on "$n" "curl -fsS --retry 3 -m 120 -o $dst http://$CI_LAN:$BINPORT/$name" || return 1
  else
    scp_ "$(binlocal "$name")" "$(sshof "$n"):$dst" || return 1
  fi
  on "$n" "echo '$sum  $dst' | sha256sum -c --quiet" || { log "$n: $name sha256 mismatch"; return 1; }
}
# binstdin NAME: the binary on stdout (for an install through ssh stdin).
binstdin() { if [ -n "$CI_BINDIR" ]; then ci_ "cat $CI_BINDIR/$1"; else cat "$(binlocal "$1")"; fi; }
urlof() { [ "$1" = c1 ] && echo $URL1 || echo $URL2; }
idof() { [ "$1" = c1 ] && echo $C1_ID || echo $C2_ID; }
otherc() { [ "$1" = c1 ] && echo c2 || echo c1; }
cli() { local c=$1; shift; if [ "$c" = c1 ]; then cp_ "$CLI1 $*"; else on c2 "$CLI2 $*"; fi; }
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }
now_ms() { py 'import time; print(int(time.time()*1000))' | strip_cr; }
iso_in() { py 'import datetime,sys; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1]))).replace(microsecond=0).isoformat().replace("+00:00","Z"))' "$1" | strip_cr; }

FAILS=0
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-6s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------ processes ---
public_ready() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$(urlof "$1")/ready" || true; }
wait_public_ready() { local i; for i in $(seq 1 "$2"); do [ "$(public_ready "$1")" = 200 ] && return 0; sleep 1; done; return 1; }
start_proc() { # CLUSTER
  on "$(nodeof "$1")" "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    $HS -c $CFG cluster serve --cluster-id $(idof "$1")"
}
proc_log() { on "$(nodeof "$1")" "sudo journalctl -u $UNIT --since -${2}s --no-pager -o short-iso-precise" 2>/dev/null; }
psql_() { pg_ "sudo docker exec mc-pg psql -At -U postgres -d $PG_DB -c \"$1\""; }

# --------------------------------------------------------------- clients ---
start_ts() {
  ci_ "docker run -d --name $1 --hostname $1 --label $LABEL --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL $2 \
    tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done"
}
ts_up() { # NAME URL KEYFILE [EXTRA]
  ci_ "docker exec -i $1 timeout 60 tailscale up --login-server=$2 --auth-key=file:/dev/stdin --hostname=$1 ${4:-}" \
    <"$3" 2>&1 | redact >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "timeout 15 docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
npeers() { field "$1" "len(d.get('Peer') or {})"; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
health_text() { field "$1" "' | '.join(d.get('Health') or [])"; }
# snap NAME...: the status of many clients in one ssh round trip (one per
# client takes ~0.7 s from the laptop: 24 clients would outlast LEAD).
# $TMPD/snap: "<name> <state> <ip> <notice True|False> <peers>", "- - -" for
# a client that did not answer (status --json has no trailing newline, so each
# marker starts on a fresh line). sget NAME COLUMN (2 state, 3 ip, 4 notice, 5 peers).
snap() {
  ci_ "for c in $*; do printf '\n@@ %s\n' \$c; timeout 10 docker exec \$c tailscale status --json 2>/dev/null; done" 2>/dev/null \
    | py 'import json,re
parts = re.split(r"^@@ (\S+)\s*$", sys.stdin.read(), flags=re.M)
for name, body in zip(parts[1::2], parts[2::2]):
    try:
        d = json.loads(body)
        ips = (d.get("Self") or {}).get("TailscaleIPs") or ["-"]
        print(name, d.get("BackendState") or "-", ips[0], any(sys.argv[1] in h for h in (d.get("Health") or [])), len(d.get("Peer") or {}))
    except Exception as e:
        print("snap %s: %s %s (%d bytes)" % (name, type(e).__name__, e, len(body)), file=sys.stderr)
        print(name, "-", "-", "-", "-")' "$NOTICE" 2>>"$ART_DIR/logs/snap.err" | strip_cr >"$TMPD/snap"
}
sget() { awk -v n="$1" -v c="$2" '$1 == n { print $c }' "$TMPD/snap"; }
scount() { awk -v c="$1" -v v="$2" '$c == v { k++ } END { print k + 0 }' "$TMPD/snap"; }
ping_ok() { ci_ "timeout 30 docker exec $1 tailscale ping -c 3 --timeout 5s $2" >>"$ART_DIR/logs/$1-ping.log" 2>&1; }
save_logs() { ci_ "timeout 30 docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }

# Tailnets: OWNER_<slug> = c1|c2, ID_<slug>.
owner() { eval "echo \$OWNER_$1"; }
tid() { eval "echo \$ID_$1"; }
# nodes_list SLUG: "<hostname> <node id> <ipv4> <short node key>" per node, by hostname (from its owner's DB view).
nodes_list() {
  cli "$(owner "$1")" tailnet nodes --slug "$1" 2>/dev/null | py 'import json
for n in sorted(json.load(sys.stdin), key=lambda n: n["hostname"]):
    print(n["hostname"], n["id"], n["ipv4"] or "-", n["node_key"])' | strip_cr
  echo
}

# placement API over mTLS on mc-cp loopback: pl METHOD PATH [IDEMPOTENCY-KEY] [BODY]
pl() {
  local ik=${3:-} b=${4:-}
  cp_ "sudo curl -s -m 10 --cacert $PLDIR/ca.crt --cert $PLDIR/op.crt --key $PLDIR/op.key -X $1 \
    ${ik:+-H 'Idempotency-Key: $ik'} ${b:+-H 'Content-Type: application/json' --data '$b'} https://127.0.0.1:9443$2"
}
jget() { py "import json; d=json.loads(sys.argv[1]); print($2)" "$1" 2>/dev/null | strip_cr; }
findings() { jget "$(pl GET /v1/reconcile)" "json.dumps(d.get('findings'))"; }

# boot_req KEYFILE TENANT -> "<status> <body>" (key id + SHA-256 proof only)
boot_req() {
  local body out=$TMPD/boot.out code
  body=$(py 'import hashlib,json
k=open(sys.argv[1]).read().strip()
i,s=k.split("hskey-auth-",1)[1].split("-",1)
sys.stdout.write(json.dumps({"credential":{"kind":"authkey_proof","key_id":i,"proof":hashlib.sha256(s.encode()).hexdigest()},"tenant":sys.argv[2]}))' "$1" "$2" | strip_cr)
  code=$(printf %s "$body" | curl -s -m 10 -o "$out" -w '%{http_code}' -H 'Content-Type: application/json' --data-binary @- "$BOOT_URL/v1/bootstrap")
  printf '%s %s' "$code" "$(cat "$out" 2>/dev/null)"; rm -f "$out"
}
boot_url() { py 'import json; d=json.loads(sys.argv[1].split(" ",1)[1]); print(d.get("control_url",""))' "$1" 2>/dev/null | strip_cr; }

# tenant_api SLUG PATH -> body, with the tailnet's API key at its owner's URL
tenant_api() { curl -s -m 10 -H "Authorization: Bearer $(cat "$TMPD/api-$1")" "$(urlof "$(owner "$1")")$2"; }

# ------------------------------------------------------------- tailnets ---
# new_tailnet SLUG: created through placement, activated, allow-all policy,
# reusable pre-auth key ($TMPD/key-SLUG) and API key ($TMPD/api-SLUG).
new_tailnet() {
  local s=$1 r id o pf=/var/lib/headscale/cluster/p12b-policy.hujson pol='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}'
  r=$(pl POST /v1/tailnets "p12b-t-$s" "{\"slug\":\"$s\",\"class\":\"shared\"}")
  printf 'create %s %s\n' "$s" "$r" >>"$ART_DIR/logs/placement.log"
  id=$(jget "$r" "d['id']"); o=$(jget "$r" "d['owner_cluster_id']")
  [ -n "$id" ] || return 1
  eval "ID_$s=$id"; if [ "$o" = $C1_ID ]; then eval "OWNER_$s=c1"; else eval "OWNER_$s=c2"; fi
  pl POST "/v1/tailnets/$id/activate" "p12b-a-$s" '{"expected_epoch":1}' >>"$ART_DIR/logs/placement.log" || return 1
  printf '%s\n' "$pol" | if [ "$(owner "$s")" = c1 ]; then cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI1 tailnet policy --slug $s --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"
    else on c2 "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI2 tailnet policy --slug $s --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc"; fi >/dev/null || return 1
  cli "$(owner "$s")" tailnet authkey --slug "$s" --user "e2e-$s" --reusable --expiration 180m \
    | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s"
  chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  r=$(pl POST "/v1/tailnets/$id/apikeys" "p12b-k-$s" '{"expires_in_seconds":10800}')
  py 'import json; print(json.loads(sys.argv[1])["key"])' "$r" | strip_cr >"$TMPD/api-$s"; chmod 600 "$TMPD/api-$s"
  [ -s "$TMPD/api-$s" ] || return 1
  local i; for i in $(seq 1 20); do proc_log "$(owner "$s")" 120 | grep -q "loaded tailnet runtime.*slug=$s" && return 0; sleep 1; done
  return 1
}

# enroll SLUG IMAGE NAME...: start and enroll at the URL bootstrap gives, PAR at a time.
enroll() {
  local s=$1 img=$2 u c n=0 rc=0; shift 2
  u=$(boot_url "$(boot_req "$TMPD/key-$s" "$s")"); [ "$u" = "$(urlof "$(owner "$s")")" ] || return 1
  for c in "$@"; do
    { start_ts "$c" "$img" </dev/null && ts_up "$c" "$u" "$TMPD/key-$s"; } >/dev/null 2>&1 || echo "$c" >>"$TMPD/enroll-failed" &
    n=$((n + 1)); [ $((n % PAR)) = 0 ] && wait
  done
  wait
  [ ! -s "$TMPD/enroll-failed" ] || { rc=1; log "enroll failed: $(tr '\n' ' ' <"$TMPD/enroll-failed")"; rm -f "$TMPD/enroll-failed"; }
  return $rc
}

# -------------------------------------------------------------- VM clients ---
# Client VMs (10.77.x) are reached through mc-ci (route 10.77.0.0/16); -J
# does not pass -i to the jump, hence ProxyCommand. Their tailscale builds
# live in /opt/tailscale/<version> (no /usr/sbin/tailscaled on them).
PX="ProxyCommand=ssh -i $SSH_KEY -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 $CI_SSH nc -q0 %h %p"
vm_() { local t=$1; shift; ssh "${SSHO[@]}" -o StrictHostKeyChecking=accept-new -o "$PX" "$t" "$@"; }
VMTS="sudo timeout 30 /opt/tailscale/$VMVER/tailscale --socket=$VMSOCK"
# vm_start TARGET SLUG URL: a second tailscaled (own state, socket, tun, port;
# --netfilter-mode=off and --accept-dns=false leave the VM's firewall and DNS
# alone), enrolled at URL; mesh-rebootstrap and jobs.sh copied for M04.
vm_start() {
  vm_ "$1" "test -x /opt/tailscale/$VMVER/tailscaled || exit 3
    sudo systemctl stop p12b-rb $VMUNIT 2>/dev/null; sudo systemctl reset-failed p12b-rb $VMUNIT 2>/dev/null
    sudo rm -rf $VMSTATE $VMDIR && sudo install -d -m 0700 $VMSTATE $VMDIR || exit 1
    sudo systemd-run --quiet --unit=$VMUNIT /opt/tailscale/$VMVER/tailscaled --state=$VMSTATE/tailscaled.state \
      --socket=$VMSOCK --tun=p12bts0 --port=41642 || exit 1
    for i in \$(seq 1 60); do $VMTS status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done; exit 1" || return 1
  vm_ "$1" "sudo install -m 0600 /dev/stdin $VMDIR/key" <"$TMPD/key-$2" || return 1
  # /run is noexec on the client VMs: the binary lives under $VMSTATE.
  binstdin mesh-rebootstrap | vm_ "$1" "sudo install -m 0700 /dev/stdin $VMSTATE/mesh-rebootstrap && echo '$(binsha mesh-rebootstrap)  $VMSTATE/mesh-rebootstrap' | sudo sha256sum -c --quiet" || return 1
  tr -d '\r' <"$HERE/jobs.sh" | vm_ "$1" "sudo install -m 0700 /dev/stdin $VMDIR/jobs.sh && sudo bash -n $VMDIR/jobs.sh" || return 1
  vm_ "$1" "sudo timeout 60 /opt/tailscale/$VMVER/tailscale --socket=$VMSOCK up --login-server=$3 --auth-key=file:$VMDIR/key \
    --hostname=\$(hostname -s)-p12b --accept-dns=false --netfilter-mode=off" 2>&1 | redact >>"$ART_DIR/logs/vm-$1.cli.log"
  return "${PIPESTATUS[0]}"
}
# vm_rb TARGET SLUG FROM_URL: mesh-rebootstrap on the VM itself (transient
# unit p12b-rb, jobs.sh rb-vm), waiting for a URL other than FROM_URL.
vm_rb() {
  vm_ "$1" "sudo rm -f $VMDIR/rb.done; sudo systemctl reset-failed p12b-rb 2>/dev/null; \
    sudo systemd-run --quiet --collect --unit=p12b-rb -p RuntimeMaxSec=900 bash $VMDIR/jobs.sh rb-vm $VMDIR $VMVER $BOOT_URL $3 $2 $VMSTATE/mesh-rebootstrap"
}
# vm_state TARGET -> "<state> <ip> <notice True|False>"
vm_state() { vm_ "$1" "$VMTS status --json" 2>/dev/null | py 'import json
try:
    d = json.load(sys.stdin); ips = (d.get("Self") or {}).get("TailscaleIPs") or ["-"]
    print(d.get("BackendState") or "-", ips[0], any(sys.argv[1] in h for h in (d.get("Health") or [])))
except Exception:
    print("- - -")' "$NOTICE" | strip_cr; }
vm_stop() {
  vm_ "$1" "sudo systemctl stop p12b-rb $VMUNIT 2>/dev/null; sudo systemctl reset-failed p12b-rb $VMUNIT 2>/dev/null; \
    sudo rm -rf $VMSTATE $VMDIR; ! ip link show p12bts0 >/dev/null 2>&1 && ! sudo test -e $VMDIR"
}

# ---------------------------------------------------------------- cleanup ---
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  binsrv_stop || { log "p12b-binsrv not stopped"; CLEAN_OK=0; }
  # Jobs left running on mc-ci (pinger, mesh-rebootstrap) are stopped before
  # their containers and files go.
  ci_ "touch $CIDIR/stop-ping 2>/dev/null; sudo systemctl stop 'p12b-ping-*' 'p12b-rb-*' 2>/dev/null; \
       sudo systemctl reset-failed 'p12b-ping-*' 'p12b-rb-*' 2>/dev/null; \
       ! pgrep -f '$CIDIR/(mesh-rebootstrap|jobs.sh)' >/dev/null" || { log "mc-ci jobs not stopped"; CLEAN_OK=0; }
  for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; rm -rf $CIDIR; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\" && ! test -e $CIDIR" || { log "mc-ci clients/files not removed"; CLEAN_OK=0; }
  local t; for t in $VM_TARGETS; do vm_stop "$t" || { log "$t: p12b tailscaled not removed"; CLEAN_OK=0; }; done
  local n
  for n in cp c2; do
    on $n "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; \
      sudo rm -f $CFG $HS; ! sudo test -e $CFG && ! test -e $HS" || { log "$n: run files not removed"; CLEAN_OK=0; }
  done
  cp_ "for u in $PLUNIT $BOOTUNIT mesh-c1-ha; do sudo systemctl stop \$u 2>/dev/null; sudo systemctl reset-failed \$u 2>/dev/null; done; \
       sudo rm -rf $PLDIR /etc/haproxy/mesh-c1-p12b.cfg /usr/local/bin/mesh-placement-p12b /usr/local/bin/mesh-bootstrap-p12b; ! sudo test -e $PLDIR" \
    || { log "mc-cp placement/bootstrap/HAProxy not removed"; CLEAN_OK=0; }
  cp_ "sudo rm -f /etc/headscale/cluster/pg.pass; ! sudo test -e /etc/headscale/cluster/pg.pass && sudo systemctl start headscale-tailnets" \
    || { log "mc-cp POC not restarted"; CLEAN_OK=0; }
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
trap 'cleanup; exit 2' INT TERM HUP

# ------------------------------------------------------------------ setup ---
setup() {
  local pass i n
  # $CIDIR is under /dev/shm: when the last session of the ssh user on mc-ci
  # ends, logind (RemoveIPC=yes, the default) deletes the user's files there
  # (run 20261126T043351Z lost key, jobs.sh and mesh-rebootstrap mid-run).
  # Linger keeps the user's manager alive; checked before anything starts.
  ci_ "loginctl show-user \$(id -un) -p Linger | grep -qx Linger=yes" \
    || { log "mc-ci: no linger for the ssh user (sudo loginctl enable-linger <user>); logind would wipe $CIDIR"; return 1; }
  pass=$(grep -E '^MC_DB_APP_PASSWORD=' "$MC_DB_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || return 1
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"
  log "database $PG_DB"
  pg_ "sudo docker exec mc-pg createdb -U postgres -O headscale $PG_DB" || return 1

  binsrv_start || return 1
  log "binary $HS on mc-cp and mc-c2${CI_BINDIR:+ (fetched from mc-ci $CI_BINDIR over the LAN)}"
  for n in cp c2; do
    put $n headscale /tmp/p12b-hs || return 1
    on $n "sudo install -m 0755 /tmp/p12b-hs $HS && rm -f /tmp/p12b-hs" || return 1
  done

  log "c1 config (e2e database; mc-cp on its LAN address behind HAProxy)"
  awk -v host="${PG_LISTEN%:*}" -v port="${PG_LISTEN##*:}" -v db="$PG_DB" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    host: " host "\n    port: " port \
      "\n    name: " db "\n    user: headscale\n    pass: file:/etc/headscale/cluster/pg.pass\n    ssl: disable"; skip = 1; next }
    skip && /^[^ ]/ { skip = 0 }
    !skip { print }' "$REPO/infra/lab/mc-cp/cluster/config.yaml" | sed "s#^listen_addr: .*#listen_addr: 192.168.1.241:8443#" >"$TMPD/c1.yaml"
  grep -q "^listen_addr: 192.168.1.241:8443" "$TMPD/c1.yaml" || return 1
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; \
       sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && \
       sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster && \
       sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/cluster/pg.pass" <"$TMPD/pg.pass" || return 1
  cp_ "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1.yaml" || return 1
  cp_ "sudo test -s /var/lib/headscale/cluster/noise_private.key" || return 1

  log "mc-c2, mc-c2b: stop long-lived c2; run config = mc-c2's on $PG_DB (mc-c2b stays stopped)"
  for n in c2 c2b; do on $n "sudo systemctl stop headscale-cluster" || return 1; done
  on c2 "set -e; sudo sed 's/^    name: .*/    name: $PG_DB/' /etc/headscale/cluster/config.yaml | sudo install -o root -g headscale -m 0640 /dev/stdin $CFG
    sudo grep -q '^    name: $PG_DB\$' $CFG" || return 1

  log "HAProxy for c1 on mc-cp ($URL1 -> mc-cp)"
  cp_ "sudo tee /etc/haproxy/mesh-c1-p12b.cfg >/dev/null && sudo haproxy -c -q -f /etc/haproxy/mesh-c1-p12b.cfg" <<EOF || return 1
# P12b E2E only (infra/lab/e2e/p12b/run.sh).
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
  server mc-cp 192.168.1.241:8443
EOF
  cp_ "sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo systemd-run --quiet --unit=mesh-c1-ha /usr/sbin/haproxy -Ws -f /etc/haproxy/mesh-c1-p12b.cfg" || return 1

  log "mesh-placement on mc-cp loopback (throwaway CA under $PLDIR, scheduler every 2 s)"
  put cp mesh-placement /tmp/p12b-pl || return 1
  cp_ "set -e; sudo install -m 0755 /tmp/p12b-pl /usr/local/bin/mesh-placement-p12b; rm -f /tmp/p12b-pl
    sudo install -d -o root -g headscale -m 0751 $PLDIR; cd $PLDIR
    sudo openssl ecparam -name prime256v1 -genkey -noout -out ca.key 2>/dev/null
    sudo openssl req -x509 -new -key ca.key -subj /CN=p12b-e2e-ca -days 1 -out ca.crt 2>/dev/null
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
    sudo systemd-run --quiet --unit=$PLUNIT --uid=headscale --gid=headscale /usr/local/bin/mesh-placement-p12b \
      -config $CFG -listen 127.0.0.1:9443 -tls-cert $PLDIR/srv.crt -tls-key $PLDIR/srv.key \
      -client-ca $PLDIR/ca.crt -operators spiffe://mesh/operator/e2e -schedule-interval 2s" || return 1
  for i in $(seq 1 30); do pl GET /v1/clusters >/dev/null 2>&1 && break; sleep 0.5; done
  pl GET /v1/clusters | grep -q '"clusters"' || { log "placement API not answering"; return 1; }

  log "register c1 and c2 through mesh-placement (ACTIVE, shared)"
  pl POST /v1/clusters p12b-c1 "{\"id\":\"$C1_ID\",\"endpoint_url\":\"$URL1\",\"class\":\"shared\",\"state\":\"ACTIVE\",\"max_devices\":5000}" >/dev/null || return 1
  pl POST /v1/clusters p12b-c2 "{\"id\":\"$C2_ID\",\"endpoint_url\":\"$URL2\",\"class\":\"shared\",\"state\":\"ACTIVE\",\"max_devices\":5000}" >/dev/null || return 1

  log "start c1 (mc-cp) and c2 (mc-c2)"
  start_proc c1 && start_proc c2 || return 1
  wait_public_ready c1 40 && wait_public_ready c2 40 || { log "public /ready (c1=$(public_ready c1) c2=$(public_ready c2))"; return 1; }

  log "bootstrap on mc-cp ($BOOT_URL)"
  put cp mesh-bootstrap /tmp/p12b-boot || return 1
  cp_ "set -e; sudo install -m 0755 /tmp/p12b-boot /usr/local/bin/mesh-bootstrap-p12b; rm -f /tmp/p12b-boot
    sudo systemctl reset-failed $BOOTUNIT 2>/dev/null || true
    sudo systemd-run --quiet --unit=$BOOTUNIT --uid=headscale --gid=headscale \
      /usr/local/bin/mesh-bootstrap-p12b -config $CFG -listen $BOOT_LISTEN \
      -tls-cert /etc/headscale/tls/fullchain.pem -tls-key /etc/headscale/tls/privkey.pem -hostname $BOOT_HOST" || return 1
  for i in $(seq 1 20); do [ "$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BOOT_URL/health")" = 200 ] && break; sleep 1; done
  [ "$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BOOT_URL/health")" = 200 ] || { log "bootstrap not healthy"; return 1; }

  log "mesh-rebootstrap and images on mc-ci"
  ci_ "rm -rf $CIDIR; install -d -m 0700 $CIDIR" || return 1
  if [ -n "$CI_BINDIR" ]; then ci_ "cp $CI_BINDIR/mesh-rebootstrap $CIDIR/mesh-rebootstrap" || return 1
  else scp_ "$RB_BIN" "$CI_SSH:$CIDIR/mesh-rebootstrap" || return 1; fi
  ci_ "echo '$(binsha mesh-rebootstrap)  $CIDIR/mesh-rebootstrap' | sha256sum -c --quiet" || { log "mc-ci: mesh-rebootstrap sha256 mismatch"; return 1; }
  tr -d '\r' <"$HERE/jobs.sh" | ci_ "cat >$CIDIR/jobs.sh && chmod 0700 $CIDIR/mesh-rebootstrap $CIDIR/jobs.sh && bash -n $CIDIR/jobs.sh" || return 1
  ci_ "sudo -n systemd-run --version >/dev/null" || { log "mc-ci: no passwordless systemd-run"; return 1; }
  local img; for img in $IMAGES; do ci_ "docker image inspect $img >/dev/null 2>&1 || docker pull -q $img >/dev/null" || return 1; done
  binsrv_stop || { log "p12b-binsrv not stopped"; return 1; }
}

# ------------------------------------------------------------------ cases ---
# M00: control tailnet o (2 clients) that must not notice anything.
t_m00() {
  local p=""
  new_tailnet o || { record M00 FAIL "control tailnet o"; return 1; }
  enroll o "${IMAGES%% *}" p12b-o1 p12b-o2 || p="$p; enroll o"
  local i; for i in $(seq 1 30); do [ "$(npeers p12b-o1)" = 1 ] && break; sleep 1; done
  ping_ok p12b-o1 p12b-o2 || p="$p; o1 cannot ping o2"
  O_ROW=$(jget "$(pl GET "/v1/tailnets/$(tid o)")" "d['owner_cluster_id']+'@'+str(d['owner_epoch'])")
  [ -z "$p" ] && record M00 PASS "control tailnet o on $(owner o) ($O_ROW): o1<->o2" || record M00 FAIL "${p#; }"
}

# pinger SLUG A IPB: on mc-ci, transient unit p12b-ping-SLUG (jobs.sh ping),
# one ICMP echo every 200 ms through A's tun, until $CIDIR/stop-ping (or
# 15 min). Lines: "<ms> <rc>".
start_pinger() {
  ci_ "rm -f $CIDIR/stop-ping $CIDIR/ping-$1; sudo systemctl reset-failed p12b-ping-$1 2>/dev/null; \
    $CIRUN --unit=p12b-ping-$1 bash $CIDIR/jobs.sh ping $2 $3 $CIDIR/ping-$1 $CIDIR/stop-ping 900"
}
stop_pinger() {
  ci_ "touch $CIDIR/stop-ping; for i in \$(seq 1 20); do systemctl is-active -q p12b-ping-$1 || break; sleep 0.5; done; \
    sudo systemctl stop p12b-ping-$1 2>/dev/null; cat $CIDIR/ping-$1; rm -f $CIDIR/ping-$1 $CIDIR/stop-ping"
}
# ping_gap FILE -> "<longest gap ms> <failed samples>/<samples>"
ping_gap() { py 'import sys
s=[tuple(map(int,l.split())) for l in open(sys.argv[1]) if len(l.split())==2]
gap=0; start=None; fails=0
for t,rc in s:
    if rc!=0:
        fails+=1
        start = t if start is None else start
    elif start is not None:
        gap=max(gap,t-start); start=None
if start is not None: gap=max(gap,(s[-1][0]-start) if s else 0)
print(gap, "%d/%d" % (fails, len(s)))' "$1" | strip_cr; }

# round R IMAGE: M01-M05 for tailnet m<R> with N clients of IMAGE.
t_round() {
  local r=$1 img=$2 s=m$1 p="" c i ver=${2##*:}
  local names=(); for i in $(seq -w 1 "$N"); do names+=("p12b-$s-$i"); done
  local a=${names[0]} b=${names[1]}

  # M01
  new_tailnet "$s" || { record "M01.$r" FAIL "$ver: tailnet $s"; return; }
  local from to; from=$(owner "$s"); to=$(otherc "$from")
  enroll "$s" "$img" "${names[@]}" || p="$p; enroll"
  local vmt; for vmt in $([ "$r" = 1 ] && echo "$VM_TARGETS"); do vm_start "$vmt" "$s" "$(urlof "$from")" || p="$p; VM $vmt enroll"; done
  local want=$N; [ "$r" = 1 ] && want=$((N + $(wc -w <<<"$VM_TARGETS")))
  for i in $(seq 1 60); do [ "$(npeers "$a")" = $((want - 1)) ] && break; sleep 1; done
  [ "$(npeers "$a")" = $((want - 1)) ] || p="$p; $a sees $(npeers "$a") peers, want $((want - 1))"
  nodes_list "$s" >"$ART_DIR/logs/M01-$s-nodes-before.txt"
  [ "$(wc -l <"$ART_DIR/logs/M01-$s-nodes-before.txt")" = "$want" ] || p="$p; nodes $(wc -l <"$ART_DIR/logs/M01-$s-nodes-before.txt") want $want"
  ping_ok "$a" "$b" || p="$p; $a cannot ping $b"
  snap "${names[@]}"; for c in "${names[@]}"; do echo "$c $(sget "$c" 3)"; done >"$TMPD/ips-before-$s"
  [ "$(scount 2 Running)" = "$N" ] || p="$p; Running $(scount 2 Running)/$N"
  [ -z "$p" ] && record "M01.$r" PASS "$ver: tailnet $s on $from, $want clients enrolled at the URL bootstrap gave (PAR=$PAR); $a sees $((want - 1)) peers; ping ok" \
    || { record "M01.$r" FAIL "$ver: ${p#; }"; return; }

  # M02 schedule + notice. The pinger and mesh-rebootstrap (waiting for a URL
  # other than the source's) start right after scheduling, well before
  # scheduled_at; the notice and API checks must also end before it (after
  # it the source hands the tailnet over and stops serving its API).
  p=""; local at at_s; at=$(iso_in "$LEAD"); at_s=$(( $(date +%s) + LEAD ))
  local mv; mv=$(pl POST "/v1/tailnets/$(tid "$s")/moves" "p12b-m-$s" "{\"to_cluster\":\"$(idof "$to")\",\"scheduled_at\":\"$at\",\"reason\":\"$REASON\"}")
  printf 'schedule %s %s\n' "$s" "$mv" >>"$ART_DIR/logs/placement.log"
  local mid; mid=$(jget "$mv" "d['id']"); [ -n "$mid" ] && [ "$(jget "$mv" "d['state']")" = SCHEDULED ] || { record "M02.$r" FAIL "$ver: schedule [$mv]"; return; }
  local t_sched; t_sched=$(now_ms)
  local since; since=$(date +%s)
  start_pinger "$s" "$a" "$(selfip "$b")" || p="$p; pinger not started"
  printf '%s\n' "${names[@]}" | ci_ "cat >$CIDIR/targets-$s"
  ci_ "umask 077; cat >$CIDIR/key-$s" <"$TMPD/key-$s"
  ci_ "rm -f $CIDIR/rb-$s.done; sudo systemctl reset-failed p12b-rb-$s 2>/dev/null; \
    $CIRUN --unit=p12b-rb-$s bash $CIDIR/jobs.sh rb-docker $CIDIR $s $BOOT_URL $(urlof "$from") $PAR" || p="$p; mesh-rebootstrap not started"
  for vmt in $([ "$r" = 1 ] && echo "$VM_TARGETS"); do vm_rb "$vmt" "$s" "$(urlof "$from")" || p="$p; VM $vmt mesh-rebootstrap not started"; done
  local seen=0 t_seen=""
  while [ "$(date +%s)" -lt $((at_s - 15)) ]; do
    snap "${names[@]}"; seen=$(scount 4 True)
    [ "$seen" = "$N" ] && { t_seen=$(now_ms); break; }; sleep 1
  done
  [ "$seen" = "$N" ] || p="$p; notice on $seen/$N clients before scheduled_at"
  health_text "$a" >"$ART_DIR/logs/M02-$s-notice.txt"
  grep -qF "$REASON" "$ART_DIR/logs/M02-$s-notice.txt" || p="$p; reason not in the notice"
  grep -qiE "$(idof "$to")|$(urlof "$to")" "$ART_DIR/logs/M02-$s-notice.txt" && p="$p; notice names the target"
  for vmt in $([ "$r" = 1 ] && echo "$VM_TARGETS"); do vm_state "$vmt" | grep -q ' True$' || p="$p; VM $vmt no notice [$(vm_state "$vmt")]"; done
  local api; api=$(tenant_api "$s" /api/v2/tailnet/-/scheduled-move); printf '%s %s\n' "$s" "$api" >>"$ART_DIR/logs/M02-api.txt"
  jget "$api" "d['scheduled'] and d['reason']=='$REASON'" | grep -q True || p="$p; API [$api]"
  grep -qiE "$(idof "$to")|$HOST1|$HOST2" <<<"$api" && p="$p; API names a cluster"
  api=$(tenant_api o /api/v2/tailnet/-/scheduled-move); printf 'o %s\n' "$api" >>"$ART_DIR/logs/M02-api.txt"
  [ "$(jget "$api" "d['scheduled']")" = False ] || p="$p; o's API [$api]"
  snap p12b-o1 p12b-o2; [ "$(scount 4 False)" = 2 ] || p="$p; o's clients got a notice [$(tr '\n' ' ' <"$TMPD/snap")]"
  [ "$(date +%s)" -lt "$at_s" ] || p="$p; M02 checks ended after scheduled_at (raise LEAD)"
  [ -z "$p" ] && record "M02.$r" PASS "$ver: move $mid $from->$to at $at scheduled; notice on all $N clients in $(( ${t_seen:-$t_sched} - t_sched )) ms, reason shown, target never named; /api/v2 scheduled-move of $s shows it, o's not; o's clients no notice" \
    || record "M02.$r" FAIL "$ver: ${p#; }"

  # M03 + M04: wait for mesh-rebootstrap (started in M02). Bounded by wall
  # time (each check is an ssh round trip): scheduled_at, then at most
  # 3 batches x 120 s.
  p=""
  local end=$(( at_s + 480 ))
  while [ "$(date +%s)" -lt "$end" ]; do ci_ "test -e $CIDIR/rb-$s.done" && break; sleep 2; done
  local rbrc; rbrc=$(ci_ "cat $CIDIR/rb-$s.done 2>/dev/null" | strip_cr)
  [ -n "$rbrc" ] || ci_ "sudo systemctl stop p12b-rb-$s 2>/dev/null"
  ci_ "cat $CIDIR/rb-$s.json 2>/dev/null" | redact >"$ART_DIR/logs/rebootstrap-$s.json"
  ci_ "cat $CIDIR/rb-$s.log; rm -f $CIDIR/key-$s $CIDIR/rb-$s.* $CIDIR/targets-$s" | redact >"$ART_DIR/logs/rebootstrap-$s.log"
  # VMs: their own mesh-rebootstrap (rc per VM in VMRB_<n>).
  local vmrb="" vrc
  for vmt in $([ "$r" = 1 ] && echo "$VM_TARGETS"); do
    while [ "$(date +%s)" -lt "$end" ]; do vm_ "$vmt" "sudo test -e $VMDIR/rb.done" 2>/dev/null && break; sleep 2; done
    vrc=$(vm_ "$vmt" "sudo cat $VMDIR/rb.done 2>/dev/null" 2>/dev/null | strip_cr)
    [ -n "$vrc" ] || vm_ "$vmt" "sudo systemctl stop p12b-rb 2>/dev/null" >/dev/null 2>&1
    vm_ "$vmt" "sudo cat $VMDIR/rb.json 2>/dev/null" 2>/dev/null | redact >"$ART_DIR/logs/rebootstrap-$s-vm-${vmt#*@}.json"
    vm_ "$vmt" "sudo cat $VMDIR/rb.log 2>/dev/null" 2>/dev/null | redact >"$ART_DIR/logs/rebootstrap-$s-vm-${vmt#*@}.log"
    vmrb="$vmrb $vmt=${vrc:-none}"
  done
  # (mesh-rebootstrap's exit code is judged in M04, with the clients.)

  # M03 the move itself
  local m; m=$(pl GET "/v1/moves/$mid"); printf 'move %s %s\n' "$s" "$m" >>"$ART_DIR/logs/placement.log"
  [ "$(jget "$m" "d['state']")" = HANDED_OVER ] || p="$p; move state [$(jget "$m" "d['state']")]"
  local row; row=$(jget "$(pl GET "/v1/tailnets/$(tid "$s")")" "d['owner_cluster_id']+' '+str(d['owner_epoch'])+' '+str(d.get('moving_to_cluster_id'))")
  [ "$row" = "$(idof "$to") 2 None" ] || p="$p; row [$row]"
  eval "OWNER_$s=$to"
  [ "$(boot_url "$(boot_req "$TMPD/key-$s" "$s")")" = "$(urlof "$to")" ] || p="$p; bootstrap not at $to"
  local th tl
  th=$(proc_log "$from" $(( $(date +%s) - since + 5 )) | grep "tailnet handed over" | grep -F -- "$(tid "$s")" | grep -o '^[0-9T:.+-]*' | head -1)
  tl=$(proc_log "$to" $(( $(date +%s) - since + 5 )) | grep "loaded tailnet runtime.*slug=$s" | grep -o '^[0-9T:.+-]*' | head -1)
  [ -n "$th" ] && [ -n "$tl" ] && [[ "$th" < "$tl" ]] || p="$p; order handed-over=[$th] loaded=[$tl]"
  [ -z "$p" ] && record "M03.$r" PASS "$ver: requested $(jget "$m" "d.get('requested_at')"), $from handed over ($th) before $to loaded ($tl); epoch 2; move HANDED_OVER $(jget "$m" "d.get('finished_at')"); bootstrap -> $to" \
    || record "M03.$r" FAIL "$ver: ${p#; }"

  # M04 clients at the target, same IPs and node IDs; timings
  p=""
  snap "${names[@]}"
  # Owner decision (a), 2026-11-26: a client not Running after its re-login
  # gets `tailscale debug restun` (client 1.84.3 kept the previous control's
  # home DERP region when the new DERP map shared no region with it: N1 S05,
  # "derpMap.Regions[901] is nil", stuck in Starting). Documented operational
  # workaround, never silent: each one is a manual intervention, recorded in
  # the result and timings.txt.
  local stuck="" restunned=""
  for c in "${names[@]}"; do [ "$(sget "$c" 2)" = Running ] || stuck="$stuck $c"; done
  if [ -n "$stuck" ]; then
    printf 'M04 %s %s: not Running after mesh-rebootstrap:%s [%s]; debug restun\n' "$s" "$ver" "$stuck" "$(grep -v ' Running ' "$TMPD/snap" | awk '{print $1"="$2}' | tr '\n' ' ')" >>"$ART_DIR/timings.txt"
    for c in $stuck; do ci_ "timeout 15 docker exec $c tailscale debug restun" >/dev/null 2>&1; done
    for i in $(seq 1 30); do snap "${names[@]}"; [ "$(scount 2 Running)" = "$N" ] && break; sleep 1; done
    for c in $stuck; do [ "$(sget "$c" 2)" = Running ] && restunned="$restunned $c"; done
    printf 'M04 %s %s: Running after debug restun:%s\n' "$s" "$ver" "${restunned:- none}" >>"$ART_DIR/timings.txt"
  fi
  local manual; manual=$(wc -w <<<"$stuck")
  local back; back=$(scount 2 Running)
  [ "$back" = "$N" ] || p="$p; Running $back/$N${stuck:+ (after debug restun of$stuck)}"
  # mesh-rebootstrap failed only for clients that the re-STUN brought back?
  if [ "$rbrc" != 0 ]; then
    local failed_t; failed_t=$(py 'import json
d=json.load(open(sys.argv[1], encoding="utf-8")); print(" ".join(x["target"] for x in d.get("results") or [] if not x.get("ok")))' "$ART_DIR/logs/rebootstrap-$s.json" 2>/dev/null | strip_cr)
    if [ -z "$failed_t" ]; then p="$p; mesh-rebootstrap rc=${rbrc:-none}"
    else for c in $failed_t; do grep -qw -- "$c" <<<"$restunned" || p="$p; $c failed in mesh-rebootstrap (rc=${rbrc:-none})"; done; fi
  fi
  for c in "${names[@]}"; do echo "$c $(sget "$c" 3)"; done >"$TMPD/ips-after-$s"
  cmp -s "$TMPD/ips-before-$s" "$TMPD/ips-after-$s" || p="$p; IPs changed: $(diff "$TMPD/ips-before-$s" "$TMPD/ips-after-$s" | grep -c '^>')"
  for i in $(seq 1 60); do [ "$(npeers "$a")" = $((want - 1)) ] && break; sleep 1; done
  [ "$(npeers "$a")" = $((want - 1)) ] || p="$p; $a sees $(npeers "$a") peers at $to"
  # Node list after (Q4: nodes kept; same node ID and IP per hostname; the
  # short node key is recorded, a re-login may rotate it).
  nodes_list "$s" >"$ART_DIR/logs/M04-$s-nodes-after.txt"
  [ "$(wc -l <"$ART_DIR/logs/M04-$s-nodes-after.txt")" = "$want" ] || p="$p; nodes at $to $(wc -l <"$ART_DIR/logs/M04-$s-nodes-after.txt") want $want (old nodes kept, Q4)"
  cut -d' ' -f1-3 "$ART_DIR/logs/M01-$s-nodes-before.txt" >"$TMPD/nb"; cut -d' ' -f1-3 "$ART_DIR/logs/M04-$s-nodes-after.txt" >"$TMPD/na"
  cmp -s "$TMPD/nb" "$TMPD/na" || p="$p; node id/IP per hostname changed: $(diff "$TMPD/nb" "$TMPD/na" | grep '^>' | head -3 | tr '\n' ' ')"
  ping_ok "$a" "$b" || p="$p; $a cannot ping $b at $to"
  snap "$a"; [ "$(sget "$a" 4)" = False ] || p="$p; notice still shown at $to"
  local vs
  for vmt in $([ "$r" = 1 ] && echo "$VM_TARGETS"); do
    vs=$(vm_state "$vmt")
    if [ "${vs%% *}" != Running ]; then # owner decision (a), as for the containers above
      printf 'M04 %s %s: VM %s not Running after mesh-rebootstrap [%s]; debug restun\n' "$s" "$VMVER" "$vmt" "$vs" >>"$ART_DIR/timings.txt"
      vm_ "$vmt" "$VMTS debug restun" >/dev/null 2>&1
      for i in $(seq 1 30); do vs=$(vm_state "$vmt"); [ "${vs%% *}" = Running ] && break; sleep 1; done
      stuck="$stuck $vmt"; manual=$((manual + 1))
      [ "${vs%% *}" = Running ] && restunned="$restunned $vmt"
    fi
    [ "${vs%% *}" = Running ] || p="$p; VM $vmt [$vs]"
    [ "${vs##* }" = False ] || p="$p; VM $vmt notice still shown"
    case " $vmrb " in *" $vmt=0 "*) ;; *) grep -qw -- "$vmt" <<<"$restunned" || p="$p; VM $vmt mesh-rebootstrap rc [$vmrb]";; esac
  done
  [ -z "$vmrb" ] || printf 'M04 %s VMs (%s) mesh-rebootstrap rc:%s\n' "$s" "$VMVER" "$vmrb" >>"$ART_DIR/timings.txt"
  stop_pinger "$s" >"$ART_DIR/logs/M04-$s-ping.txt"
  local gap; gap=$(ping_gap "$ART_DIR/logs/M04-$s-ping.txt")
  case "${gap#* }" in */0) p="$p; pinger recorded no samples";; esac
  local tt; tt=$(py 'import json,sys,datetime,re
iso=lambda v: datetime.datetime.fromisoformat(re.sub(r"(\.\d{6})\d+", r"\1", v).replace("Z", "+00:00"))
d=json.load(open(sys.argv[1], encoding="utf-8")); at=iso(sys.argv[2]); st=iso(d["started_at"])
off=int((st-at).total_seconds()*1000)
f=lambda v: v if v<0 else v+off
print("ok=%d failed=%d retried=%d t50=%d t90=%d t100=%d ms (from run start); from scheduled_at: t50=%d t90=%d t100=%d ms" % (d["ok"],d["failed"],d.get("retried",0),d["t50_ms"],d["t90_ms"],d["t100_ms"],f(d["t50_ms"]),f(d["t90_ms"]),f(d["t100_ms"])))' \
    "$ART_DIR/logs/rebootstrap-$s.json" "$at" 2>/dev/null | strip_cr)
  # Retried targets (owner decision 2026-11-26: mesh-rebootstrap runs a target
  # again when its login came before the target cluster loaded the tailnet).
  py 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
for x in d.get("results") or []:
    if x.get("attempts", 1) > 1:
        why = " | ".join(sorted({(e.strip().splitlines() or ["-"])[-1] for e in x.get("earlier_failures") or []}))
        print("M04 %s retried %s: attempts=%d ok=%s start=%dms end=%dms earlier: %s" % (sys.argv[2], x["target"], x["attempts"], x["ok"], x["start_ms"], x["end_ms"], why))' \
    "$ART_DIR/logs/rebootstrap-$s.json" "$s" 2>/dev/null | tr -d '\r' >>"$ART_DIR/timings.txt"
  printf 'M04 %s %s N=%d PAR=%d: %s; ping gap %s ms (failed/samples %s); manual interventions %d%s\n' \
    "$s" "$ver" "$N" "$PAR" "$tt" "${gap%% *}" "${gap#* }" "$manual" "${stuck:+ (debug restun:$stuck)}" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "M04.$r" PASS "$ver: $N clients re-bootstrapped $PAR at a time: $tt; all Running at $to, same node IDs and IPs (nodes kept), $a sees $((want - 1)) peers, notice gone; ping gap ${gap%% *} ms; manual interventions $manual${stuck:+ (owner decision (a): debug restun of$stuck)}" \
    || record "M04.$r" FAIL "$ver: ${p#; }; $tt; gap ${gap%% *} ms; manual interventions $manual"

  # M05 control tailnet untouched
  p=""
  [ "$(jget "$(pl GET "/v1/tailnets/$(tid o)")" "d['owner_cluster_id']+'@'+str(d['owner_epoch'])")" = "$O_ROW" ] || p="$p; o owner/epoch changed"
  ping_ok p12b-o1 p12b-o2 || p="$p; o1 cannot ping o2"
  snap p12b-o1 p12b-o2
  for c in p12b-o1 p12b-o2; do [ "$(sget "$c" 4)" = False ] || p="$p; $c notice"; [ "$(sget "$c" 5)" = 1 ] || p="$p; $c peers $(sget "$c" 5)"; done
  [ -z "$p" ] && record "M05.$r" PASS "$ver: control tailnet o unchanged ($O_ROW), o1<->o2, no notice, sees only its own peer" || record "M05.$r" FAIL "$ver: ${p#; }"
}

t_m06() {
  local p="" f open
  local ep; ep=$(psql_ "SELECT slug||'='||owner_cluster_id||'@'||owner_epoch||'/'||COALESCE(moving_to_cluster_id,'-') FROM tailnets ORDER BY slug" | tr '\n' ' ')
  printf 'owners %s\n' "$ep" >>"$ART_DIR/logs/audit.txt"
  grep -q '/[^-]' <<<"$ep" && p="$p; a move still requested [$ep]"
  open=$(psql_ "SELECT count(*) FROM tailnet_moves WHERE state IN ('SCHEDULED','REQUESTED')")
  [ "$open" = 0 ] || p="$p; $open open moves"
  psql_ "SELECT tailnet_id||' '||from_cluster_id||'->'||to_cluster_id||' '||state||' '||scheduled_at||' '||COALESCE(requested_at::text,'-')||' '||COALESCE(finished_at::text,'-') FROM tailnet_moves ORDER BY created_at" >>"$ART_DIR/logs/audit.txt"
  psql_ "SELECT actor||' '||action||' '||outcome FROM platform_audit WHERE action IN ('ScheduleMove','MoveDue','MoveOwner','MoveFinished','HandOver') ORDER BY id" >>"$ART_DIR/logs/audit.txt"
  f=$(findings); printf 'M06 reconcile %s\n' "$f" >>"$ART_DIR/logs/reconcile.txt"
  [ "$f" = "[]" ] || p="$p; reconcile findings $f"
  [ -z "$p" ] && record M06 PASS "one owner per tailnet, none moving ($ep); no open move; reconcile []" || record M06 FAIL "${p#; }"
}

t_m07() {
  local hits
  hits=$(grep -rlE 'hskey-(auth|api)-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}|BEGIN [A-Z ]*PRIVATE KEY' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
  [ -z "$hits" ] && record M07 PASS "no auth key, API key, private key or full machine/node key in the artifacts" || record M07 FAIL "unredacted in: $(echo "$hits" | tr '\n' ' ')"
}

# ------------------------------------------------------------------- main ---
T0=$(date +%s)
log "P12b E2E $RUN_ID at $SHA: c1 $URL1, c2 $URL2, images [$IMAGES], N=$N PAR=$PAR LEAD=${LEAD}s, VMs [${VM_TARGETS:-none}] (artifacts: ${ART_DIR#"$REPO"/})"
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "images: $IMAGES"; echo "client VMs: ${VM_TARGETS:-none} (tailscale $VMVER, round 1)"; echo "commit: $SHA"; echo "database: $PG_DB (mc-db via PgBouncer $PG_LISTEN)"; echo "N=$N PAR=$PAR LEAD=$LEAD"
  echo "binaries: ${CI_BINDIR:+built on mc-ci in $CI_BINDIR, fetched over the LAN}${CI_BINDIR:-copied from the laptop}"
  echo "cluster binary: $(binsha headscale | cut -c1-16)"; echo "mesh-placement: $(binsha mesh-placement | cut -c1-16)"
  echo "mesh-bootstrap: $(binsha mesh-bootstrap | cut -c1-16)"; echo "mesh-rebootstrap: $(binsha mesh-rebootstrap | cut -c1-16)"; } >>"$ART_DIR/env.txt"
if t_m00; then
  R=0; for img in $IMAGES; do R=$((R + 1)); t_round "$R" "$img"; done
  t_m06
fi
for c in c1 c2; do proc_log $c $(( $(date +%s) - T0 + 5 )) | redact >"$ART_DIR/logs/$c-process.log"; done
cp_ "sudo journalctl -u $PLUNIT -u $BOOTUNIT --no-pager" 2>&1 | redact >"$ART_DIR/logs/placement-bootstrap.log"
t_m07
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
