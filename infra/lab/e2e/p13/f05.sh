#!/usr/bin/env bash
# P13a drill F05: PostgreSQL primary failure while both clusters serve real
# clients (tickets/P13.md §P13a, ADR 0011; thresholds D12, owner decision
# 2026-11-26). Topology, probes and rules: lib.sh.
#
# Cases, each once per client image (IMAGES, default v1.104.7 then v1.84.3):
#   F05.1 patronictl switchover leader -> sync standby (planned)
#   F05.2 kill -9 of the leader's postmaster (Patroni restarts it or fails over)
#   F05.3 hard stop of the leader's VM through the Proxmox API (VMID 7708/7709,
#         pool meshcontrol-lab only), started again after the failover
#   F05.4 nft partition inside the leader's VM: no etcd (2379/2380), no
#         replication (5432) and no Patroni API (8008) to/from the other PG
#         node and mc-db; SSH and PgBouncer stay reachable, so the cluster
#         processes may still reach the isolated old primary. Healed after the
#         checks (dead-man switch PART_MAX s on the VM).
# Every case (D12): both clusters' fenced control-plane writes back within
# RECOVER_MAX_MS (45 s) of the fault; ping gaps (docker<->docker, docker<->VM)
# <= GAP_MAX_MS (10 s); lease epochs and owner_epoch never go back, one lease
# holder per cluster afterwards; clients Running without any action and never
# logged out (MANUAL_MAX 0); every write the SQL writer got acknowledged is on
# the new leader; no "read-only" write error later than 5 s after the
# promotion (PgBouncer never sends writes to a replica).
# F05.4 also (negative, BLOCKER if it fails, AGENTS 13): no write acknowledged
# by the isolated old primary after the promotion; rows the old primary
# committed locally after the promotion (never acknowledged, rewound by
# pg_rewind) are counted and reported.
# Run-wide: L01 lease epochs strictly increasing with one holder each (journal
# of the four processes), S01 no secret in the artifacts.
#
# Usage: CI_BINDIR=/home/mesh/<dir with headscale> infra/lab/e2e/p13/f05.sh
#    or: BIN=<linux/amd64 headscale> infra/lab/e2e/p13/f05.sh
#    CASES="f05_1 f05_3" (default all four), IMAGES=..., VM_TARGETS=... (empty: no VM),
#    PART_MAX=180, MIGRATE_VIA=direct|pgbouncer, KEEP=1 (no cleanup)
# Exit: 0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven.
# shellcheck disable=SC2029
set -u -o pipefail
DRILL=f05
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
PART_MAX=${PART_MAX:-180}
CASES=${CASES:-f05_1 f05_2 f05_3 f05_4}
for c in $CASES; do case $c in f05_1|f05_2|f05_3|f05_4) ;; *) echo "unknown case $c" >&2; exit 2 ;; esac; done

# f05_finish TAG DESC OLD NEW T_PROMOTE: common checks + SQL checks, record.
f05_finish() {
  local desc=$2 old=$3 new=$4 tp=$5 oldip=- s acked failed ro late gap lost
  [ "$old" = "$new" ] || oldip=$(ip_of_member "$old")
  case_check "$RECOVER_MAX_MS"
  s=$(sql_check "$C_T0" "$C_T1" "$tp" "$oldip"); read -r acked failed ro late gap lost <<<"$s"
  M="$M; sql acked $acked failed $failed max ack gap ${gap}ms"
  [ "$ro" = 0 ] || P="$P; $ro read-only write errors later than 5 s after the promotion (writes sent to a replica)"
  [ "$late" = 0 ] || P="$P; BLOCKER: $late writes acknowledged by the old primary $old after the promotion"
  [ "$lost" = 0 ] || P="$P; BLOCKER: $lost acknowledged writes missing on the leader"
  printf '%s %s\n' "$C_TAG" "${M#; }" >>"$ART_DIR/timings.txt"
  [ -z "$P" ] && record "$C_TAG" PASS "$desc; ${M#; }" || record "$C_TAG" FAIL "${P#; }; ${M#; }"
}

