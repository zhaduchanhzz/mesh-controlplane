#!/usr/bin/env bash
# P13c drill F08 (tickets/P13.md §P13c, ADR 0012; 06-tests-benchmarks.md
# F08): a PostgreSQL point-in-time restore must never let an epoch go back.
#
#   backup B -> F01 on c1 (lease +1) -> move tailnet m c1 -> c2 (owner_epoch
#   +1) -> restore to B (PITR) -> [window: processes reach the restored
#   database BEFORE the fence] -> epoch fence -> recovery
#
# Where: the HA cluster mc-pg (Patroni, mc-pg-01 .247 / mc-pg-02 .248), its
# PgBouncers :6432 (DSN host list, target_session_attrs read-write), pgBackRest
# stanza STANZA (repo as configured on the PG nodes). The drill database
# e2e_p13f08<run> is the ONLY data the drill changes: the restore goes into a
# scratch instance (port 5439, data in /var/lib/p13-f08 on the replica node,
# archive off), and the drill database alone is swapped for its restored copy
# (pg_dump of the scratch instance -> DROP/CREATE/pg_restore on the primary).
# meshlab and every other database are never written; the scratch copy is
# deleted at cleanup. Clusters: c1 = mc-cp + mc-c1b behind a transient HAProxy
# on mc-cp (as P11b/N1), c2 = mc-c2 + mc-c2b behind the long-lived mesh-c2-ha;
# run binary/config/unit/password file of their own (-p13f08); the P02 POC and
# the long-lived c2 are stopped for the run and restarted at cleanup.
# Clients: docker on mc-ci, one per image (IMAGES) in each tailnet m (moved),
# k (c1, control), o (c2, control), plus m-new enrolled after B.
#
# Note on order: the runbook (ADR 0012) fences BEFORE PgBouncer lets the
# processes back in. The drill deliberately leaves the restored database
# reachable for F08_PREFENCE_S (40 s) first: that window is the negative test
# "a pre-restore process still alive is blocked before the fence" (in-process
# high-water marks of lease and owner epochs).
#
# Cases:
#   P00 preconditions (before any change; a failure exits 2 and lists what is
#       missing): CI_BINDIR headscale, MC_PG_HEADSCALE_PASSWORD, one Patroni
#       primary and one replica, pgBackRest stanza ok and archive_command =
#       pgbackrest, PgBouncer :6432 on both PG nodes reachable from mc-cp,
#       scratch port free, pg_ctl/pg_dump present
#   F00 baseline: lease epochs 1/1, clients Running, pairs ping
#   F01 backup (type F08_BACKUP_TYPE, default full) + restore point B, timed
#   F02 F01 on c1 (kill -9 active; standby takes lease epoch 2; killed
#       process back as standby); clients without login
#   F03 m-new enrolled in m at c1 after B
#   F04 m moved to c2 (`tailnet reassign --to`), m clients re-pointed to c2
#   F05 restore to B into the scratch instance (timed); the restored state is
#       the state recorded at B; the drill database swapped
#   F06 window before the fence: both c1 processes refuse the lease (HWM;
#       /ready 503 at every sample, "CRITICAL epoch regression" logged); c2
#       never loads m
#   F07 epoch fence (`headscale cluster epoch-fence`): floor F, rows raised
#   F08 recovery <= F08_RECOVERY_MAX_S (D12: 300 s) after the restore: c1 and
#       c2 serve; every lease epoch and owner_epoch >= F; m owned by c1, never
#       loaded by c2; every lease acquired and runtime loaded after the fence
#       at an epoch >= F; c2's pre-restore holder lost its lease; k and o
#       clients without login; m clients back to c1 (counted re-logins: the
#       two moved after B keep their node IDs, m-new re-enrols)
#   F09 audit: lease epochs per cluster strictly increasing over the whole
#       run (logs of all four processes), owner_epoch of m after the fence
#       above any value before the restore, platform_epoch_fences row,
#       DELETE FROM cluster_leases refused
#   S99 no secret in the artifacts
#
# Usage: CI_BINDIR=/home/mesh/<bindir with headscale> bash infra/lab/e2e/p13/f08.sh
#   Twice for the ticket's "×2". Never while N1/P12b/another drill runs.
# Exit: 0 no FAIL, 1 any FAIL, 2 precondition/setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail

SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
CP_SSH=mesh@192.168.1.241 C1B_SSH=mesh@192.168.1.245 C2_SSH=mesh@192.168.1.244 C2B_SSH=mesh@192.168.1.246
CI_SSH=mesh@192.168.1.240
PG1_SSH=mesh@192.168.1.247 PG2_SSH=mesh@192.168.1.248
PG_HOSTS=${PG_HOSTS:-192.168.1.247:6432,192.168.1.248:6432}
MC_PG_ENV=${MC_PG_ENV:-$HOME/.mesh-lab/mc-pg.env}
STANZA=${STANZA:-mc-pg}
PGBIN=${PGBIN:-/usr/lib/postgresql/16/bin}
SCRATCH=/var/lib/p13-f08
SCRATCH_PORT=${SCRATCH_PORT:-5439}
F08_BACKUP_TYPE=${F08_BACKUP_TYPE:-full}
F08_PREFENCE_S=${F08_PREFENCE_S:-40}
F08_RECOVERY_MAX_S=${F08_RECOVERY_MAX_S:-300}
IMAGES=${IMAGES:-tailscale/tailscale:v1.104.7 tailscale/tailscale:v1.84.3}
C1_ID=mc-cp-c1 C2_ID=mc-c2
HOST1=c1.cp.gta5web.online HOST2=c2.cp.gta5web.online
URL1=https://$HOST1:8443 URL2=https://$HOST2:8445
POC_HEALTH=https://a.cp.gta5web.online:8443/health
CI_BINDIR=${CI_BINDIR:-}
BINPORT=${BINPORT:-18092}
KEEP=${KEEP:-0}
PY=${PY:-python}
export PYTHONUTF8=1 PYTHONIOENCODING=utf-8
[[ $CI_BINDIR =~ ^/[A-Za-z0-9/_.-]+$ ]] || { echo "CI_BINDIR: absolute path on mc-ci, [A-Za-z0-9/_.-] only" >&2; exit 2; }
[[ $F08_PREFENCE_S =~ ^[0-9]+$ ]] && [ "$F08_PREFENCE_S" -ge 25 ] || { echo "F08_PREFENCE_S >= 25 (two lease TTLs + margin)" >&2; exit 2; }

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../../.." && pwd)
SHA=$(git -C "$REPO" rev-parse --short HEAD)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUN_LC=$(tr 'A-Z' 'a-z' <<<"$RUN_ID")
PG_DB=e2e_p13f08$RUN_LC
RP=p13f08_$RUN_LC
ART_DIR=$REPO/artifacts/$SHA/P13/f08-$RUN_ID
mkdir -p "$ART_DIR/logs"
TMPD=$(mktemp -d) && chmod 700 "$TMPD"
CR=$(printf '\r')

