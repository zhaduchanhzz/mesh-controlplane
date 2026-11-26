#!/usr/bin/env bash
# P13g drill "restart relay" (tickets/P13.md §P13g, 04-proxmox-lab.md §8):
# restart the cluster's DERP relay while clients use it; measure how long the
# relayed path is gone, that no client loses a peer, and that the relay's
# client verification (/verify of the cluster, fail closed) holds after the
# restart. Runs inside the N1 E2E harness (lib-ops.sh p13_overlay): N1's c1,
# c2, relay mc-relay (regions 901/902, derper units n1-derp-c1/-c2) and client
# VMs a1/a2/b1/b2.
#
# Cases (after N1's E00, S01 and S15):
#   R00 setup: a1<->a2 forced onto DERP(n1c1) (UDP between them dropped in a2,
#       as S15), relayed ping works; INFO: whether the verify secret is visible
#       in the derper argv (P13g verify-proxy not done yet: counted, never
#       printed)
#   R01 graceful restart of n1-derp-c1 (stop + start, same flags as N1)
#   R02 kill -9 of derper, down RELAY_DOWN_S (5 s), start
#   R03 reboot of the relay VM (only with RELAY_REBOOT=1; /run is tmpfs: cert,
#       key and verify secret are installed again from the run's copies)
#   For each: a1->a2 ICMP every 200 ms through the tailnet (pinger on a1),
#   a1's peers sampled every second (never empty), time until `tailscale ping`
#   is again "via DERP(n1c1)", a1 home DERP n1c1 again, the relay refuses an
#   unknown node key afterwards (n1probe derp-check, as S15), derper log since
#   the restart (verify errors counted), b1 (tailnet b) never sees an a node.
# Threshold: no peer lost (hard); relayed ping gap <= RELAY_GAP_MAX_MS
# (proposal 20000; D12 sets none for the relay: measured and reported).
#
# Usage (laptop, Git Bash, as the N1 harness):
#   CI_BINDIR=/home/mesh/<bindir> MODE=off V=1.104.7 bash infra/lab/e2e/p13/relay-restart.sh
#   RELAY_REBOOT=1 adds R03. Run once per client version (V=1.104.7, V=1.84.3).
# Exit: as N1 (0 no FAIL, 1 any FAIL, 2 setup error or cleanup not proven).
# shellcheck disable=SC2029,SC2034 # SC2034: CLEAN_OK and friends belong to the N1 harness this file is sourced into

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -u
  # shellcheck source=lib-ops.sh
  . "$(dirname "$0")/lib-ops.sh"
  add="t_r00 t_r01 t_r02 t_r03"
  p13_overlay relay "$0" "$add" "${P13_CASES:-t_e00 t_s01 t_s15 $add}"
  exit $?
fi

# ------------------------------------------- sourced into the N1 run copy ---
# shellcheck source=lib-ops.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-ops.sh"

R_GAP_MAX_MS=${RELAY_GAP_MAX_MS:-20000}
R_BACK_MAX_S=${RELAY_BACK_MAX_S:-90}
R_DOWN_S=${RELAY_DOWN_S:-5}
R_REBOOT=${RELAY_REBOOT:-0}

