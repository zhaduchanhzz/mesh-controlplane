#!/usr/bin/env bash
# P13b drill: the PgBouncer the processes use first fails while both clusters
# serve real clients and write (tickets/P13.md §P13b drill, ADR 0011 §D,
# ADR 0006 §3.4/§3.6; thresholds D12, owner decision 2026-11-26).
# Topology, probes and rules: lib.sh. The processes list
# .247:6432,.248:6432 (target_session_attrs=read-write); the target is the
# first host (PGB_TARGET=pg1, or pg2).
#
# Cases, each once per client image, the PgBouncer kept unavailable for
# HOLD_S (30 s) and then restored (dead-man switch HOLD_S+60 s on the VM):
#   K1 kill -9 of PgBouncer (then held stopped: systemd would restart it
#      after 1 s); clients see their connections reset
#   K2 systemctl stop (clean shutdown)
#   K3 nft drop of every packet to tcp/6432 on the target (no RST: open
#      connections hang; the hardest case for renewals in flight)
# Every case (D12 "kill PgBouncer"): 0 cluster failovers (lease epoch and
# holder of c1 and c2 unchanged, no self_fence or lost event on any process);
# each cluster's fenced writes back within WERR_MAX_MS (10 s) of the fault
# while the PgBouncer is still down; ping gaps <= GAP_MAX_MS; owner epochs
# unchanged; clients Running without action; every acknowledged SQL write
# present; no read-only error.
#
# Usage: CI_BINDIR=/home/mesh/<dir with headscale> infra/lab/e2e/p13/pgb-kill.sh
#    CASES="k1 k2 k3" (default), PGB_TARGET=pg1|pg2, HOLD_S=30, IMAGES=..., VM_TARGETS=...
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail
DRILL=pgb-kill
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
HOLD_S=${HOLD_S:-30}
PGB_TARGET=${PGB_TARGET:-pg1}
CASES=${CASES:-k1 k2 k3}
for c in $CASES; do case $c in k1|k2|k3) ;; *) echo "unknown case $c" >&2; exit 2 ;; esac; done
case $PGB_TARGET in pg1) TH=$PG1_SSH ;; pg2) TH=$PG2_SSH ;; *) echo "PGB_TARGET: pg1 or pg2" >&2; exit 2 ;; esac
UNDO_START="systemctl start $PGB_UNIT"
UNDO_NFT="nft delete table inet p13_drill"

# pgb_case NAME DESC FAULT UNDO
pgb_case() {
  local name=$1 desc=$2 fault=$3 undo=$4 P="" M="" s acked failed gap lost c e0 e1 tu
  ssh_ "$TH" "systemctl is-active -q $PGB_UNIT" || { record "R$ROUND.$name" FAIL "$PGB_UNIT not active on ${TH#*@} before the case"; return; }
  case_begin "R$ROUND.$name"
  deadman "$TH" pgb $((HOLD_S + 60)) "$undo"
  ssh_ "$TH" "$fault" >>"$ART_DIR/logs/$C_TAG-fault.txt" 2>&1 || P="$P; fault command failed"
  sleep "$HOLD_S"
  tu=$(ci_now)
  ssh_ "$TH" "sudo sh -c '$undo'" >>"$ART_DIR/logs/$C_TAG-fault.txt" 2>&1 || P="$P; undo failed"
  deadman_cancel "$TH" pgb
  ssh_ "$TH" "systemctl is-active -q $PGB_UNIT && ! sudo nft list table inet p13_drill >/dev/null 2>&1" || P="$P; PgBouncer not restored on ${TH#*@}"
  M="$M; ${TH#*@} PgBouncer unavailable ${HOLD_S}s (restored @$((tu - C_T0))ms)"
  case_recover
  case_check "$WERR_MAX_MS"
  # 0 failovers: same epoch, same holders, no self-fence/lost anywhere.
  for c in c1 c2; do
    e0=$(awk -v c="$(idof $c)" '{for (i = 1; i <= NF; i += 3) if ($i == c) print $(i + 2)}' <<<"$C_LEASE0")
    e1=$(lease_epoch $c)
    [ "$e0" = "$e1" ] || P="$P; $c failed over (lease epoch $e0 -> $e1)"
  done
  [ "$(held_nodes "$C_TAG-1")" = "$C_HELD0" ] || P="$P; lease holders [$C_HELD0] -> [$(held_nodes "$C_TAG-1")]"
  [ -z "$(fence_delta "$C_TAG-0" "$C_TAG-1" self_fence)" ] || P="$P; self-fence on $(fence_delta "$C_TAG-0" "$C_TAG-1" self_fence)"
  [ -z "$(fence_delta "$C_TAG-0" "$C_TAG-1" lost)" ] || P="$P; lease lost on $(fence_delta "$C_TAG-0" "$C_TAG-1" lost)"
  [ "${OUT_c1:-0}" -le "$WERR_MAX_MS" ] && [ "${OUT_c2:-0}" -le "$WERR_MAX_MS" ] || P="$P; write outage c1 ${OUT_c1:-?} ms c2 ${OUT_c2:-?} ms > $WERR_MAX_MS"
  s=$(sql_check "$C_T0" "$C_T1" - -); read -r acked failed _ _ gap lost <<<"$s"
  M="$M; sql acked $acked failed $failed max ack gap ${gap}ms"
  [ "$gap" -le "$WERR_MAX_MS" ] || P="$P; SQL writer gap ${gap} ms > $WERR_MAX_MS (DSN failover to the second PgBouncer)"
  [ "$lost" = 0 ] || P="$P; BLOCKER: $lost acknowledged writes missing"
  local ro_all; ro_all=$(awk -v t0="$C_T0" -v t1="$C_T1" '$2 == "fail" && $3 >= t0 && $3 <= t1 && /read-only/' "$ART_DIR/probes/r$ROUND/probe-sql.txt" | wc -l)
  [ "$ro_all" = 0 ] || P="$P; $ro_all read-only write errors"
  printf '%s %s\n' "$C_TAG" "${M#; }" >>"$ART_DIR/timings.txt"
  [ -z "$P" ] && record "$C_TAG" PASS "$desc; ${M#; }" || record "$C_TAG" FAIL "${P#; }; ${M#; }"
}

t_k1() { pgb_case K1 "kill -9 of PgBouncer on ${TH#*@}, held down" "sudo systemctl kill -s KILL $PGB_UNIT; sudo systemctl stop $PGB_UNIT" "$UNDO_START"; }
t_k2() { pgb_case K2 "systemctl stop of PgBouncer on ${TH#*@}" "sudo systemctl stop $PGB_UNIT" "$UNDO_START"; }
t_k3() {
  pgb_case K3 "tcp/$PGB_PORT silently dropped on ${TH#*@} (no RST)" \
    "printf '%s\n' 'table inet p13_drill { chain in { type filter hook input priority -10; policy accept; tcp dport $PGB_PORT drop; } }' | sudo nft -f -" \
    "$UNDO_NFT"
}

# shellcheck disable=SC2086
main_run $CASES
