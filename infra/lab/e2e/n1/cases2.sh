# shellcheck shell=bash
# N1 E2E batch 2 (sourced by run.sh; uses its helpers and variables).
# Cases: S07 DNS, S06/S08 routes and exit node, S10 credentials across
# tailnets/clusters, S12 events, S14 public surface, S16 restart + F01,
# S17 F02 with frame timing (evidence/P09-s17-frame-timing.md), S18 revoke and
# delete, M06 incremental netmap with real clients.

# live CLI (unix socket) on the active process of a cluster: live c1|c2 ARGS
live() { local c=$1 a; shift; a=$(active_of "$c"); [ "$a" = none ] && return 1
  if [ "$c" = c1 ]; then on "$a" "$CLI1 tailnet $*"; else on "$a" "$CLI2 tailnet $*"; fi; }
node_id() { # CLUSTER SLUG HOSTNAME
  live "$1" node list --slug "$2" 2>/dev/null | py 'import json
d = json.load(sys.stdin)
for n in (d.get("nodes") if isinstance(d, dict) else d) or []:
    if n.get("hostname")==sys.argv[1] or n.get("name")==sys.argv[1]: print(n["id"]); break' "$3" | strip_cr
}
netmap() { TS "$1" "debug netmap" 2>/dev/null; }
set_dns() { # SLUG JSON  (c1 tailnets only here)
  local f=/var/lib/headscale/cluster/n1-dns.json
  printf '%s\n' "$2" | cp_ "sudo install -o headscale -g headscale -m 0640 /dev/stdin $f && $CLI1 tailnet dns --slug $1 --file $f; rc=\$?; sudo rm -f $f; exit \$rc" \
    >>"$ART_DIR/logs/dns-cli.log" 2>&1
}

t_s07() {
  local p="" i nb na
  set_dns b '{"override_local_dns":true,"nameservers":{"global":["9.9.9.9"]},"search_domains":["b.corp.example"]}' || p="$p; dns b refused"
  for i in $(seq 1 30); do nb=$(netmap $B1); grep -q b.corp.example <<<"$nb" && break; sleep 1; done
  na=$(netmap $A1)
  grep -q b.corp.example <<<"$nb" || p="$p; b1 did not get B's search domain"
  grep -qE 'b\.corp\.example|9\.9\.9\.9' <<<"$na" && p="$p; A's netmap carries B's DNS"
  set_dns b '{"extra_records":[{"name":"n1-a1.a.c1.tailnet.internal","type":"A","value":"100.64.0.9"}]}' && p="$p; a record under A's domain stored for B"
  set_dns b '{}' >/dev/null 2>&1 || true
  [ -z "$p" ] && record S07 PASS "B's DNS (nameserver, search domain) reaches b1 only; a record under A's domain refused for B" || record S07 FAIL "${p#; }"
}

