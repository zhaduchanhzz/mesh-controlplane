#!/usr/bin/env bash
# P13e drill: rotate the Noise key of cluster c2 (ADR 0014, runbook
# docs/ops/noise-key-rotation.md §2 planned and §3 emergency) with real
# clients of two tenants and both client versions, and measure (D12: measure
# and report, no threshold) how each client recovers. Runs inside the N1 E2E
# harness (lib-ops.sh p13_overlay): c2 = mc-c2 + mc-c2b (active/standby
# behind the long-lived HAProxy mesh-c2-ha on mc-cp, public :8445), c1 and its
# client VMs as the "other cluster" control.
#
# The long-lived c2 key file (/var/lib/headscale/cluster/noise_private.key)
# is never touched: the run config of both c2 processes points at a copy,
# $NOISE_KEY below, that the drill rotates; cleanup deletes the copies and
# checks the long-lived file's sha256 is unchanged (N1's cleanup then starts
# the long-lived c2, whose registry in meshlab still names that key).
#
# Cases (after N1's E00 and S01, which put the VMs a1/a2 on c1):
#   N00 setup: c2 processes on the key copy (registry of the run database
#       still matches), tailnet d on c2 next to N1's c, docker clients on
#       mc-ci: per tailnet (c, d) two clients per image (NOISE_IMAGES,
#       default 1.104.7 and 1.84.3); all see each other, a pair pings
#   N01 planned rotation (§2): standby S stopped, `noise-key install
#       --generate`, S started (standby, match false), `noise-key rotate` on S
#       = switchover; then A stopped, key piped S -> A (`install --from
#       /dev/stdin`), A started, sha256 equal on both hosts
#   N02 emergency rotation (§3 step 4): new key installed on the stopped
#       standby, `noise-key rotate --public-key` while it is still stopped
#       (fail closed: no c2 process serves), then the standby started; key to
#       the other host as in N01
#   For N01 and N02: time rotate -> S /ready 200 (and A /ready 503, public
#   /ready 200, public /key = new key); then a new probe client per tailnet
#   enrols at the new key; each existing client counts as recovered when it
#   sees the probe (a fresh map from the new key); share recovered after
#   NOISE_CHECKPOINTS seconds (default "60 300 900"; add 3600 for the
#   ticket's 60-minute point); clients not recovered at the end get
#   `docker restart` (counted) and must then recover within 120 s, or FAIL
#   (ticket stop condition); no client needs a login; node IDs and IPs
#   unchanged; c1 untouched (same /key, a1<->a2 ping).
#   Per-client rows: noise-recovery.tsv; summary lines: timings.txt.
#
# Usage: CI_BINDIR=/home/mesh/<bindir> MODE=off V=1.104.7 bash infra/lab/e2e/p13/noise-rotate.sh
#   Twice for the ticket's "×2" (the VMs follow V; containers always run both
#   NOISE_IMAGES).
# Exit: as N1.
# shellcheck disable=SC2029,SC2034 # SC2034: CLEAN_OK and friends belong to the N1 harness this file is sourced into

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -u
  # shellcheck source=lib-ops.sh
  . "$(dirname "$0")/lib-ops.sh"
  add="t_n00 t_n01 t_n02"
  p13_overlay noise "$0" "$add" "${P13_CASES:-t_e00 t_s01 $add}"
  exit $?
fi

# ------------------------------------------- sourced into the N1 run copy ---
# shellcheck source=lib-ops.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-ops.sh"

NOISE_IMAGES=${NOISE_IMAGES:-tailscale/tailscale:v1.104.7 tailscale/tailscale:v1.84.3}
NOISE_CHECKPOINTS=${NOISE_CHECKPOINTS:-60 300 900}
NOISE_KEY=/var/lib/headscale/cluster/noise-p13e.key
LIVE_KEY=/var/lib/headscale/cluster/noise_private.key
E_LABEL=mesh-e2e=p13e
E_TAILNETS="c d"
E_CLIENTS=""   # all drill containers (filled by N00)
E_OK=0

e_hsc() { local n=$1; shift; on "$n" "sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $C2_ID $*"; }
e_jget() { py "import json; print(json.load(sys.stdin).get('$1', ''))" | strip_cr; }
e_short() { cut -c1-16; }