HS=/usr/local/bin/headscale-cluster-p13f08
CFG=/etc/headscale/cluster/config-p13f08.yaml
PGPASS=/etc/headscale/cluster/pg-p13f08.pass
UNIT=mesh-p13f08
LABEL=mesh-e2e=p13f08
CLI1="sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $C1_ID"
CLI2="sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $C2_ID"

redact() {
  sed -E \
    -e 's/(tskey|hskey)-[A-Za-z0-9_-]+/\1-<redacted>/g' \
    -e 's#/register/[A-Za-z0-9_-]+#/register/<redacted>#g' \
    -e 's/((mkey|nodekey|discokey|nlpub|privkey):)([0-9a-f]{8})[0-9a-f]{56}/\1\3…/g'
}
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$ART_DIR/run.log" >&2; }
SSHO=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
ssh_() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
cp_() { ssh_ "$CP_SSH" "$@"; }
sshof() { case $1 in cp) echo $CP_SSH;; c1b) echo $C1B_SSH;; c2) echo $C2_SSH;; c2b) echo $C2B_SSH;; pg1) echo $PG1_SSH;; pg2) echo $PG2_SSH;; esac; }
on() { local n=$1; shift; ssh_ "$(sshof "$n")" "$@"; }
ipof() { case $1 in cp) echo 192.168.1.241;; c1b) echo 192.168.1.245;; c2) echo 192.168.1.244;; c2b) echo 192.168.1.246;; esac; }
clusterof() { case $1 in cp|c1b) echo c1;; *) echo c2;; esac; }
hostof() { [ "$1" = c1 ] && echo $HOST1 || echo $HOST2; }
urlof() { [ "$1" = c1 ] && echo $URL1 || echo $URL2; }
idof() { [ "$1" = c1 ] && echo $C1_ID || echo $C2_ID; }
nodesof() { [ "$1" = c1 ] && echo "cp c1b" || echo "c2 c2b"; }
py() { local code=$1; shift; "$PY" -c "import sys; sys.stdout.reconfigure(newline='\n')
$code" "$@"; }
strip_cr() { local v; v=$(cat); printf '%s' "${v//$CR/}"; }
now_ms() { py 'import time; print(int(time.time()*1000))' | strip_cr; }
cli() { local c=$1; shift; if [ "$c" = c1 ]; then cp_ "$CLI1 $*"; else on c2 "$CLI2 $*"; fi; }

FAILS=0
record() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$ART_DIR/results.tsv"
  printf '%-5s %-4s %s\n' "$2" "$1" "$3" | tee -a "$ART_DIR/run.log"
  [ "$2" = FAIL ] && FAILS=$((FAILS + 1))
  return 0
}
# shellcheck source=lib-ops.sh
. "$HERE/lib-ops.sh"

# ------------------------------------------------------------ processes ----
direct() {
  local c h; h=$(hostof "$(clusterof "$1")")
  c=$(cp_ "curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve '$h:8443:$(ipof "$1")' 'https://$h:8443$2'" 2>/dev/null)
  echo "${c:-000}"
}
public_ready() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$(urlof "$1")/ready" || true; }
wait_direct_ready() { local end=$(( $(date +%s) + $2 )); while [ "$(date +%s)" -lt "$end" ]; do [ "$(direct "$1" /ready)" = 200 ] && return 0; sleep 0.2; done; return 1; }
wait_public_ready() { for _ in $(seq 1 "$2"); do [ "$(public_ready "$1")" = 200 ] && return 0; sleep 1; done; return 1; }
start_proc() {
  on "$1" "sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemd-run --quiet --unit=$UNIT --uid=headscale --gid=headscale \
    -p RuntimeDirectory=headscale-cluster -p RuntimeDirectoryMode=0770 \
    $HS -c $CFG cluster serve --cluster-id $(idof "$(clusterof "$1")")"
}
stop_proc() { on "$1" "sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; true"; }
active_of() { local n; for n in $(nodesof "$1"); do [ "$(direct "$n" /ready)" = 200 ] && { echo "$n"; return; }; done; echo none; }
standby_of() { local a n; a=$(active_of "$1"); for n in $(nodesof "$1"); do [ "$n" != "$a" ] && echo "$n"; done; }
proc_log() { on "$1" "sudo journalctl -u $UNIT --since @$2 --no-pager" 2>/dev/null; } # NODE UNIX_SINCE

# --------------------------------------------------------------- database --
PRIM="" REPL=""
pg_role() { on "$1" "curl -s -o /dev/null -w '%{http_code}' -m 3 http://127.0.0.1:8008/$2" 2>/dev/null | strip_cr; }
find_roles() {
  PRIM="" REPL=""
  local n
  for n in pg1 pg2; do
    [ "$(pg_role $n primary)" = 200 ] && PRIM=$n
    [ "$(pg_role $n replica)" = 200 ] && REPL=$n
  done
  [ -n "$PRIM" ] && [ -n "$REPL" ] && [ "$PRIM" != "$REPL" ]
}
# psql_ NODE DB: SQL on stdin, as postgres over the local socket; -At output.
psql_() { on "$1" "sudo -u postgres psql -X -At -v ON_ERROR_STOP=1 -d $2 -f -" | strip_cr; }
db_state() { # NODE [PORT_ARGS]: "lease c1/c2, tailnets" one line per row
  on "$1" "sudo -u postgres psql -X -At -v ON_ERROR_STOP=1 ${2:-} -d $PG_DB -f -" <<'SQL' | strip_cr
SELECT 'lease '||cluster_id||' '||epoch FROM cluster_leases ORDER BY cluster_id;
SELECT 'tailnet '||slug||' '||owner_cluster_id||' '||owner_epoch||' '||COALESCE(moving_to_cluster_id,'-') FROM tailnets ORDER BY slug;
SELECT 'nodes '||count(*) FROM nodes;
SQL
}

