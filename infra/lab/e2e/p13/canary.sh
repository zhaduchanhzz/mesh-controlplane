#!/usr/bin/env bash
# P13d drill: canary upgrade of cluster c2 from build N to build N+1 (ADR
# 0013: the migrate step is a separate operator step, schema_compat lets an
# older binary run on a newer *expand* schema; owner decision: canary only
# within the same minor, versioncheck kept) and rollback, with real clients
# pinging the whole time. Runs inside the N1 E2E harness (lib-ops.sh
# p13_overlay): c2 = mc-c2 + mc-c2b behind the long-lived HAProxy mesh-c2-ha
# (/ready check), c1 and its client VMs as control.
#
# Builds: N = CI_BINDIR (the integration tree, as N1 uses it); N+1 =
# CI_BINDIR2 = the same tree plus one harmless expand migration that exists
# only for this drill (never committed; ticket P13d). Make it with
#   CI_BINDIR2=/home/mesh/<dir> bash infra/lab/e2e/p13/canary.sh build
# (on mc-ci: clone of the mcci repo at this commit, patch, go build; the
# commit must be pushed to mcci). Same-minor check: `headscale version` of N
# and N+1 must share major.minor (both "dev"/pseudo versions pass with INFO).
#
# Cases (after N1's E00 and S01):
#   D00 setup: N+1 on c2/c2b next to N ($HS.next, $HS.prev; sha256 checked),
#       same minor, docker clients of tailnet c on mc-ci (two per image of
#       CANARY_IMAGES), pingers both ways
#   D01 expand migrate with N+1 (`cluster migrate`): applies only the drill
#       migration; N keeps serving (public /ready 200 sampled), a restarted N
#       standby starts on the newer schema (/health 200, /ready 503), the N
#       admin CLI reads (schema_compat)
#   D02 standby -> N+1, switchover (SIGTERM active): new active ready and
#       public /ready 200 within CANARY_SWITCH_MAX_MS (D12: 15000)
#   D03 observation window CANARY_OBSERVE_S (120 s): public /ready 200 at
#       every sample, a new client enrols through N+1, pings
#   D04 other process -> N+1, switchover back (<= 15 s): both on N+1
#   D05 rollback: standby -> N, switchover to N (<= 15 s) on the N+1 schema,
#       other process -> N: both on N
#   D06 two `cluster migrate` of N+1 at once (mc-c2 and mc-c2b) on a scratch
#       database migrated by N: both exit 0, exactly one applied the drill
#       migration (ADR 0013, L7)
#   For every switchover: client ping gap (reported), no NeedsLogin, node IDs
#   and IPs kept, all clients Running; c1 untouched (a1<->a2 ping at the end).
#
# Usage: CI_BINDIR=/home/mesh/<N> CI_BINDIR2=/home/mesh/<N+1> MODE=off V=1.104.7 bash infra/lab/e2e/p13/canary.sh
#   Twice for the ticket's "×2".
# Exit: as N1.
# shellcheck disable=SC2029,SC2034 # SC2034: CLEAN_OK and friends belong to the N1 harness this file is sourced into

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -u
  here=$(cd "$(dirname "$0")" && pwd)
  # shellcheck source=lib-ops.sh
  . "$here/lib-ops.sh"
  CI_BINDIR2=${CI_BINDIR2:-}
  [[ $CI_BINDIR2 =~ ^/[A-Za-z0-9/_.-]+$ ]] || { echo "CI_BINDIR2: absolute path on mc-ci, [A-Za-z0-9/_.-] only" >&2; exit 2; }
  if [ "${1:-}" = build ]; then
    SSH_KEY=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
    CI=${CI_SSH:-mesh@192.168.1.240}
    sha=$(git -C "$here" rev-parse HEAD)
    s() { ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 "$CI" "$@"; }
    s "cat >/tmp/p13d-patch.py" <<'PY' || exit 2