t_n00() {
  local p="" n s img vt i nm
  for n in c2 c2b; do
    on $n "sudo sha256sum $LIVE_KEY" | cut -c1-64 >"$TMPD/livekey-$n" || p="$p; $n live key"
  done
  [ "$(cat "$TMPD/livekey-c2")" = "$(cat "$TMPD/livekey-c2b")" ] || p="$p; c2 hosts hold different keys"
  # Both processes onto the key copy: standby first, active last.
  local a sb; a=$(active_of c2); sb=$(standby_of c2)
  [ "$a" != none ] || p="$p; no active c2 process"
  for n in c2 c2b; do
    on $n "set -e; [ \"\$(sudo grep -c '^  private_key_path: ' $CFG)\" = 1 ]
      sudo install -o headscale -g headscale -m 0600 $LIVE_KEY $NOISE_KEY
      sudo sed -i 's#^  private_key_path: .*#  private_key_path: $NOISE_KEY#' $CFG
      sudo grep -q '^  private_key_path: $NOISE_KEY\$' $CFG" || p="$p; $n config"
  done
  stop_proc "$sb"; stop_proc "$a"
  start_proc "$a" && wait_direct_ready "$a" 40 || p="$p; $a not ready on the key copy"
  start_proc "$sb" || p="$p; start $sb"
  wait_public_ready c2 30 || p="$p; public c2 /ready $(public_ready c2)"
  e_hsc "$a" "noise-key show" | e_jget match | grep -qx True || p="$p; noise-key show: no match on the copy"

  cli c2 "tailnet create --slug d" >/dev/null && cli c2 "tailnet status --slug d --set ACTIVE" >/dev/null || p="$p; tailnet d"
  p13_policy_c2 d '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; policy d"
  cli c2 "tailnet authkey --slug d --user p13e-d --reusable --expiration 240m" \
    | py 'import json; print(json.load(sys.stdin)["key"])' | strip_cr >"$TMPD/key-d"; chmod 600 "$TMPD/key-d"
  [ -s "$TMPD/key-c" ] && [ -s "$TMPD/key-d" ] || p="$p; auth keys"

  p13_ci_prepare || p="$p; mc-ci work dir"
  for img in $NOISE_IMAGES; do
    ci_ "docker image inspect $img >/dev/null 2>&1 || docker pull -q $img >/dev/null" || p="$p; image $img"
    vt=$(tr -d . <<<"${img##*:v}")
    for s in $E_TAILNETS; do for i in 1 2; do
      nm=p13e-$s-$vt-$i
      p13_ts_start "$nm" "$img" "$E_LABEL" && p13_ts_up "$nm" "$URL2" "$TMPD/key-$s" || p="$p; $nm up"
      E_CLIENTS="$E_CLIENTS $nm"
    done; done
  done
  # Everyone sees the other three of its tailnet.
  local want ok=0 line
  for _ in $(seq 1 60); do
    ok=1
    while read -r nm _ line; do
      s=${nm#p13e-}; s=${s%%-*}
      want=$(tr ' ' '\n' <<<"$E_CLIENTS" | grep "^p13e-$s-" | grep -vx "$nm" | LC_ALL=C sort | paste -sd, -)
      [ "$line" = "$want" ] || ok=0
    done < <(p13_peers $E_CLIENTS)
    [ $ok = 1 ] && break; sleep 2
  done
  [ $ok = 1 ] || p="$p; peers not complete [$(p13_peers $E_CLIENTS | tr '\n' ';')]"
  for s in $E_TAILNETS; do p13_node_map c2 $s >"$TMPD/nodes0-$s"; done
  p13_server_key "$URL1" >"$TMPD/c1key0"
  for nm in $E_CLIENTS; do p13_needs_login "$nm" >"$TMPD/nl0-$nm"; done
  [ -z "$p" ] && { E_OK=1; record N00 PASS "c2 on a copy of its key; tailnets c, d with $(wc -w <<<"$E_CLIENTS") clients ($NOISE_IMAGES), all peers seen"; } \
    || record N00 FAIL "${p#; }"
}

# e_wait_ready NODE CODE TIMEOUT_S: ms until direct NODE /ready == CODE, or -1.
e_wait_code() {
  local t0 end; t0=$(now_ms); end=$(( $(date +%s) + $3 ))
  while [ "$(date +%s)" -lt "$end" ]; do [ "$(direct "$1" /ready)" = "$2" ] && { echo $(( $(now_ms) - t0 )); return 0; }; sleep 0.2; done
  echo -1; return 1
}
e_wait_public() { # CODE TIMEOUT_S -> ms or -1
  local t0 end; t0=$(now_ms); end=$(( $(date +%s) + $2 ))
  while [ "$(date +%s)" -lt "$end" ]; do [ "$(public_ready c2)" = "$1" ] && { echo $(( $(now_ms) - t0 )); return 0; }; sleep 0.2; done
  echo -1; return 1
}

# e_rotate ID MODE: one rotation (planned|emergency) and the client measurement.
e_rotate() {
  local id=$1 mode=$2 p="" a s knew kold out t0 tr ta tp tdown=- nm s2 i
  [ "$E_OK" = 1 ] || { record "$id" FAIL "N00 did not pass"; return; }
  a=$(active_of c2); s=$(standby_of c2)
  [ "$a" != none ] && [ -n "$s" ] || { record "$id" FAIL "no active/standby pair (active $a)"; return; }
  kold=$(p13_server_key "$URL2")
  stop_proc "$s"
  out=$(e_hsc "$s" "noise-key install --generate") || { record "$id" FAIL "install on $s"; start_proc "$s"; return; }
  printf '%s %s install: %s\n' "$id" "$s" "$(redact <<<"$out")" >>"$ART_DIR/logs/noise-key.log"
  knew=$(e_jget public_key <<<"$out" | sed 's/^mkey://' | e_short)
  [ -n "$knew" ] || p="$p; no public key from install"
  if [ "$mode" = planned ]; then
    start_proc "$s"; sleep 3
    [ "$(direct "$s" /ready)" = 503 ] || p="$p; $s with the new key is not standby before rotate"
    e_hsc "$s" "noise-key show" | e_jget match | grep -qx False || p="$p; show on $s: match not false"
    t0=$(now_ms)
    e_hsc "$s" "noise-key rotate" >>"$ART_DIR/logs/noise-key.log" 2>&1 || p="$p; rotate"
    tr=$(e_wait_code "$s" 200 30); ta=$(e_wait_code "$a" 503 10); tp=$(e_wait_public 200 30)
  else
    t0=$(now_ms)
    e_hsc "$a" "noise-key rotate --public-key mkey:$(e_jget public_key <<<"$out" | sed 's/^mkey://')" >>"$ART_DIR/logs/noise-key.log" 2>&1 || p="$p; rotate"
    tdown=$(e_wait_public 503 15)
    [ "$tdown" -ge 0 ] || p="$p; c2 kept serving on the old key after the emergency rotate"
    start_proc "$s"
    tr=$(e_wait_code "$s" 200 40); ta=$(e_wait_code "$a" 503 5); tp=$(e_wait_public 200 30)
  fi
  [ "$tr" -ge 0 ] || p="$p; $s never ready"
  [ "$ta" -ge 0 ] || p="$p; $a still ready"
  local kpub; kpub=$(p13_server_key "$URL2")
  [ -n "$knew" ] && [ "${kpub:0:16}" = "$knew" ] || p="$p; public /key [${kpub:0:16}] is not the new key [$knew]"
  [ "$kpub" != "$kold" ] || p="$p; public /key unchanged"
  # The key to the other host (pipe only), then it starts as standby.
  stop_proc "$a"
  on "$s" "sudo cat $NOISE_KEY" | on "$a" "sudo -u headscale timeout 60 $HS -c $CFG cluster --cluster-id $C2_ID noise-key install --from /dev/stdin" \
    | redact >>"$ART_DIR/logs/noise-key.log" || p="$p; key copy to $a"
  start_proc "$a"; sleep 3
  [ "$(direct "$a" /ready)" = 503 ] || p="$p; $a not standby after the copy"
  [ "$(on "$a" "sudo sha256sum $NOISE_KEY" | cut -c1-64)" = "$(on "$s" "sudo sha256sum $NOISE_KEY" | cut -c1-64)" ] || p="$p; key files differ"
  printf '%s %s: rotate -> new process /ready 200 %s ms, old 503 %s ms, public 200 %s ms, public down %s ms\n' \
    "$id" "$mode" "$tr" "$ta" "$tp" "$tdown" >>"$ART_DIR/timings.txt"

  # Probes at the new key, one per tailnet; recovery = sees its probe.
  local probes="" pr tstart now el cp line
  for s2 in $E_TAILNETS; do
    pr=p13e-$s2-x$(tr 'A-Z' 'a-z' <<<"$id")
    p13_ts_start "$pr" "${NOISE_IMAGES%% *}" "$E_LABEL" && p13_ts_up "$pr" "$URL2" "$TMPD/key-$s2" || p="$p; probe $pr"
    probes="$probes $pr"
  done
  tstart=$(now_ms)
  declare -A rec=()
  local last; last=$(tr ' ' '\n' <<<"$NOISE_CHECKPOINTS" | sort -n | tail -1)
  local cps=" $NOISE_CHECKPOINTS " summary=""
  while :; do
    now=$(now_ms); el=$(( (now - tstart) / 1000 ))
    while read -r nm _ line; do
      [ -n "${rec[$nm]:-}" ] && continue
      s2=${nm#p13e-}; s2=${s2%%-*}
      case ",$line," in *",p13e-$s2-x$(tr 'A-Z' 'a-z' <<<"$id"),"*) rec[$nm]=$(( now - t0 ));; esac
    done < <(p13_peers $E_CLIENTS)
    for cp in $NOISE_CHECKPOINTS; do
      if [ "$el" -ge "$cp" ] && [[ $cps == *" $cp "* ]]; then
        cps=${cps/ $cp / }
        summary="$summary ${cp}s=${#rec[@]}/$(wc -w <<<"$E_CLIENTS")"
      fi
    done
    [ "${#rec[@]}" = "$(wc -w <<<"$E_CLIENTS")" ] && [ "$el" -ge 30 ] && break
    [ "$el" -ge "$last" ] && break
    sleep 5
  done
  # Not recovered: restart tailscaled (docker restart), must recover then.
  local restarted="" failed=""
  declare -A rr=()
  for nm in $E_CLIENTS; do
    [ -n "${rec[$nm]:-}" ] && continue
    restarted="$restarted $nm"; ci_ "docker restart $nm >/dev/null" || true
    local tq; tq=$(now_ms)
    for _ in $(seq 1 24); do
      line=$(p13_peers "$nm" | awk '{print $3}'); s2=${nm#p13e-}; s2=${s2%%-*}
      case ",$line," in *",p13e-$s2-x$(tr 'A-Z' 'a-z' <<<"$id"),"*) rr[$nm]=$(( $(now_ms) - tq )); break;; esac
      sleep 5
    done
    [ -n "${rr[$nm]:-}" ] || failed="$failed $nm"
  done
  [ -z "$failed" ] || p="$p; not recovered even after restarting tailscaled:$failed (ticket stop condition)"
  for nm in $E_CLIENTS; do
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "$mode" "$nm" "${rec[$nm]:--}" "$([[ $restarted == *" $nm"* ]] && echo yes || echo no)" "${rr[$nm]:--}" \
      >>"$ART_DIR/noise-recovery.tsv"
  done
  # No login, same node IDs/IPs, c1 untouched.
  for nm in $E_CLIENTS; do [ "$(p13_needs_login "$nm")" = "$(cat "$TMPD/nl0-$nm")" ] || p="$p; $nm NeedsLogin"; done
  for s2 in $E_TAILNETS; do
    local now_map; now_map=$(p13_node_map c2 "$s2")
    for kv in $(cat "$TMPD/nodes0-$s2"); do grep -qw -- "$kv" <<<"$now_map" || p="$p; node $kv changed"; done
  done
  [ "$(p13_server_key "$URL1")" = "$(cat "$TMPD/c1key0")" ] || p="$p; c1 /key changed"
  ping_ok $A1 n1-a2 || p="$p; c1: a1 cannot ping a2"
  ci_ "docker rm -f $probes >/dev/null; docker volume rm $probes >/dev/null 2>&1; true"
  local nrec=${#rec[@]} ntot; ntot=$(wc -w <<<"$E_CLIENTS")
  printf '%s %s: recovered without restart %d/%d;%s; restarted:%s\n' "$id" "$mode" "$nrec" "$ntot" "$summary" "${restarted:- none}" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "$id" PASS "$mode rotation: new process ready $tr ms after rotate (public $tp ms), /key = new key; clients recovered without restart $nrec/$ntot ($summary ), restarted:${restarted:- none}; no login, node IDs/IPs kept, c1 untouched" \
    || record "$id" FAIL "${p#; }"
}
t_n01() { e_rotate N01 planned; }
t_n02() { e_rotate N02 emergency; }

p13_cleanup() {
  p13_ci_cleanup "$E_LABEL" >/dev/null 2>&1 || { log "p13e: clients on mc-ci not removed"; CLEAN_OK=0; }
  local n
  for n in c2 c2b; do
    on $n "sudo systemctl stop $UNIT 2>/dev/null; sudo rm -f $NOISE_KEY; ! sudo test -e $NOISE_KEY" || { log "$n: $NOISE_KEY not removed"; CLEAN_OK=0; }
    if [ -s "$TMPD/livekey-$n" ]; then
      [ "$(on $n "sudo sha256sum $LIVE_KEY" | cut -c1-64)" = "$(cat "$TMPD/livekey-$n")" ] || { log "$n: long-lived Noise key file changed"; CLEAN_OK=0; }
    fi
  done
}
