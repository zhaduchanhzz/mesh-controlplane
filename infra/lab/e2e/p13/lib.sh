#!/usr/bin/env bash
# P13 drills, shared part: sourced by f05.sh (PostgreSQL failover, F05) and
# pgb-kill.sh (PgBouncer killed under load). Not run on its own.
#
# Topology of a run (ticket tickets/P13.md §P13a/§P13b, ADR 0011):
#
#   c1  https://c1.cp.gta5web.online:8443: VPS -> wg-mesh -> mc-cp 10.99.77.2:8443
#       (HAProxy mesh-c1-ha, transient, /ready check) -> mc-cp 192.168.1.241:8443
#       (active first) or mc-c1b 192.168.1.245:8443 (standby)
#   c2  https://c2.cp.gta5web.online:8445: VPS -> wg-mesh -> mc-cp 10.99.77.2:8445
#       (long-lived HAProxy mesh-c2-ha, /ready check) -> mc-c2 .244 (active
#       first) or mc-c2b .246 (standby)
#   DB  e2e_p13<run> on the Patroni cluster mc-pg (mc-pg-01 .247, mc-pg-02
#       .248): the processes connect to the PgBouncer of both nodes
#       (hosts .247:6432,.248:6432, target_session_attrs=read-write,
#       connect_timeout_secs 2); each PgBouncer sends to its local HAProxy
#       127.0.0.1:5000, which follows Patroni's /primary. The schema comes
#       from the operator migrate step (ADR 0013) run on mc-ci straight to
#       the primary (5432, ADR 0006 §4); MIGRATE_VIA=pgbouncer runs it on
#       mc-cp through PgBouncer instead.
#   clients per round (one round per image of IMAGES): docker on mc-ci,
#       p13-r<N>-a1/a2/aw in tailnet fa (owner c1) and -b1/b2/bw in fb
#       (owner c2); the client VMs of VM_TARGETS (first -> fa, second -> fb)
#       get a second tailscaled (unit p13-ts, tun p13ts0, port 41643) of the
#       round's version when /opt/tailscale/<version> exists on the VM.
#   probes for the whole round, on mc-ci, one clock (jobs.sh): pings a1->a2,
#       a1->VM A, b1->b2, b1->VM B; GET /ready of c1 and c2; a fenced
#       control-plane write every 500 ms per cluster (rename of aw/bw through
#       the tenant API); a SQL writer through the client DSN every 200 ms
#       (table p13_probe).
#
# Same isolation from the long-lived lab as P11b/P12b: binary
# /usr/local/bin/headscale-cluster-p13, config
# /etc/headscale/cluster/config-p13.yaml, unit mesh-p13-e2e, password file
# /etc/headscale/cluster/pg-p13.pass. Services found running on mc-cp
# (P02 POC), mc-c2 and mc-c2b (long-lived c2 on meshlab) are stopped for the
# run and restarted at cleanup, and only those (standing permission of the
# owner for E2E runs). The database is dropped at cleanup.
#
# Operating rules (lessons of the N1, P12, P12b runs):
# - Never edit these files while a run is in progress (bash reads a running
#   script incrementally).
# - Start a run in the foreground of a background task, never with `&`.
# - One harness on the lab at a time: setup refuses to start when another
#   E2E unit runs on mc-cp.
# - Every fault has a dead-man switch on the host where it is injected (a
#   transient timer that undoes it), so a lost run never leaves a partition
#   or a stopped PgBouncer behind; cleanup undoes every fault again.
# shellcheck disable=SC2029,SC2034
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
MESH_LAB_DIR=${MESH_LAB_DIR:-$HOME/.mesh-lab}
PG_ENV=${PG_ENV:-$MESH_LAB_DIR/mc-pg.env}
CP_SSH=${CP_SSH:-mesh@192.168.1.241}
C1B_SSH=${C1B_SSH:-mesh@192.168.1.245}
C2_SSH=${C2_SSH:-mesh@192.168.1.244}
C2B_SSH=${C2B_SSH:-mesh@192.168.1.246}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
PG1_IP=192.168.1.247
PG2_IP=192.168.1.248
MCDB_IP=192.168.1.243
PG1_SSH=mesh@$PG1_IP
PG2_SSH=mesh@$PG2_IP
PG1_VMID=7708
PG2_VMID=7709
PGB_PORT=${PGB_PORT:-6432}
PGB_UNIT=${PGB_UNIT:-pgbouncer}
PGDATA=${PGDATA:-/var/lib/postgresql/16/mc-pg}
C1_ID=mc-cp-c1
C2_ID=mc-c2
HOST1=c1.cp.gta5web.online
HOST2=c2.cp.gta5web.online
URL1=https://$HOST1:8443
URL2=https://$HOST2:8445
POC_HEALTH=https://a.cp.gta5web.online:8443/health
CP_IP=192.168.1.241
C1B_IP=192.168.1.245
IMAGES=${IMAGES:-tailscale/tailscale:v1.104.7 tailscale/tailscale:v1.84.3}
VM_TARGETS=${VM_TARGETS-mesh@10.77.20.11 mesh@10.77.30.11}
MIGRATE_VIA=${MIGRATE_VIA:-direct}
# D12 thresholds (owner decision 2026-11-26).
RECOVER_MAX_MS=${RECOVER_MAX_MS:-45000}
GAP_MAX_MS=${GAP_MAX_MS:-10000}
MANUAL_MAX=${MANUAL_MAX:-0}
WERR_MAX_MS=${WERR_MAX_MS:-10000}
# Waits.
PG_HEAL_MAX=${PG_HEAL_MAX:-300}
CLIENT_WAIT=${CLIENT_WAIT:-60}
CI_BINDIR=${CI_BINDIR:-}
CI_LAN=${CI_SSH#*@}
BINPORT=${BINPORT:-18090}
KEEP=${KEEP:-0}
LABEL=mesh-e2e=p13
PY=${PY:-python}
export PYTHONUTF8=1 PYTHONIOENCODING=utf-8

if [ -n "$CI_BINDIR" ]; then
  [[ $CI_BINDIR =~ ^/[A-Za-z0-9/_.-]+$ ]] || { echo "CI_BINDIR: absolute path, [A-Za-z0-9/_.-] only" >&2; exit 2; }
  BIN=-
else
  BIN=${BIN:?BIN= linux/amd64 cluster binary (or CI_BINDIR= on mc-ci)}
  [ -s "$BIN" ] || { echo "missing binary $BIN" >&2; exit 2; }
fi
for t in $VM_TARGETS; do [[ $t =~ ^mesh@10\.77\.[0-9]+\.[0-9]+$ ]] || { echo "VM_TARGETS: mesh@10.77.x.y only ($t)" >&2; exit 2; }; done
case $MIGRATE_VIA in direct|pgbouncer) ;; *) echo "MIGRATE_VIA: direct or pgbouncer" >&2; exit 2 ;; esac

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$HERE/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
PG_DB=e2e_p13$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
ART_DIR=$REPO/artifacts/$SHA/P13/${DRILL:?DRILL}-$RUN_ID
mkdir -p "$ART_DIR/logs" "$ART_DIR/probes"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

