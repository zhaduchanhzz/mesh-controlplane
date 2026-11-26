# shellcheck shell=bash
# N1 E2E batch 5, sourced by run.sh (uses its helpers and those of cases2-4).
#
# S08c (P12d §2): Tailscale-equivalent subnet routers driven ONLY by the
# tenant API (/api/v2 with the tailnet's API key; docs/tenant-api/
# subnet-routers.md). The operator CLI only issues and revokes the two API
# keys (a tenant cannot create API keys) and is the last-resort fallback of
# the policy restore.
#
# Topology on a2 (tailnet A): bridge c77br, no address of the VM on it.
#   netns c77lan  LAN host 192.168.77.2 + .53 (DNS), fd77:77::2; NO default
#                 route (the SNAT-off case needs an explicit return route);
#                 TCP 7777 answers "LAN-A src=<address it saw>", UDP 53 on
#                 .53 answers svc.corp.example (A .2, AAAA fd77:77::2, TXT
#                 "LAN-A"), NXDOMAIN otherwise.
#   netns c77r1   router n1-ar1: LAN 192.168.77.1 / fd77:77::1, uplink
#                 10.177.77.2/30 NATed out of a2 (nft table ip n1s08c),
#                 tailscaled $V (own state, socket, tun c77ts1, port 41643,
#                 netfilter on inside its netns).
#   netns c77r2   router n1-ar2: LAN .3 / ::3, uplink 10.177.77.6/30, tun
#                 c77ts2, port 41644. Same LAN, same prefixes: HA pair.
# b2 (tailnet B): the same bridge/LAN host (no IPv6), b2's own tailscaled
# routes it (bridge address 192.168.77.1); TCP and DNS answer "LAN-B".
#
#   S08c.1 policy with a grant to the subnet, autoApprovers for tag:router
#          and tests, through GET/POST .../acl (HuJSON, ETag/If-Match:
#          validate, stale If-Match 412, failing test 400, nothing changes);
#          a tagged auth key minted by the API; n1-ar1 joins with it and
#          advertises -> routes enabled with no approval call; a1 ping +
#          TCP banner; keyExpiryDisabled through POST /device/{id}/key.
#   S08c.3 IPv6: fd77:77::2 behind n1-ar1 (ping -6 + TCP banner).
#   S08c.2 HA: n1-ar2 joins and is auto-approved; the primary is SIGKILLed
#          while a1 pings every 200 ms; the route moves to the other
#          router without intervention; gap and failover time recorded,
#          the LAN host sees the other router's SNAT address.
#   S08c.4 --snat-subnet-routes=false on the serving router: no answer
#          without a return route; with 100.64.0.0/10 via the router, the
#          LAN host sees a1's tailnet IP; SNAT restored.
#   S08c.5 split DNS corp.example -> 192.168.77.53 through PATCH/GET
#          .../dns/split-dns; a domain under A's MagicDNS domain -> 400; a1
#          (accept-dns on for this case) resolves svc.corp.example through
#          quad-100 and the OS resolver, and reaches it by name.
#   S08c.6 B: same prefix behind b2, same split domain; before B acts, b1
#          has no route, no answer, no A name; after B approves its route
#          and sets its split DNS with its key: LAN-B only, a1 LAN-A only;
#          B's key on A's tailnet/devices (policy, DNS, routes, key, delete)
#          and A's key on B's tailnet -> 404, A's policy/DNS/devices intact.
#   S08c.0 setup and undo: routers, namespaces, bridge, nft, ip_forward,
#          accept-routes/accept-dns, split DNS, policy, router devices,
#          tagged auth key and API keys all removed or restored.

S08C_V4=192.168.77.0/24
S08C_V6=fd77:77::/64
S08C_HOST=192.168.77.2
S08C_HOST6=fd77:77::2
S08C_DNS=192.168.77.53
S08C_DOM=corp.example
S08C_NAME=svc.corp.example
S08C_INDOM=corp.a.c1.tailnet.internal # under A's MagicDNS domain: refused
S08C_GAP_MAX_MS=${S08C_GAP_MAX_MS:-20000} # 15 s reconnect grace (poll.go) + margin
S08C_POLICY_A='{
  // S08c: tenant A, subnet routers (written through /api/v2 with If-Match)
  "tagOwners": {"tag:router": ["n1-a@"]},
  "grants": [
    {"src": ["*"], "dst": ["100.64.0.0/10", "fd7a:115c:a1e0::/48"], "ip": ["*"]},
    {"src": ["n1-a@"], "dst": ["192.168.77.0/24", "fd77:77::/64"], "ip": ["*"]},
  ],
  "autoApprovers": {"routes": {"192.168.77.0/24": ["tag:router"], "fd77:77::/64": ["tag:router"]}},
  "tests": [{"src": "n1-a@", "accept": ["192.168.77.2:7777"]}],
}'
# Its test fails (no grant to the subnet): 400, nothing stored.
S08C_POLICY_BAD='{"grants":[{"src":["*"],"dst":["100.64.0.0/10"],"ip":["*"]}],"tests":[{"src":"n1-a@","accept":["192.168.77.2:7777"]}]}'
S08C_KEYBODY='{"capabilities":{"devices":{"create":{"reusable":true,"ephemeral":false,"preauthorized":true,"tags":["tag:router"]}}},"expirySeconds":1800}'
S08C_ORIG="" S08C_KID="" S08C_R1ID="" S08C_R2ID="" S08C_B2ID="" S08C_SETUP="" S08C_POLICY_TOUCHED=""