# ------------------------------------------------------------------ binary -
binsrv_start() {
  ci_ "cd $CI_BINDIR && sha256sum headscale" | strip_cr | cut -c1-64 >"$TMPD/hs.sha" || return 1
  [ "$(wc -c <"$TMPD/hs.sha")" -ge 64 ] || return 1
  ci_ "sudo systemctl stop p13f08-binsrv 2>/dev/null; sudo systemctl reset-failed p13f08-binsrv 2>/dev/null; \
    sudo systemd-run --quiet --collect --unit=p13f08-binsrv --uid=mesh --gid=mesh -p RuntimeMaxSec=900 \
      python3 -m http.server $BINPORT --bind ${CI_SSH#*@} --directory $CI_BINDIR; \
    for i in \$(seq 1 20); do curl -fsS -o /dev/null -m 2 http://${CI_SSH#*@}:$BINPORT/ && exit 0; sleep 0.5; done; exit 1"
}
binsrv_stop() { ci_ "sudo systemctl stop p13f08-binsrv 2>/dev/null; sudo systemctl reset-failed p13f08-binsrv 2>/dev/null; ! systemctl is-active -q p13f08-binsrv"; }
put_hs() {
  on "$1" "curl -fsS --retry 3 -m 120 -o /tmp/p13f08-hs http://${CI_SSH#*@}:$BINPORT/headscale && \
    echo '$(cat "$TMPD/hs.sha")  /tmp/p13f08-hs' | sha256sum -c --quiet && sudo install -m 0755 /tmp/p13f08-hs $HS && rm -f /tmp/p13f08-hs"
}

# database: block of a cluster config -> the HA cluster through both PgBouncers.
cfg_db() { # SRC_FILE DST_FILE LISTEN
  local hosts; hosts=$(sed 's/,/, /g' <<<"$PG_HOSTS")
  awk -v hosts="$hosts" -v db="$PG_DB" -v pass="$PGPASS" -v listen="$3" '
    /^database:/ { print "database:\n  type: postgres\n  postgres:\n    hosts: [" hosts "]\n    target_session_attrs: read-write\n    name: " db \
      "\n    user: headscale\n    pass: file:" pass "\n    ssl: disable"; skip = 1; next }
    skip && /^[^ #]/ { skip = 0 }
    /^listen_addr:/ && listen != "" { print "listen_addr: " listen; next }
    !skip { print }' "$1" >"$2"
  [ "$(grep -c '^database:' "$2")" = 1 ] && grep -q "^    name: $PG_DB\$" "$2"
}

# --------------------------------------------------------------- cleanup ---
CLEAN_OK=1
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: leaving everything running"; return; }
  log "cleanup"
  local n
  for n in cp c1b c2 c2b; do proc_log $n "$T0" | redact >"$ART_DIR/logs/$n-process.log" 2>/dev/null || true; done
  p13_ci_cleanup "$LABEL" >/dev/null 2>&1 || { log "clients on mc-ci not removed"; CLEAN_OK=0; }
  binsrv_stop >/dev/null 2>&1 || { log "p13f08-binsrv not stopped"; CLEAN_OK=0; }
  for n in cp c1b c2 c2b; do
    on $n "sudo systemctl kill -s CONT $UNIT 2>/dev/null; sudo systemctl stop $UNIT 2>/dev/null; sudo systemctl reset-failed $UNIT 2>/dev/null; \
      sudo rm -f $CFG $PGPASS $HS; ! sudo test -e $PGPASS && ! test -e $HS" || { log "$n: run files not removed"; CLEAN_OK=0; }
  done
  cp_ "sudo systemctl stop mesh-c1-ha 2>/dev/null; sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo rm -f /etc/haproxy/mesh-c1-p13f08.cfg; true"
  on c1b "sudo rm -f /var/lib/headscale/cluster/noise_private.key; ! sudo test -e /var/lib/headscale/cluster/noise_private.key" \
    || { log "c1 key not removed on mc-c1b"; CLEAN_OK=0; }
  # Scratch instance and its data (a restored copy of the whole PG cluster).
  for n in pg1 pg2; do
    on $n "if sudo test -d $SCRATCH/pgdata; then sudo -u postgres $PGBIN/pg_ctl -D $SCRATCH/pgdata -m fast -w -t 60 stop >/dev/null 2>&1; fi; \
      sudo rm -rf $SCRATCH; ! sudo test -e $SCRATCH" || { log "$n: scratch instance not removed"; CLEAN_OK=0; }
  done
  if [[ $PG_DB =~ ^e2e_p13f08[0-9a-z]+$ ]] && find_roles; then
    on "$PRIM" "sudo -u postgres psql -X -At -c 'DROP DATABASE IF EXISTS $PG_DB WITH (FORCE)' && \
      ! sudo -u postgres psql -X -At -c \"SELECT 1 FROM pg_database WHERE datname = '$PG_DB'\" | grep -q 1" \
      && log "dropped database $PG_DB" || { log "database $PG_DB not dropped"; CLEAN_OK=0; }
  else
    log "database $PG_DB: primary not found, not dropped"; CLEAN_OK=0
  fi
  cp_ "sudo systemctl start headscale-tailnets" || { log "mc-cp POC not restarted"; CLEAN_OK=0; }
  for n in c2 c2b; do on $n "sudo systemctl start headscale-cluster" || { log "$n: long-lived c2 not started"; CLEAN_OK=0; }; done
  local p=000 q=000
  for _ in $(seq 1 40); do
    p=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$POC_HEALTH" || true); q=$(public_ready c2)
    [ "$p$q" = 200200 ] && break; sleep 1
  done
  [ "$p$q" = 200200 ] && log "POC and long-lived c2 back (200/200)" || { log "POC $p, c2 $q"; CLEAN_OK=0; }
  rm -rf "$TMPD"
}