# Drill-only N+1 (P13d canary): one expand migration, never committed.
import pathlib, sys
root = pathlib.Path(sys.argv[1])
mid = "209912310000-p13d-canary-probe"
ddl = "CREATE TABLE p13d_canary_probe(id integer PRIMARY KEY);\n"
(root / "hscontrol/db/p13d_canary_probe.go").write_text('''package db

// Drill-only (infra/lab/e2e/p13/canary.sh build): a harmless expand
// migration that makes build N+1 of the P13d canary drill. Never committed.

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

const p13dCanaryProbeID = "%s"

func p13dCanaryProbeMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: p13dCanaryProbeID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`CREATE TABLE p13d_canary_probe(id integer PRIMARY KEY)`).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Exec(`DROP TABLE IF EXISTS p13d_canary_probe`).Error
		},
	}
}
''' % mid)
p = root / "hscontrol/db/platform.go"
t = p.read_text()
i = t.index("func v01Migrations()")
j = t.index("\n\t}\n}", i)
p.write_text(t[:j] + "\n\t\tp13dCanaryProbeMigration()," + t[j:])
c = root / "hscontrol/db/schema_compat.go"
t = c.read_text()
i = t.index("var migrationCompat = map[string]string{")
j = t.index("\n}", i)
c.write_text(t[:j] + "\n\tp13dCanaryProbeID: SchemaCompatMigrationID," + t[j:])
for f in ("schema.sql", "schema_postgres.sql"):
    q = root / "hscontrol/db" / f
    q.write_text(q.read_text() + ddl)
print("patched", mid)
PY
    s "set -e; export PATH=/usr/local/go/bin:\$PATH; d=\$HOME/src/p13d-canary
      [ -d \$d/.git ] || git clone -q /home/mesh/git/mesh.git \$d
      cd \$d; git fetch -q origin; git checkout -q --detach $sha; git reset -q --hard; git clean -qfd
      python3 /tmp/p13d-patch.py \$d; gofmt -w hscontrol/db/platform.go hscontrol/db/schema_compat.go hscontrol/db/p13d_canary_probe.go
      mkdir -p $CI_BINDIR2; go build -o $CI_BINDIR2/headscale ./cmd/headscale
      git reset -q --hard; git clean -qfd; rm -f /tmp/p13d-patch.py
      $CI_BINDIR2/headscale version | head -1; sha256sum $CI_BINDIR2/headscale" || exit 2
    exit 0
  fi
  export CI_BINDIR2
  add="t_d00 t_d01 t_d02 t_d03 t_d04 t_d05 t_d06"
  p13_overlay canary "$0" "$add" "${P13_CASES:-t_e00 t_s01 $add}"
  exit $?
fi

# ------------------------------------------- sourced into the N1 run copy ---
# shellcheck source=lib-ops.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-ops.sh"

CANARY_IMAGES=${CANARY_IMAGES:-tailscale/tailscale:v1.104.7 tailscale/tailscale:v1.84.3}
CANARY_SWITCH_MAX_MS=${CANARY_SWITCH_MAX_MS:-15000}
CANARY_OBSERVE_S=${CANARY_OBSERVE_S:-120}
D_BINPORT=${D_BINPORT:-18091}
D_LABEL=mesh-e2e=p13d
D_PROBE=209912310000-p13d-canary-probe
D_CLIENTS=""
D_PINGS=""
D_OK=0

d_hs() { local n=$1 b=$2; shift 2; on "$n" "sudo -u headscale timeout 120 $b -c $CFG cluster --cluster-id $C2_ID $*"; }
d_ver() { on "$1" "$2 version" 2>/dev/null | strip_cr | head -1 | awk '{print $3}'; }
d_minor() { sed -E 's/^v?([0-9]+)\.([0-9]+)\..*/\1.\2/' <<<"$1"; }
d_install() { on "$1" "sudo install -m 0755 $2 $HS.tmp && sudo mv -f $HS.tmp $HS"; } # NODE SRC: $HS := SRC (process stopped)