# ------------------------------------------------------------- tenant API ---
# s08c_api SLUG METHOD PATH [BODY [CONTENT_TYPE [HEADER...]]]: like api_v2
# (key on stdin, never on a command line) with a body, a content type and
# extra headers; response headers in $TMPD/s08c-hdr; prints "<status> <body>";
# every call is logged redacted to logs/S08c-api.log (or logs/$S08C_APILOG).
s08c_api() {
  local s=$1 m=$2 path=$3 body=${4:-} ct=${5:-application/json} d=() out
  shift $(($# < 5 ? $# : 5))
  [ -n "$body" ] && d=(--data-binary "$body")
  rm -f "$TMPD/s08c-hdr"
  out=$(py 'print("Authorization: Bearer " + open(sys.argv[1]).read().strip())
for h in sys.argv[2:]: print(h)' "$TMPD/api-$s" "$@" | strip_cr \
    | curl -s -m 10 -X "$m" -H @- -H "Content-Type: $ct" "${d[@]}" -D "$TMPD/s08c-hdr" \
      -w '\n%{http_code}' "$URL1$path" 2>/dev/null \
    | py 'd = sys.stdin.read().rsplit("\n", 1); print(d[-1].strip(), d[0] if len(d) > 1 else "")' | strip_cr)
  printf '%s %s %s %s -> %s\n' "$(date -u +%T)" "$s" "$m" "$path" "$(redact <<<"$out" | tr '\n' ' ' | head -c 240)" >>"$ART_DIR/logs/${S08C_APILOG:-S08c-api.log}"
  printf '%s\n' "$out"
}
s08c_etag() { tr -d '\r' <"$TMPD/s08c-hdr" 2>/dev/null | awk 'tolower($1) == "etag:" {v = $2} END {print v}'; }
# s08c_json_is "<status> <body>" JSON: status 200 and the body equals JSON.
s08c_json_is() {
  py 'import json
st, _, body = sys.argv[1].partition(" ")
try:
    ok = st == "200" and json.loads(body) == json.loads(sys.argv[2])
except ValueError:
    ok = False
print("yes" if ok else "no")' "$1" "$2" | strip_cr | grep -q yes
}
# s08c_devfield SLUG HOSTNAME FIELD: the field (JSON) of that device in the
# tailnet's device list, through the tenant API.
s08c_devfield() {
  s08c_api "$1" GET /api/v2/tailnet/-/devices | py 'import json
st, _, body = sys.stdin.read().partition(" ")
for d in (json.loads(body).get("devices") or []) if st == "200" else []:
    if d.get("hostname") == sys.argv[1] or d.get("name", "").split(".")[0] == sys.argv[1]:
        print(json.dumps(d.get(sys.argv[2]))); break' "$2" "$3" | strip_cr
}
s08c_dev() { s08c_devfield "$1" "$2" id | tr -d '"'; }
# s08c_enabled SLUG ID -> the device's enabled routes, sorted, comma-separated
s08c_enabled() {
  s08c_api "$1" GET "/api/v2/device/$2/routes" | py 'import json
st, _, body = sys.stdin.read().partition(" ")
print(",".join(sorted((json.loads(body).get("enabledRoutes") or []) if st == "200" else [])))' | strip_cr
}
s08c_both_enabled() { [ "$(s08c_enabled "$1" "$2")" = "$S08C_V4,$S08C_V6" ]; }

# ---------------------------------------------------------------- the VMs ---
# s08c_vm_undo IP: remove every S08c object from the VM (idempotent; also
# called by run.sh's cleanup); true when nothing is left.
s08c_vm_undo() {
  vm "$1" "sudo bash -s" <<'EOF'
for u in n1-s08c-r1 n1-s08c-r2 n1-s08c-lan n1-s08c-ping; do systemctl stop $u 2>/dev/null; systemctl reset-failed $u 2>/dev/null; done
for n in c77r1 c77r2 c77lan; do ip netns del $n 2>/dev/null; rm -rf /etc/netns/$n; done
rmdir /etc/netns 2>/dev/null
for l in c77br c77u1 c77u2 c77lb c77l1b c77l2b; do ip link del $l 2>/dev/null; done
nft delete table ip n1s08c 2>/dev/null
[ -e /run/n1-s08c.fwd ] && sysctl -qw net.ipv4.ip_forward="$(cat /run/n1-s08c.fwd)"
rm -rf /run/n1-s08c.fwd /run/n1-s08c-lan.py /run/n1-s08c-ping.log /var/lib/n1-s08c-r1 /var/lib/n1-s08c-r2
! ip netns list | grep -q c77 && ! ip link show c77br >/dev/null 2>&1 && ! nft list table ip n1s08c >/dev/null 2>&1 \
  && ! systemctl is-active -q n1-s08c-r1 n1-s08c-r2 n1-s08c-lan n1-s08c-ping
EOF
}

# The LAN host: TCP banner (dual stack) and a one-name DNS server on .53.
s08c_lan_py() {
  vm "$1" "sudo tee /run/n1-s08c-lan.py >/dev/null" <<'PY'
import socket, struct, sys, threading
TAG, V6 = sys.argv[1], sys.argv[2] == "1"
NAME, A4, A6, DNSIP = "svc.corp.example", "192.168.77.2", "fd77:77::2", "192.168.77.53"


def qname(q, off):
    labels = []
    while q[off]:
        n = q[off]
        labels.append(q[off + 1:off + 1 + n].decode("ascii", "replace").lower())
        off += 1 + n
    return ".".join(labels), off + 1


def answer(q):
    qid, flags = struct.unpack(">HH", q[:4])
    name, off = qname(q, 12)
    qtype = struct.unpack(">H", q[off:off + 2])[0]
    rd = None
    if name == NAME and qtype == 1:
        rd = socket.inet_aton(A4)
    elif name == NAME and qtype == 28 and V6:
        rd = socket.inet_pton(socket.AF_INET6, A6)
    elif name == NAME and qtype == 16:
        t = ("LAN-" + TAG).encode()
        rd = bytes([len(t)]) + t
    rr = b"" if rd is None else b"\xc0\x0c" + struct.pack(">HHIH", qtype, 1, 30, len(rd)) + rd
    rcode = 0 if name == NAME else 3
    hdr = struct.pack(">HHHHHH", qid, 0x8480 | (flags & 0x0100) | rcode, 1, 1 if rr else 0, 0, 0)
    return hdr + q[12:off + 4] + rr, name, qtype


def dns():
    u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    u.bind((DNSIP, 53))
    while True:
        q, a = u.recvfrom(4096)
        try:
            r, name, qtype = answer(q)
            u.sendto(r, a)
            print("dns %s type %d from %s" % (name, qtype, a[0]), flush=True)
        except Exception as e:
            print("dns error from %s: %s" % (a[0], e), flush=True)


threading.Thread(target=dns, daemon=True).start()
s = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
s.bind(("::", 7777))
s.listen(16)
while True:
    c, a = s.accept()
    src = a[0][7:] if a[0].startswith("::ffff:") else a[0]
    print("tcp from %s" % src, flush=True)
    try:
        c.sendall(("LAN-%s src=%s\n" % (TAG, src)).encode())
    finally:
        c.close()
PY
}

# s08c_lan_up IP TAG HOSTROUTER: bridge c77br + LAN host netns c77lan.
# HOSTROUTER=1 (b2): the VM's own tailscaled routes, the bridge holds
# 192.168.77.1, no IPv6. HOSTROUTER=0 (a2): routers live in their own
# netns (s08c_router_start); the VM only bridges and NATs their uplinks.
s08c_lan_up() {
  local v6=1; [ "$3" = 1 ] && v6=0
  s08c_vm_undo "$1" >/dev/null 2>&1
  s08c_lan_py "$1" || return 1
  vm "$1" "sudo bash -s" <<EOF
set -e
ip link add c77br type bridge mcast_snooping 0
sysctl -qw net.ipv6.conf.c77br.disable_ipv6=1
ip link set c77br up
ip netns add c77lan
ip link add c77lh type veth peer name c77lb
ip link set c77lh netns c77lan
ip link set c77lb master c77br up
ip -n c77lan link set lo up
ip -n c77lan addr add $S08C_HOST/24 dev c77lh
ip -n c77lan addr add $S08C_DNS/24 dev c77lh
if [ $v6 = 1 ]; then ip -n c77lan -6 addr add $S08C_HOST6/64 dev c77lh nodad; fi
ip -n c77lan link set c77lh up
if [ $3 = 1 ]; then ip addr add 192.168.77.1/24 dev c77br; fi
if [ $3 = 0 ]; then printf '%s\n' 'table ip n1s08c {' '  chain post {' '    type nat hook postrouting priority srcnat; policy accept;' \
  '    ip saddr 10.177.77.0/29 ip daddr != 10.177.77.0/29 masquerade' '  }' '}' | nft -f -; fi
[ -e /run/n1-s08c.fwd ] || cat /proc/sys/net/ipv4/ip_forward >/run/n1-s08c.fwd
sysctl -qw net.ipv4.ip_forward=1
systemd-run --quiet --collect --unit=n1-s08c-lan -p RuntimeMaxSec=1800 ip netns exec c77lan python3 -u /run/n1-s08c-lan.py $2 $v6
set +e
for i in \$(seq 1 20); do ip netns exec c77lan ss -ltnH 'sport = :7777' | grep -q 7777 && exit 0; sleep 0.25; done
exit 1
EOF
}

# s08c_router_start N: netns c77rN on a2 (LAN 192.168.77.(2N-1), uplink
# 10.177.77.(4N-2)/30 via the VM), tailscaled $V in it.
s08c_router_start() {
  local n=$1 l=$((2 * $1 - 1)) u=$((4 * $1 - 3))
  vm "$A2" "sudo bash -s" <<EOF
set -e
ns=c77r$n
ip netns add \$ns
ip link add c77l$n type veth peer name c77l${n}b
ip link set c77l$n netns \$ns
ip link set c77l${n}b master c77br up
ip link add c77u$n type veth peer name c77u${n}n
ip link set c77u${n}n netns \$ns
ip addr add 10.177.77.$u/30 dev c77u$n
ip link set c77u$n up
ip -n \$ns link set lo up
ip -n \$ns addr add 192.168.77.$l/24 dev c77l$n
ip -n \$ns -6 addr add fd77:77::$l/64 dev c77l$n nodad
ip -n \$ns link set c77l$n up
ip -n \$ns addr add 10.177.77.$((u + 1))/30 dev c77u${n}n
ip -n \$ns link set c77u${n}n up
ip -n \$ns route add default via 10.177.77.$u
ip netns exec \$ns sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1
mkdir -p /etc/netns/\$ns
cp /run/systemd/resolve/resolv.conf /etc/netns/\$ns/resolv.conf
rm -rf /var/lib/n1-s08c-r$n; install -d -m 0700 /var/lib/n1-s08c-r$n
systemctl reset-failed n1-s08c-r$n 2>/dev/null || true
systemd-run --quiet --unit=n1-s08c-r$n -p RuntimeDirectory=n1-s08c-r$n -p RuntimeMaxSec=1800 \
  ip netns exec \$ns /opt/tailscale/$V/tailscaled --state=/var/lib/n1-s08c-r$n/tailscaled.state \
  --socket=/run/n1-s08c-r$n/ts.sock --tun=c77ts$n --port=$((41642 + n))
ip netns exec \$ns ping -c 1 -W 2 -q $S08C_HOST >/dev/null
set +e
for i in \$(seq 1 40); do /opt/tailscale/$V/tailscale --socket=/run/n1-s08c-r$n/ts.sock status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.25; done
exit 1
EOF
}
s08c_rts() { local n=$1; shift; vm "$A2" "sudo /opt/tailscale/$V/tailscale --socket=/run/n1-s08c-r$n/ts.sock $*"; }
s08c_router_up() { # N: enrol router N in A with the tagged key, as n1-arN
  vm "$A2" "sudo timeout 60 /opt/tailscale/$V/tailscale --socket=/run/n1-s08c-r$1/ts.sock up --login-server=$URL1 --auth-key=file:/dev/stdin --hostname=n1-ar$1 --accept-dns=false" \
    <"$TMPD/key-router" 2>&1 | redact >>"$ART_DIR/logs/n1-ar$1.cli.log"
  return "${PIPESTATUS[0]}"
}
s08c_router_stop() { # N: journal to the artifacts, then the router and its netns go
  vm "$A2" "sudo journalctl -u n1-s08c-r$1 --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/S08c-r$1-tailscaled.log"
  vm "$A2" "sudo systemctl stop n1-s08c-r$1 2>/dev/null; sudo systemctl reset-failed n1-s08c-r$1 2>/dev/null; sudo ip netns del c77r$1 2>/dev/null; \
    sudo ip link del c77u$1 2>/dev/null; sudo rm -rf /etc/netns/c77r$1 /var/lib/n1-s08c-r$1; ! ip link show c77u$1 >/dev/null 2>&1"
}

# ---------------------------------------------------------------- clients ---
# s08c_ping CLIENT DEST -> "<received>/<sent> <avg rtt ms>"; the raw output
# goes to logs/S08c-ping.log. Status 0 whatever ping did: test the OUTPUT
# ([[ $(s08c_ping ..) == 0/* ]]), never "s08c_ping | grep" (pipefail: a ping
# without replies made such a check fail, run 20261126T132519Z).
s08c_ping() {
  local out; out=$(vm "$1" "ping -c 3 -W 2 $2 2>&1" | strip_cr)
  printf '== %s %s -> %s\n%s\n' "$(date -u +%T)" "$1" "$2" "$out" >>"$ART_DIR/logs/S08c-ping.log"
  awk -F'[ /,]+' '/packets transmitted/ {r=$4; t=$1} /rtt/ {a=$8} END {printf "%s/%s %s", r+0, t+0, (a == "" ? "-" : a)}' <<<"$out"
}
s08c_ping_ok() { [[ $(s08c_ping "$1" "$2") == 3/3* ]]; }
# s08c_banner CLIENT HOST -> "LAN-<T> src=<address the LAN host saw>", or "- <error>"
s08c_banner() {
  vm "$1" "python3 -c 'import socket
try:
    c = socket.create_connection((\"$2\", 7777), timeout=4); print(c.recv(200).decode().strip())
except Exception as e:
    print(\"-\", type(e).__name__)'" 2>/dev/null | strip_cr
}
s08c_banner_is() { [ "$(s08c_banner "$1" "$2")" = "$3" ]; }
# s08c_dig CLIENT TYPE: svc.corp.example through the client's quad-100 -> answer, unquoted
s08c_dig() { vm "$1" "dig +short +time=2 +tries=1 @100.100.100.100 $S08C_NAME $2" 2>/dev/null | tr -d '\r"' | grep -v '^;' | head -1; }
s08c_dig_is() { [ "$(s08c_dig "$1" "$2")" = "$3" ]; }
# s08c_getent CLIENT: the first IPv4 address of svc.corp.example through the OS resolver
s08c_getent() { vm "$1" "getent ahostsv4 $S08C_NAME" 2>/dev/null | awk 'NR == 1 {print $1}' | strip_cr; }
# s08c_primary CLIENT PREFIX -> host name of the peer serving PREFIX, or "-"
s08c_primary() { field "$1" "next((q['HostName'] for q in (d.get('Peer') or {}).values() if '$2' in (q.get('PrimaryRoutes') or [])), '-')"; }
s08c_primary_is() { [ "$(s08c_primary "$1" "$2")" = "$3" ]; }
s08c_pn() { local h; h=$(s08c_primary "$A1" "$S08C_V4"); case $h in n1-ar1) echo 1;; n1-ar2) echo 2;; esac; }
s08c_peer_seen() { grep -qw "$2" <<<"$(peers "$1")"; }
# s08c_until SECONDS CMD...: CMD every second until it succeeds
s08c_until() { local n=$1 i; shift; for i in $(seq 1 "$n"); do "$@" && return 0; sleep 1; done; return 1; }
s08c_t() { date +%s%3N; }

# s08c_diag LABEL CLIENT DEST: state snapshot to logs/S08c-diag.log (the
# client's route to DEST, rules, table 52, prefs, peers' PrimaryRoutes and
# AllowedIPs; addresses, routes, netns of a2 and b2 and of the router netns).
s08c_diag() {
  {
    printf '=== %s %s: client %s, destination %s\n' "$(date -u +%T)" "$1" "$2" "$3"
    vm "$2" "echo '--- ip route get'; ip route get $3 2>&1; echo '--- ip rule'; ip rule; echo '--- table 52'; ip route show table 52; ip -6 route show table 52
      echo '--- prefs'; sudo /opt/tailscale/$V/tailscale --socket=$TSS debug prefs 2>&1 | grep -E '\"(RouteAll|CorpDNS|AdvertiseRoutes|NoSNAT|WantRunning)\"'" 2>&1
    echo '--- peers (HostName Online PrimaryRoutes AllowedIPs)'
    ts_json "$2" | py 'import json
try:
    d = json.load(sys.stdin)
except ValueError:
    print("(no status: tailscaled not answering)"); sys.exit()
for q in (d.get("Peer") or {}).values():
    print(q.get("HostName"), q.get("Online"), q.get("PrimaryRoutes"), q.get("AllowedIPs"))' 2>&1
    for h in "$A2:a2" "$B2:b2"; do
      echo "--- ${h##*:}: addresses, routes, forwarding, netns"
      vm "${h%%:*}" "ip -br addr; ip route; ip -6 route | grep -v '^fe80'; sysctl net.ipv4.ip_forward; ip netns list; for n in \$(ip netns list | awk '{print \$1}'); do echo \"[\$n]\"; sudo ip -n \$n -br addr; sudo ip -n \$n route; done" 2>&1
    done
  } | strip_cr | redact >>"$ART_DIR/logs/S08c-diag.log"
}
# s08c_done ID PASS_TEXT [CLIENT DEST]: record from the caller's p; on FAIL a
# diagnostic snapshot first.
s08c_done() {
  p=${p#; }
  if [ -z "$p" ]; then record "$1" PASS "$2"; return; fi
  s08c_diag "$1" "${3:-$A1}" "${4:-$S08C_HOST}"
  printf '%s FAIL: %s (snapshot: logs/S08c-diag.log)\n' "$1" "$p" >>"$ART_DIR/timings.txt"
  record "$1" FAIL "$p"
}

# ------------------------------------------------------------------ cases ---
s08c_setup() {
  local p="" s r
  for s in a b; do
    cli c1 "tailnet apikey create --slug $s --user n1-$s --expiration 1h" >"$TMPD/s08c-key-$s.json" || p="$p; apikey $s"
    py 'import json; d = json.load(open(sys.argv[1])); print(d["key"])' "$TMPD/s08c-key-$s.json" | strip_cr >"$TMPD/api-$s"
  done
  s08c_lan_up "$A2" A 0 || p="$p; LAN behind a2"
  s08c_lan_up "$B2" B 1 || p="$p; LAN behind b2"
  s08c_banner_is "$B2" $S08C_HOST "LAN-B src=192.168.77.1" || p="$p; b2 cannot reach its own LAN host"
  r=$(s08c_ping "$A1" $S08C_HOST); [[ $r == 0/* ]] || p="$p; a1 reaches $S08C_HOST before any router [$r]"
  S08C_SETUP=${p#; }
  [ -z "$p" ] || s08c_diag setup "$A1" $S08C_HOST
  [ -z "$p" ]
}

# Helpers named s08c_<step> below append their failures to the CALLER's p
# (bash dynamic scope) and set the S08C_* globals: never call them in $(...).

# S08c.1: policy through the API (ETag/If-Match, validate, tests), tagged
# key through the API, autoApprovers approve the router's routes.
s08c_policy() {
  local r e0 e1
  r=$(s08c_api a GET /api/v2/tailnet/-/acl "" "" "Accept: application/hujson")
  [ "${r%% *}" = 200 ] && S08C_ORIG=${r#* } || p="$p; GET acl -> ${r%% *}"
  e0=$(s08c_etag); [ -n "$e0" ] || p="$p; GET acl without ETag"
  r=$(s08c_api a POST /api/v2/tailnet/-/acl/validate "$S08C_POLICY_A" application/hujson)
  [ "${r%% *}" = 200 ] && ! grep -q '"message"' <<<"$r" || p="$p; validate(policy) [${r:0:160}]"
  r=$(s08c_api a POST /api/v2/tailnet/-/acl/validate "$S08C_POLICY_BAD" application/hujson)
  [ "${r%% *}" = 200 ] && grep -q '"message"' <<<"$r" || p="$p; validate(failing test) [${r:0:160}]"
  S08C_POLICY_TOUCHED=1
  r=$(s08c_api a POST /api/v2/tailnet/-/acl "$S08C_POLICY_A" application/hujson 'If-Match: "0000000000000000"')
  [ "${r%% *}" = 412 ] || p="$p; stale If-Match -> ${r%% *}"
  r=$(s08c_api a POST /api/v2/tailnet/-/acl "$S08C_POLICY_BAD" application/hujson "If-Match: $e0")
  [ "${r%% *}" = 400 ] || p="$p; failing test -> ${r%% *}"
  s08c_api a GET /api/v2/tailnet/-/acl >/dev/null
  [ "$(s08c_etag)" = "$e0" ] || p="$p; a refused write changed the policy"
  r=$(s08c_api a POST /api/v2/tailnet/-/acl "$S08C_POLICY_A" application/hujson "If-Match: $e0")
  [ "${r%% *}" = 200 ] || p="$p; POST policy -> ${r%% *} [${r:4:160}]"
  e1=$(s08c_etag); [ -n "$e1" ] && [ "$e1" != "$e0" ] || p="$p; ETag after the write [$e0 -> $e1]"
  r=$(s08c_api a GET /api/v2/tailnet/-/acl "" "" "Accept: application/hujson")
  grep -q 'S08c: tenant A' <<<"$r" || p="$p; stored HuJSON lost its comment"
  [ "$(s08c_etag)" = "$e1" ] || p="$p; GET ETag differs from the write's"
  r=$(s08c_api a POST /api/v2/tailnet/-/keys "$S08C_KEYBODY")
  [ "${r%% *}" = 200 ] || p="$p; tagged auth key -> ${r%% *}"
  py 'import json; print(json.loads(sys.argv[1].split(" ", 1)[1])["key"])' "$r" 2>/dev/null | strip_cr >"$TMPD/key-router"
  S08C_KID=$(py 'import json; print(json.loads(sys.argv[1].split(" ", 1)[1])["id"])' "$r" 2>/dev/null | strip_cr)
  chmod 600 "$TMPD/key-router"
}
t_s08c_1() {
  local p="" r b t0 t1 t2 t3
  s08c_policy
  TS "$A1" "set --accept-routes" >/dev/null 2>&1 || p="$p; a1 accept-routes"
  s08c_router_start 1 || p="$p; router 1 netns/tailscaled"
  s08c_router_up 1 || p="$p; n1-ar1 up with the tagged key"
  t0=$(s08c_t)
  s08c_rts 1 "set --advertise-routes=$S08C_V4,$S08C_V6" >/dev/null 2>&1 || p="$p; n1-ar1 advertise"
  S08C_R1ID=$(s08c_dev a n1-ar1)
  [ -n "$S08C_R1ID" ] || p="$p; n1-ar1 not in A's device list"
  s08c_until 30 s08c_both_enabled a "$S08C_R1ID" && t1=$(s08c_t) || { t1=-1; p="$p; routes not auto-approved [$(s08c_enabled a "$S08C_R1ID")]"; }
  [ "$(s08c_devfield a n1-ar1 tags)" = '["tag:router"]' ] || p="$p; n1-ar1 tags [$(s08c_devfield a n1-ar1 tags)]"
  s08c_until 30 s08c_primary_is "$A1" $S08C_V4 n1-ar1 && t2=$(s08c_t) || { t2=-1; p="$p; a1 does not see n1-ar1's route"; }
  s08c_until 20 s08c_ping_ok "$A1" $S08C_HOST && t3=$(s08c_t) || { t3=-1; p="$p; a1 ping $S08C_HOST [$(s08c_ping "$A1" $S08C_HOST)]"; }
  b=$(s08c_banner "$A1" $S08C_HOST); [ "$b" = "LAN-A src=192.168.77.1" ] || p="$p; a1 TCP to $S08C_HOST [$b]"
  r=$(s08c_api a POST "/api/v2/device/$S08C_R1ID/key" '{"keyExpiryDisabled":true}')
  [ "${r%% *}" = 200 ] || p="$p; keyExpiryDisabled -> ${r%% *}"
  [ "$(s08c_devfield a n1-ar1 keyExpiryDisabled)" = true ] || p="$p; keyExpiryDisabled not set"
  printf 'S08c.1 advertise -> routes enabled by autoApprovers (API) %s ms, -> a1 primary %s ms, -> first 3/3 ping %s ms; TCP [%s]\n' \
    "$((t1 < 0 ? -1 : t1 - t0))" "$((t2 < 0 ? -1 : t2 - t0))" "$((t3 < 0 ? -1 : t3 - t0))" "$b" >>"$ART_DIR/timings.txt"
  s08c_done S08c.1 "policy written by A's key (HuJSON, If-Match; validate ok/refused; stale ETag 412; failing test 400 and nothing stored); tagged key by API; n1-ar1 routes $S08C_V4,$S08C_V6 approved by autoApprovers (no approval call); a1 ping 3/3 + TCP LAN-A; keyExpiryDisabled by API"
}

t_s08c_3() {
  local p="" r b
  s08c_until 20 s08c_primary_is "$A1" $S08C_V6 n1-ar1 || p="$p; a1 does not see $S08C_V6 [$(s08c_primary "$A1" $S08C_V6)]"
  s08c_until 20 s08c_ping_ok "$A1" $S08C_HOST6 || p="$p; a1 ping $S08C_HOST6"
  r=$(s08c_ping "$A1" $S08C_HOST6)
  b=$(s08c_banner "$A1" $S08C_HOST6); [[ $b == LAN-A\ src=* ]] || p="$p; a1 TCP to $S08C_HOST6 [$b]"
  printf 'S08c.3 a1 -> %s: ping %s; TCP [%s]\n' "$S08C_HOST6" "$r" "$b" >>"$ART_DIR/timings.txt"
  s08c_done S08c.3 "IPv6 subnet $S08C_V6 behind n1-ar1: a1 ping 3/3 and TCP [$b]" "$A1" $S08C_HOST6
}

# s08c_gap LOG KILL_MS -> "<max gap ms> <replies before> <replies after> <end of the max gap - kill ms>"
# (a reply can land between the kill timestamp and SIGKILL: "first reply after
# the kill" said 1 ms in run 20261126T140829Z; the end of the gap is when
# traffic came back).
s08c_gap() {
  py 'import re
ts = [float(m.group(1)) * 1000 for m in (re.match(r"\[(\d+\.\d+)\] \d+ bytes from", l) for l in open(sys.argv[1])) if m]
k = float(sys.argv[2])
after = [t for t in ts if t > k]
i = max(range(1, len(ts)), key=lambda j: ts[j] - ts[j - 1], default=0)
gap = ts[i] - ts[i - 1] if i else -1
print("%d %d %d %d" % (gap, len(ts) - len(after), len(after), (ts[i] - k) if i else -1))' "$1" "$2" | strip_cr
}
s08c_ha_ready() { # second router joined, auto-approved, seen by a1
  s08c_router_start 2 || p="$p; router 2 netns/tailscaled"
  s08c_router_up 2 || p="$p; n1-ar2 up with the tagged key"
  s08c_rts 2 "set --advertise-routes=$S08C_V4,$S08C_V6" >/dev/null 2>&1 || p="$p; n1-ar2 advertise"
  S08C_R2ID=$(s08c_dev a n1-ar2)
  s08c_until 30 s08c_both_enabled a "$S08C_R2ID" || p="$p; n1-ar2 routes not auto-approved [$(s08c_enabled a "$S08C_R2ID")]"
  s08c_until 30 s08c_peer_seen "$A1" n1-ar2 || p="$p; a1 does not see n1-ar2"
}
t_s08c_2() {
  local p="" pn sn ka tk ts g b0 b1 r
  s08c_ha_ready
  pn=$(s08c_pn)
  if [ -z "$pn" ]; then p="$p; no primary for $S08C_V4 at a1 before the kill"; s08c_done S08c.2 -; return; fi
  sn=$((3 - pn))
  b0=$(s08c_banner "$A1" $S08C_HOST); [ "$b0" = "LAN-A src=192.168.77.$((2 * pn - 1))" ] || p="$p; before: TCP [$b0]"
  vm "$A1" "sudo systemctl stop n1-s08c-ping 2>/dev/null; sudo systemctl reset-failed n1-s08c-ping 2>/dev/null; sudo rm -f /run/n1-s08c-ping.log
    sudo systemd-run --quiet --collect --unit=n1-s08c-ping -p RuntimeMaxSec=150 sh -c 'exec ping -D -n -i 0.2 -W 1 -w 120 $S08C_HOST >/run/n1-s08c-ping.log 2>&1'" || p="$p; pinger"
  sleep 3
  tk=$(s08c_t)
  ka=$(vm "$A2" "date +%s%3N; sudo systemctl kill -s KILL n1-s08c-r$pn" | head -1 | strip_cr)
  s08c_until 60 s08c_primary_is "$A1" $S08C_V4 "n1-ar$sn" && ts=$(s08c_t) || { ts=-1; p="$p; route did not move to n1-ar$sn [$(s08c_primary "$A1" $S08C_V4)]"; }
  s08c_until 20 s08c_banner_is "$A1" $S08C_HOST "LAN-A src=192.168.77.$((2 * sn - 1))" || p="$p; after: TCP [$(s08c_banner "$A1" $S08C_HOST)]"
  b1=$(s08c_banner "$A1" $S08C_HOST)
  sleep 3
  vm "$A1" "sudo systemctl stop n1-s08c-ping 2>/dev/null; sudo systemctl reset-failed n1-s08c-ping 2>/dev/null; sudo cat /run/n1-s08c-ping.log; sudo rm -f /run/n1-s08c-ping.log" \
    >"$ART_DIR/logs/S08c-ha-ping.log" 2>/dev/null
  g=$(s08c_gap "$ART_DIR/logs/S08c-ha-ping.log" "${ka:-0}"); read -r -a r <<<"${g:--1 0 0 -1}"
  [ "${r[1]}" -ge 5 ] || p="$p; only ${r[1]} replies before the kill"
  [ "${r[2]}" -ge 5 ] || p="$p; only ${r[2]} replies after the kill"
  [ "${r[0]}" -ge 0 ] && [ "${r[0]}" -le "$S08C_GAP_MAX_MS" ] || p="$p; ping gap ${r[0]} ms > $S08C_GAP_MAX_MS"
  printf 'S08c.2 HA: SIGKILL n1-ar%s (primary); a1 primary -> n1-ar%s after %s ms (harness clock); ping every 200 ms: max gap %s ms, traffic back %s ms after the kill (end of the gap, a1 vs a2 clock; kill at %s), replies %s before / %s after; TCP before [%s] after [%s]\n' \
    "$pn" "$sn" "$((ts < 0 ? -1 : ts - tk))" "${r[0]}" "${r[3]}" "$ka" "${r[1]}" "${r[2]}" "$b0" "$b1" >>"$ART_DIR/timings.txt"
  s08c_done S08c.2 "HA: n1-ar1 and n1-ar2 (same LAN, both auto-approved); primary n1-ar$pn SIGKILLed -> a1 moved to n1-ar$sn by itself, LAN host sees the new router; ping gap ${r[0]} ms (<= $S08C_GAP_MAX_MS)"
}

t_s08c_4() {
  local p="" n la ip4 b0 b1 b2
  n=$(s08c_pn); [ -n "$n" ] || { p="no router serves $S08C_V4"; s08c_done S08c.4 -; return; }
  la=192.168.77.$((2 * n - 1)); ip4=$(selfip "$A1")
  s08c_rts "$n" "set --snat-subnet-routes=false" >/dev/null 2>&1 || p="$p; snat off"
  sleep 3
  b0=$(s08c_banner "$A1" $S08C_HOST); [[ $b0 == -* ]] || p="$p; an answer without a return route [$b0] (SNAT still on?)"
  vm "$A2" "sudo ip -n c77lan route add 100.64.0.0/10 via $la" || p="$p; return route"
  s08c_until 15 s08c_banner_is "$A1" $S08C_HOST "LAN-A src=$ip4" || p="$p; with the return route: TCP [$(s08c_banner "$A1" $S08C_HOST)], want src=$ip4"
  b1=$(s08c_banner "$A1" $S08C_HOST)
  s08c_ping_ok "$A1" $S08C_HOST || p="$p; ping without SNAT"
  s08c_rts "$n" "set --snat-subnet-routes=true" >/dev/null 2>&1 || p="$p; snat back on"
  vm "$A2" "sudo ip -n c77lan route del 100.64.0.0/10" || p="$p; return route not removed"
  s08c_until 15 s08c_banner_is "$A1" $S08C_HOST "LAN-A src=$la" || p="$p; SNAT not restored [$(s08c_banner "$A1" $S08C_HOST)]"
  b2=$(s08c_banner "$A1" $S08C_HOST)
  printf 'S08c.4 SNAT off on n1-ar%s: no return route [%s]; 100.64.0.0/10 via %s [%s]; SNAT on again [%s]\n' "$n" "$b0" "$la" "$b1" "$b2" >>"$ART_DIR/timings.txt"
  s08c_done S08c.4 "--snat-subnet-routes=false on n1-ar$n: no answer without a return route; with 100.64.0.0/10 via $la the LAN host sees a1's tailnet IP $ip4; SNAT restored"
}

t_s08c_5() {
  local p="" r t0 t1 g b n
  n=$(s08c_pn)
  TS "$A1" "set --accept-dns=true" >/dev/null 2>&1 || p="$p; a1 accept-dns"
  t0=$(s08c_t)
  r=$(s08c_api a PATCH /api/v2/tailnet/-/dns/split-dns "{\"$S08C_DOM\":[\"$S08C_DNS\"]}")
  s08c_json_is "$r" "{\"$S08C_DOM\":[\"$S08C_DNS\"]}" || p="$p; PATCH split-dns [${r:0:160}]"
  r=$(s08c_api a GET /api/v2/tailnet/-/dns/split-dns)
  s08c_json_is "$r" "{\"$S08C_DOM\":[\"$S08C_DNS\"]}" || p="$p; GET split-dns [${r:0:160}]"
  r=$(s08c_api a PATCH /api/v2/tailnet/-/dns/split-dns "{\"$S08C_INDOM\":[\"$S08C_DNS\"]}")
  [ "${r%% *}" = 400 ] || p="$p; split domain under A's MagicDNS domain -> ${r%% *}"
  s08c_json_is "$(s08c_api a GET /api/v2/tailnet/-/dns/split-dns)" "{\"$S08C_DOM\":[\"$S08C_DNS\"]}" || p="$p; refused PATCH changed split DNS"
  s08c_until 30 s08c_dig_is "$A1" TXT LAN-A && t1=$(s08c_t) || { t1=-1; p="$p; a1 quad-100 TXT [$(s08c_dig "$A1" TXT)]"; }
  [ "$(s08c_dig "$A1" A)" = $S08C_HOST ] || p="$p; a1 quad-100 A [$(s08c_dig "$A1" A)]"
  g=$(s08c_getent "$A1"); [ "$g" = $S08C_HOST ] || p="$p; a1 OS resolver [$g]"
  b=$(s08c_banner "$A1" $S08C_NAME); [[ $b == LAN-A\ src=* ]] || p="$p; a1 TCP to $S08C_NAME [$b]"
  printf 'S08c.5 split DNS: PATCH -> a1 resolves %s through quad-100 after %s ms; OS resolver [%s]; TCP by name [%s] (router n1-ar%s)\n' \
    "$S08C_NAME" "$((t1 < 0 ? -1 : t1 - t0))" "$g" "$b" "$n" >>"$ART_DIR/timings.txt"
  s08c_done S08c.5 "split DNS $S08C_DOM -> $S08C_DNS by A's key (PATCH/GET); $S08C_INDOM refused 400; a1 resolves $S08C_NAME (quad-100 TXT LAN-A, A $S08C_HOST; OS resolver $g) and reaches it by name"
}

# S08c.6 part 1: B, same prefix and same split domain, its own key only.
s08c_iso_b() {
  local r b d t0 t1
  S08C_B2ID=$(s08c_dev b n1-b2); [ -n "$S08C_B2ID" ] || p="$p; b2 not in B's device list"
  TS "$B2" "set --advertise-routes=$S08C_V4" >/dev/null 2>&1 || p="$p; b2 advertise"
  TS "$B1" "set --accept-routes --accept-dns=true" >/dev/null 2>&1 || p="$p; b1 accept-routes/dns"
  sleep 4
  [ "$(s08c_primary "$B1" $S08C_V4)" = - ] || p="$p; b1 has a route before B approved"
  r=$(s08c_ping "$B1" $S08C_HOST); [[ $r == 0/* ]] || p="$p; b1 reached $S08C_HOST before B approved [$r]"
  b=$(s08c_banner "$B1" $S08C_HOST); [[ $b == LAN-A* ]] && p="$p; b1 reached A's LAN host"
  d=$(s08c_dig "$B1" TXT); [[ $d == *LAN-A* ]] && p="$p; b1 resolved A's internal name [$d]"
  grep -q "$S08C_DOM" <<<"$(netmap "$B1")" && p="$p; b1's netmap carries A's split DNS"
  grep -qE 'n1-ar[12]|tag:router' <<<"$(ts_json "$B1")$(netmap "$B1")" && p="$p; B sees A's routers"
  r=$(s08c_api b POST "/api/v2/device/$S08C_B2ID/routes" "{\"routes\":[\"$S08C_V4\"]}"); [ "${r%% *}" = 200 ] || p="$p; B approves b2 -> ${r%% *}"
  t0=$(s08c_t)
  r=$(s08c_api b PATCH /api/v2/tailnet/-/dns/split-dns "{\"$S08C_DOM\":[\"$S08C_DNS\"]}")
  s08c_json_is "$r" "{\"$S08C_DOM\":[\"$S08C_DNS\"]}" || p="$p; B PATCH split-dns [${r:0:160}]"
  s08c_until 30 s08c_dig_is "$B1" TXT LAN-B && t1=$(s08c_t) || { t1=-1; p="$p; b1 quad-100 TXT [$(s08c_dig "$B1" TXT)]"; }
  [ "$(s08c_getent "$B1")" = $S08C_HOST ] || p="$p; b1 OS resolver [$(s08c_getent "$B1")]"
  b=$(s08c_banner "$B1" $S08C_NAME); [ "$b" = "LAN-B src=192.168.77.1" ] || p="$p; b1 TCP to $S08C_NAME [$b]"
  s08c_dig_is "$A1" TXT LAN-A || p="$p; a1 after B's split DNS [$(s08c_dig "$A1" TXT)]"
  b=$(s08c_banner "$A1" $S08C_NAME); [[ $b == LAN-A* ]] || p="$p; a1 TCP by name after B [$b]"
  printf 'S08c.6 B: PATCH -> b1 resolves %s (TXT LAN-B) after %s ms; b1 TCP by name [%s]\n' "$S08C_NAME" "$((t1 < 0 ? -1 : t1 - t0))" \
    "$(s08c_banner "$B1" $S08C_NAME)" >>"$ART_DIR/timings.txt"
}
# S08c.6 part 2: the keys cross tailnets -> 404, and A is unchanged.
s08c_iso_api() {
  local r e q who m path body ida idb reqs=()
  ida=$(cat "$TMPD/id-a"); idb=$(cat "$TMPD/id-b")
  s08c_api a GET /api/v2/tailnet/-/acl >/dev/null; e=$(s08c_etag)
  mapfile -t reqs <<EOF
b|GET|/api/v2/tailnet/$ida/acl|
b|POST|/api/v2/tailnet/$ida/acl|{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}
b|POST|/api/v2/tailnet/$ida/acl/validate|{"grants":[]}
b|GET|/api/v2/tailnet/$ida/dns/split-dns|
b|PATCH|/api/v2/tailnet/$ida/dns/split-dns|{"$S08C_DOM":["192.168.77.99"]}
b|PUT|/api/v2/tailnet/$ida/dns/split-dns|{}
b|GET|/api/v2/tailnet/$ida/dns/nameservers|
b|POST|/api/v2/tailnet/$ida/dns/nameservers|{"dns":["192.168.77.99"]}
b|POST|/api/v2/tailnet/$ida/dns/searchpaths|{"searchPaths":["evil.example"]}
b|POST|/api/v2/tailnet/$ida/dns/preferences|{"magicDNS":false}
b|GET|/api/v2/device/$S08C_R1ID/routes|
b|POST|/api/v2/device/$S08C_R1ID/routes|{"routes":[]}
b|POST|/api/v2/device/$S08C_R1ID/key|{"keyExpiryDisabled":false}
b|DELETE|/api/v2/device/$S08C_R1ID|
a|GET|/api/v2/tailnet/$idb/acl|
a|PATCH|/api/v2/tailnet/$idb/dns/split-dns|{"$S08C_DOM":["192.168.77.99"]}
a|POST|/api/v2/device/$S08C_B2ID/routes|{"routes":[]}
EOF
  for q in "${reqs[@]}"; do
    IFS='|' read -r who m path body <<<"$q"
    r=$(s08c_api "$who" "$m" "$path" "$body"); [ "${r%% *}" = 404 ] || p="$p; key $who $m $path -> ${r%% *}"
  done
  s08c_api a GET /api/v2/tailnet/-/acl >/dev/null; [ "$(s08c_etag)" = "$e" ] || p="$p; A's policy changed"
  s08c_json_is "$(s08c_api a GET /api/v2/tailnet/-/dns/split-dns)" "{\"$S08C_DOM\":[\"$S08C_DNS\"]}" || p="$p; A's split DNS changed"
  grep -qE '192\.168\.77\.99|evil\.example' <<<"$(s08c_api a GET /api/v2/tailnet/-/dns/nameservers)$(s08c_api a GET /api/v2/tailnet/-/dns/searchpaths)" && p="$p; A's DNS took B's values"
  grep -q '"magicDNS": *true' <<<"$(s08c_api a GET /api/v2/tailnet/-/dns/preferences)" || p="$p; A's MagicDNS preference changed"
  s08c_both_enabled a "$S08C_R1ID" || p="$p; A's n1-ar1 routes changed [$(s08c_enabled a "$S08C_R1ID")]"
  [ "$(s08c_devfield a n1-ar1 keyExpiryDisabled)" = true ] || p="$p; A's n1-ar1 key expiry changed (or device gone)"
  [ "$(s08c_enabled b "$S08C_B2ID")" = "$S08C_V4" ] || p="$p; B's b2 route changed by A's key"
  r=$(s08c_api b GET /api/v2/tailnet/-/acl); grep -qE 'tag:router|S08c' <<<"$r" && p="$p; B's policy shows A's"
  return 0
}
t_s08c_6() {
  local p=""
  s08c_iso_b; s08c_iso_api
  s08c_done S08c.6 "B with the same prefix and split domain: nothing of A's before B acts; after B's own approval and split DNS b1 gets LAN-B only, a1 LAN-A only; 17 cross-tailnet API calls (B on A's policy/DNS/router routes/key/delete, A on B's) all 404, A's policy/DNS/devices unchanged" "$B1" $S08C_HOST
}

# S08c.0: undo, in the order the API needs (keys revoked last).
s08c_undo() {
  local s i r e
  TS "$A1" "set --accept-dns=false --accept-routes=false" >/dev/null 2>&1 || p="$p; a1 accept-dns/routes off"
  TS "$B1" "set --accept-dns=false --accept-routes=false" >/dev/null 2>&1 || p="$p; b1 accept-dns/routes off"
  TS "$B2" "set --advertise-routes=" >/dev/null 2>&1 || p="$p; b2 advertise off"
  if [ -n "$S08C_B2ID" ]; then r=$(s08c_api b POST "/api/v2/device/$S08C_B2ID/routes" '{"routes":[]}'); [ "${r%% *}" = 200 ] || p="$p; B unapprove b2 -> ${r%% *}"; fi
  for s in a b; do s08c_json_is "$(s08c_api $s PUT /api/v2/tailnet/-/dns/split-dns '{}')" '{}' || p="$p; split DNS $s not cleared"; done
  s08c_router_stop 1 || p="$p; router 1 not removed"
  s08c_router_stop 2 || p="$p; router 2 not removed"
  [ -n "$S08C_R1ID" ] || S08C_R1ID=$(s08c_dev a n1-ar1)
  [ -n "$S08C_R2ID" ] || S08C_R2ID=$(s08c_dev a n1-ar2)
  for i in $S08C_R1ID $S08C_R2ID; do r=$(s08c_api a DELETE "/api/v2/device/$i"); [ "${r%% *}" = 200 ] || p="$p; delete device $i -> ${r%% *}"; done
  if [ -n "$S08C_KID" ]; then r=$(s08c_api a DELETE "/api/v2/tailnet/-/keys/$S08C_KID"); [ "${r%% *}" = 200 ] || p="$p; delete tagged key -> ${r%% *}"; fi
  s08c_api a GET /api/v2/tailnet/-/acl >/dev/null; e=$(s08c_etag)
  r=000; [ -n "$S08C_ORIG" ] && r=$(s08c_api a POST /api/v2/tailnet/-/acl "$S08C_ORIG" application/hujson "If-Match: $e")
  if [ "${r%% *}" = 200 ] || [ -z "$S08C_POLICY_TOUCHED" ]; then : # restored, or never written by S08c
  else p="$p; policy a not restored by API (operator fallback)"
    set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}],"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; policy a not restored"; fi
  for i in "$A2:a2" "$B2:b2"; do vm "${i%%:*}" "sudo journalctl -u n1-s08c-lan --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/S08c-lan-${i##*:}.log"; done
  for i in $A1 $A2 $B2; do s08c_vm_undo "$i" >/dev/null 2>&1 || p="$p; $i: S08c objects left"; done
  for s in a b; do
    i=$(py 'import json; print(json.load(open(sys.argv[1])).get("id", ""))' "$TMPD/s08c-key-$s.json" 2>/dev/null | strip_cr)
    [ -n "$i" ] && cli c1 "tailnet apikey revoke --slug $s --id $i" >/dev/null 2>&1 || p="$p; API key $s not revoked"
  done
  rm -f "$TMPD"/s08c-key-?.json "$TMPD"/api-? "$TMPD/key-router" "$TMPD/s08c-hdr"
  wait_peers "$A1" n1-a2 40 || p="$p; a1 peers after S08c [$(peers "$A1")]"
  wait_peers "$B1" n1-b2 40 || p="$p; b1 peers after S08c [$(peers "$B1")]"
}

t_s08c() {
  local p="" c
  S08C_ORIG="" S08C_KID="" S08C_R1ID="" S08C_R2ID="" S08C_B2ID="" S08C_SETUP="" S08C_POLICY_TOUCHED=""
  if s08c_setup; then
    t_s08c_1; t_s08c_3; t_s08c_2; t_s08c_4; t_s08c_5; t_s08c_6
  else
    for c in 1 2 3 4 5 6; do record "S08c.$c" FAIL "not run: setup [$S08C_SETUP]"; done
  fi
  s08c_undo; p=${p#; }
  [ -z "$S08C_SETUP$p" ] && record S08c.0 PASS "setup and undo: routers, netns, bridge, nft, ip_forward, accept-routes/dns, split DNS, policy (API, If-Match), router devices and tagged key (API), API keys all removed or restored" \
    || record S08c.0 FAIL "${S08C_SETUP:+setup: $S08C_SETUP; }${p:+undo: $p}"
}