HS=/usr/local/bin/headscale-cluster-p13
CFG=/etc/headscale/cluster/config-p13.yaml
PASSF=/etc/headscale/cluster/pg-p13.pass
UNIT=mesh-p13-e2e
HAUNIT=mesh-c1-ha
HACFG=/etc/haproxy/mesh-c1-p13.cfg
CIDIR=/dev/shm/p13
CIRUN="sudo systemd-run --quiet --collect --uid=mesh --gid=mesh -p SupplementaryGroups=docker -p RuntimeMaxSec=7200"
VMUNIT=p13-ts
VMDIR=/run/p13-ts
VMSTATE=/var/lib/p13-ts
VMSOCK=$VMDIR/tailscaled.sock

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
SSHO=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
ssh_() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
scp_() { scp -q "${SSHO[@]}" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
ci_() { ssh_ "$CI_SSH" "$@"; }
sshof() { case $1 in cp) echo "$CP_SSH";; c1b) echo "$C1B_SSH";; c2) echo "$C2_SSH";; c2b) echo "$C2B_SSH";; pg1) echo "$PG1_SSH";; pg2) echo "$PG2_SSH";; esac; }
on() { local n=$1; shift; ssh_ "$(sshof "$n")" "$@"; }
nodes_of() { [ "$1" = c1 ] && echo "cp c1b" || echo "c2 c2b"; }
ALLNODES="cp c1b c2 c2b"
idof() { [ "$1" = c1 ] && echo $C1_ID || echo $C2_ID; }
urlof() { [ "$1" = c1 ] && echo $URL1 || echo $URL2; }
# cli CLUSTER ARGS: the cluster's admin CLI (talks to the database, not to the process).
cli() { local c=$1 n; shift; n=c2; [ "$c" = c1 ] && n="cp"; on $n "sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $(idof "$c") $*"; }
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }
# ci_now: the mc-ci clock (epoch ms), the clock of every probe.
ci_now() { ci_ 'date +%s%3N' | strip_cr; }
jget() { py "import json; d=json.loads(sys.argv[1]); print($2)" "$1" 2>/dev/null | strip_cr; }

FAILS=0
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-10s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}

# ------------------------------------------------------------- binaries ---
binsha() { if [ -n "$CI_BINDIR" ]; then strip_cr <"$TMPD/binsha"; else sha256sum "$BIN" | cut -d' ' -f1; fi; }
binsrv_start() {
  [ -n "$CI_BINDIR" ] || return 0
  ci_ "sha256sum $CI_BINDIR/headscale" | cut -d' ' -f1 | strip_cr >"$TMPD/binsha" || return 1
  [ "$(wc -c <"$TMPD/binsha")" -ge 64 ] || { log "CI_BINDIR: no headscale binary"; return 1; }
  ci_ "sudo systemctl stop p13-binsrv 2>/dev/null; sudo systemctl reset-failed p13-binsrv 2>/dev/null; \
    sudo systemd-run --quiet --collect --unit=p13-binsrv --uid=mesh --gid=mesh -p RuntimeMaxSec=900 \
      python3 -m http.server $BINPORT --bind $CI_LAN --directory $CI_BINDIR; \
    for i in \$(seq 1 20); do curl -fsS -o /dev/null -m 2 http://$CI_LAN:$BINPORT/ && exit 0; sleep 0.5; done; exit 1" || { log "p13-binsrv not answering"; return 1; }
}
binsrv_stop() { [ -z "$CI_BINDIR" ] || ci_ "sudo systemctl stop p13-binsrv 2>/dev/null; sudo systemctl reset-failed p13-binsrv 2>/dev/null; ! systemctl is-active -q p13-binsrv"; }
# put NODE DEST: the cluster binary at DEST on NODE, sha256 checked there.
put() {
  local n=$1 dst=$2 sum; sum=$(binsha)
  [ ${#sum} = 64 ] || return 1
  if [ -n "$CI_BINDIR" ]; then on "$n" "curl -fsS --retry 3 -m 120 -o $dst http://$CI_LAN:$BINPORT/headscale" || return 1
  else scp_ "$BIN" "$(sshof "$n"):$dst" || return 1; fi
  on "$n" "echo '$sum  $dst' | sha256sum -c --quiet" || { log "$n: sha256 mismatch"; return 1; }
}

# ----------------------------------------------------------- PostgreSQL ---
# pg_cluster: Patroni's /cluster JSON from the first PG node that answers.
PG_ORDER="$PG1_SSH $PG2_SSH"
pg_cluster() { local h o; for h in $PG_ORDER; do o=$(ssh_ "$h" "curl -fsS -m 3 http://127.0.0.1:8008/cluster" 2>/dev/null) && [ -n "$o" ] && { printf '%s' "$o"; return 0; }; done; return 1; }
# pg_state -> "<leader|-> <sync standby|-> <sync state|-> <leader timeline|->"
pg_state() {
  pg_cluster | py 'import json
try:
    d = json.load(sys.stdin)
except Exception:
    print("- - - -"); sys.exit()
m = d.get("members") or []
L = [x for x in m if x.get("role") == "leader" and x.get("state") == "running"]
S = [x for x in m if x.get("role") == "sync_standby"]
print(L[0]["name"] if L else "-", S[0]["name"] if S else "-", S[0].get("state", "-") if S else "-", L[0].get("timeline", "-") if L else "-")' | strip_cr
}
pg_healthy() { local s; s=$(pg_state); read -r l _ st _ <<<"$s"; [ "$l" != - ] && [ "$st" = streaming ]; }
wait_pg_healthy() { local i; for i in $(seq 1 "$1"); do pg_healthy && return 0; sleep 1; done; return 1; }
ssh_of_member() { case $1 in mc-pg-01) echo "$PG1_SSH";; mc-pg-02) echo "$PG2_SSH";; esac; }
ip_of_member() { case $1 in mc-pg-01) echo "$PG1_IP";; mc-pg-02) echo "$PG2_IP";; esac; }
vmid_of_member() { case $1 in mc-pg-01) echo "$PG1_VMID";; mc-pg-02) echo "$PG2_VMID";; esac; }
other_member() { [ "$1" = mc-pg-01 ] && echo mc-pg-02 || echo mc-pg-01; }
pg_leader() { pg_state | awk '{print $1}'; }
# psql_ SQL: as postgres (peer auth) on the current leader, database $PG_DB.
psql_() { local l h; l=$(pg_leader); h=$(ssh_of_member "$l"); [ -n "$h" ] || return 1; ssh_ "$h" "sudo -u postgres psql -X -At -d $PG_DB -c \"$1\""; }
# psql_on MEMBER SQL: on that member whatever its role (for the isolated old primary).
psql_on() { ssh_ "$(ssh_of_member "$1")" "sudo -u postgres timeout 10 psql -X -At -d $PG_DB -c \"$2\""; }
lease_rows() { psql_ "SELECT cluster_id||' '||holder||' '||epoch FROM cluster_leases ORDER BY cluster_id" | strip_cr; }
lease_epoch() { lease_rows | awk -v c="$(idof "$1")" '$1 == c {print $3}'; }
owner_epochs() { psql_ "SELECT slug||':'||owner_cluster_id||':'||owner_epoch FROM tailnets ORDER BY slug" | strip_cr | tr '\n' ' '; }
chrony_offsets() { local h; for h in $PG1_SSH $PG2_SSH $CP_SSH $C1B_SSH $C2_SSH $C2B_SSH $CI_SSH; do
  printf '%s %s\n' "${h#*@}" "$(ssh_ "$h" "chronyc -c tracking 2>/dev/null | cut -d, -f5" 2>/dev/null | strip_cr)"; done; }

# Proxmox API (VM power of the two PG VMs only): pinned certificate,
# credentials read by pve_api.py from ~/.mesh-lab/proxmox.env, never printed.
pve() { MSYS_NO_PATHCONV=1 "$PY" "$REPO/infra/lab/scripts/pve_api.py" "$@"; }
# pve_vm stop|start|status VMID
pve_vm() {
  local act=$1 v=$2 info node pool status
  case $v in "$PG1_VMID"|"$PG2_VMID") ;; *) log "pve_vm: VMID $v is not a PG VM of the drill"; return 1 ;; esac
  info=$(pve get cluster/resources type=vm 2>/dev/null | py 'import json
for x in json.load(sys.stdin):
    if x.get("vmid") == int(sys.argv[1]):
        print(x.get("node"), x.get("pool", "-"), x.get("status"))' "$v" | strip_cr)
  read -r node pool status <<<"$info"
  [ "$pool" = meshcontrol-lab ] || { log "pve_vm: VMID $v not in pool meshcontrol-lab ($info)"; return 1; }
  [ "$act" = status ] && { echo "$status"; return 0; }
  pve --write post "nodes/$node/qemu/$v/status/$act" >/dev/null
}

