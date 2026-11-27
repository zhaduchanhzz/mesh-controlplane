# shellcheck shell=bash
# N1 E2E batch 7 (sourced by run.sh; uses its helpers and variables).
# Case: M06x, P14f F6 (ADR 0015 M06 extended). With the incremental netmap
# ON and reconcile_interval = full_map_interval = 1h (cfg-n1.py default,
# N1_RECONCILE_INTERVAL / N1_FULL_MAP_INTERVAL), a change can only reach a
# client through the delta path. Every change below is made through tenant
# A's own API key; the time from the API call returning to the change being
# visible in a1's `tailscale status --json` is measured, M06X_ROUNDS rounds
# per direction, each round <= M06X_BOUND seconds:
#   M06x.a  tag change that removes visibility (tag:m06x-on -> tag:m06x-off)
#           and back (addition)
#   M06x.b  route approval / unapproval (POST /device/{id}/routes): the prefix
#           appears in / leaves the peer's PrimaryRoutes/AllowedIPs at a1
#   M06x.c  setACL (If-Match) that removes the pair's grants, and back
#   M06x.d  device delete through the API, and a new enrolment (join)
# Isolation, every scenario: B's clients see exactly their own peer, never an
# M06x node; B's key on A's device gets 404.
# The extra node is a fresh identity on a2's tailscaled (state parked with
# ts_aside, restored with ts_back), so a2 itself is never tagged or deleted.

M06X_ROUNDS=${M06X_ROUNDS:-5}
M06X_BOUND=${M06X_BOUND:-10}
M06X_ROUTE=10.99.88.0/24
M06X_P1='{
  // M06x base: n1-a@ <-> tag:m06x-on, and n1-a@ -> the M06x route
  "tagOwners": {"tag:m06x-on": ["n1-a@"], "tag:m06x-off": ["n1-a@"]},
  "grants": [
    {"src": ["n1-a@"], "dst": ["n1-a@", "tag:m06x-on"], "ip": ["*"]},
    {"src": ["tag:m06x-on"], "dst": ["n1-a@"], "ip": ["*"]},
    {"src": ["n1-a@"], "dst": ["10.99.88.0/24"], "ip": ["*"]},
  ],
}'
# P2: no grant between n1-a@ and tag:m06x-on (the pair is blocked).
M06X_P2='{
  // M06x block: the n1-a@ <-> tag:m06x-on grants removed
  "tagOwners": {"tag:m06x-on": ["n1-a@"], "tag:m06x-off": ["n1-a@"]},
  "grants": [
    {"src": ["n1-a@"], "dst": ["n1-a@"], "ip": ["*"]},
    {"src": ["n1-a@"], "dst": ["10.99.88.0/24"], "ip": ["*"]},
  ],
}'
M06X_ORIG="" M06X_DEV=""