t_f05_1() {
  local P="" M="" L S x tl tn new
  read -r L S _ _ <<<"$(pg_state)"
  [ "$L" != - ] && [ "$S" != - ] || { record "R$ROUND.F05.1" FAIL "no leader/sync standby: $(pg_state)"; return; }
  case_begin "R$ROUND.F05.1"
  ssh_ "$(ssh_of_member "$L")" "sudo mc-patronictl switchover --leader $L --candidate $S --force" >>"$ART_DIR/logs/$C_TAG-patroni.txt" 2>&1 \
    || P="$P; switchover command failed"
  read -r tl new <<<"$(wait_leader_not "$L" 60)"
  [ "$new" = "$S" ] || P="$P; leader after switchover: $new (want $S)"
  tn=$(wait_promoted "$S" 5); [ -n "$tn" ] || tn=-
  M="$M; leader $L -> $new seen @$( [ "$tl" = - ] && echo - || echo $((tl - C_T0)))ms"
  case_recover
  f05_finish "$C_TAG" "switchover $L -> $S" "$L" "$new" "$tn"
}

t_f05_2() {
  local P="" M="" L x tl new
  L=$(pg_leader); [ "$L" != - ] || { record "R$ROUND.F05.2" FAIL "no leader: $(pg_state)"; return; }
  case_begin "R$ROUND.F05.2"
  ssh_ "$(ssh_of_member "$L")" "sudo kill -9 \$(sudo head -1 $PGDATA/postmaster.pid)" || P="$P; kill -9 failed"
  # Patroni either restarts PostgreSQL on L (same leader) or fails over.
  local i seen_down=0; tl=-; new=-
  for i in $(seq 1 90); do
    x=$(pg_leader)
    if [ "$x" = - ]; then seen_down=1; elif [ "$x" != "$L" ] || [ $seen_down = 1 ] || [ "$i" -gt 5 ]; then tl=$(ci_now); new=$x; break; fi
    sleep 1
  done
  [ "$new" != - ] || P="$P; no running leader within 90 s"
  M="$M; leader $L -> $new ($([ "$new" = "$L" ] && echo restarted in place || echo failover)) seen @$( [ "$tl" = - ] && echo - || echo $((tl - C_T0)))ms"
  case_recover
  f05_finish "$C_TAG" "kill -9 postmaster on $L" "$L" "$new" "$( [ "$new" = "$L" ] || [ "$tl" = - ] && echo - || echo "$tl")"
}

t_f05_3() {
  local P="" M="" L S v tl new tn tb
  read -r L S _ _ <<<"$(pg_state)"
  [ "$L" != - ] && [ "$S" != - ] || { record "R$ROUND.F05.3" FAIL "no leader/sync standby: $(pg_state)"; return; }
  v=$(vmid_of_member "$L")
  [ "$(pve_vm status "$v")" = running ] || { record "R$ROUND.F05.3" FAIL "VM $v not running before the case"; return; }
  PG_ORDER="$(ssh_of_member "$S") $(ssh_of_member "$L")"
  case_begin "R$ROUND.F05.3"
  pve_vm stop "$v" || P="$P; Proxmox stop of VM $v failed"
  read -r tl new <<<"$(wait_leader_not "$L" 90)"
  [ "$new" = "$S" ] || P="$P; leader after the VM stop: $new (want $S)"
  tn=$(wait_promoted "$S" 5); [ -n "$tn" ] || tn=-
  M="$M; VM $v ($L) hard-stopped; leader $S seen @$( [ "$tl" = - ] && echo - || echo $((tl - C_T0)))ms"
  case_recover
  pve_vm start "$v" || P="$P; Proxmox start of VM $v failed"
  tb=$(date +%s)
  wait_pg_healthy "$PG_HEAL_MAX" && M="$M; $L back as sync standby after $(( $(date +%s) - tb ))s" \
    || P="$P; $L not back as streaming sync standby within ${PG_HEAL_MAX}s ($(pg_state); patronictl reinit is a manual step)"
  PG_ORDER="$PG1_SSH $PG2_SSH"
  f05_finish "$C_TAG" "hard stop of $L's VM $v (Proxmox API)" "$L" "$new" "$tn"
}