# ------------------------------------------------------------ processes ---
start_proc() { # NODE
  on "$1" "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    $HS -c $CFG cluster serve --cluster-id $(idof "$(cluster_of "$1")")"
}
cluster_of() { case $1 in cp|c1b) echo c1;; *) echo c2;; esac; }
proc_log_since() { on "$1" "sudo journalctl -u $UNIT --since @$2 --no-pager -o short-iso-precise" 2>/dev/null; }
public_ready() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$(urlof "$1")/ready" || true; }
wait_public_ready() { local i; for i in $(seq 1 "$2"); do [ "$(public_ready "$1")" = 200 ] && return 0; sleep 1; done; return 1; }
# metrics NODE: lease metrics of the process on NODE (operator listener, loopback).
metrics() { on "$1" "curl -s -m 3 http://127.0.0.1:9095/metrics" 2>/dev/null | grep -E '^headscale_cluster_lease_(events_total|held|epoch)' | strip_cr; }
# lease_events NODE EVENT -> count from a saved metrics snapshot file
mcount() { awk -v e="event=\"$2\"" 'index($0, e) { s += $NF } END { printf "%d", s }' "$1"; }
snap_metrics() { local n; for n in $ALLNODES; do metrics "$n" >"$TMPD/m-$1-$n" || true; cp "$TMPD/m-$1-$n" "$ART_DIR/logs/metrics-$1-$n.txt" 2>/dev/null; done; }
# fence_delta TAG0 TAG1 EVENT -> "node:delta ..." for nodes whose count grew
fence_delta() { local n a b out=""; for n in $ALLNODES; do a=$(mcount "$TMPD/m-$1-$n" "$3"); b=$(mcount "$TMPD/m-$2-$n" "$3")
  [ "$b" -gt "$a" ] && out="$out $n:+$((b - a))"; done; echo "${out# }"; }
held_nodes() { local n out=""; for n in $ALLNODES; do grep -q '^headscale_cluster_lease_held{.*} 1' "$TMPD/m-$1-$n" 2>/dev/null && out="$out $n"; done; echo "${out# }"; }