# shellcheck disable=SC2034 # read by s08c_api through bash dynamic scope
m06x_api() { local S08C_APILOG=M06x-api.log; s08c_api "$@"; }
m06x_sees() { grep -qw "$2" <<<"$(peers "$1")"; }
m06x_not_sees() { ! m06x_sees "$1" "$2"; }
# m06x_route_at CLIENT HOST PREFIX: the peer HOST carries PREFIX at CLIENT.
m06x_route_at() {
  [ "$(field "$1" "any('$3' in ((p.get('PrimaryRoutes') or []) + (p.get('AllowedIPs') or [])) for p in (d.get('Peer') or {}).values() if p.get('HostName') == '$2')")" = True ]
}
m06x_no_route_at() { ! m06x_route_at "$@"; }
# m06x_wait PREDICATE...: ms from now until PREDICATE holds, or -1 after
# M06X_BOUND seconds (each poll includes an ssh round trip to the client).
m06x_wait() {
  local t0 t; t0=$(now_ms)
  while :; do
    if "$@"; then t=$(now_ms); echo $((t - t0)); return 0; fi
    t=$(now_ms); [ $((t - t0)) -ge $((M06X_BOUND * 1000)) ] && { echo -1; return 1; }
    sleep 0.2
  done
}
# m06x_stats MS...: "p50=… max=…" (-1 = not within the bound).
m06x_stats() { py 'v = sorted(int(x) for x in sys.argv[1:]); print("p50=%d max=%d" % (v[(len(v) - 1) // 2], v[-1]) if v and min(v) >= 0 else "MISSED " + " ".join(sys.argv[1:]))' "$@" | strip_cr; }
m06x_time() { printf 'M06x %s %s %s round %s: %s ms\n' "$MODE" "$V" "$1" "$2" "$3" >>"$ART_DIR/timings.txt"; }
# m06x_isolated: B's clients unaffected, no M06x node in B's view.
m06x_isolated() {
  [ "$(peers "$B1")" = n1-b2 ] || p="$p; b1 peers [$(peers "$B1")]"
  [ "$(peers "$B2")" = n1-b1 ] || p="$p; b2 peers [$(peers "$B2")]"
}
# m06x_setacl JSON: write A's policy through A's key with a fresh ETag.
m06x_setacl() {
  local r e
  r=$(m06x_api a GET /api/v2/tailnet/-/acl "" "" "Accept: application/hujson"); e=$(s08c_etag)
  [ "${r%% *}" = 200 ] && [ -n "$e" ] || { echo "GET acl -> ${r%% *}"; return 1; }
  r=$(m06x_api a POST /api/v2/tailnet/-/acl "$1" application/hujson "If-Match: $e")
  [ "${r%% *}" = 200 ] || { echo "POST acl -> ${r%% *}"; return 1; }
}
m06x_tag() { local r; r=$(m06x_api a POST "/api/v2/device/$M06X_DEV/tags" "{\"tags\":[\"$1\"]}"); [ "${r%% *}" = 200 ]; }
m06x_routes() { local r; r=$(m06x_api a POST "/api/v2/device/$M06X_DEV/routes" "{\"routes\":[$1]}"); [ "${r%% *}" = 200 ]; }