# S06/S08: same subnet route and an exit node in A and B; approvals are per
# tailnet; a route or exit node of one tailnet never appears in the other.
t_s08() {
  local p="" ia ib i
  TS $A2 "set --advertise-routes=10.99.0.0/24 --advertise-exit-node" >/dev/null 2>&1 || p="$p; a2 advertise"
  TS $B2 "set --advertise-routes=10.99.0.0/24 --advertise-exit-node" >/dev/null 2>&1 || p="$p; b2 advertise"
  sleep 4
  ia=$(node_id c1 a n1-a2); ib=$(node_id c1 b n1-b2)
  [ -n "$ia" ] && [ -n "$ib" ] || p="$p; node ids a2=[$ia] b2=[$ib]"
  # Nothing approved yet: no route, no exit node offered anywhere.
  grep -q '10.99.0.0/24' <<<"$(ts_json $A1)$(ts_json $B1)" && p="$p; route offered before approval"
  live c1 routes approve --slug a --id "$ia" --routes 10.99.0.0/24,0.0.0.0/0,::/0 >>"$ART_DIR/logs/routes.log" 2>&1 || p="$p; approve a2"
  for i in $(seq 1 30); do field $A1 "any('10.99.0.0/24' in (q.get('PrimaryRoutes') or []) for q in (d.get('Peer') or {}).values())" | grep -q True && break; sleep 1; done
  field $A1 "any('10.99.0.0/24' in (q.get('PrimaryRoutes') or []) for q in (d.get('Peer') or {}).values())" | grep -q True || p="$p; a1 does not see a2's approved route"
  field $A1 "any(q.get('ExitNodeOption') for q in (d.get('Peer') or {}).values())" | grep -q True || p="$p; a1 has no exit node option"
  # B: nothing approved there; A's approval does not leak.
  field $B1 "any('10.99.0.0/24' in (q.get('PrimaryRoutes') or []) for q in (d.get('Peer') or {}).values())" | grep -q True && p="$p; B got a route approved in A"
  field $B1 "any(q.get('ExitNodeOption') for q in (d.get('Peer') or {}).values())" | grep -q True && p="$p; B got an exit node approved in A"
  # The same route ID space: approving node id of A under slug b must fail.
  live c1 routes approve --slug b --id "$ia" --routes 10.99.0.0/24 >>"$ART_DIR/logs/routes.log" 2>&1 && p="$p; approved A's node under B"
  # Use the exit node in A. With an exit node a1's replies to the harness
  # (mc-ci, a private address the lab may not reach) leave through a2 and the
  # SSH session hangs (run at e2183ed): set, check and unset run detached on
  # a1 in one transient unit, which always unsets; the harness reads the
  # result after.
  vm $A1 "cat >/tmp/n1-exit.sh" <<EOF
T="/opt/tailscale/$V/tailscale --socket=$TSS"
trap '\$T set --exit-node= ; touch /tmp/n1-exit.done' EXIT
rm -f /tmp/n1-exit.json /tmp/n1-exit.done
\$T set --exit-node=n1-a2 || exit 1
sleep 4
\$T status --json >/tmp/n1-exit.json
EOF
  vm $A1 "sudo systemd-run --quiet --collect --unit=n1-exit -p RuntimeMaxSec=40 bash /tmp/n1-exit.sh" || p="$p; exit-node unit"
  local j; for j in $(seq 1 30); do vm $A1 "test -e /tmp/n1-exit.done" 2>/dev/null && break; sleep 2; done
  vm $A1 "sudo cat /tmp/n1-exit.json" 2>/dev/null | py 'import json; d = json.load(sys.stdin); print("yes" if d.get("ExitNodeStatus") else "no")' | strip_cr | grep -q yes \
    || p="$p; a1 did not use a2 as exit node"
  [ -z "$(field $A1 "(d.get('ExitNodeStatus') or {}).get('ID', '')")" ] || p="$p; a1 still has an exit node"
  TS $A2 "set --advertise-routes= --advertise-exit-node=false" >/dev/null 2>&1
  TS $B2 "set --advertise-routes= --advertise-exit-node=false" >/dev/null 2>&1
  [ -z "$p" ] && record S08 PASS "same route 10.99.0.0/24 + exit node in A and B: approved only in A, seen only by A; A's node id refused under B; a1 used a2 as exit node" || record S08 FAIL "${p#; }"
}

t_s10() {
  local p="" r=0
  # A key at c2 (another cluster) and C's key at c1: refused, no node.
  ts_aside $B2 >/dev/null 2>&1 || p="$p; park b2"
  r=0; ts_up $B2 "$URL2" "$TMPD/key-a" n1-x1 || r=$?; [ $r != 0 ] || p="$p; key of a accepted at c2"
  ts_start $B2 >/dev/null 2>&1 || true
  r=0; ts_up $B2 "$URL1" "$TMPD/key-c" n1-x2 || r=$?; [ $r != 0 ] || p="$p; key of c accepted at c1"
  [ -z "$(node_id c1 a n1-x1)$(node_id c1 b n1-x2)" ] || p="$p; a node was created"
  ts_back $B2 >/dev/null 2>&1 || p="$p; b2 back"
  wait_peers $B1 n1-b2 40 || p="$p; b1 peers [$(peers $B1)]"
  [ -z "$p" ] && record S10 PASS "key of a at c2 and key of c at c1 refused, no node; b2 re-enrolled" || record S10 FAIL "${p#; }"
}