# --------------------------------------------------------------- clients ---
start_ts() { # NAME IMAGE
  ci_ "docker run -d --name $1 --hostname $1 --label $LABEL --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$LABEL $2 \
    tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done"
}
ts_up() { # NAME URL KEYFILE
  ci_ "docker exec -i $1 timeout 60 tailscale up --login-server=$2 --auth-key=file:/dev/stdin --hostname=$1" \
    <"$3" 2>&1 | redact >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
ts_json() { ci_ "timeout 15 docker exec $1 tailscale status --json" 2>/dev/null; }
field() { ts_json "$1" | py "import json; d=json.load(sys.stdin); print($2)" 2>/dev/null | strip_cr; }
selfip() { field "$1" "d['Self']['TailscaleIPs'][0]"; }
state() { field "$1" "d['BackendState']"; }
ping_ok() { ci_ "timeout 30 docker exec $1 tailscale ping -c 3 --timeout 5s $2" >>"$ART_DIR/logs/$1-ping.log" 2>&1; }
needs_login() { ci_ "timeout 20 docker logs $1 2>&1" | grep -c 'NeedsLogin' || true; }
save_logs() { ci_ "timeout 30 docker logs $1 2>&1" | redact >"$ART_DIR/logs/$1.tailscaled.log" 2>/dev/null || true; }
node_id() { # SLUG HOSTNAME
  cli "$(owner_of "$1")" tailnet nodes --slug "$1" 2>/dev/null \
    | py 'import json
for n in json.load(sys.stdin):
    if n["hostname"] == sys.argv[1] or n.get("given_name") == sys.argv[1]:
        print(n["id"]); break' "$2" | strip_cr
}
owner_of() { [ "$1" = fa ] && echo c1 || echo c2; }

# Client VMs through mc-ci (ProxyCommand, as P12b).
PX="ProxyCommand=ssh -i $SSH_KEY -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 $CI_SSH nc -q0 %h %p"
vm_() { local t=$1; shift; ssh "${SSHO[@]}" -o StrictHostKeyChecking=accept-new -o "$PX" "$t" "$@"; }
vmts() { echo "sudo timeout 30 /opt/tailscale/$1/tailscale --socket=$VMSOCK"; }
vm_start() { # TARGET VERSION URL KEYFILE
  vm_ "$1" "test -x /opt/tailscale/$2/tailscaled || exit 3
    sudo systemctl stop $VMUNIT 2>/dev/null; sudo systemctl reset-failed $VMUNIT 2>/dev/null
    sudo rm -rf $VMSTATE $VMDIR && sudo install -d -m 0700 $VMSTATE $VMDIR || exit 1
    sudo systemd-run --quiet --unit=$VMUNIT /opt/tailscale/$2/tailscaled --state=$VMSTATE/tailscaled.state \
      --socket=$VMSOCK --tun=p13ts0 --port=41643 || exit 1
    for i in \$(seq 1 60); do $(vmts "$2") status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done; exit 1"
  local rc=$?; [ $rc = 0 ] || return $rc
  vm_ "$1" "sudo install -m 0600 /dev/stdin $VMDIR/key" <"$4" || return 1
  vm_ "$1" "$(vmts "$2") up --login-server=$3 --auth-key=file:$VMDIR/key --hostname=\$(hostname -s)-p13 \
    --accept-dns=false --netfilter-mode=off; rc=\$?; sudo rm -f $VMDIR/key; exit \$rc" 2>&1 | redact >>"$ART_DIR/logs/vm-${1#*@}.cli.log"
  return "${PIPESTATUS[0]}"
}
vm_field() { vm_ "$1" "$(vmts "$2") status --json" 2>/dev/null | py "import json; d=json.load(sys.stdin); print($3)" 2>/dev/null | strip_cr; }
vm_stop() { vm_ "$1" "sudo systemctl stop $VMUNIT 2>/dev/null; sudo systemctl reset-failed $VMUNIT 2>/dev/null; \
    sudo rm -rf $VMSTATE $VMDIR; ! ip link show p13ts0 >/dev/null 2>&1"; }

# ---------------------------------------------------------------- probes ---
# job NAME MODE ARGS...: jobs.sh MODE on mc-ci as transient unit p13-NAME.
job() { local n=$1; shift; ci_ "sudo systemctl reset-failed p13-$n 2>/dev/null; $CIRUN --unit=p13-$n bash $CIDIR/jobs.sh $*"; }
stop_jobs() { ci_ "touch $CIDIR/stop 2>/dev/null; sleep 1; sudo systemctl stop 'p13-ping-*' 'p13-ready-*' 'p13-wprobe-*' p13-sqlw 2>/dev/null; \
  sudo systemctl reset-failed 'p13-ping-*' 'p13-ready-*' 'p13-wprobe-*' p13-sqlw 2>/dev/null; rm -f $CIDIR/stop; \
  ! pgrep -f '$CIDIR/jobs.sh' >/dev/null"; }
# fetch_probes: copy every probe file of the round to $ART_DIR/probes/r<N>/.
fetch_probes() {
  mkdir -p "$ART_DIR/probes/r$ROUND"
  ci_ "cd $CIDIR && tar cf - probe-*.txt 2>/dev/null" | tar xf - -C "$ART_DIR/probes/r$ROUND" 2>/dev/null || true
}

# analyze KIND FILE T0 T1 -> one line of numbers (window [T0, T1], mc-ci ms)
#   write FILE (wprobe/ready): "<first failure ms after T0|-> <recovered ms after T0|-> <error window ms> <failures>"
#     recovered = first success after the last failure (0 when nothing failed)
#   ping FILE: "<max gap between successes ms> <successes>"
#   sql FILE [T_PROMOTE OLD_IP]: "<acked> <failed> <readonly errors after T_PROMOTE+5 s> <acked by OLD_IP after T_PROMOTE> <max ack gap ms>"
analyze() {
  py 'import sys
kind, f, t0, t1 = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
rows = []
for l in open(f, encoding="utf-8", errors="replace"):
    p = l.split()
    if kind == "sql":
        if len(p) >= 4 and p[2].isdigit():
            rows.append((int(p[2]), p[1], p[3]))
    elif len(p) >= 2 and p[0].isdigit():
        rows.append((int(p[0]), p[1]))
if kind == "write":
    w = [r for r in rows if t0 <= r[0] <= t1]
    bad = [r[0] for r in w if r[1] not in ("200",)]
    if not bad:
        print("-", 0, 0, 0)
    else:
        rec = [r[0] for r in w if r[0] > bad[-1] and r[1] == "200"]
        print(bad[0] - t0, (rec[0] - t0) if rec else "-", bad[-1] - bad[0], len(bad))
elif kind == "ping":
    prev = max([r[0] for r in rows if r[0] < t0 and r[1] == "0"] or [t0])
    gap, n = 0, 0
    for r in rows:
        if t0 <= r[0] <= t1 and r[1] == "0":
            gap = max(gap, r[0] - prev); prev = r[0]; n += 1
    gap = max(gap, t1 - prev) if n else t1 - t0
    print(gap, n)
else:
    tp = int(sys.argv[5]) if len(sys.argv) > 5 and sys.argv[5] != "-" else None
    old = sys.argv[6] if len(sys.argv) > 6 else ""
    w = [r for r in rows if t0 <= r[0] <= t1]
    ok = [r for r in w if r[1] == "ok"]
    ro = [r for r in w if r[1] == "fail" and "read-only" in r[2] and tp and r[0] > tp + 5000]
    late = [r for r in ok if tp and r[0] > tp and r[2] == old]
    gap, prev = 0, None
    for r in ok:
        if prev is not None: gap = max(gap, r[0] - prev)
        prev = r[0]
    print(len(ok), len(w) - len(ok), len(ro), len(late), gap)' "$@" | strip_cr
}

# sql_check T0 T1 T_PROMOTE|- OLD_IP|- -> "<acked> <failed> <read-only after promote+5s> <acked by old after promote> <max ack gap ms> <acked but missing on the leader>"
sql_check() {
  local f="$ART_DIR/probes/r$ROUND/probe-sql.txt" a lo hi lost=0
  a=$(analyze sql "$f" "$1" "$2" "$3" "$4")
  awk -v t0="$1" -v t1="$2" '$2 == "ok" && $3 >= t0 && $3 <= t1 { print $1 }' "$f" | sort -n >"$TMPD/acked"
  if [ -s "$TMPD/acked" ]; then
    lo=$(head -1 "$TMPD/acked"); hi=$(tail -1 "$TMPD/acked")
    psql_ "SELECT seq FROM p13_probe WHERE seq BETWEEN $lo AND $hi ORDER BY seq" | strip_cr | sort -n >"$TMPD/present"
    lost=$(comm -23 "$TMPD/acked" "$TMPD/present" | wc -l)
  fi
  echo "$a $lost"
}
# wait_leader_not OLD SECS -> "<mc-ci ms> <new leader>" once a running leader other than OLD shows up
wait_leader_not() { local i l; for i in $(seq 1 "$2"); do l=$(pg_leader); [ "$l" != - ] && [ "$l" != "$1" ] && { echo "$(ci_now) $l"; return 0; }; sleep 1; done; echo "- -"; return 1; }
# wait_promoted MEMBER SECS -> its own clock (epoch ms) when it left recovery (polled on the host, 100 ms)
wait_promoted() {
  ssh_ "$(ssh_of_member "$1")" "end=\$((\$(date +%s)+$2)); while [ \$(date +%s) -lt \$end ]; do     [ \"\$(sudo -u postgres timeout 2 psql -X -At -d postgres -c 'SELECT pg_is_in_recovery()' 2>/dev/null)\" = f ] && { date +%s%3N; exit 0; }; sleep 0.1; done; exit 1" | strip_cr
}
# wait_not_writable MEMBER SECS -> its own clock (epoch ms) when it is in recovery or down (polled on the host)
wait_not_writable() {
  ssh_ "$(ssh_of_member "$1")" "end=\$((\$(date +%s)+$2)); while [ \$(date +%s) -lt \$end ]; do     r=\$(sudo -u postgres timeout 2 psql -X -At -d postgres -c 'SELECT pg_is_in_recovery()' 2>/dev/null);     [ \"\$r\" != f ] && { date +%s%3N; exit 0; }; sleep 0.1; done; exit 1" | strip_cr
}

# ------------------------------------------------------------ tailnets -----
new_tailnet() { # SLUG CLUSTER
  local s=$1 c=$2 pf=/var/lib/headscale/cluster/p13-policy.hujson n=c2; [ "$c" = c1 ] && n="cp"
  cli "$c" tailnet create --slug "$s" >/dev/null && cli "$c" tailnet status --slug "$s" --set ACTIVE >/dev/null || return 1
  printf '%s\n' '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' \
    | on $n "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $(idof "$c") tailnet policy --slug $s --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc" >/dev/null || return 1
  cli "$c" tailnet authkey --slug "$s" --user "e2e-$s" --reusable --expiration 240m \
    | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s"
  chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  cli "$c" tailnet apikey create --slug "$s" --expiration 4h \
    | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/api-$s"
  chmod 600 "$TMPD/api-$s"; [ -s "$TMPD/api-$s" ] || return 1
}

# ------------------------------------------------------------- cleanup ----
CLEAN_OK=1
WAS_ACTIVE=""
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  # Faults first: every PG VM running, no partition, PgBouncer up on both.
  local v s h
  for v in $PG1_VMID $PG2_VMID; do
    s=$(pve_vm status "$v" 2>/dev/null); [ "$s" = stopped ] && { log "PG VM $v stopped: starting it"; pve_vm start "$v" || CLEAN_OK=0; }
  done
  for h in $PG1_SSH $PG2_SSH; do
    for i in $(seq 1 30); do ssh_ "$h" true 2>/dev/null && break; sleep 2; done
    ssh_ "$h" "sudo nft delete table inet p13_drill 2>/dev/null; sudo systemctl stop 'p13-undo-*' 2>/dev/null; \
      sudo systemctl reset-failed 'p13-undo-*' 2>/dev/null; sudo systemctl start $PGB_UNIT; \
      ! sudo nft list table inet p13_drill >/dev/null 2>&1 && systemctl is-active -q $PGB_UNIT" \
      || { log "${h#*@}: fault not undone (nft table p13_drill or $PGB_UNIT)"; CLEAN_OK=0; }
  done
  wait_pg_healthy "$PG_HEAL_MAX" && log "Patroni healthy: $(pg_state)" || { log "Patroni not healthy after cleanup: $(pg_state)"; CLEAN_OK=0; }
  binsrv_stop || { log "p13-binsrv not stopped"; CLEAN_OK=0; }
  stop_jobs || { log "mc-ci probe jobs not stopped"; CLEAN_OK=0; }
  local c; for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; \
       docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null; rm -rf $CIDIR; \
       test -z \"\$(docker ps -aq --filter label=$LABEL)\" && ! test -e $CIDIR" || { log "mc-ci clients/files not removed"; CLEAN_OK=0; }
  local t; for t in $VM_TARGETS; do vm_stop "$t" || { log "$t: p13 tailscaled not removed"; CLEAN_OK=0; }; done
  local n
  for n in $ALLNODES; do
    on $n "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; \
      sudo rm -f $CFG $HS $PASSF; ! sudo test -e $CFG && ! test -e $HS && ! sudo test -e $PASSF" || { log "$n: run files not removed"; CLEAN_OK=0; }
  done
  cp_ "sudo systemctl stop $HAUNIT 2>/dev/null; sudo systemctl reset-failed $HAUNIT 2>/dev/null; sudo rm -f $HACFG; ! test -e $HACFG" \
    || { log "mc-cp: HAProxy of the run not removed"; CLEAN_OK=0; }
  # mc-c1b keeps no secret of c1 after the run (as P11b).
  on c1b "sudo rm -f /var/lib/headscale/cluster/noise_private.key; ! sudo test -e /var/lib/headscale/cluster/noise_private.key" \
    || { log "c1 key not removed on mc-c1b"; CLEAN_OK=0; }
  # Restart exactly what setup stopped.
  local e
  for e in $WAS_ACTIVE; do on "${e%%:*}" "sudo systemctl start ${e#*:}" || { log "${e%%:*}: ${e#*:} not restarted"; CLEAN_OK=0; }; done
  if [[ $PG_DB =~ ^e2e_p13[0-9a-z]+$ ]]; then
    local l lh; l=$(pg_leader); lh=$(ssh_of_member "$l")
    [ -n "$lh" ] && ssh_ "$lh" "sudo -u postgres dropdb --if-exists --force $PG_DB && \
      ! sudo -u postgres psql -X -At -d postgres -c \"SELECT 1 FROM pg_database WHERE datname = '$PG_DB'\" | grep -q 1" \
      && log "dropped database $PG_DB" || { log "database $PG_DB not dropped"; CLEAN_OK=0; }
  fi
  local i p=000 q=000
  if grep -q 'cp:headscale-tailnets' <<<"$WAS_ACTIVE"; then
    for i in $(seq 1 30); do p=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$POC_HEALTH" || true); [ "$p" = 200 ] && break; sleep 1; done
    [ "$p" = 200 ] && log "P02 POC restored on mc-cp" || { log "POC not healthy ($p)"; CLEAN_OK=0; }
  fi
  if grep -q 'c2:headscale-cluster' <<<"$WAS_ACTIVE"; then
    for i in $(seq 1 40); do q=$(public_ready c2); [ "$q" = 200 ] && break; sleep 1; done
    [ "$q" = 200 ] && log "long-lived c2 /ready 200" || { log "long-lived c2 /ready $q"; CLEAN_OK=0; }
  fi
  rm -rf "$TMPD"
}
trap 'cleanup; exit 2' INT TERM HUP