m06x_setup() {
  local s r
  # The peer sets before M06x: undo must bring them back. In the full run
  # other cases may have left nodes of the same tailnet (S13: A's n1flood-*).
  M06X_BASE_A1=$(peers "$A1"); M06X_BASE_B1=$(peers "$B1")
  grep -qE '(^| )(n1-a|n1flood-)' <<<"$M06X_BASE_B1" && p="$p; b1 sees nodes of A before M06x [$M06X_BASE_B1]"
  for s in a b; do
    cli c1 "tailnet apikey create --slug $s --user n1-$s --expiration 1h" >"$TMPD/m06x-key-$s.json" || p="$p; apikey $s"
    py 'import json; d = json.load(open(sys.argv[1])); print(d["key"])' "$TMPD/m06x-key-$s.json" | strip_cr >"$TMPD/api-$s"
  done
  r=$(m06x_api a GET /api/v2/tailnet/-/acl "" "" "Accept: application/hujson")
  [ "${r%% *}" = 200 ] && M06X_ORIG=${r#* } || p="$p; GET acl -> ${r%% *}"
  r=$(m06x_setacl "$M06X_P1") || p="$p; base policy: $r"
  ts_aside "$A2" >/dev/null 2>&1 || p="$p; a2 state not parked"
  ts_up "$A2" "$URL1" "$TMPD/key-a" n1-a2x >/dev/null 2>&1 || p="$p; n1-a2x up"
  s08c_until 40 m06x_sees "$A1" n1-a2x || p="$p; a1 does not see n1-a2x [$(peers "$A1")]"
  M06X_DEV=$(s08c_dev a n1-a2x); [ -n "$M06X_DEV" ] || p="$p; n1-a2x not in A's device list"
  m06x_tag tag:m06x-on || p="$p; tag n1-a2x tag:m06x-on"
  s08c_until 20 m06x_sees "$A1" n1-a2x || p="$p; a1 lost n1-a2x after tag:m06x-on"
  r=$(api_v2 b POST "/api/v2/device/$M06X_DEV/tags" '{"tags":["tag:m06x-off"]}'); [ "${r%% *}" = 404 ] || p="$p; B's key tags A's device -> ${r%% *}"
}

# M06x.a: tag:m06x-on -> tag:m06x-off removes the pair; back adds it.
m06x_tags() {
  local i ms off=() on=()
  for i in $(seq 1 "$M06X_ROUNDS"); do
    m06x_tag tag:m06x-off || { p="$p; tag off round $i"; break; }
    ms=$(m06x_wait m06x_not_sees "$A1" n1-a2x); off+=("$ms"); m06x_time tag-off "$i" "$ms"
    m06x_tag tag:m06x-on || { p="$p; tag on round $i"; break; }
    ms=$(m06x_wait m06x_sees "$A1" n1-a2x); on+=("$ms"); m06x_time tag-on "$i" "$ms"
  done
  M06X_A="removal $(m06x_stats "${off[@]}"), addition $(m06x_stats "${on[@]}")"
  [[ $M06X_A == *MISSED* ]] && p="$p; tag change not within ${M06X_BOUND}s: $M06X_A"
  m06x_isolated
}

# M06x.b: route approval adds the prefix to the peer at a1, unapproval removes it.
m06x_route() {
  local i ms add=() del=()
  TS "$A2" "set --advertise-routes=$M06X_ROUTE" >/dev/null 2>&1 || p="$p; advertise $M06X_ROUTE"
  sleep 2
  m06x_no_route_at "$A1" n1-a2x "$M06X_ROUTE" || p="$p; route visible before approval"
  for i in $(seq 1 "$M06X_ROUNDS"); do
    m06x_routes "\"$M06X_ROUTE\"" || { p="$p; approve round $i"; break; }
    ms=$(m06x_wait m06x_route_at "$A1" n1-a2x "$M06X_ROUTE"); add+=("$ms"); m06x_time route-approve "$i" "$ms"
    m06x_routes "" || { p="$p; unapprove round $i"; break; }
    ms=$(m06x_wait m06x_no_route_at "$A1" n1-a2x "$M06X_ROUTE"); del+=("$ms"); m06x_time route-unapprove "$i" "$ms"
  done
  TS "$A2" "set --advertise-routes=" >/dev/null 2>&1
  M06X_B="unapproval $(m06x_stats "${del[@]}"), approval $(m06x_stats "${add[@]}")"
  [[ $M06X_B == *MISSED* ]] && p="$p; route change not within ${M06X_BOUND}s: $M06X_B"
  m06x_route_at "$B1" n1-a2x "$M06X_ROUTE" && p="$p; b1 sees A's route"
  m06x_isolated
}

# M06x.c: setACL without the pair's grants removes it; the base policy adds it back.
m06x_acl() {
  local i ms r blk=() alw=()
  for i in $(seq 1 "$M06X_ROUNDS"); do
    r=$(m06x_setacl "$M06X_P2") || { p="$p; block round $i: $r"; break; }
    ms=$(m06x_wait m06x_not_sees "$A1" n1-a2x); blk+=("$ms"); m06x_time acl-block "$i" "$ms"
    r=$(m06x_setacl "$M06X_P1") || { p="$p; allow round $i: $r"; break; }
    ms=$(m06x_wait m06x_sees "$A1" n1-a2x); alw+=("$ms"); m06x_time acl-allow "$i" "$ms"
  done
  M06X_C="block $(m06x_stats "${blk[@]}"), allow $(m06x_stats "${alw[@]}")"
  [[ $M06X_C == *MISSED* ]] && p="$p; setACL not within ${M06X_BOUND}s: $M06X_C"
  m06x_isolated
}

# M06x.d: device delete through A's API removes the node at a1; a new
# enrolment (fresh state on a2's tailscaled) appears at a1.
m06x_delete() {
  local i ms r host=n1-a2x del=() add=()
  for i in $(seq 1 "$M06X_ROUNDS"); do
    r=$(m06x_api a DELETE "/api/v2/device/$M06X_DEV"); [ "${r%% *}" = 200 ] || { p="$p; delete $host round $i -> ${r%% *}"; break; }
    ms=$(m06x_wait m06x_not_sees "$A1" "$host"); del+=("$ms"); m06x_time delete "$i" "$ms"
    host=n1-a2d$i
    ts_start "$A2" >/dev/null 2>&1 || { p="$p; fresh tailscaled round $i"; break; }
    ts_up "$A2" "$URL1" "$TMPD/key-a" "$host" >/dev/null 2>&1 || { p="$p; $host up"; break; }
    ms=$(m06x_wait m06x_sees "$A1" "$host"); add+=("$ms"); m06x_time join "$i" "$ms"
    M06X_DEV=$(s08c_dev a "$host"); [ -n "$M06X_DEV" ] || { p="$p; $host not in A's device list"; break; }
  done
  M06X_D="delete $(m06x_stats "${del[@]}"), join (enrolment included) $(m06x_stats "${add[@]}")"
  [[ $M06X_D == *MISSED* ]] && p="$p; delete/join not within ${M06X_BOUND}s: $M06X_D"
  m06x_isolated
}

m06x_undo() {
  local s i r
  if [ -n "$M06X_DEV" ]; then r=$(m06x_api a DELETE "/api/v2/device/$M06X_DEV"); [ "${r%% *}" = 200 ] || p="$p; undo: delete device -> ${r%% *}"; fi
  TS "$A2" "set --advertise-routes=" >/dev/null 2>&1
  ts_back "$A2" >/dev/null 2>&1 || p="$p; a2 state not restored"
  if [ -n "$M06X_ORIG" ]; then r=$(m06x_setacl "$M06X_ORIG") || p="$p; original policy not restored: $r"
  else set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}' || p="$p; policy a not restored"; fi
  for s in a b; do
    i=$(py 'import json; print(json.load(open(sys.argv[1])).get("id", ""))' "$TMPD/m06x-key-$s.json" 2>/dev/null | strip_cr)
    [ -n "$i" ] && cli c1 "tailnet apikey revoke --slug $s --id $i" >/dev/null 2>&1 || p="$p; API key $s not revoked"
  done
  rm -f "$TMPD"/m06x-key-?.json "$TMPD"/api-? "$TMPD/s08c-hdr"
  wait_peers "$A1" "${M06X_BASE_A1:-n1-a2}" 40 || p="$p; a1 peers after M06x [$(peers "$A1")], before [${M06X_BASE_A1:-n1-a2}]"
  wait_peers "$B1" "${M06X_BASE_B1:-n1-b2}" 40 || p="$p; b1 peers after M06x [$(peers "$B1")], before [${M06X_BASE_B1:-n1-b2}]"
  grep -qE '(^| )(n1-a|n1flood-)' <<<"$(peers "$B1")" && p="$p; b1 sees nodes of A after M06x"
  M06X_ORIG="" M06X_DEV=""
}

