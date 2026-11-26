# shellcheck shell=bash
# N1 E2E batch 3 (sourced by run.sh; uses its helpers and variables).
# Cases: S05 one machine key in two tailnets of two clusters (client
# profiles), S06 the same hostname in A and B, S09 tags bounded by the
# tailnet (tenant API v2 + a tagged real client), S13 a light flood of tenant
# A through the public path via the VPS while tenant B is measured.

VPS=${VPS:-root@116.118.3.46}
VPS_KEY=${VPS_KEY:-$HOME/.mesh-lab/mesh-vps}
VSSHO=(-i "$VPS_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10
  -o UserKnownHostsFile="$HOME/.mesh-lab/known_hosts" -o StrictHostKeyChecking=yes)
VPS_RD=/tmp/n1s13-$RUN_ID
vps() { ssh "${VSSHO[@]}" "$VPS" "$@"; }

# api_v2 SLUG METHOD PATH [JSON]: tenant API v2 on c1's public URL with the
# tailnet's API key from $TMPD/api-SLUG (sent on stdin, never on a command
# line); prints "<status> <body>".
api_v2() {
  local s=$1 m=$2 path=$3 d=()
  [ -n "${4:-}" ] && d=(--data "$4")
  py 'print("Authorization: Bearer " + open(sys.argv[1]).read().strip())' "$TMPD/api-$s" | strip_cr \
    | curl -s -m 10 -X "$m" -H @- -H 'Content-Type: application/json' "${d[@]}" \
        -w '\n%{http_code}' "$URL1$path" 2>/dev/null \
    | py 'd = sys.stdin.read().rsplit("\n", 1); print(d[-1].strip(), d[0] if len(d) > 1 else "")' | strip_cr
}