# --------------------------------------------------------------- setup ----
# db_section HOSTS PORT DB: the database: block of a run config.
db_section() {
  printf 'database:\n  type: postgres\n  postgres:\n    hosts:\n'
  local h; for h in $1; do printf '      - "%s:%s"\n' "$h" "$2"; done
  printf '    name: %s\n    user: headscale\n    pass: file:%s\n    ssl: disable\n' "$3" "$4"
  printf '    target_session_attrs: read-write\n    connect_timeout_secs: 2\n'
}
# with_db FILE HOSTS PORT DB PASSFILE: FILE with its database: block replaced.
with_db() {
  db_section "$2" "$3" "$4" "$5" >"$TMPD/dbsec"
  awk -v sec="$TMPD/dbsec" '
    /^database:/ { while ((getline l < sec) > 0) print l; skip = 1; next }
    skip && /^[^ #]/ { skip = 0 }
    !skip { print }' "$1"
}

setup() {
  local pass i n s
  log "checks: lab free, mc-ci linger, Patroni healthy, PgBouncer on both PG nodes"
  cp_ "for u in mesh-c1-ha mesh-n1-e2e mesh-p12b-e2e mesh-c1-e2e mesh-p11b-e2e $UNIT; do systemctl is-active -q \$u && { echo \$u; exit 1; }; done; exit 0" \
    >"$TMPD/busy" || { log "lab busy: $(cat "$TMPD/busy") runs on mc-cp"; return 1; }
  ci_ "loginctl show-user \$(id -un) -p Linger | grep -qx Linger=yes" || { log "mc-ci: no linger (logind would wipe $CIDIR)"; return 1; }
  wait_pg_healthy 10 || { log "Patroni not healthy: $(pg_state)"; return 1; }
  for h in $PG1_IP $PG2_IP; do ci_ "timeout 3 bash -c '</dev/tcp/$h/$PGB_PORT'" || { log "PgBouncer $h:$PGB_PORT not reachable from mc-ci"; return 1; }; done
  pg_state >"$ART_DIR/logs/patroni-start.txt"
  pass=$(grep -E '^MC_PG_HEADSCALE_PASSWORD=' "$PG_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || { log "no MC_PG_HEADSCALE_PASSWORD in $PG_ENV"; return 1; }
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"

  log "database $PG_DB on the Patroni leader $(pg_leader)"
  ssh_ "$(ssh_of_member "$(pg_leader)")" "sudo -u postgres psql -X -q -v ON_ERROR_STOP=1 -d postgres -c 'CREATE DATABASE $PG_DB OWNER headscale'" || return 1

  log "what runs today (restarted at cleanup)"
  for s in cp:headscale-tailnets cp:headscale-cluster cp:mesh-bootstrap-e2e c2:headscale-cluster c2b:headscale-cluster c1b:headscale-cluster; do
    on "${s%%:*}" "systemctl is-active -q ${s#*:}" && WAS_ACTIVE="$WAS_ACTIVE $s"
  done
  log "  active: ${WAS_ACTIVE:- none}"

  binsrv_start || return 1
  log "binary $HS on $ALLNODES${CI_BINDIR:+ (from mc-ci $CI_BINDIR)}"
  for n in $ALLNODES; do put $n /tmp/p13-hs && on $n "sudo install -m 0755 /tmp/p13-hs $HS && rm -f /tmp/p13-hs" || return 1; done

  log "configs: c1 from the repo template, c2 from mc-c2's and mc-c2b's own; database $PG_DB through PgBouncer on both PG nodes"
  with_db "$REPO/infra/lab/mc-cp/cluster/config.yaml" "$PG1_IP $PG2_IP" "$PGB_PORT" "$PG_DB" "$PASSF" >"$TMPD/c1.yaml"
  sed "s#^listen_addr: .*#listen_addr: $CP_IP:8443#" "$TMPD/c1.yaml" >"$TMPD/cfg-cp.yaml"
  sed "s#^listen_addr: .*#listen_addr: 0.0.0.0:8443#" "$TMPD/c1.yaml" >"$TMPD/cfg-c1b.yaml"
  for n in c2 c2b; do
    on $n "sudo cat /etc/headscale/cluster/config.yaml" | strip_cr >"$TMPD/live-$n.yaml" || return 1
    with_db "$TMPD/live-$n.yaml" "$PG1_IP $PG2_IP" "$PGB_PORT" "$PG_DB" "$PASSF" >"$TMPD/cfg-$n.yaml"
  done
  for n in $ALLNODES; do
    grep -q "^    name: $PG_DB\$" "$TMPD/cfg-$n.yaml" && grep -q '^metrics_listen_addr: 127.0.0.1:9095' "$TMPD/cfg-$n.yaml" || { log "$n: config not as expected"; return 1; }
    cp "$TMPD/cfg-$n.yaml" "$ART_DIR/logs/config-$n.yaml"
  done

  log "stop what the run replaces"
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; true"
  for n in c2 c2b c1b; do on $n "sudo systemctl stop headscale-cluster 2>/dev/null; true"; done
  for n in $ALLNODES; do
    on $n "sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && \
      sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster && \
      sudo install -o root -g headscale -m 0640 /dev/stdin $PASSF" <"$TMPD/pg.pass" || return 1
    on $n "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/cfg-$n.yaml" || return 1
  done

  log "mc-c1b: TLS and the Noise key of c1 (pipes only), nft as P11b"
  cp_ "sudo test -s /var/lib/headscale/cluster/noise_private.key" || { log "mc-cp has no c1 Noise key"; return 1; }
  on c1b "id headscale >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/headscale --shell /usr/sbin/nologin headscale; \
    sudo install -d -o root -g headscale -m 0750 /etc/headscale /etc/headscale/tls" || return 1
  cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | on c1b 'sudo install -o root -g headscale -m 0644 /dev/stdin /etc/headscale/tls/fullchain.pem' || return 1
  cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | on c1b 'sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/tls/privkey.pem' || return 1
  cp_ 'sudo cat /var/lib/headscale/cluster/noise_private.key' | on c1b 'sudo install -o headscale -g headscale -m 0600 /dev/stdin /var/lib/headscale/cluster/noise_private.key' || return 1
  [ "$(cp_ 'sudo sha256sum /var/lib/headscale/cluster/noise_private.key' | cut -c1-64)" = \
    "$(on c1b 'sudo sha256sum /var/lib/headscale/cluster/noise_private.key' | cut -c1-64)" ] || { log "noise key copy mismatch"; return 1; }
  on c1b "sudo nft list table inet mc_c1b >/dev/null 2>&1" || { log "mc-c1b has no nft table mc_c1b (run the P11b setup once)"; return 1; }

  log "HAProxy for c1 on mc-cp ($URL1 -> /ready of mc-cp or mc-c1b)"
  cp_ "sudo tee $HACFG >/dev/null && sudo haproxy -c -q -f $HACFG" <<EOF || return 1
# P13 drill only (infra/lab/e2e/p13): c1 traffic from the VPS tunnel to the
# process whose /ready is 200. TCP, TLS passthrough.
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
  server mc-cp $CP_IP:8443
  server mc-c1b $C1B_IP:8443
EOF
  cp_ "sudo systemctl reset-failed $HAUNIT 2>/dev/null; sudo systemd-run --quiet --unit=$HAUNIT /usr/sbin/haproxy -Ws -f $HACFG" || return 1
  cp_ "systemctl is-active -q mesh-c2-ha" || { log "long-lived mesh-c2-ha (c2 front door) not running on mc-cp"; return 1; }

  log "mc-ci: $CIDIR (jobs.sh, passfile for the SQL writer)"
  ci_ "rm -rf $CIDIR; install -d -m 0700 $CIDIR" || return 1
  tr -d '\r' <"$HERE/jobs.sh" | ci_ "cat >$CIDIR/jobs.sh && chmod 0700 $CIDIR/jobs.sh && bash -n $CIDIR/jobs.sh" || return 1
  ci_ "umask 077; IFS= read -r p; for h in $PG1_IP $PG2_IP; do for port in $PGB_PORT 5432; do printf '%s:%s:*:headscale:%s\n' \$h \$port \"\$p\"; done; done >$CIDIR/pgpass" <"$TMPD/pg.pass" || return 1
  ci_ "sudo -n systemd-run --version >/dev/null" || { log "mc-ci: no passwordless systemd-run"; return 1; }

  log "schema: migrate step ($MIGRATE_VIA)"
  if [ "$MIGRATE_VIA" = direct ]; then
    with_db "$TMPD/c1.yaml" "$PG1_IP $PG2_IP" 5432 "$PG_DB" "$CIDIR/pgpass.raw" >"$TMPD/migrate.yaml"
    ci_ "umask 077; cat >$CIDIR/migrate.yaml" <"$TMPD/migrate.yaml" || return 1
    ci_ "umask 077; IFS= read -r p; printf '%s' \"\$p\" >$CIDIR/pgpass.raw" <"$TMPD/pg.pass" || return 1
    if [ -n "$CI_BINDIR" ]; then ci_ "timeout 600 $CI_BINDIR/headscale -c $CIDIR/migrate.yaml cluster migrate"
    else scp_ "$BIN" "$CI_SSH:$CIDIR/hs" && ci_ "chmod 0700 $CIDIR/hs && timeout 600 $CIDIR/hs -c $CIDIR/migrate.yaml cluster migrate"; fi \
      >"$ART_DIR/logs/migrate.json" 2>>"$ART_DIR/logs/migrate.err" || { log "migrate step failed (logs/migrate.err; MIGRATE_VIA=pgbouncer runs it through PgBouncer)"; return 1; }
    ci_ "rm -f $CIDIR/pgpass.raw $CIDIR/migrate.yaml $CIDIR/hs"
  else
    cli c1 migrate >"$ART_DIR/logs/migrate.json" || { log "migrate step failed"; return 1; }
  fi
  log "probe table p13_probe (role headscale)"
  psql_ "SET ROLE headscale; CREATE TABLE p13_probe(seq int NOT NULL, server inet, ts timestamptz NOT NULL DEFAULT clock_timestamp())" >/dev/null || return 1

  log "registry: c1 and c2 ACTIVE (CLI), processes: active first, then standby"
  cli c1 register --class shared --max-devices 5000 >/dev/null || return 1
  cli c2 register --class shared --max-devices 5000 >/dev/null || return 1
  start_proc cp && start_proc c2 || return 1
  wait_public_ready c1 40 && wait_public_ready c2 40 || { log "public /ready c1=$(public_ready c1) c2=$(public_ready c2)"; return 1; }
  start_proc c1b && start_proc c2b || return 1
  sleep 5
  snap_metrics setup
  [ "$(held_nodes setup)" = "cp c2" ] || { log "lease holders after start: [$(held_nodes setup)] (want cp c2)"; return 1; }

  log "tailnets fa (c1) and fb (c2), allow-all, keys"
  new_tailnet fa c1 && new_tailnet fb c2 || { log "tailnets"; return 1; }
  for s in fa fb; do ci_ "umask 077; cat >$CIDIR/api-$s" <"$TMPD/api-$s" || return 1; done
  local img; for img in $IMAGES; do ci_ "docker image inspect $img >/dev/null 2>&1 || docker pull -q $img >/dev/null" || return 1; done
  binsrv_stop || { log "p13-binsrv not stopped"; return 1; }
  T_SETUP=$(date +%s)
}

# ---------------------------------------------------------------- rounds ---
ROUND=0
VMA_IP=- VMB_IP=- VMVER=-
# round_start IMAGE: clients of the round enrolled, baseline pings, probes started.
round_start() {
  local img=$1 r p="" c
  ROUND=$((ROUND + 1)); r=p13-r$ROUND; VMVER=${img##*:v}
  log "round $ROUND: $img"
  for c in a1 a2 aw; do start_ts "$r-$c" "$img" && ts_up "$r-$c" "$URL1" "$TMPD/key-fa" || p="$p; enroll $r-$c"; done
  for c in b1 b2 bw; do start_ts "$r-$c" "$img" && ts_up "$r-$c" "$URL2" "$TMPD/key-fb" || p="$p; enroll $r-$c"; done
  VMA_IP=- VMB_IP=-
  set -- $VM_TARGETS
  if [ $# -ge 1 ]; then local rc=0; vm_start "$1" "$VMVER" "$URL1" "$TMPD/key-fa" || rc=$?
    if [ $rc = 3 ]; then log "  $1: no /opt/tailscale/$VMVER; VM A skipped this round"; elif [ $rc != 0 ]; then p="$p; VM A up $rc"; else VMA_IP=$(vm_field "$1" "$VMVER" "d['Self']['TailscaleIPs'][0]"); fi; fi
  if [ $# -ge 2 ]; then local rc=0; vm_start "$2" "$VMVER" "$URL2" "$TMPD/key-fb" || rc=$?
    if [ $rc = 3 ]; then log "  $2: no /opt/tailscale/$VMVER; VM B skipped this round"; elif [ $rc != 0 ]; then p="$p; VM B up $rc"; else VMB_IP=$(vm_field "$2" "$VMVER" "d['Self']['TailscaleIPs'][0]"); fi; fi
  local a2ip b2ip; a2ip=$(selfip "$r-a2"); b2ip=$(selfip "$r-b2")
  ping_ok "$r-a1" "$a2ip" || p="$p; a1 cannot ping a2"
  ping_ok "$r-b1" "$b2ip" || p="$p; b1 cannot ping b2"
  [ "$VMA_IP" = - ] || ping_ok "$r-a1" "$VMA_IP" || p="$p; a1 cannot ping VM A"
  [ "$VMB_IP" = - ] || ping_ok "$r-b1" "$VMB_IP" || p="$p; b1 cannot ping VM B"
  local awid bwid; awid=$(node_id fa "$r-aw"); bwid=$(node_id fb "$r-bw")
  [ -n "$awid" ] && [ -n "$bwid" ] || p="$p; no node id for aw/bw"
  for c in a1 a2 b1 b2; do echo "$(needs_login "$r-$c")" >"$TMPD/nl-$c"; done
  ci_ "rm -f $CIDIR/stop $CIDIR/probe-*.txt"
  job ping-a "ping $r-a1 $a2ip $CIDIR/probe-ping-a.txt $CIDIR/stop" || p="$p; job ping-a"
  job ping-b "ping $r-b1 $b2ip $CIDIR/probe-ping-b.txt $CIDIR/stop" || p="$p; job ping-b"
  [ "$VMA_IP" = - ] || job ping-va "ping $r-a1 $VMA_IP $CIDIR/probe-ping-va.txt $CIDIR/stop" || p="$p; job ping-va"
  [ "$VMB_IP" = - ] || job ping-vb "ping $r-b1 $VMB_IP $CIDIR/probe-ping-vb.txt $CIDIR/stop" || p="$p; job ping-vb"
  job ready-c1 "ready $URL1 $CIDIR/probe-ready-c1.txt $CIDIR/stop" || p="$p; job ready-c1"
  job ready-c2 "ready $URL2 $CIDIR/probe-ready-c2.txt $CIDIR/stop" || p="$p; job ready-c2"
  job wprobe-c1 "wprobe $URL1 $CIDIR/api-fa $awid $r-aw $CIDIR/probe-write-c1.txt $CIDIR/stop" || p="$p; job wprobe-c1"
  job wprobe-c2 "wprobe $URL2 $CIDIR/api-fb $bwid $r-bw $CIDIR/probe-write-c2.txt $CIDIR/stop" || p="$p; job wprobe-c2"
  job sqlw "sqlw $CIDIR/pgpass $PG_DB $PG1_IP,$PG2_IP $PGB_PORT $CIDIR/probe-sql.txt $CIDIR/stop $((ROUND * 10000000))" || p="$p; job sqlw"
  sleep 10
  # The probes must be green before any fault.
  local t1 t0 w; t1=$(ci_now); t0=$((t1 - 8000)); fetch_probes
  for w in write-c1 write-c2; do [ "$(analyze write "$ART_DIR/probes/r$ROUND/probe-$w.txt" "$t0" "$t1" | awk '{print $4}')" = 0 ] || p="$p; $w probe failing before any fault"; done
  [ "$(analyze sql "$ART_DIR/probes/r$ROUND/probe-sql.txt" "$t0" "$t1" | awk '{print $1}')" -gt 0 ] || p="$p; SQL writer has no acknowledged write"
  [ -z "$p" ] && record "R$ROUND.0" PASS "round $ROUND ($img): a1/a2/aw at c1, b1/b2/bw at c2$([ "$VMA_IP" = - ] || echo ", VM A")$([ "$VMB_IP" = - ] || echo ", VM B"); pings, write probes (aw $awid, bw $bwid) and SQL writer green" \
    || { record "R$ROUND.0" FAIL "${p#; }"; return 1; }
}
round_end() {
  stop_jobs || true; fetch_probes
  local c; for c in $(ci_ "docker ps -a --filter label=$LABEL --format '{{.Names}}'" 2>/dev/null); do save_logs "$c"; done
  ci_ "docker ps -aq --filter label=$LABEL | xargs -r docker rm -f >/dev/null; docker volume ls -q --filter label=$LABEL | xargs -r docker volume rm >/dev/null"
  local t; for t in $VM_TARGETS; do vm_stop "$t" >/dev/null 2>&1 || true; done
}

# ---------------------------------------------------------- case frame ----
# case_begin TAG: snapshot before a fault; sets C_T0 (mc-ci ms) last.
case_begin() {
  C_TAG=$1
  C_PG0=$(pg_state); C_LEASE0=$(lease_rows | tr '\n' ' '); C_OWN0=$(owner_epochs)
  snap_metrics "$C_TAG-0"
  C_HELD0=$(held_nodes "$C_TAG-0")
  chrony_offsets >"$ART_DIR/logs/$C_TAG-chrony.txt"
  printf '%s before: patroni [%s] leases [%s] owners [%s] holders [%s]\n' "$C_TAG" "$C_PG0" "$C_LEASE0" "$C_OWN0" "$C_HELD0" >>"$ART_DIR/timeline.txt"
  C_T0=$(ci_now)
}
# case_recover: waits for both write probes to succeed again (up to
# RECOVER_MAX_MS + 30 s past C_T0); sets C_T1.
case_recover() {
  local end=$(( $(date +%s) + RECOVER_MAX_MS / 1000 + 30 )) t1 ok
  while [ "$(date +%s)" -lt "$end" ]; do
    sleep 2; t1=$(ci_now); fetch_probes; ok=1
    local w; for w in write-c1 write-c2; do
      # recovered when the last 4 s of the probe are all 200
      [ "$(analyze write "$ART_DIR/probes/r$ROUND/probe-$w.txt" $((t1 - 4000)) "$t1" | awk '{print $4}')" = 0 ] || ok=0
    done
    [ $ok = 1 ] && [ $((t1 - C_T0)) -gt 6000 ] && break
  done
  C_T1=$(ci_now); fetch_probes
}
# case_check: the common assertions of a case, appended to $P (problems)
# and $M (measurements); RECOVERY and PING thresholds apply.
case_check() { # MAX_RECOVER_MS
  local maxrec=$1 w a f rec win nfail g
  snap_metrics "$C_TAG-1"
  for w in c1 c2; do
    read -r f rec win nfail <<<"$(analyze write "$ART_DIR/probes/r$ROUND/probe-write-$w.txt" "$C_T0" "$C_T1")"
    M="$M; write $w: fail@${f}ms recovered@${rec}ms window ${win}ms ($nfail failed)"
    if [ "$f" = - ]; then eval "OUT_$w=0"; elif [ "$rec" = - ]; then eval "OUT_$w=999999"; else eval "OUT_$w=$((rec - f))"; fi
    [ "$rec" != - ] || P="$P; $w writes never recovered"
    [ "$rec" = - ] || [ "$rec" -le "$maxrec" ] || P="$P; $w recovered after ${rec} ms > ${maxrec}"
    read -r f rec win nfail <<<"$(analyze write "$ART_DIR/probes/r$ROUND/probe-ready-$w.txt" "$C_T0" "$C_T1")"
    M="$M; /ready $w: not-200@${f}ms back@${rec}ms"
  done
  for a in a b va vb; do
    [ -s "$ART_DIR/probes/r$ROUND/probe-ping-$a.txt" ] || continue
    read -r g _ <<<"$(analyze ping "$ART_DIR/probes/r$ROUND/probe-ping-$a.txt" "$C_T0" "$C_T1")"
    M="$M; ping-$a gap ${g}ms"
    [ "$g" -le "$GAP_MAX_MS" ] || P="$P; ping-$a gap ${g} ms > $GAP_MAX_MS"
  done
  # Epochs never go back; owners unchanged (no move in a drill).
  local l1 o1 c e0 e1; l1=$(lease_rows | tr '\n' ' '); o1=$(owner_epochs)
  for c in c1 c2; do
    e0=$(awk -v c="$(idof $c)" '{for (i = 1; i <= NF; i += 3) if ($i == c) print $(i + 2)}' <<<"$C_LEASE0")
    e1=$(awk -v c="$(idof $c)" '{for (i = 1; i <= NF; i += 3) if ($i == c) print $(i + 2)}' <<<"$l1")
    [ -n "$e0" ] && [ -n "$e1" ] && [ "$e1" -ge "$e0" ] || P="$P; lease epoch of $c $e0 -> $e1"
    M="$M; lease $c $e0->$e1"
  done
  [ "$o1" = "$C_OWN0" ] || P="$P; owner epochs changed [$C_OWN0] -> [$o1]"
  local held1; held1=$(held_nodes "$C_TAG-1")
  [ "$(wc -w <<<"$held1")" = 2 ] || P="$P; lease holders now [$held1] (want one per cluster)"
  M="$M; holders [$C_HELD0] -> [$held1]; self_fence [$(fence_delta "$C_TAG-0" "$C_TAG-1" self_fence)] lost [$(fence_delta "$C_TAG-0" "$C_TAG-1" lost)] renew_error [$(fence_delta "$C_TAG-0" "$C_TAG-1" renew_error)]"
  # Clients: Running without any action, no new NeedsLogin (0 manual interventions).
  local r=p13-r$ROUND manual=0 i
  for c in a1 a2 b1 b2; do
    for i in $(seq 1 "$CLIENT_WAIT"); do [ "$(state "$r-$c")" = Running ] && break; sleep 1; done
    [ "$(state "$r-$c")" = Running ] || { manual=$((manual + 1)); P="$P; $r-$c not Running"; }
    [ "$(needs_login "$r-$c")" = "$(cat "$TMPD/nl-$c")" ] || P="$P; $r-$c logged out (NeedsLogin)"
  done
  [ "$manual" -le "$MANUAL_MAX" ] || P="$P; $manual client(s) would need a manual intervention"
  printf '%s after: patroni [%s] leases [%s] owners [%s] holders [%s] window %s..%s\n' "$C_TAG" "$(pg_state)" "$l1" "$o1" "$held1" "$C_T0" "$C_T1" >>"$ART_DIR/timeline.txt"
}
# dead-man: undo CMD on a PG node after SECS unless cancelled (p13-undo-NAME).
deadman() { # SSH NAME SECS CMD
  ssh_ "$1" "sudo systemctl reset-failed p13-undo-$2 2>/dev/null; sudo systemd-run --quiet --collect --unit=p13-undo-$2 --on-active=$3 /bin/sh -c '$4'"
}
deadman_cancel() { ssh_ "$1" "sudo systemctl stop p13-undo-$2.timer p13-undo-$2 2>/dev/null; true"; }

# ------------------------------------------------------------ run-wide ----
# t_l01: per cluster, lease epochs acquired in time order are strictly
# increasing and each has one holder (journal of all four processes).
t_l01() {
  local n p=""
  for n in $ALLNODES; do proc_log_since "$n" "$T_SETUP" | redact >"$ART_DIR/logs/$n-process.log"; done
  cat "$ART_DIR"/logs/{cp,c1b,c2,c2b}-process.log | grep -aF 'cluster lease acquired: active' >"$ART_DIR/logs/lease-events.log"
  py 'import re,sys
ev = {}
for l in open(sys.argv[1], encoding="utf-8", errors="replace"):
    t = re.match(r"(\S+)", l); e = re.search(r"lease_epoch=(\d+)", l); c = re.search(r"cluster=(\S+)", l); h = re.search(r"holder=(\S+)", l)
    if t and e and c:
        ev.setdefault(c.group(1), []).append((t.group(1), int(e.group(1)), h.group(1) if h else "?"))
bad = []
for c, xs in sorted(ev.items()):
    xs.sort()
    eps = [x[1] for x in xs]
    if eps != sorted(set(eps)): bad.append(c)
    print(c, " ".join("%d@%s" % (x[1], x[2][-12:]) for x in xs))
print("BAD" if bad else "OK", " ".join(bad))' "$ART_DIR/logs/lease-events.log" | strip_cr >"$TMPD/l01"
  cp "$TMPD/l01" "$ART_DIR/logs/l01.txt"
  grep -q '^OK' "$TMPD/l01" || p="epochs not strictly increasing or reused: $(tr '\n' ' ' <"$TMPD/l01")"
  [ -z "$p" ] && record L01 PASS "lease epochs in time order, one holder each: $(grep -v '^OK' "$TMPD/l01" | tr '\n' ';')" || record L01 FAIL "$p"
}
# t_secrets: no key, private key or the PG password in the artifacts (M07-style).
t_secrets() {
  local hits
  hits=$(grep -rlE 'hskey-(auth|api)-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}|BEGIN [A-Z ]*PRIVATE KEY' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
  [ -s "$TMPD/pg.pass" ] && hits="$hits $(grep -rlF -f "$TMPD/pg.pass" "$ART_DIR" 2>/dev/null)"
  hits=$(echo "$hits" | xargs)
  [ -z "$hits" ] && record S01 PASS "no auth/API key, private key or database password in the artifacts" || record S01 FAIL "unredacted in: $hits"
}

# main_run CASES...: setup, one round per image running the cases, run-wide
# checks, cleanup; exit 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
main_run() {
  T0=$(date +%s)
  log "P13 drill $DRILL $RUN_ID at $SHA (artifacts: ${ART_DIR#"$REPO"/}); cases: $*"
  if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then log "setup failed (see logs/setup.log)"; cleanup; exit 2; fi
  { echo "commit: $SHA"; echo "database: $PG_DB"; echo "binary: $(binsha | cut -c1-16)"; echo "images: $IMAGES"; echo "vm targets: $VM_TARGETS"
    echo "thresholds: recover ${RECOVER_MAX_MS} ms, ping gap ${GAP_MAX_MS} ms, write errors ${WERR_MAX_MS} ms, manual ${MANUAL_MAX}"; } >>"$ART_DIR/env.txt"
  local img c
  for img in $IMAGES; do
    if round_start "$img"; then
      for c in "$@"; do
        "t_$c"
        wait_pg_healthy "$PG_HEAL_MAX" || { record "R$ROUND.abort" FAIL "Patroni not healthy after $c: $(pg_state); remaining cases skipped"; break 2; }
      done
    fi
    round_end
  done
  t_l01
  t_secrets
  cleanup
  log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
  [ "$CLEAN_OK" = 1 ] || exit 2
  [ "$FAILS" = 0 ] || exit 1
  exit 0
}