# m06x_fail ID: record the failures added since $ps (caller's p/ps).
m06x_fail() { local f=${p#"$ps"}; record "$1" FAIL "${f#; }"; }

t_m06x() {
  local p="" ps
  M06X_A="" M06X_B="" M06X_C="" M06X_D=""
  m06x_setup
  if [ -z "$p" ]; then
    ps=$p; m06x_tags;   [ "$p" = "$ps" ] && record M06x.a PASS "netmap $MODE ($V): tag change via A's API, ${M06X_ROUNDS} rounds <= ${M06X_BOUND}s: $M06X_A" || m06x_fail M06x.a
    ps=$p; m06x_route;  [ "$p" = "$ps" ] && record M06x.b PASS "netmap $MODE ($V): route approval via A's API: $M06X_B; b1 never sees the route" || m06x_fail M06x.b
    ps=$p; m06x_acl;    [ "$p" = "$ps" ] && record M06x.c PASS "netmap $MODE ($V): setACL (If-Match) via A's API: $M06X_C" || m06x_fail M06x.c
    ps=$p; m06x_delete; [ "$p" = "$ps" ] && record M06x.d PASS "netmap $MODE ($V): device delete via A's API / new enrolment: $M06X_D" || m06x_fail M06x.d
  else
    record M06x.a FAIL "setup: ${p#; }"
  fi
  p=""; m06x_undo
  [ -z "$p" ] && record M06x.0 PASS "setup and undo: extra node deleted, a2 restored, A's policy restored (If-Match), API keys revoked; B unaffected and refused (404) on A's device" \
    || record M06x.0 FAIL "${p#; }"
}