# S05: a2 (node of A at c1) logs into tailnet C at c2 as a second client
# profile: the same tailscaled, the same machine key, two nodes in two
# tailnets of two clusters. Each profile sees only its own tailnet; switching
# back restores A.
t_s05() {
  local p="" r i other n nudge=""
  printf 'S05 login start %s\n' "$(date -u +%T)" >>"$ART_DIR/timings.txt"
  r=0; vm $A2 "sudo timeout 45 /opt/tailscale/$V/tailscale --socket=$TSS login --login-server=$URL2 --auth-key=file:/dev/stdin --hostname=n1-a2c --accept-dns=false" \
    <"$TMPD/key-c" 2>&1 | redact >>"$ART_DIR/logs/n1-a2c.cli.log"; r=${PIPESTATUS[0]}
  # Client 1.84.3 keeps the home DERP region of the previous profile (901,
  # c1's relay) when the new profile's DERP map has only 902 (ADR 0009: one
  # region per cluster): "derpMap.Regions[901] is nil", it never reaches
  # Running and login waits (runs at 3ad5743, 5fd66e0). A re-STUN picks a
  # home region from the new map. Recorded in the result, never silent.
  if [ "$(field $A2 "d.get('BackendState', '')")" != Running ]; then
    nudge=" (client needed 'debug restun' after login rc=$r: stale home DERP from the previous profile)"
    printf 'S05 %s %s: not Running after login rc=%s; debug restun\n' "$MODE" "$V" "$r" >>"$ART_DIR/timings.txt"
    TS $A2 "debug restun" >/dev/null 2>&1
    for i in $(seq 1 30); do [ "$(field $A2 "d.get('BackendState', '')")" = Running ] && break; sleep 1; done
    [ "$(field $A2 "d.get('BackendState', '')")" = Running ] || p="$p; profile c not Running after login rc=$r and restun"
  fi
  for i in $(seq 1 40); do [ "$(field $A2 "d['Self']['HostName']")" = n1-a2c ] && [ -n "$(node_id c2 c n1-a2c)" ] && break; sleep 1; done
  [ -n "$(node_id c2 c n1-a2c)" ] || p="$p; no node n1-a2c in c"
  grep -qE 'n1-(a1|b1|b2)' <<<"$(ts_json $A2)" && p="$p; profile c sees an A or B node"
  grep -qw n1-a2c <<<"$(peers $A1)$(peers $B1)" && p="$p; A or B sees n1-a2c"
  [ -z "$(node_id c1 a n1-a2c)" ] || p="$p; n1-a2c created in A"
  # Server side: one machine key, two nodes, two tailnets (counts only).
  n=$(pg_ "sudo docker exec mc-pg psql -At -U postgres -d $PG_DB -c \"SELECT count(*) || ' ' || count(DISTINCT tailnet_id) FROM nodes WHERE machine_key = (SELECT machine_key FROM nodes WHERE hostname = 'n1-a2c' LIMIT 1) AND hostname IN ('n1-a2', 'n1-a2c')\"" 2>/dev/null | strip_cr)
  [ "$n" = "2 2" ] || p="$p; same machine key: [$n] (want 2 nodes in 2 tailnets)"
  # Back to the A profile.
  TS $A2 "switch --list" >"$TMPD/profiles" 2>/dev/null
  redact <"$TMPD/profiles" >"$ART_DIR/logs/S05-profiles.txt"
  other=$(py 'import re
rows = [l.split() for l in open(sys.argv[1]).read().splitlines()[1:] if l.strip()]
print(next((r[0] for r in rows if not any(c.endswith("*") for c in r)), ""))' "$TMPD/profiles" | strip_cr)
  [ -n "$other" ] && TS $A2 "switch $other" >/dev/null 2>&1 || p="$p; switch back [$other]"
  for i in $(seq 1 40); do [ "$(field $A2 "d['Self']['HostName']")" = n1-a2 ] && ping_ok $A1 n1-a2 && break; sleep 1; done
  ping_ok $A1 n1-a2 || p="$p; a1 cannot ping a2 after switch back"
  [ "$(peers $A2)" = n1-a1 ] || p="$p; a2 back in A sees [$(peers $A2)]"
  live c2 node delete --slug c --id "$(node_id c2 c n1-a2c)" >>"$ART_DIR/logs/s05.log" 2>&1 || p="$p; delete n1-a2c"
  [ -z "$p" ] && record S05 PASS "one tailscaled (one machine key) as n1-a2 in A at c1 and n1-a2c in C at c2: two nodes in two tailnets, each profile sees only its own; switch back to A, a1<->a2 ping$nudge" || record S05 FAIL "${p#; }"
}

# S06: B gets a node named n1-a1, as A's. Neither is renamed; the name
# resolves to the tailnet's own node on each side.
t_s06() {
  local p="" r i out
  ts_aside $B2 >/dev/null 2>&1 || p="$p; park b2"
  r=0; ts_up $B2 "$URL1" "$TMPD/key-b" n1-a1 || r=$?; [ $r = 0 ] || p="$p; b2 as n1-a1 rc=$r"
  for i in $(seq 1 40); do grep -qw n1-a1 <<<"$(peers $B1)" && break; sleep 1; done
  grep -qw n1-a1 <<<"$(peers $B1)" || p="$p; b1 does not see B's n1-a1"
  case "$(field $B2 "d['Self']['DNSName']")" in n1-a1.b.*) ;; *) p="$p; B's node named [$(field $B2 "d['Self']['DNSName']")]";; esac
  case "$(field $A1 "d['Self']['DNSName']")" in n1-a1.a.*) ;; *) p="$p; A's n1-a1 named [$(field $A1 "d['Self']['DNSName']")]";; esac
  out=$(TS $B1 "ping -c 1 --timeout 5s n1-a1" 2>&1); printf 'b1 -> n1-a1: %s\n' "$out" >>"$ART_DIR/logs/S06.txt"
  grep -qF "($(selfip $B2))" <<<"$out" || p="$p; b1's n1-a1 is not B's node"
  out=$(TS $A2 "ping -c 1 --timeout 5s n1-a1" 2>&1); printf 'a2 -> n1-a1: %s\n' "$out" >>"$ART_DIR/logs/S06.txt"
  grep -qF "($(selfip $A1))" <<<"$out" || p="$p; a2's n1-a1 is not A's node"
  [ "$(peers $A2)" = n1-a1 ] || p="$p; a2 sees [$(peers $A2)]"
  live c1 node delete --slug b --id "$(node_id c1 b n1-a1)" >>"$ART_DIR/logs/s06.log" 2>&1 || p="$p; delete B's n1-a1"
  ts_back $B2 >/dev/null 2>&1 || p="$p; b2 back"
  wait_peers $B1 n1-b2 40 || p="$p; b1 peers [$(peers $B1)]"
  [ -z "$p" ] && record S06 PASS "hostname n1-a1 in A and B: no rename (n1-a1.a.*, n1-a1.b.*), the name resolves to each tailnet's own node (same subnet route and overlapping IPs: S08, S02)" || record S06 FAIL "${p#; }"
}