# ---------------------------------------------------------- preconditions --
precheck() {
  local miss="" n out
  [ -s "$MC_PG_ENV" ] && grep -q '^MC_PG_HEADSCALE_PASSWORD=.' "$MC_PG_ENV" || miss="$miss; MC_PG_HEADSCALE_PASSWORD in $MC_PG_ENV"
  ci_ "test -s $CI_BINDIR/headscale" || miss="$miss; $CI_BINDIR/headscale on mc-ci"
  find_roles || miss="$miss; one Patroni primary and one replica (8008 /primary, /replica): primary=[$PRIM] replica=[$REPL]"
  if [ -n "$PRIM" ]; then
    out=$(on "$PRIM" "sudo -u postgres pgbackrest --stanza=$STANZA --output=json info" 2>/dev/null)
    [ "$(py 'import json; d=json.load(sys.stdin); print(d[0]["status"]["code"])' <<<"$out" 2>/dev/null | strip_cr)" = 0 ] \
      || miss="$miss; pgBackRest stanza '$STANZA' (stanza-create + check on the primary; repo, cipher pass outside Git)"
    out=$(on "$PRIM" "sudo -u postgres psql -X -At -c 'SHOW archive_command'" 2>/dev/null | strip_cr)
    [[ $out == *pgbackrest* ]] || miss="$miss; archive_command = pgbackrest archive-push (now: '$out')"
  fi
  for n in ${PG_HOSTS//,/ }; do
    cp_ "timeout 3 bash -c '</dev/tcp/${n%:*}/${n##*:}'" 2>/dev/null || miss="$miss; PgBouncer $n from mc-cp"
  done
  if [ -n "$REPL" ]; then
    on "$REPL" "! ss -ltn | grep -q ':$SCRATCH_PORT ' && test -x $PGBIN/pg_ctl && command -v pg_dump >/dev/null && ! sudo test -e $SCRATCH" \
      || miss="$miss; on the replica: port $SCRATCH_PORT free, $PGBIN/pg_ctl, pg_dump, no leftover $SCRATCH"
  fi
  [ -z "$miss" ] && return 0
  printf 'missing infrastructure / preconditions:\n%s\n' "$(tr ';' '\n' <<<"${miss#; }" | sed 's/^ */  - /')" | tee "$ART_DIR/precondition.txt" >&2
  return 1
}

# ------------------------------------------------------------------ setup --
CLIENTS="" M_CLIENTS="" K_CLIENTS="" O_CLIENTS=""
setup() {
  local pass n s img vt nm
  pass=$(grep -E '^MC_PG_HEADSCALE_PASSWORD=' "$MC_PG_ENV" | head -1 | cut -d= -f2-); pass=${pass%"$CR"}
  [ -n "$pass" ] || return 1
  printf '%s' "$pass" >"$TMPD/pg.pass"; unset pass; chmod 600 "$TMPD/pg.pass"

  log "database $PG_DB on the primary ($PRIM)"
  psql_ "$PRIM" postgres <<<"CREATE DATABASE $PG_DB OWNER headscale;" >/dev/null || return 1

  log "binary $HS on the four cluster hosts (from mc-ci $CI_BINDIR)"
  binsrv_start || return 1
  for n in cp c1b c2 c2b; do put_hs $n || return 1; done
  binsrv_stop || return 1

  log "configs: c1 from the repo, c2 from mc-c2's own config; database = $PG_DB via $PG_HOSTS"
  cfg_db "$REPO/infra/lab/mc-cp/cluster/config.yaml" "$TMPD/c1-cp.yaml" 192.168.1.241:8443 || return 1
  cfg_db "$REPO/infra/lab/mc-cp/cluster/config.yaml" "$TMPD/c1-b.yaml" 0.0.0.0:8443 || return 1
  on c2 "sudo cat /etc/headscale/cluster/config.yaml" >"$TMPD/c2-src.yaml" || return 1
  cfg_db "$TMPD/c2-src.yaml" "$TMPD/c2.yaml" "" || return 1
  rm -f "$TMPD/c2-src.yaml"

  log "stop the P02 POC (mc-cp) and the long-lived c2 (mc-c2, mc-c2b)"
  cp_ "sudo systemctl stop headscale-tailnets headscale-cluster mesh-bootstrap-e2e 2>/dev/null; true"
  for n in c2 c2b; do on $n "sudo systemctl stop headscale-cluster" || return 1; done
  for n in cp c1b c2 c2b; do
    on $n "sudo install -d -o root -g headscale -m 0750 /etc/headscale/cluster && sudo install -o root -g headscale -m 0640 /dev/stdin $PGPASS" <"$TMPD/pg.pass" || return 1
  done
  cp_ "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1-cp.yaml" || return 1
  for n in c2 c2b; do on $n "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c2.yaml" || return 1; done

  log "mc-c1b: user, TLS, Noise key of c1, config (pipes only)"
  on c1b "set -e
    id headscale >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/headscale --shell /usr/sbin/nologin headscale
    sudo install -d -o root -g headscale -m 0750 /etc/headscale /etc/headscale/tls
    sudo install -d -o headscale -g headscale -m 0750 /var/lib/headscale /var/lib/headscale/cluster" || return 1
  cp_ "sudo test -s /var/lib/headscale/cluster/noise_private.key" || return 1
  cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | on c1b 'sudo install -o root -g headscale -m 0644 /dev/stdin /etc/headscale/tls/fullchain.pem' || return 1
  cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | on c1b 'sudo install -o root -g headscale -m 0640 /dev/stdin /etc/headscale/tls/privkey.pem' || return 1
  cp_ 'sudo cat /var/lib/headscale/cluster/noise_private.key' | on c1b 'sudo install -o headscale -g headscale -m 0600 /dev/stdin /var/lib/headscale/cluster/noise_private.key' || return 1
  on c1b "sudo install -o root -g headscale -m 0640 /dev/stdin $CFG" <"$TMPD/c1-b.yaml" || return 1

  log "HAProxy for c1 on mc-cp (transient)"
  cp_ "sudo tee /etc/haproxy/mesh-c1-p13f08.cfg >/dev/null && sudo haproxy -c -q -f /etc/haproxy/mesh-c1-p13f08.cfg" <<EOF || return 1
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
  cp_ "sudo systemctl reset-failed mesh-c1-ha 2>/dev/null; sudo systemd-run --quiet --unit=mesh-c1-ha /usr/sbin/haproxy -Ws -f /etc/haproxy/mesh-c1-p13f08.cfg" || return 1

  log "schema: migrate step (through PgBouncer, one transaction)"
  cli c1 "migrate" >"$ART_DIR/logs/migrate.json" || { log "migrate failed: PgBouncer route for $PG_DB? (catch-all or alias needed)"; return 1; }

  log "processes: actives first, then standbys"
  start_proc cp && wait_direct_ready cp 40 || { log "c1 on mc-cp not ready"; return 1; }
  start_proc c2 && wait_direct_ready c2 40 || { log "c2 on mc-c2 not ready"; return 1; }
  start_proc c1b && start_proc c2b || return 1
  wait_public_ready c1 30 && wait_public_ready c2 30 || { log "public /ready c1=$(public_ready c1) c2=$(public_ready c2)"; return 1; }

  log "registry; tailnets m, k (c1) and o (c2); policies; keys"
  cli c1 "register --class shared --max-devices 5000 >/dev/null" || return 1
  cli c2 "register --class shared --max-devices 5000 >/dev/null" || return 1
  local o pf=/var/lib/headscale/cluster/p13f08-policy.hujson
  for s in m k o; do
    o=c1; [ $s = o ] && o=c2
    cli $o "tailnet create --slug $s" >/dev/null && cli $o "tailnet status --slug $s --set ACTIVE" >/dev/null || return 1
    printf '%s\n' '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' \
      | on "$([ $o = c1 ] && echo cp || echo c2)" "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && \
          $([ $o = c1 ] && echo "$CLI1" || echo "$CLI2") tailnet policy --slug $s --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc" >/dev/null || return 1
    cli $o "tailnet authkey --slug $s --user f08-$s --reusable --expiration 240m" \
      | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-$s" || return 1
    chmod 600 "$TMPD/key-$s"; [ -s "$TMPD/key-$s" ] || return 1
  done

  log "clients on mc-ci"
  p13_ci_prepare || return 1
  for img in $IMAGES; do
    ci_ "docker image inspect $img >/dev/null 2>&1 || docker pull -q $img >/dev/null" || return 1
    vt=$(tr -d . <<<"${img##*:v}")
    for s in m k o; do
      nm=f08-$s-$vt
      o=$URL1; [ $s = o ] && o=$URL2
      p13_ts_start "$nm" "$img" "$LABEL" && p13_ts_up "$nm" "$o" "$TMPD/key-$s" || return 1
      CLIENTS="$CLIENTS $nm"
      case $s in m) M_CLIENTS="$M_CLIENTS $nm";; k) K_CLIENTS="$K_CLIENTS $nm";; o) O_CLIENTS="$O_CLIENTS $nm";; esac
    done
  done
}