# S12: activity of A (route change, down/up x3) is invisible to B: B's
# per-tailnet update counter does not move, B's netmap never names A.
tailnet_updates() { # ACTIVE_NODE TAILNET_ID
  on "$1" "curl -s -m 3 http://127.0.0.1:9095/metrics" 2>/dev/null \
    | awk -v t="$2" '$0 ~ "^headscale_tailnet_mapresponses_sent_total\\{" && index($0,"tailnet=\""t"\"") && index($0,"type=\"update\"") {s+=$2} END {printf "%d", s}'
}
t_s12() {
  local p="" act tb u0 u1 i
  act=$(active_of c1); tb=$(cat "$TMPD/id-b" 2>/dev/null)
  u0=$(tailnet_updates "$act" "$tb")
  for i in 1 2 3; do
    TS $A2 "set --advertise-routes=10.98.$i.0/24" >/dev/null 2>&1; sleep 1
    TS $A2 down >/dev/null 2>&1; sleep 1; TS $A2 up >/dev/null 2>&1; sleep 2
  done
  TS $A2 "set --advertise-routes=" >/dev/null 2>&1; sleep 3
  u1=$(tailnet_updates "$act" "$tb")
  grep -qE 'n1-a[12]' <<<"$(ts_json $B1)$(netmap $B1)" && p="$p; B saw an A node"
  [ -n "$tb" ] && [ "$u0" = "$u1" ] || p="$p; B update frames $u0 -> $u1 (tailnet id [$tb])"
  wait_peers $A1 n1-a2 30 || p="$p; a1 lost a2"
  [ -z "$p" ] && record S12 PASS "3 rounds of A activity: B's update frames unchanged ($u0), B never named an A node" || record S12 FAIL "${p#; }"
}