# Clients Running, peers complete, no new NeedsLogin, node IDs/IPs kept.
d_clients_ok() {
  local p="" nm line st want kv now_map i ok
  for _ in $(seq 1 30); do
    ok=1
    while read -r nm st line; do
      want=$(tr ' ' '\n' <<<"$D_CLIENTS" | grep -vx "$nm" | grep . | LC_ALL=C sort | paste -sd, -)
      [ "$st" = Running ] || ok=0
      [ -z "$(LC_ALL=C comm -23 <(tr ',' '\n' <<<"$want" | LC_ALL=C sort) <(tr ',' '\n' <<<"$line" | LC_ALL=C sort))" ] || ok=0
    done < <(p13_peers $D_CLIENTS)
    [ $ok = 1 ] && break; sleep 2
  done
  [ $ok = 1 ] || p="$p; clients [$(p13_peers $D_CLIENTS | tr '\n' ';')]"
  for nm in $D_CLIENTS; do [ "$(p13_needs_login "$nm")" = "$(cat "$TMPD/dnl0-$nm")" ] || p="$p; $nm NeedsLogin"; done
  now_map=$(p13_node_map c2 c)
  for kv in $(cat "$TMPD/dnodes0"); do grep -qw -- "$kv" <<<"$now_map" || p="$p; node $kv changed"; done
  printf '%s' "${p#; }"
}
# d_switch ID: SIGTERM the active, the standby takes over; prints "<ms to
# new active ready> <ms to public ready> <max ping gap ms>"; rc 1 if no takeover.
d_switch() {
  local a s t0 t0ci tr=-1 tp=-1 end g gmax=-1 pn
  a=$(active_of c2); s=$(standby_of c2)
  [ "$a" != none ] && [ "$(direct "$s" /ready)" = 503 ] || { echo "-1 -1 -1"; return 1; }
  t0ci=$(ci_now_ms); t0=$(now_ms)
  stop_proc "$a"
  end=$(( $(date +%s) + 40 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    [ "$tr" -lt 0 ] && [ "$(direct "$s" /ready)" = 200 ] && tr=$(( $(now_ms) - t0 ))
    [ "$tr" -ge 0 ] && [ "$(public_ready c2)" = 200 ] && { tp=$(( $(now_ms) - t0 )); break; }
    sleep 0.2
  done
  sleep 15
  for pn in $D_PINGS; do
    ci_pinger_snap "$pn" "$TMPD/$pn.snap"
    read -r g _ _ <<<"$(gap_stats "$TMPD/$pn.snap" $((t0ci - 2000)) $((t0ci + 60000)))"
    [ "${g:--1}" -gt "$gmax" ] && gmax=$g
  done
  printf '%s %s %s\n' "$tr" "$tp" "$gmax" | tee -a "$ART_DIR/logs/switch-$1.txt"
  [ "$tp" -ge 0 ]
}
d_switch_case() { # ID WHAT
  local p="" r tr tp g e
  r=$(d_switch "$1") || p="$p; no takeover within 40 s"
  read -r tr tp g <<<"$r"
  [ "$tp" -ge 0 ] && [ "$tp" -le "$CANARY_SWITCH_MAX_MS" ] || p="$p; switchover $tp ms (max $CANARY_SWITCH_MAX_MS)"
  e=$(d_clients_ok); [ -z "$e" ] || p="$p; $e"
  printf '%s %s: new active ready %s ms, public %s ms, ping gap %s ms\n' "$1" "$2" "$tr" "$tp" "$g" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "$1" PASS "$2: switchover $tp ms (new active ready $tr ms; <= $CANARY_SWITCH_MAX_MS), ping gap $g ms; clients Running, no login, node IDs/IPs kept" \
    || record "$1" FAIL "${p#; }"
}

t_d00() {
  local p="" n sum vn vn1 img vt i nm s
  ci_ "cd $CI_BINDIR2 && sha256sum headscale" | strip_cr | cut -c1-64 >"$TMPD/next.sha"
  [ "$(wc -c <"$TMPD/next.sha")" -ge 64 ] || { record D00 FAIL "no headscale in CI_BINDIR2 ($CI_BINDIR2)"; return; }
  sum=$(cat "$TMPD/next.sha")
  ci_ "sudo systemctl stop p13d-binsrv 2>/dev/null; sudo systemctl reset-failed p13d-binsrv 2>/dev/null; \
    sudo systemd-run --quiet --collect --unit=p13d-binsrv --uid=mesh --gid=mesh -p RuntimeMaxSec=600 \
      python3 -m http.server $D_BINPORT --bind ${CI_SSH#*@} --directory $CI_BINDIR2; \
    for i in \$(seq 1 20); do curl -fsS -o /dev/null -m 2 http://${CI_SSH#*@}:$D_BINPORT/ && exit 0; sleep 0.5; done; exit 1" || p="$p; binsrv"
  for n in c2 c2b; do
    on $n "sudo cp -f $HS $HS.prev && curl -fsS --retry 3 -m 120 -o /tmp/p13d-next http://${CI_SSH#*@}:$D_BINPORT/headscale && \
      echo '$sum  /tmp/p13d-next' | sha256sum -c --quiet && sudo install -m 0755 /tmp/p13d-next $HS.next && rm -f /tmp/p13d-next" || p="$p; $n N+1"
  done
  ci_ "sudo systemctl stop p13d-binsrv 2>/dev/null; true"
  vn=$(d_ver c2 "$HS.prev"); vn1=$(d_ver c2 "$HS.next")
  printf 'N %s (%s)\nN+1 %s (%s)\n' "$vn" "$(on c2 "sha256sum $HS.prev" | cut -c1-16)" "$vn1" "${sum:0:16}" >>"$ART_DIR/env.txt"
  if [ "$(d_minor "$vn")" != "$(d_minor "$vn1")" ]; then p="$p; N $vn and N+1 $vn1 are not the same minor (owner decision: canary within a minor)"
  elif [[ $vn == dev || $vn1 == dev || $vn == v0.0.0-* ]]; then record D00i INFO "versions $vn / $vn1: development builds, versioncheck skipped by the binaries"; fi
  [ "$(on c2 "sha256sum $HS.prev" | cut -c1-64)" != "$sum" ] || p="$p; N and N+1 are the same binary"

  p13_ci_prepare || p="$p; mc-ci work dir"
  for img in $CANARY_IMAGES; do
    ci_ "docker image inspect $img >/dev/null 2>&1 || docker pull -q $img >/dev/null" || p="$p; image $img"
    vt=$(tr -d . <<<"${img##*:v}")
    for i in 1 2; do
      nm=p13d-c-$vt-$i
      p13_ts_start "$nm" "$img" "$D_LABEL" && p13_ts_up "$nm" "$URL2" "$TMPD/key-c" || p="$p; $nm up"
      D_CLIENTS="$D_CLIENTS $nm"
    done
  done
  for _ in $(seq 1 40); do [ "$(p13_peers $D_CLIENTS | awk '$2 == "Running"' | wc -l)" = "$(wc -w <<<"$D_CLIENTS")" ] && break; sleep 2; done
  p13_node_map c2 c >"$TMPD/dnodes0"
  for nm in $D_CLIENTS; do p13_needs_login "$nm" >"$TMPD/dnl0-$nm"; done
  local first last; first=$(awk '{print $1}' <<<"$D_CLIENTS"); last=$(awk '{print $NF}' <<<"$D_CLIENTS")
  ci_pinger_start d-fwd "$first" "$(p13_ts_ip "$last")" && ci_pinger_start d-rev "$last" "$(p13_ts_ip "$first")" || p="$p; pingers"
  D_PINGS="d-fwd d-rev"
  local e; e=$(d_clients_ok); [ -z "$e" ] || p="$p; $e"
  [ -z "$p" ] && { D_OK=1; record D00 PASS "N $vn and N+1 $vn1 (same minor) on c2/c2b; $(wc -w <<<"$D_CLIENTS") clients of c ($CANARY_IMAGES) Running, pinging"; } \
    || record D00 FAIL "${p#; }"
}

t_d01() {
  [ "$D_OK" = 1 ] || { record D01 FAIL "D00 did not pass"; return; }
  local p="" out a s i bad=0
  out=$(d_hs c2 "$HS.next" migrate) || p="$p; migrate with N+1 failed"
  printf '%s\n' "$out" >"$ART_DIR/logs/D01-migrate.json"
  [ "$(py 'import json; print(",".join(json.load(sys.stdin)["applied"]))' <<<"$out" | strip_cr)" = "$D_PROBE" ] || p="$p; applied [$out]"
  for _ in $(seq 1 10); do [ "$(public_ready c2)" = 200 ] || bad=$((bad + 1)); sleep 2; done
  [ $bad = 0 ] || p="$p; public /ready not 200 in $bad/10 samples on N after the expand"
  s=$(standby_of c2)
  stop_proc "$s"; start_proc "$s"
  for _ in $(seq 1 30); do [ "$(direct "$s" /health)" = 200 ] && break; sleep 1; done
  [ "$(direct "$s" /health)" = 200 ] && [ "$(direct "$s" /ready)" = 503 ] || p="$p; N standby on the N+1 schema: /health $(direct "$s" /health) /ready $(direct "$s" /ready)"
  d_hs c2 "$HS.prev" "tailnet nodes --slug c" >/dev/null || p="$p; N admin CLI refused the N+1 schema"
  local e; e=$(d_clients_ok); [ -z "$e" ] || p="$p; $e"
  [ -z "$p" ] && record D01 PASS "migrate (N+1) applied only $D_PROBE; N kept serving, a restarted N standby starts on the newer expand schema, N CLI reads" \
    || record D01 FAIL "${p#; }"
}

t_d02() {
  [ "$D_OK" = 1 ] || { record D02 FAIL "D00 did not pass"; return; }
  local s i; s=$(standby_of c2)
  stop_proc "$s"; d_install "$s" "$HS.next" && start_proc "$s"
  for _ in $(seq 1 30); do [ "$(direct "$s" /health)" = 200 ] && break; sleep 1; done
  [ "$(direct "$s" /ready)" = 503 ] || { record D02 FAIL "N+1 on $s is not standby ($(direct "$s" /ready))"; return; }
  d_switch_case D02 "standby $s on N+1 takes over from N"
}

t_d03() {
  [ "$D_OK" = 1 ] || { record D03 FAIL "D00 did not pass"; return; }
  local p="" bad=0 end n=0
  end=$(( $(date +%s) + CANARY_OBSERVE_S ))
  while [ "$(date +%s)" -lt "$end" ]; do n=$((n + 1)); [ "$(public_ready c2)" = 200 ] || bad=$((bad + 1)); sleep 5; done
  [ $bad = 0 ] || p="$p; public /ready not 200 in $bad/$n samples"
  p13_ts_start p13d-new "${CANARY_IMAGES%% *}" "$D_LABEL" && p13_ts_up p13d-new "$URL2" "$TMPD/key-c" || p="$p; new client did not enrol on N+1"
  ci_ "docker exec p13d-new tailscale ping -c 3 --timeout 5s $(awk '{print $1}' <<<"$D_CLIENTS")" >>"$ART_DIR/logs/D03-ping.log" 2>&1 || p="$p; new client cannot ping"
  on "$(active_of c2)" "sudo journalctl -u $UNIT --since -$((CANARY_OBSERVE_S + 60))s --no-pager" | redact >"$ART_DIR/logs/D03-active.log"
  local errs; errs=$(grep -c '"level":"error"\|ERR ' "$ART_DIR/logs/D03-active.log" || true)
  local e; e=$(d_clients_ok); [ -z "$e" ] || p="$p; $e"
  [ -z "$p" ] && record D03 PASS "${CANARY_OBSERVE_S}s on N+1: public /ready 200 in $n/$n samples, a new client enrolled and pings; error lines in the active's log: $errs" \
    || record D03 FAIL "${p#; }"
}

t_d04() {
  [ "$D_OK" = 1 ] || { record D04 FAIL "D00 did not pass"; return; }
  local o i; o=$(standby_of c2)   # the old active, stopped by D02
  d_install "$o" "$HS.next" && start_proc "$o"
  for _ in $(seq 1 30); do [ "$(direct "$o" /health)" = 200 ] && break; sleep 1; done
  d_switch_case D04 "second process on N+1 takes over (cluster fully on N+1)"
  local s; s=$(standby_of c2); start_proc "$s"
}

t_d05() {
  [ "$D_OK" = 1 ] || { record D05 FAIL "D00 did not pass"; return; }
  local s o i; s=$(standby_of c2)
  # The process stopped by D04 was started again (N+1 standby): roll it back.
  stop_proc "$s"; d_install "$s" "$HS.prev" && start_proc "$s"
  for _ in $(seq 1 30); do [ "$(direct "$s" /health)" = 200 ] && break; sleep 1; done
  [ "$(direct "$s" /ready)" = 503 ] || { record D05 FAIL "N on $s is not standby on the N+1 schema ($(direct "$s" /ready))"; return; }
  d_switch_case D05 "rollback: N takes over on the N+1 schema"
  o=$(standby_of c2)
  d_install "$o" "$HS.prev" && start_proc "$o"
  for _ in $(seq 1 30); do [ "$(direct "$o" /ready)" = 503 ] && break; sleep 1; done
  [ "$(direct "$o" /ready)" = 503 ] || record D05b FAIL "$o back on N is not standby"
}

t_d06() {
  local p="" db=${PG_DB}_mig n o1 o2 a1 a2 mcfg=/etc/headscale/cluster/config-p13d-mig.yaml
  pg_ "sudo docker exec mc-pg createdb -U postgres -O headscale $db" || { record D06 FAIL "scratch database"; return; }
  for n in c2 c2b; do
    on $n "sudo sed 's#^    name: .*#    name: $db#' $CFG | sudo install -o root -g headscale -m 0640 /dev/stdin $mcfg && sudo grep -q '^    name: $db\$' $mcfg" || p="$p; $n config"
  done
  on c2 "sudo -u headscale timeout 120 $HS.prev -c $mcfg cluster migrate" >"$ART_DIR/logs/D06-n.json" || p="$p; N migrate (fresh)"
  local w1 w2
  on c2 "sudo -u headscale timeout 120 $HS.next -c $mcfg cluster migrate" >"$TMPD/d06-1" 2>&1 &
  w1=$!
  on c2b "sudo -u headscale timeout 120 $HS.next -c $mcfg cluster migrate" >"$TMPD/d06-2" 2>&1 &
  w2=$!
  wait "$w1" "$w2"
  cp "$TMPD/d06-1" "$ART_DIR/logs/D06-c2.json"; cp "$TMPD/d06-2" "$ART_DIR/logs/D06-c2b.json"
  a1=$(py 'import json; print(",".join(json.load(sys.stdin)["applied"]))' <"$TMPD/d06-1" 2>/dev/null | strip_cr)
  a2=$(py 'import json; print(",".join(json.load(sys.stdin)["applied"]))' <"$TMPD/d06-2" 2>/dev/null | strip_cr)
  o1=$(grep -c '"latest"' "$TMPD/d06-1"); o2=$(grep -c '"latest"' "$TMPD/d06-2")
  [ "$o1" = 1 ] && [ "$o2" = 1 ] || p="$p; a migrate run failed [$(head -c 200 "$TMPD/d06-1") | $(head -c 200 "$TMPD/d06-2")]"
  [ "$(printf '%s\n%s\n' "$a1" "$a2" | grep -c "^$D_PROBE\$")" = 1 ] && [ "$(printf '%s\n%s\n' "$a1" "$a2" | grep -c '^$')" = 1 ] \
    || p="$p; applied [$a1] and [$a2] (want the drill migration exactly once)"
  for n in c2 c2b; do on $n "sudo rm -f $mcfg"; done
  pg_ "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $db" || p="$p; scratch database not dropped"
  [ -z "$p" ] && record D06 PASS "two N+1 migrate runs at once on a database at N: both exit 0, the drill migration applied once" \
    || record D06 FAIL "${p#; }"
  ping_ok $A1 n1-a2 && record D07 PASS "c1 untouched: a1<->a2 ping after the canary" || record D07 FAIL "c1: a1 cannot ping a2"
}

p13_cleanup() {
  local pn
  for pn in $D_PINGS; do ci_pinger_stop "$pn" 2>/dev/null; done
  p13_ci_cleanup "$D_LABEL" >/dev/null 2>&1 || { log "p13d: clients on mc-ci not removed"; CLEAN_OK=0; }
  ci_ "sudo systemctl stop p13d-binsrv 2>/dev/null; true" >/dev/null 2>&1
  local n
  for n in c2 c2b; do
    on $n "sudo rm -f $HS.prev $HS.next $HS.tmp /etc/headscale/cluster/config-p13d-mig.yaml; ! test -e $HS.next" || { log "$n: canary binaries not removed"; CLEAN_OK=0; }
  done
  [[ ${PG_DB}_mig =~ ^e2e_[0-9a-z_]+$ ]] && pg_ "sudo docker exec mc-pg dropdb -U postgres --if-exists --force ${PG_DB}_mig" >/dev/null 2>&1
  true
}