# ---------------------------------------------------------------- helpers --
# pair_ok "A B": both Running and A pings B (tailscale ping, 3 tries).
pair_ok() {
  local a b; read -r a b <<<"$1"
  [ "$(p13_peers "$a" "$b" | awk '$2 == "Running"' | wc -l)" = 2 ] || return 1
  ci_ "docker exec $a tailscale ping -c 3 --timeout 5s $b" >>"$ART_DIR/logs/ping.log" 2>&1
}
wait_pair() { for _ in $(seq 1 "$2"); do pair_ok "$1" && return 0; sleep 2; done; return 1; }
lease_of() { db_state "$PRIM" | awk -v c="$1" '$1 == "lease" && $2 == c {print $3}'; }
owner_of() { db_state "$PRIM" | awk -v s="$1" '$1 == "tailnet" && $2 == s {print $3, $4, $5}'; }
nl_snapshot() { local nm; for nm in $CLIENTS; do p13_needs_login "$nm" >"$TMPD/nl-$1-$nm"; done; }
nl_same() { local nm p=""; for nm in $2; do [ "$(p13_needs_login "$nm")" = "$(cat "$TMPD/nl-$1-$nm")" ] || p="$p $nm"; done; printf '%s' "$p"; }
# repoint NAME URL SLUG: same node identity, other control URL.
repoint() { p13_ts_up "$1" "$2" "$TMPD/key-$3" --force-reauth; }

# ------------------------------------------------------------------ cases --
t_f00() {
  local p="" l1 l2
  l1=$(lease_of $C1_ID); l2=$(lease_of $C2_ID)
  [ "$l1" = 1 ] && [ "$l2" = 1 ] || p="$p; lease epochs c1=$l1 c2=$l2 (want 1/1)"
  wait_pair "$M_CLIENTS" 30 || p="$p; m pair"
  wait_pair "$K_CLIENTS" 30 || p="$p; k pair"
  wait_pair "$O_CLIENTS" 30 || p="$p; o pair"
  nl_snapshot base
  for s in m k o; do p13_node_map "$([ $s = o ] && echo c2 || echo c1)" $s >"$TMPD/nodes-$s"; done
  db_state "$PRIM" >"$ART_DIR/logs/state-F00.txt"
  [ -z "$p" ] && record F00 PASS "lease epochs 1/1; m, k (c1) and o (c2) pairs Running and pinging ($IMAGES)" || record F00 FAIL "${p#; }"
}