# derper unit of cluster c1|c2, exactly the flags of N1's setup.
r_derp_start() {
  if [ "$1" = c1 ]; then
    relay "sudo systemctl reset-failed n1-derp-c1 2>/dev/null
      sudo systemd-run --quiet --unit=n1-derp-c1 /bin/bash -c 'exec /usr/local/bin/derper -a :443 -http-port -1 -hostname relay-c1.cp.gta5web.online \
        -certmode manual -certdir /run/n1-derp/c1 -stun -stun-port 3478 -verify-client-url-fail-open=false \
        -verify-client-url \"$URL1/verify?secret=\$(cat /run/n1-derp/c1.secret)\"'
      for i in \$(seq 1 20); do systemctl is-active -q n1-derp-c1 && exit 0; sleep 0.25; done; exit 1"
  else
    relay "sudo systemctl reset-failed n1-derp-c2 2>/dev/null
      sudo systemd-run --quiet --unit=n1-derp-c2 /bin/bash -c 'exec /usr/local/bin/derper -a :444 -http-port -1 -hostname relay-c2.cp.gta5web.online \
        -certmode manual -certdir /run/n1-derp/c2 -stun=false -verify-client-url-fail-open=false \
        -verify-client-url \"$URL2/verify?secret=\$(cat /run/n1-derp/c2.secret)\"'
      for i in \$(seq 1 20); do systemctl is-active -q n1-derp-c2 && exit 0; sleep 0.25; done; exit 1"
  fi
}
# cert, key and verify secret of cluster c on the relay (again after a reboot).
r_derp_files() {
  local c=$1
  relay "sudo test -s /run/n1-derp/$c.secret && sudo test -s /run/n1-derp/$c/relay-$c.cp.gta5web.online.key" && return 0
  relay "sudo install -d -m 0700 /run/n1-derp /run/n1-derp/$c" || return 1
  cp_ 'sudo cat /etc/headscale/tls/fullchain.pem' | relay "sudo install -m 0644 /dev/stdin /run/n1-derp/$c/relay-$c.cp.gta5web.online.crt" || return 1
  cp_ 'sudo cat /etc/headscale/tls/privkey.pem' | relay "sudo install -m 0600 /dev/stdin /run/n1-derp/$c/relay-$c.cp.gta5web.online.key" || return 1
  relay "sudo install -m 0600 /dev/stdin /run/n1-derp/$c.secret" <"$TMPD/vsecret-$c"
}
r_force_derp() {
  vm $A2 "sudo nft delete table inet n1test 2>/dev/null; sudo nft -f - <<'NFT'
table inet n1test {
  chain input { type filter hook input priority filter; policy accept; ip saddr $A1 udp dport 41641 drop; }
  chain output { type filter hook output priority filter; policy accept; ip daddr $A1 udp dport 41641 drop; }
}
NFT" || return 1
  TS $A1 "debug restun" >/dev/null 2>&1 || true
  local i
  for _ in $(seq 1 15); do TS $A1 "ping -c 1 --timeout 3s n1-a2" 2>&1 | grep -q 'via DERP(n1c1)' && return 0; sleep 1; done
  return 1
}

R_READY=0
t_r00() {
  local p="" n
  r_force_derp && R_READY=1 || p="$p; a1->a2 not on DERP(n1c1)"
  [ "$(relayof $A1)" = n1c1 ] || p="$p; a1 home DERP [$(relayof $A1)]"
  n=$(relay "ps -eo args | grep -c '[d]erper.*secret=[A-Za-z0-9]'" 2>/dev/null | strip_cr)
  printf 'R00 derper processes with the verify secret in argv: %s\n' "${n:-?}" >>"$ART_DIR/timings.txt"
  [ "${n:-0}" = 0 ] || record R00i INFO "verify secret visible in $n derper argv (P13g verify-proxy pending; not printed)"
  [ -z "$p" ] && record R00 PASS "a1<->a2 relayed via DERP(n1c1); a1 home n1c1" || record R00 FAIL "${p#; }"
}