t_f05_4() {
  local P="" M="" L S lh peer tl new tn td local_after lease_old lease_new
  read -r L S _ _ <<<"$(pg_state)"
  [ "$L" != - ] && [ "$S" != - ] || { record "R$ROUND.F05.4" FAIL "no leader/sync standby: $(pg_state)"; return; }
  lh=$(ssh_of_member "$L"); peer=$(ip_of_member "$S")
  PG_ORDER="$(ssh_of_member "$S") $lh"
  case_begin "R$ROUND.F05.4"
  deadman "$lh" part "$PART_MAX" "nft delete table inet p13_drill"
  ssh_ "$lh" "sudo nft -f -" <<EOF || P="$P; nft partition not applied"
table inet p13_drill {
  chain in {
    type filter hook input priority -10; policy accept;
    ip saddr { $peer, $MCDB_IP } tcp dport { 2379, 2380, 5432, 8008 } drop
    ip saddr { $peer, $MCDB_IP } tcp sport { 2379, 2380, 5432, 8008 } drop
  }
  chain out {
    type filter hook output priority -10; policy accept;
    ip daddr { $peer, $MCDB_IP } tcp dport { 2379, 2380, 5432, 8008 } drop
    ip daddr { $peer, $MCDB_IP } tcp sport { 2379, 2380, 5432, 8008 } drop
  }
}
EOF
  tn=$(wait_promoted "$S" 90); [ -n "$tn" ] || { tn=-; P="$P; $S not promoted within 90 s"; }
  td=$(wait_not_writable "$L" 60); [ -n "$td" ] || { td=-; P="$P; isolated $L still writable 60 s later"; }
  read -r tl new <<<"$(wait_leader_not "$L" 10)"
  M="$M; partition of $L: $S promoted @$( [ "$tn" = - ] && echo - || echo $((tn - C_T0)))ms, $L not writable @$( [ "$td" = - ] && echo - || echo $((td - C_T0)))ms ($S/$L clocks)"
  case_recover
  # Negative checks while $L is still isolated.
  if [ "$tn" != - ]; then
    local_after=$(psql_on "$L" "SELECT count(*) FROM p13_probe WHERE ts > to_timestamp($tn / 1000.0)" 2>/dev/null | strip_cr)
    M="$M; rows committed locally on the isolated $L after the promotion (never acknowledged, rewound): ${local_after:-?}"
  fi
  lease_old=$(psql_on "$L" "SELECT cluster_id||':'||epoch FROM cluster_leases ORDER BY 1" 2>/dev/null | strip_cr | tr '\n' ' ')
  lease_new=$(psql_ "SELECT cluster_id||':'||epoch FROM cluster_leases ORDER BY 1" 2>/dev/null | strip_cr | tr '\n' ' ')
  M="$M; leases on isolated $L [$lease_old] on $S [$lease_new]"
  ssh_ "$lh" "sudo nft delete table inet p13_drill" || P="$P; partition not removed"
  deadman_cancel "$lh" part
  wait_pg_healthy "$PG_HEAL_MAX" || P="$P; $L not back as streaming sync standby within ${PG_HEAL_MAX}s ($(pg_state))"
  PG_ORDER="$PG1_SSH $PG2_SSH"
  f05_finish "$C_TAG" "nft partition of $L from etcd/replica" "$L" "$S" "$tn"
}

# shellcheck disable=SC2086
main_run $CASES