t_f01() {
  local p="" t0 t1 out
  t0=$(now_ms)
  out=$(on "$PRIM" "sudo -u postgres timeout 3600 pgbackrest --stanza=$STANZA --type=$F08_BACKUP_TYPE --log-level-console=info backup" 2>&1) || p="$p; backup failed"
  printf '%s\n' "$out" | redact >"$ART_DIR/logs/backup.log"
  t1=$(now_ms)
  psql_ "$PRIM" "$PG_DB" <<<"SELECT pg_create_restore_point('$RP'); SELECT pg_switch_wal();" >"$ART_DIR/logs/restore-point.txt" || p="$p; restore point"
  on "$PRIM" "sudo -u postgres timeout 300 pgbackrest --stanza=$STANZA check" >>"$ART_DIR/logs/backup.log" 2>&1 || p="$p; pgbackrest check after the restore point"
  db_state "$PRIM" >"$TMPD/state-B"; cp "$TMPD/state-B" "$ART_DIR/logs/state-B.txt"
  printf 'F01 backup (%s) %d ms\n' "$F08_BACKUP_TYPE" $((t1 - t0)) >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record F01 PASS "$F08_BACKUP_TYPE backup in $((t1 - t0)) ms; restore point $RP, WAL archived (check)" || record F01 FAIL "${p#; }"
}

t_f02() {
  local p="" a s e0 t0 t1
  a=$(active_of c1); s=$(standby_of c1); e0=$(lease_of $C1_ID)
  [ "$a" != none ] || { record F02 FAIL "no active c1"; return; }
  t0=$(now_ms)
  on "$a" "sudo systemctl kill --kill-whom=main -s KILL $UNIT" || p="$p; kill"
  wait_direct_ready "$s" 40 || p="$p; standby not ready"
  t1=$(now_ms)
  stop_proc "$a"; start_proc "$a"; sleep 3
  [ "$(direct "$a" /ready)" = 503 ] || p="$p; killed process not back as standby"
  [ "$(lease_of $C1_ID)" = $((e0 + 1)) ] || p="$p; c1 lease $e0 -> $(lease_of $C1_ID)"
  wait_pair "$M_CLIENTS" 30 && wait_pair "$K_CLIENTS" 30 || p="$p; c1 clients not back"
  local nl; nl=$(nl_same base "$M_CLIENTS $K_CLIENTS"); [ -z "$nl" ] || p="$p; NeedsLogin:$nl"
  printf 'F02 kill->standby ready %d ms\n' $((t1 - t0)) >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record F02 PASS "F01 on c1: standby ready in $((t1 - t0)) ms, lease epoch $((e0 + 1)); m and k clients back without login" || record F02 FAIL "${p#; }"
}

t_f03() {
  local p="" img=${IMAGES%% *}
  p13_ts_start f08-m-new "$img" "$LABEL" && p13_ts_up f08-m-new "$URL1" "$TMPD/key-m" || p="$p; m-new up"
  wait_pair "$(awk '{print $1}' <<<"$M_CLIENTS") f08-m-new" 30 || p="$p; m-new not reachable"
  [ -z "$p" ] && record F03 PASS "m-new enrolled in m at c1 after B" || record F03 FAIL "${p#; }"
}

t_f04() {
  local p="" e0 st nm
  e0=$(owner_of m | awk '{print $2}')
  cli c1 "tailnet reassign --slug m --to $C2_ID" >"$ART_DIR/logs/F04-reassign.json" 2>&1 || p="$p; reassign"
  for _ in $(seq 1 60); do st=$(owner_of m); [ "${st%% *}" = $C2_ID ] && break; sleep 1; done
  [ "${st%% *}" = $C2_ID ] || p="$p; m not handed to c2 [$st]"
  [ "$(awk '{print $2}' <<<"$st")" = $((e0 + 1)) ] || p="$p; owner_epoch $e0 -> $(awk '{print $2}' <<<"$st")"
  for nm in $M_CLIENTS f08-m-new; do repoint "$nm" "$URL2" m || p="$p; $nm to c2"; done
  wait_pair "$M_CLIENTS" 40 || p="$p; m pair on c2"
  owner_of m >"$TMPD/m-before-restore"
  db_state "$PRIM" >"$ART_DIR/logs/state-before-restore.txt"
  [ -z "$p" ] && record F04 PASS "m moved c1 -> c2 (owner_epoch $e0 -> $((e0 + 1))); its three clients re-pointed to c2 and pinging" || record F04 FAIL "${p#; }"
}