# r_drill ID HOW: one relay restart, measured.
r_drill() {
  local id=$1 how=$2 p="" t0 t1 t2 peers0 lostpeer=0 i pv ip2 since gap
  [ "$R_READY" = 1 ] || r_force_derp || { record "$id" FAIL "a1<->a2 not on DERP(n1c1) before the restart"; return; }
  peers0=$(peers $A1); ip2=$(selfip $A2)
  [ -n "$peers0" ] && [ -n "$ip2" ] || { record "$id" FAIL "no peers/IP before ($peers0/$ip2)"; return; }
  vm_pinger_start $A1 "$id" "$ip2" || { record "$id" FAIL "pinger on a1"; return; }
  sleep 5
  since=$(relay 'date -u +%Y-%m-%dT%H:%M:%S' | strip_cr)
  t0=$(now_ms)
  case $how in
    graceful) relay "sudo systemctl stop n1-derp-c1" && r_derp_files c1 && r_derp_start c1 ;;
    kill) relay "sudo systemctl kill -s KILL n1-derp-c1; sleep $R_DOWN_S; sudo systemctl stop n1-derp-c1 2>/dev/null; true" \
          && r_derp_files c1 && r_derp_start c1 ;;
    reboot)
      relay "sudo systemctl reboot" >/dev/null 2>&1
      sleep 10
      for _ in $(seq 1 60); do relay true >/dev/null 2>&1 && break; sleep 3; done
      r_derp_files c1 && r_derp_files c2 && r_derp_start c1 && r_derp_start c2 ;;
  esac || p="$p; relay not back ($how)"
  t1=$(now_ms)
  for _ in $(seq 1 "$R_BACK_MAX_S"); do
    [ "$(peers $A1)" = "$peers0" ] || lostpeer=$((lostpeer + 1))
    pv=$(TS $A1 "ping -c 1 --timeout 2s n1-a2" 2>&1)
    grep -q 'via DERP(n1c1)' <<<"$pv" && break
    sleep 1
  done
  t2=$(now_ms)
  grep -q 'via DERP(n1c1)' <<<"$pv" || p="$p; no relayed pong within ${R_BACK_MAX_S}s"
  sleep 5
  vm_pinger_stop $A1 "$id"
  read -r gap _ _ <<<"$(gap_stats "$ART_DIR/logs/$id.ping")"
  [ "$lostpeer" = 0 ] || p="$p; a1's peers differed in $lostpeer sample(s)"
  [ "${gap:--1}" -ge 0 ] && [ "$gap" -le "$R_GAP_MAX_MS" ] || p="$p; ping gap ${gap} ms > $R_GAP_MAX_MS"
  [ "$(relayof $A1)" = n1c1 ] || p="$p; a1 home DERP [$(relayof $A1)] after"
  # Verification holds: an unknown node key is refused by the restarted relay.
  if ssh_ $CI_SSH "/tmp/n1probe derp-check 2>&1 | grep -qi -e usage -e host"; then
    local out; out=$(vm $A1 "/tmp/n1probe derp-check --host relay-c1.cp.gta5web.online --ip $RELAY --port 443 --count 3; echo rc=\$?" 2>&1)
    printf '%s\n' "$out" >"$ART_DIR/logs/$id-derp-check.jsonl"
    grep -q 'rc=0' <<<"$out" || p="$p; unknown key admitted after the restart"
  else
    p="$p; n1probe derp-check missing (verify not checked)"
  fi
  relay "sudo journalctl -u n1-derp-c1 --since '$since' --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/$id-derper.log"
  local verr; verr=$(grep -ciE 'verify.*(error|fail)|(error|fail).*verify' "$ART_DIR/logs/$id-derper.log" || true)
  grep -qE 'n1-a[12]' <<<"$(peers $B1)" && p="$p; b1 sees an a node"
  printf '%s %s: relay back %d ms; relayed pong again %d ms; ICMP gap %s ms; peer samples off %d; derper verify errors %s\n' \
    "$id" "$how" $((t1 - t0)) $((t2 - t0)) "$gap" "$lostpeer" "$verr" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "$id" PASS "relay $how: back in $((t1 - t0)) ms, a1->a2 relayed again in $((t2 - t0)) ms, ICMP gap $gap ms (<= $R_GAP_MAX_MS), peers kept, unknown key refused, verify errors in log: $verr" \
    || record "$id" FAIL "${p#; }"
}
t_r01() { r_drill R01 graceful; }
t_r02() { r_drill R02 kill; }
t_r03() {
  [ "$R_REBOOT" = 1 ] || { record R03 SKIP "relay VM reboot not requested (RELAY_REBOOT=1)"; return; }
  r_drill R03 reboot
}

p13_cleanup() {
  vm $A1 "sudo touch $P13VM/R01.stop $P13VM/R02.stop $P13VM/R03.stop 2>/dev/null; \
    for u in \$(systemctl list-units --plain --no-legend 'p13x-ping-*' | awk '{print \$1}'); do sudo systemctl stop \$u; done; sudo rm -rf $P13VM" \
    >/dev/null 2>&1 || { log "a1: p13 pinger not removed"; CLEAN_OK=0; }
  vm $A2 "sudo nft delete table inet n1test 2>/dev/null; true" >/dev/null 2>&1
}