# S09: A defines tag:srv (owner n1-a); B does not. A's API key mints a tagged
# key and a real client joins A as tag:srv; B's API key cannot mint tag:srv;
# A's API key cannot address B; B never sees the tag.
t_s09() {
  local p="" s r rb i ida idb
  set_policy a '{"tagOwners":{"tag:srv":["n1-a@"]},"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}' || p="$p; policy a"
  for s in a b; do
    cli c1 "tailnet apikey create --slug $s --user n1-$s --expiration 1h" >"$TMPD/apikey-$s.json" || p="$p; apikey $s"
    py 'import json; d = json.load(open(sys.argv[1])); print(d["key"])' "$TMPD/apikey-$s.json" | strip_cr >"$TMPD/api-$s"
  done
  ida=$(py 'import json; print(json.load(open(sys.argv[1])).get("id", ""))' "$TMPD/apikey-a.json" | strip_cr)
  idb=$(py 'import json; print(json.load(open(sys.argv[1])).get("id", ""))' "$TMPD/apikey-b.json" | strip_cr)
  rm -f "$TMPD"/apikey-?.json
  local body='{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":false,"preauthorized":true,"tags":["tag:srv"]}}},"expirySeconds":1800}'
  r=$(api_v2 a POST /api/v2/tailnet/-/keys "$body")
  [ "${r%% *}" = 200 ] || p="$p; A tagged key: ${r%% *}"
  py 'import json; print(json.loads(sys.argv[1].split(" ", 1)[1])["key"])' "$r" 2>/dev/null | strip_cr >"$TMPD/key-srv"
  r=$(api_v2 b POST /api/v2/tailnet/-/keys "$body"); rb=${r%% *}
  printf 'B mints tag:srv: %s\n' "$(redact <<<"$r" | head -c 300)" >>"$ART_DIR/logs/S09.txt"
  case "$rb" in 4??) ;; *) p="$p; B minted tag:srv ($rb)";; esac
  grep -q 'hskey-auth' <<<"$r" && p="$p; B got a key for tag:srv"
  r=$(api_v2 a GET "/api/v2/tailnet/$(cat "$TMPD/id-b")/devices")
  case "${r%% *}" in 403|404) ;; *) p="$p; A's API key on B's tailnet: ${r%% *}";; esac
  # A real client joins A with the tagged key.
  ts_aside $A2 >/dev/null 2>&1 || p="$p; park a2"
  r=0; [ -s "$TMPD/key-srv" ] && { ts_up $A2 "$URL1" "$TMPD/key-srv" n1-asrv || r=$?; } || r=99
  [ $r = 0 ] || p="$p; a2 as tag:srv rc=$r"
  for i in $(seq 1 40); do grep -qw n1-asrv <<<"$(peers $A1)" && break; sleep 1; done
  field $A1 "[t for q in (d.get('Peer') or {}).values() if q['HostName'] == 'n1-asrv' for t in (q.get('Tags') or [])]" | grep -q 'tag:srv' \
    || p="$p; a1 does not see n1-asrv tagged"
  live c1 node list --slug a 2>/dev/null | grep -q 'tag:srv' || p="$p; A's node list has no tag:srv"
  grep -qE 'tag:srv|n1-asrv' <<<"$(ts_json $B1)$(netmap $B1)" && p="$p; B sees A's tag or node"
  live c1 node list --slug b 2>/dev/null | grep -q 'tag:srv' && p="$p; a tag:srv node in B"
  live c1 node delete --slug a --id "$(node_id c1 a n1-asrv)" >>"$ART_DIR/logs/s09.log" 2>&1 || p="$p; delete n1-asrv"
  ts_back $A2 >/dev/null 2>&1 || p="$p; a2 back"
  set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}' || p="$p; restore a"
  wait_peers $A1 n1-a2 40 || p="$p; a1 peers [$(peers $A1)]"
  cli c1 "tailnet apikey revoke --slug a --id $ida" >/dev/null 2>&1; cli c1 "tailnet apikey revoke --slug b --id $idb" >/dev/null 2>&1
  rm -f "$TMPD"/api-? "$TMPD/key-srv"
  [ -z "$p" ] && record S09 PASS "A's tag:srv (owner n1-a): A's API key mints a tagged key, a real client joins A tagged; B's API key cannot mint tag:srv ($rb); A's key cannot address B; B never sees the tag" || record S09 FAIL "${p#; }"
}