T_RESTORED=0 T_RESTORED_S=0 T_FENCE=0 FLOOR=0
t_f05() {
  local p="" t0 t1 out
  find_roles || { record F05 FAIL "Patroni roles unclear"; return; }
  t0=$(now_ms)
  out=$(on "$REPL" "set -e; sudo install -d -o postgres -g postgres -m 0700 $SCRATCH $SCRATCH/pgdata
    sudo -u postgres timeout 3600 pgbackrest --stanza=$STANZA --pg1-path=$SCRATCH/pgdata --type=name --target=$RP \
      --target-action=promote --archive-mode=off --log-level-console=warn restore
    sudo -u postgres $PGBIN/pg_ctl -D $SCRATCH/pgdata -l $SCRATCH/pg.log -w -t 900 -o \"-c port=$SCRATCH_PORT -c listen_addresses=127.0.0.1 \
      -c unix_socket_directories=$SCRATCH -c archive_mode=off -c synchronous_standby_names= -c primary_conninfo= -c primary_slot_name= \
      -c hot_standby=on -c cluster_name=p13f08\" start >/dev/null
    for i in \$(seq 1 300); do [ \"\$(sudo -u postgres psql -X -At -h $SCRATCH -p $SCRATCH_PORT -d postgres -c 'SELECT pg_is_in_recovery()')\" = f ] && exit 0; sleep 1; done
    exit 1" 2>&1) || p="$p; scratch restore failed [$(tail -3 <<<"$out" | tr '\n' ' ')]"
  printf '%s\n' "$out" | redact >"$ART_DIR/logs/restore.log"
  db_state "$REPL" "-h $SCRATCH -p $SCRATCH_PORT" >"$TMPD/state-restored"
  cp "$TMPD/state-restored" "$ART_DIR/logs/state-restored.txt"
  cmp -s "$TMPD/state-B" "$TMPD/state-restored" || p="$p; restored state differs from B ($(diff "$TMPD/state-B" "$TMPD/state-restored" | grep '^[<>]' | tr '\n' ' '))"
  # Swap the drill database (only it) for its restored copy.
  on "$REPL" "sudo -u postgres pg_dump -h $SCRATCH -p $SCRATCH_PORT -Fc -d $PG_DB" >"$TMPD/f08.dump" || p="$p; pg_dump"
  [ -s "$TMPD/f08.dump" ] || p="$p; empty dump"
  if [ -z "$p" ]; then
    psql_ "$PRIM" postgres <<<"DROP DATABASE $PG_DB WITH (FORCE); CREATE DATABASE $PG_DB OWNER headscale;" >/dev/null || p="$p; drop/create"
    on "$PRIM" "sudo -u postgres pg_restore --exit-on-error --no-owner --role=headscale -d $PG_DB" <"$TMPD/f08.dump" || p="$p; pg_restore"
  fi
  rm -f "$TMPD/f08.dump"
  t1=$(now_ms); T_RESTORED=$t1; T_RESTORED_S=$(date +%s)
  on "$REPL" "sudo -u postgres $PGBIN/pg_ctl -D $SCRATCH/pgdata -m fast -w -t 60 stop >/dev/null; sudo rm -rf $SCRATCH" || p="$p; scratch not removed"
  printf 'F05 restore to %s (scratch + swap) %d ms\n' "$RP" $((t1 - t0)) >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record F05 PASS "PITR to $RP in a scratch instance and swap of $PG_DB in $((t1 - t0)) ms; restored state = state at B" || record F05 FAIL "${p#; }"
}

t_f06() {
  local p="" n bad=0 samples=0 since=$T_RESTORED_S
  [ "$T_RESTORED" -gt 0 ] || { record F06 FAIL "no restore"; return; }
  local end=$(( $(date +%s) + F08_PREFENCE_S ))
  while [ "$(date +%s)" -lt "$end" ]; do
    samples=$((samples + 1))
    for n in cp c1b; do [ "$(direct $n /ready)" = 503 ] || bad=$((bad + 1)); done
    db_state "$PRIM" | grep '^lease' | tr '\n' ' ' >>"$ART_DIR/logs/F06-leases.txt"; echo >>"$ART_DIR/logs/F06-leases.txt"
    sleep 2
  done
  [ $bad = 0 ] || p="$p; a c1 process served before the fence ($bad /ready 200 samples of $((samples * 2)))"
  { proc_log cp "$since"; proc_log c1b "$since"; } | redact >"$ART_DIR/logs/F06-c1.log"
  { proc_log c2 "$since"; proc_log c2b "$since"; } | redact >"$ART_DIR/logs/F06-c2.log"
  grep -q 'CRITICAL epoch regression' "$ART_DIR/logs/F06-c1.log" || p="$p; no epoch-regression log on c1"
  grep 'loaded tailnet runtime' "$ART_DIR/logs/F06-c2.log" | grep -q 'slug=m' && p="$p; c2 loaded m from the restored database"
  [ -z "$p" ] && record F06 PASS "${F08_PREFENCE_S}s on the restored database before the fence: both c1 processes refused the lease (HWM; /ready 503 in all $samples samples, epoch regression logged); c2 never loaded m" \
    || record F06 FAIL "${p#; }"
}

t_f07() {
  local p="" out
  out=$(cli c1 "epoch-fence --reason 'P13c F08 drill: restore to $RP' --operator p13-f08-harness") || p="$p; fence failed"
  printf '%s\n' "$out" >"$ART_DIR/logs/F07-fence.json"
  T_FENCE=$(date +%s)
  FLOOR=$(py 'import json; print(json.load(sys.stdin)["floor"])' <<<"$out" 2>/dev/null | strip_cr)
  [[ $FLOOR =~ ^[0-9]+$ ]] && [ "$FLOOR" -gt 1 ] || p="$p; floor [$FLOOR]"
  [ -z "$p" ] && record F07 PASS "epoch fence: floor $FLOOR ($(py 'import json; d=json.load(sys.stdin); print(d["leases"], "leases,", d["tailnets"], "tailnets,", d["moves_cancelled"], "moves cancelled")' <<<"$out" | strip_cr))" \
    || record F07 FAIL "${p#; }"
}

t_f08() {
  local p="" st nm relog="" t_ok rec_s
  [ "$FLOOR" -gt 1 ] 2>/dev/null || { record F08 FAIL "no fence floor"; return; }
  wait_public_ready c1 60 || p="$p; c1 not serving after the fence"
  wait_public_ready c2 60 || p="$p; c2 not serving after the fence"
  # Every epoch at or above the floor; m back with c1.
  db_state "$PRIM" >"$ART_DIR/logs/state-after-fence.txt"
  awk -v f="$FLOOR" '($1 == "lease" && $3 < f) || ($1 == "tailnet" && $4 < f) {print}' "$ART_DIR/logs/state-after-fence.txt" >"$TMPD/below"
  [ -s "$TMPD/below" ] && p="$p; epochs below the floor: $(tr '\n' ' ' <"$TMPD/below")"
  st=$(owner_of m); [ "${st%% *}" = $C1_ID ] || p="$p; m owner after the fence [$st]"
  # m clients back to c1: the two pre-B nodes keep their identity, m-new re-enrols.
  for nm in $M_CLIENTS f08-m-new; do repoint "$nm" "$URL1" m && relog="$relog $nm" || p="$p; $nm back to c1"; done
  wait_pair "$M_CLIENTS" 60 || p="$p; m pair on c1"
  local now_map kv; now_map=$(p13_node_map c1 m)
  for kv in $(cat "$TMPD/nodes-m"); do grep -qw -- "$kv" <<<"$now_map" || p="$p; m node $kv changed"; done
  wait_pair "$K_CLIENTS" 60 || p="$p; k pair"
  wait_pair "$O_CLIENTS" 60 || p="$p; o pair"
  local nl; nl=$(nl_same base "$K_CLIENTS $O_CLIENTS"); [ -z "$nl" ] || p="$p; NeedsLogin of k/o:$nl"
  t_ok=$(now_ms); rec_s=$(( (t_ok - T_RESTORED) / 1000 ))
  [ "$rec_s" -le "$F08_RECOVERY_MAX_S" ] || p="$p; recovery $rec_s s > $F08_RECOVERY_MAX_S s"
  # After the fence: no lease acquired and no runtime loaded below the floor; c2 never loads m.
  local n; for n in cp c1b c2 c2b; do proc_log $n "$T_FENCE" | redact >"$ART_DIR/logs/F08-$n.log"; done
  cat "$ART_DIR"/logs/F08-c*.log | py 'import re,sys
f = int(sys.argv[1]); bad = []
for l in sys.stdin:
    if "cluster lease acquired" in l or "loaded tailnet runtime" in l:
        m = re.search(r"(?:lease_epoch|epoch)=(\d+)", l)
        if m and int(m.group(1)) < f:
            bad.append(l.strip()[:160])
print("\n".join(bad))' "$FLOOR" | strip_cr >"$TMPD/stale"
  [ -s "$TMPD/stale" ] && p="$p; stale authority after the fence: $(head -2 "$TMPD/stale" | tr '\n' ' ')"
  grep 'loaded tailnet runtime' "$ART_DIR"/logs/F08-c2*.log | grep -q 'slug=m' && p="$p; c2 loaded m after the fence"
  grep -qE 'cluster lease ended' "$ART_DIR"/logs/F08-c2*.log || p="$p; c2's pre-restore holder did not lose its lease at the fence"
  printf 'F08 recovery after the restore %d s (fence after %d s window); re-logins:%s\n' "$rec_s" "$F08_PREFENCE_S" "$relog" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record F08 PASS "recovery $rec_s s after the restore (<= $F08_RECOVERY_MAX_S, incl. the ${F08_PREFENCE_S}s window); all epochs >= floor $FLOOR; m with c1 (pre-B node IDs kept), never on c2; no stale authority after the fence; c2's old holder lost its lease; k/o without login; re-logins:$relog (moved or enrolled after B)" \
    || record F08 FAIL "${p#; }"
}

t_f09() {
  local p="" out mb ma n
  for n in cp c1b c2 c2b; do proc_log $n "$T0" | redact; done | grep -aE 'cluster lease acquired' >"$ART_DIR/logs/lease-events.log"
  out=$(py 'import re,sys
ev = {}
for l in sys.stdin:
    c = re.search(r"cluster=(\S+)", l); e = re.search(r"lease_epoch=(\d+)", l); t = re.search(r"(\d{4}-\d\d-\d\dT[0-9:.]+Z)", l)
    if c and e and t:
        ev.setdefault(c.group(1), []).append((t.group(1), int(e.group(1))))
bad = []
for c, v in ev.items():
    v.sort(); eps = [e for _, e in v]
    if eps != sorted(set(eps)):
        bad.append(f"{c}: {eps}")
print("bad " + "; ".join(bad) if bad else "ok " + "; ".join(f"{c}: {[e for _, e in sorted(v)]}" for c, v in ev.items()))' <"$ART_DIR/logs/lease-events.log" | strip_cr)
  [[ $out == ok* ]] || p="$p; lease epochs not strictly increasing: ${out#bad }"
  mb=$(awk '{print $2}' "$TMPD/m-before-restore"); ma=$(owner_of m | awk '{print $2}')
  [ "${ma:-0}" -gt "${mb:-0}" ] 2>/dev/null || p="$p; owner_epoch of m $mb before the restore, $ma now"
  [ "$(psql_ "$PRIM" "$PG_DB" <<<"SELECT count(*) FROM platform_epoch_fences WHERE reason LIKE '%$RP%';")" = 1 ] || p="$p; no platform_epoch_fences row"
  on "$PRIM" "sudo -u postgres psql -X -At -d $PG_DB -c 'BEGIN; DELETE FROM cluster_leases; ROLLBACK;'" >/dev/null 2>&1 && p="$p; DELETE FROM cluster_leases was accepted"
  [ -z "$p" ] && record F09 PASS "lease epochs strictly increasing per cluster (${out#ok }); m owner_epoch $mb -> $ma; fence recorded; DELETE FROM cluster_leases refused" \
    || record F09 FAIL "${p#; }"
}

t_secrets() {
  local hits
  hits=$(grep -rlE 'hskey-(auth|api)-[A-Za-z0-9]{6,}|privkey:[0-9a-f]{64}|nodekey:[0-9a-f]{64}|mkey:[0-9a-f]{64}|cipher-pass|PASSWORD=' "$ART_DIR" 2>/dev/null | grep -v results.tsv)
  [ -z "$hits" ] && record S99 PASS "no auth key, private key, full key or password in the artifacts" || record S99 FAIL "unredacted in: $hits"
}

# ------------------------------------------------------------------- main --
T0=$(date +%s)
log "P13c F08 drill $RUN_ID at $SHA (artifacts: ${ART_DIR#"$REPO"/})"
if ! precheck; then log "preconditions not met (artifacts precondition.txt); nothing was changed"; rm -rf "$TMPD"; exit 2; fi
trap 'cleanup; exit 2' INT TERM
if ! setup 2> >(redact >>"$ART_DIR/logs/setup.log"); then
  log "setup failed (see logs/setup.log)"; cleanup; exit 2
fi
{ echo "commit: $SHA"; echo "database: $PG_DB (primary $PRIM, replica $REPL)"; echo "stanza: $STANZA"; echo "restore point: $RP"
  echo "binary: $(cut -c1-16 "$TMPD/hs.sha")"; echo "images: $IMAGES"; } >>"$ART_DIR/env.txt"
t_f00
t_f01
t_f02
t_f03
t_f04
t_f05
t_f06
t_f07
t_f08
t_f09
t_secrets
cleanup
log "done in $(( $(date +%s) - T0 ))s: $FAILS FAIL, cleanup_ok=$CLEAN_OK"
[ "$CLEAN_OK" = 1 ] || exit 2
[ "$FAILS" = 0 ] || exit 1
exit 0