t_s14() {
  local p="" c u path
  for u in "$URL1" "$URL2"; do
    for path in /metrics /debug/pprof/ /debug/vars /swagger /api/v1/node /api/v2/tailnet/-/devices /verify; do
      c=$(curl -s -o /dev/null -w '%{http_code}' -m 6 "$u$path")
      case "$path:$c" in /api/v2/*:401|/verify:405|/verify:403|*:404|*:403|*:421) ;; *) p="$p; $u$path=$c";; esac
    done
    c=$(curl -s -o /dev/null -w '%{http_code}' -m 6 -X POST "$u/verify"); [ "$c" = 403 ] || p="$p; POST $u/verify without secret=$c"
  done
  # gRPC admin is not served on the public listener: an HTTP/2 gRPC call gets no service.
  c=$(ssh_ $CI_SSH "curl -s -o /dev/null -w '%{http_code}' -m 6 --http2 -X POST -H 'content-type: application/grpc' '$URL1/headscale.v1.HeadscaleService/ListNodes'" 2>/dev/null)
  case "$c" in 404|403|415|421) ;; *) p="$p; gRPC path=$c";; esac
  [ -z "$p" ] && record S14 PASS "public listeners of c1 and c2: no metrics, debug, swagger, /api/v1, gRPC admin; /api/v2 needs a key; /verify needs the secret" || record S14 FAIL "${p#; }"
}

t_s16() {
  local id=$1 c=$2 p="" a b act sb nk ip e0 t0 t1
  if [ "$c" = c1 ]; then a=$A1; b=n1-a2; else return 0; fi
  nk=$(selfkey $a); ip=$(selfip $a); e0=$(lease_epoch_n1 c1)
  # Restart of both processes.
  for n in $(nodesof $c); do stop_proc $n; done; sleep 3
  set -- $(nodesof $c); start_proc $1; wait_direct_ready $1 40 || p="$p; $1 not ready"; start_proc $2
  wait_public_ready $c 30 || p="$p; public /ready"
  local i; for i in $(seq 1 60); do ping_ok $a $b && break; sleep 1; done
  ping_ok $a $b || p="$p; $a cannot ping $b after restart"
  # F01: kill -9 the active.
  act=$(active_of $c); sb=$(standby_of $c); t0=$(now_ms)
  on "$act" "sudo systemctl kill --kill-whom=main -s KILL $UNIT"
  wait_direct_ready "$sb" 40 || p="$p; $sb not ready after kill"
  t1=$(now_ms)
  for i in $(seq 1 60); do ping_ok $a $b && break; sleep 1; done
  ping_ok $a $b || p="$p; $a cannot ping $b after failover"
  [ "$(selfkey $a)" = "$nk" ] && [ "$(selfip $a)" = "$ip" ] || p="$p; $a node key/IP changed (login)"
  start_proc "$act"
  printf '%s %s restart+F01: standby ready %d ms\n' "$id" "$c" $((t1 - t0)) >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record "$id" PASS "$c: restart of both processes and kill -9 of the active ($((t1 - t0)) ms to standby ready): $a back, same node key and IP, ping" || record "$id" FAIL "${p#; }"
}
lease_epoch_n1() { pg_ "sudo docker exec mc-pg psql -At -U postgres -d $PG_DB -c \"SELECT epoch FROM cluster_leases WHERE cluster_id='$(idof "$1")'\"" 2>/dev/null; }

# S17: F02 with the frame-timing evidence (P09 S17 note), repeated.
t_s17() {
  local c=$1 n=$2 p="" k act sb out S0 S1 TS TC G1 W1 NB0 NB1 verdicts=""
  for k in $(seq 1 "$n"); do
    act=$(active_of $c); sb=$(standby_of $c)
    [ "$act" != none ] || { p="$p; round $k: no active"; break; }
    out=$(on "$act" "m() { curl -s -m 3 http://127.0.0.1:9095/metrics; }
      S0=\$(m | awk '/^headscale_mapresponse_sent_total\\{status=\"ok\"/ {s+=\$2} END {printf \"%d\", s}')
      NB0=\$(m | awk '/^headscale_mapresponse_not_servable_total\\{stage=\"before_write\"/ {printf \"%d\", \$2}')
      TS=\$(date +%s%N); sudo systemctl kill --kill-whom=main -s STOP $UNIT
      echo \"S0=\$S0 NB0=\${NB0:-0} TS=\$TS\"")
    eval "$(grep -E '^S0=' <<<"$out")"
    wait_direct_ready "$sb" $((TTL + 25)) || p="$p; round $k: $sb did not take over"
    sleep 3
    out=$(on "$act" "TC=\$(date +%s%N); sudo systemctl kill --kill-whom=main -s CONT $UNIT; sleep 12
      m() { curl -s -m 3 http://127.0.0.1:9095/metrics; }
      S1=\$(m | awk '/^headscale_mapresponse_sent_total\\{status=\"ok\"/ {s+=\$2} END {printf \"%d\", s}')
      NB1=\$(m | awk '/^headscale_mapresponse_not_servable_total\\{stage=\"before_write\"/ {printf \"%d\", \$2}')
      G1=\$(m | awk '/^headscale_mapresponse_last_gate_unix_nanos / {printf \"%.0f\", \$2}')
      W1=\$(m | awk '/^headscale_mapresponse_last_write_unix_nanos / {printf \"%.0f\", \$2}')
      echo \"TC=\$TC S1=\$S1 NB1=\${NB1:-0} G1=\$G1 W1=\$W1\"")
    eval "$(grep -E '^TC=' <<<"$out")"
    local v=PASS
    if [ "${S1:-0}" != "${S0:-0}" ]; then
      if [ "${G1:-0}" -gt "${TC:-0}" ]; then v="FAIL-gate-after-resume"
      elif [ "${W1:-0}" -lt "${TS:-0}" ]; then v="pass-artefact(frame before STOP)"
      elif [ "${W1:-0}" -gt "${TC:-0}" ]; then v="FAIL-us-window"
      else v="FAIL-unclassified"; fi
    fi
    printf 'S17 %s round %d act=%s TS=%s TC=%s S0=%s S1=%s G1=%s W1=%s before_write %s->%s verdict=%s\n' \
      "$c" "$k" "$act" "$TS" "$TC" "$S0" "$S1" "$G1" "$W1" "$NB0" "$NB1" "$v" >>"$ART_DIR/timings.txt"
    verdicts="$verdicts $v"
    case "$v" in FAIL*) p="$p; round $k $v";; esac
    [ "$(direct "$act" /ready)" = 503 ] || p="$p; round $k: resumed $act /ready $(direct "$act" /ready)"
  done
  [ -z "$p" ] && record "S17-$c" PASS "F02 x$n on $c: after SIGCONT no frame passed the lease gate (verdicts:$verdicts; timings.txt)" || record "S17-$c" FAIL "${p#; }"
}

# S18: revoked key cannot enrol; a deleted node cannot come back; a DELETING
# tailnet is terminal and its slug is not reusable.
t_s18() {
  local p="" kid r
  kid=$(cli c1 "tailnet authkey --slug b --user n1-b-rev --reusable --expiration 30m" | tee "$TMPD/rev.json" | py 'import json; print(json.load(sys.stdin).get("id",""))' | strip_cr)
  py 'import json; print(json.load(open(sys.argv[1]))["key"])' "$TMPD/rev.json" | strip_cr >"$TMPD/key-rev"; rm -f "$TMPD/rev.json"
  cli c1 "tailnet authkey-revoke --slug b --id $kid" >/dev/null 2>&1 || p="$p; revoke failed"
  ts_aside $B2 >/dev/null 2>&1; r=0; ts_up $B2 "$URL1" "$TMPD/key-rev" n1-rev || r=$?
  [ $r != 0 ] || p="$p; revoked key enrolled"
  [ -z "$(node_id c1 b n1-rev)" ] || p="$p; node from a revoked key"
  # Delete b2's own node: it does not come back with its old identity.
  ts_back $B2 >/dev/null 2>&1
  wait_peers $B1 n1-b2 40 || p="$p; b2 not back before delete"
  local idb2; idb2=$(node_id c1 b n1-b2)
  live c1 node delete --slug b --id "$idb2" >>"$ART_DIR/logs/s18.log" 2>&1 || p="$p; delete b2"
  local i; for i in $(seq 1 20); do [ -z "$(peers $B1)" ] && break; sleep 1; done
  [ -z "$(peers $B1)" ] || p="$p; b1 still sees [$(peers $B1)] after delete"
  sleep 5; [ -z "$(node_id c1 b n1-b2)" ] || p="$p; deleted node came back"
  [ -z "$p" ] && record S18 PASS "revoked key refused, no node; deleted node dropped from peers and not resurrected" || record S18 FAIL "${p#; }"
}

# M06: with real clients, removals reach peers within BOUND seconds (switch
# ON and OFF): delete a node, block by policy, restore.
t_m06() {
  local p="" t0 t1 ida bound=10
  ts_aside $A2 >/dev/null 2>&1; ts_up $A2 "$URL1" "$TMPD/key-a" n1-a2m >/dev/null 2>&1
  local i; for i in $(seq 1 40); do grep -qw n1-a2m <<<"$(peers $A1)" && break; sleep 1; done
  grep -qw n1-a2m <<<"$(peers $A1)" || p="$p; a2m not seen"
  ida=$(node_id c1 a n1-a2m); t0=$(now_ms)
  live c1 node delete --slug a --id "$ida" >>"$ART_DIR/logs/m06.log" 2>&1 || p="$p; delete [$ida]"
  while [ $(( $(now_ms) - t0 )) -lt $((bound * 1000)) ]; do grep -qw n1-a2m <<<"$(peers $A1)" || break; sleep 0.2; done
  t1=$(now_ms); grep -qw n1-a2m <<<"$(peers $A1)" && p="$p; a1 still sees n1-a2m after ${bound}s"
  printf 'M06 %s delete->gone %d ms (includes the ssh round trip of each poll)\n' "$MODE" $((t1 - t0)) >>"$ART_DIR/timings.txt"
  ts_back $A2 >/dev/null 2>&1
  wait_peers $A1 n1-a2 40 || p="$p; a2 back [$(peers $A1)]"
  t0=$(now_ms); set_policy a '{"grants":[]}' || p="$p; block policy"
  while [ $(( $(now_ms) - t0 )) -lt $((bound * 1000)) ]; do [ -z "$(peers $A1)" ] && break; sleep 0.2; done
  t1=$(now_ms); [ -z "$(peers $A1)" ] || p="$p; policy block not applied in ${bound}s"
  printf 'M06 %s policy-block->gone %d ms\n' "$MODE" $((t1 - t0)) >>"$ART_DIR/timings.txt"
  set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}'; wait_peers $A1 n1-a2 40 || p="$p; a2 not back after restore"
  [ -z "$p" ] && record M06 PASS "netmap $MODE: node delete and policy block reach the peer within ${bound}s (timings.txt)" || record M06 FAIL "${p#; }"
}