# S13 (owner Q4: "qua VPS, tải nhẹ"): from the VPS through c1's public URL,
# tenant A floods enroll (3/s) and map streams (1/s, <= 40) for 40 s. During
# the flood tenant B enrols a node, changes its policy and pings; bounds: B's
# enrolment seen by its peer <= 20 s, policy change <= 10 s, ping works.
S13_RATE_ENROLL=${S13_RATE_ENROLL:-3}
S13_RATE_MAP=${S13_RATE_MAP:-1}
S13_SECONDS=${S13_SECONDS:-40}
t_s13() {
  local p="" i t0 t1 enroll_ms pol_ms fpid
  scp -q "${VSSHO[@]}" "$PROBE_BIN" "$VPS:$VPS_RD-probe" || { record S13 FAIL "copy n1probe to the VPS"; return; }
  vps "umask 077; cat >$VPS_RD-key" <"$TMPD/key-a" || { record S13 FAIL "key to the VPS"; return; }
  vps "chmod 0755 $VPS_RD-probe
    $VPS_RD-probe flood --kind enroll --url $URL1 --key $VPS_RD-key --rate $S13_RATE_ENROLL --duration ${S13_SECONDS}s --max-inflight 16 >$VPS_RD-enroll.json 2>$VPS_RD-enroll.err &
    $VPS_RD-probe flood --kind mapstream --url $URL1 --key $VPS_RD-key --rate $S13_RATE_MAP --duration ${S13_SECONDS}s --max-inflight 40 >$VPS_RD-map.json 2>$VPS_RD-map.err &
    wait; cat $VPS_RD-enroll.json $VPS_RD-map.json; tail -3 $VPS_RD-enroll.err $VPS_RD-map.err >&2; rm -f $VPS_RD-*" \
    >"$ART_DIR/logs/S13-flood.jsonl" 2> >(redact >"$ART_DIR/logs/S13-flood.err") &
  fpid=$!
  sleep 8
  # B enrols a fresh node (b2's old node was deleted in S18) during the flood.
  ts_start $B2 >/dev/null 2>&1 || p="$p; b2 start"
  t0=$(now_ms); ts_up $B2 "$URL1" "$TMPD/key-b" n1-b2 || p="$p; b2 enrol"
  while [ $(( $(now_ms) - t0 )) -lt 20000 ]; do grep -qw n1-b2 <<<"$(peers $B1)" && break; sleep 0.3; done
  t1=$(now_ms); enroll_ms=$((t1 - t0))
  grep -qw n1-b2 <<<"$(peers $B1)" || p="$p; b1 did not see the new b2 in 20 s"
  ping_ok $B1 n1-b2 || p="$p; b1 cannot ping b2 during the flood"
  t0=$(now_ms); set_policy b '{"grants":[]}' || p="$p; policy b"
  while [ $(( $(now_ms) - t0 )) -lt 10000 ]; do [ -z "$(peers $B1)" ] && break; sleep 0.3; done
  t1=$(now_ms); pol_ms=$((t1 - t0)); [ -z "$(peers $B1)" ] || p="$p; B's policy change not applied in 10 s"
  set_policy b '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; restore b"
  wait_peers $B1 n1-b2 30 || p="$p; b1 not back [$(peers $B1)]"
  wait "$fpid" || p="$p; flood ssh rc=$?"
  vps "rm -f $VPS_RD-*; ! ls $VPS_RD-* 2>/dev/null" >/dev/null || p="$p; VPS files left"
  local sum; sum=$(py 'import json
out = []
for l in open(sys.argv[1]):
    l = l.strip()
    if not l.startswith("{"): continue
    d = json.loads(l)
    out.append("%s started=%d 2xx=%d 4xx=%d 429=%d 5xx=%d err=%d p50=%.0fms p99=%.0fms" % (d["kind"], d["started"], d["status_2xx"], d["status_4xx"], d["status_429"], d["status_5xx"], d["errors"], d["p50_ms"], d["p99_ms"]))
print("; ".join(out))' "$ART_DIR/logs/S13-flood.jsonl" | strip_cr)
  grep -q 'enroll started=[1-9]' <<<"$sum" && grep -q 'mapstream started=[1-9]' <<<"$sum" || p="$p; flood did not run [$sum]"
  printf 'S13 %s flood: %s; B enrol->seen %d ms, B policy->applied %d ms\n' "$MODE" "$sum" "$enroll_ms" "$pol_ms" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record S13 PASS "A flood via VPS ($sum): B enrol seen in $enroll_ms ms, B policy in $pol_ms ms, b1<->b2 ping" || record S13 FAIL "${p#; }"
}

# drop_nodes CLUSTER SLUG PREFIX: delete the nodes whose hostname starts with
# PREFIX (n1probe registers machines of its own in A and B; left in place
# they are offline peers that every later peer comparison would see).
drop_nodes() {
  local id
  for id in $(live "$1" node list --slug "$2" 2>/dev/null | py 'import json
d = json.load(sys.stdin)
for n in (d.get("nodes") if isinstance(d, dict) else d) or []:
    if n.get("hostname", "").startswith(sys.argv[1]): print(n["id"])' "$3" | strip_cr); do
    live "$1" node delete --slug "$2" --id "$id" >>"$ART_DIR/logs/drop-nodes.log" 2>&1 || return 1
  done
}

# S04: a new tailnet is created PENDING with {"grants":[]} in the same
# transaction. Before ACTIVE no client can enrol; once ACTIVE its default
# policy denies (two real clients in it see no peer), {} denies too, and an
# explicit grant connects them (positive control).
t_s04() {
  local p="" r i k
  cli c1 "tailnet create --slug d" | py 'import json; print(json.load(sys.stdin)["id"])' | strip_cr >"$TMPD/id-d" || p="$p; create d"
  k=$(cli c1 "tailnet authkey --slug d --user n1-d --reusable --expiration 30m" 2>>"$ART_DIR/logs/s04.log")
  if [ -n "$k" ]; then
    py 'import json; print(json.loads(sys.argv[1])["key"])' "$k" | strip_cr >"$TMPD/key-d"
    ts_aside $B2 >/dev/null 2>&1 || p="$p; park b2"
    r=0; ts_up $B2 "$URL1" "$TMPD/key-d" n1-d0 || r=$?; [ $r != 0 ] || p="$p; enrolled into a PENDING tailnet"
    [ -z "$(node_id c1 d n1-d0)" ] || p="$p; node in a PENDING tailnet"
    ts_back $B2 >/dev/null 2>&1
  else
    echo "authkey for PENDING d refused (expected or acceptable)" >>"$ART_DIR/logs/s04.log"
  fi
  cli c1 "tailnet status --slug d --set ACTIVE" >/dev/null || p="$p; activate d"
  k=$(cli c1 "tailnet authkey --slug d --user n1-d --reusable --expiration 30m") || p="$p; authkey d"
  py 'import json; print(json.loads(sys.argv[1])["key"])' "$k" | strip_cr >"$TMPD/key-d"
  ts_aside $A2 >/dev/null 2>&1 || p="$p; park a2"
  ts_aside $B2 >/dev/null 2>&1 || p="$p; park b2"
  ts_up $A2 "$URL1" "$TMPD/key-d" n1-d1 || p="$p; d1 up"
  ts_up $B2 "$URL1" "$TMPD/key-d" n1-d2 || p="$p; d2 up"
  for i in $(seq 1 20); do [ -n "$(node_id c1 d n1-d1)" ] && [ -n "$(node_id c1 d n1-d2)" ] && break; sleep 1; done
  sleep 8
  [ -z "$(peers $A2)$(peers $B2)" ] || p="$p; default policy of a new tailnet lets d1/d2 see [$(peers $A2)|$(peers $B2)]"
  set_policy d '{}' || p="$p; policy {} refused"
  sleep 8
  [ -z "$(peers $A2)$(peers $B2)" ] || p="$p; policy {} lets d1/d2 see [$(peers $A2)|$(peers $B2)]"
  set_policy d '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}' || p="$p; policy allow d"
  wait_peers $A2 n1-d2 40 || p="$p; positive control: d1 sees [$(peers $A2)]"
  grep -qE 'n1-(a1|b1)' <<<"$(peers $A2)$(peers $B2)" && p="$p; d sees A or B"
  ts_back $A2 >/dev/null 2>&1 || p="$p; a2 back"
  ts_back $B2 >/dev/null 2>&1 || p="$p; b2 back"
  wait_peers $A1 n1-a2 40 || p="$p; a1 peers [$(peers $A1)]"
  rm -f "$TMPD/key-d"
  [ -z "$p" ] && record S04 PASS "new tailnet d: no enrolment while PENDING; once ACTIVE two real clients see no peer under the default policy and under {}; an explicit grant connects them" || record S04 FAIL "${p#; }"
}

# S13b (owner: "chạy luôn bootstrap flood cho S13, tải nhẹ qua VPS"): the
# bootstrap (P11) runs on mc-cp against this run's database and is reached
# through the VPS (boot.cp.gta5web.online:8444 -> wg -> mc-cp). From the VPS
# tenant A floods /v1/bootstrap at S13B_RATE/s (above the default per-tenant
# limit of 20/s, below the global 200/s); meanwhile B asks at 2/s from mc-ci
# through the same public path. Pass: B gets every answer (no 429/5xx/error,
# p99 <= 2 s) and still gets c1's URL; A's own limit is what stops A.
BOOT_HOST=boot.cp.gta5web.online
BOOT_URL=https://$BOOT_HOST:8444
BOOT_LISTEN=10.99.77.2:8444
S13B_RATE=${S13B_RATE:-30}
S13B_SECONDS=${S13B_SECONDS:-40}
boot_start() {
  local i
  scp -q "${SSHO[@]}" "$BOOT_BIN" "$CP_SSH:/tmp/mesh-bootstrap" || return 1
  cp_ "set -e; sudo install -m 0755 /tmp/mesh-bootstrap /usr/local/bin/mesh-bootstrap; rm -f /tmp/mesh-bootstrap
    sudo systemctl stop mesh-bootstrap-e2e 2>/dev/null || true; sudo systemctl reset-failed mesh-bootstrap-e2e 2>/dev/null || true
    sudo systemd-run --quiet --unit=mesh-bootstrap-e2e --uid=headscale --gid=headscale \
      /usr/local/bin/mesh-bootstrap -config $CFG -listen $BOOT_LISTEN \
      -tls-cert /etc/headscale/tls/fullchain.pem -tls-key /etc/headscale/tls/privkey.pem -hostname $BOOT_HOST" || return 1
  for i in $(seq 1 20); do [ "$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BOOT_URL/health")" = 200 ] && return 0; sleep 1; done
  return 1
}
boot_stop() {
  cp_ "sudo journalctl -u mesh-bootstrap-e2e --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/bootstrap.log"
  cp_ "sudo systemctl stop mesh-bootstrap-e2e 2>/dev/null; sudo systemctl reset-failed mesh-bootstrap-e2e 2>/dev/null; sudo rm -f /usr/local/bin/mesh-bootstrap; ! test -e /usr/local/bin/mesh-bootstrap"
}
# boot_url_of KEYFILE TENANT: the control_url bootstrap answers (proof only;
# the secret never leaves this host).
boot_url_of() {
  py 'import hashlib, json
k = open(sys.argv[1]).read().strip()
i, s = k.split("hskey-auth-", 1)[1].split("-", 1)
print(json.dumps({"credential": {"kind": "authkey_proof", "key_id": i, "proof": hashlib.sha256(s.encode()).hexdigest()}, "tenant": sys.argv[2]}))' "$1" "$2" | strip_cr \
    | curl -s -m 10 -H 'Content-Type: application/json' --data-binary @- "$BOOT_URL/v1/bootstrap" \
    | py 'import json; print(json.load(sys.stdin).get("control_url", ""))' 2>/dev/null | strip_cr
}
flood_sum() { # FILE: one line per flood JSON in FILE
  py 'import json
for l in open(sys.argv[1]):
    l = l.strip()
    if not l.startswith("{"): continue
    d = json.loads(l)
    print("started=%d 2xx=%d 4xx=%d 429=%d 5xx=%d err=%d p50=%.0fms p99=%.0fms" % (d["started"], d["status_2xx"], d["status_4xx"], d["status_429"], d["status_5xx"], d["errors"], d["p50_ms"], d["p99_ms"]))' "$1" | strip_cr
}
t_s13b() {
  local p="" fpid sa sb u
  [ -n "${BOOT_BIN:-}" ] || { record S13b FAIL "BOOT_BIN not set: bootstrap flood not run"; return; }
  boot_start || { record S13b FAIL "bootstrap did not start (logs/bootstrap.log)"; boot_stop; return; }
  u=$(boot_url_of "$TMPD/key-b" b); [ "$u" = "$URL1" ] || p="$p; before: bootstrap for b -> [$u]"
  local e="$ART_DIR/logs/S13b-copy.err"
  scp -q "${VSSHO[@]}" "$PROBE_BIN" "$VPS:$VPS_RD-probe" 2>>"$e" || { record S13b FAIL "copy probe to the VPS (logs/S13b-copy.err)"; boot_stop; return; }
  vps "umask 077; cat >$VPS_RD-key" <"$TMPD/key-a" 2>>"$e" || { record S13b FAIL "key of a to the VPS (logs/S13b-copy.err)"; boot_stop; return; }
  ssh_ $CI_SSH "umask 077; cat >/tmp/n1-s13b-kb" <"$TMPD/key-b" 2>>"$e" || { record S13b FAIL "key of b to mc-ci (logs/S13b-copy.err)"; boot_stop; return; }
  ssh_ $CI_SSH "test -x /tmp/n1probe" 2>>"$e" || { record S13b FAIL "no /tmp/n1probe on mc-ci"; boot_stop; return; }
  vps "chmod 0755 $VPS_RD-probe; $VPS_RD-probe flood --kind bootstrap --bootstrap-url $BOOT_URL --tenant a --key $VPS_RD-key \
      --rate $S13B_RATE --duration ${S13B_SECONDS}s --max-inflight 16; rm -f $VPS_RD-*" \
    >"$ART_DIR/logs/S13b-flood-a.jsonl" 2> >(redact >"$ART_DIR/logs/S13b-flood-a.err") &
  fpid=$!
  sleep 5
  ssh_ $CI_SSH "/tmp/n1probe flood --kind bootstrap --bootstrap-url $BOOT_URL --tenant b --key /tmp/n1-s13b-kb --rate 2 --duration 30s --max-inflight 4; rm -f /tmp/n1-s13b-kb" \
    >"$ART_DIR/logs/S13b-b.jsonl" 2> >(redact >"$ART_DIR/logs/S13b-b.err")
  wait "$fpid" || p="$p; A flood rc=$?"
  vps "rm -f $VPS_RD-*; ! ls $VPS_RD-* 2>/dev/null" >/dev/null || p="$p; VPS files left"
  u=$(boot_url_of "$TMPD/key-b" b); [ "$u" = "$URL1" ] || p="$p; after: bootstrap for b -> [$u]"
  sa=$(flood_sum "$ART_DIR/logs/S13b-flood-a.jsonl"); sb=$(flood_sum "$ART_DIR/logs/S13b-b.jsonl")
  grep -q 'started=[1-9]' <<<"$sa" || p="$p; A flood did not run [$sa]"
  py 'import json, sys
d = [json.loads(l) for l in open(sys.argv[1]) if l.startswith("{")][0]
ok = d["started"] > 0 and d["status_2xx"] == d["started"] and d["status_429"] == 0 and d["status_5xx"] == 0 and d["errors"] == 0 and d["p99_ms"] <= 2000
sys.exit(0 if ok else 1)' "$ART_DIR/logs/S13b-b.jsonl" 2>/dev/null || p="$p; B during A's flood: [$sb]"
  boot_stop || p="$p; bootstrap not stopped"
  printf 'S13b %s bootstrap flood via VPS: A %s; B %s\n' "$MODE" "$sa" "$sb" >>"$ART_DIR/timings.txt"
  [ -z "$p" ] && record S13b PASS "A floods bootstrap via VPS at ${S13B_RATE}/s ($sa): B every answer ($sb), B still -> c1" || record S13b FAIL "${p#; }"
}
