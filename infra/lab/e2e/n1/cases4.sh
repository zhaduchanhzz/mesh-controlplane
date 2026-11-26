# shellcheck shell=bash
# N1 E2E batch 4, sourced by run.sh.
#
# S08b (owner request 2026-11-26): the DATA path of a subnet route, not only
# its approval. A "LAN host" 192.168.77.2 sits behind router a2 (a network
# namespace + veth on the VM, reachable from a1 only through the route), and
# a host with the SAME address behind b2. Each answers TCP 7777 with a banner
# naming its tailnet (LAN-A / LAN-B).
#   1. advertised but not approved: a1 cannot reach 192.168.77.2;
#   2. the TENANT approves its own route with its API key (/api/v2
#      setDeviceRoutes); B's key cannot approve A's device;
#   3. a1 pings 192.168.77.2 and gets the LAN-A banner over TCP;
#   4. b1, before B approves: no answer (never LAN-A); after the operator
#      approves B's route: LAN-B only;
#   5. A's policy without the subnet: a1 cannot reach the LAN host but still
#      pings a2; the policy is restored.
# Everything is undone at the end (routes, accept-routes, namespace, veth,
# ip_forward, API keys).

S08B_NET=192.168.77.0/24
S08B_HOST=192.168.77.2

s08b_lan_up() { # IP TAG
  vm "$1" "sudo bash -s" <<EOF
set -e
systemctl stop n1-lan77 2>/dev/null || true; systemctl reset-failed n1-lan77 2>/dev/null || true
ip netns del lan77 2>/dev/null || true; ip link del v77h 2>/dev/null || true
ip netns add lan77
ip link add v77h type veth peer name v77n
ip link set v77n netns lan77
ip addr add 192.168.77.1/24 dev v77h
ip link set v77h up
ip netns exec lan77 ip addr add $S08B_HOST/24 dev v77n
ip netns exec lan77 ip link set v77n up
ip netns exec lan77 ip link set lo up
ip netns exec lan77 ip route add default via 192.168.77.1
[ -e /run/n1-lan77.fwd ] || cat /proc/sys/net/ipv4/ip_forward > /run/n1-lan77.fwd
sysctl -qw net.ipv4.ip_forward=1
cat > /run/n1-lan77.py <<'PY'
import socket
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 7777)); s.listen(8)
while True:
    c, a = s.accept()
    c.sendall(b"LAN-$2 from $S08B_HOST to " + a[0].encode() + b"\n"); c.close()
PY
systemd-run --quiet --collect --unit=n1-lan77 -p RuntimeMaxSec=900 ip netns exec lan77 python3 /run/n1-lan77.py
EOF
}

s08b_lan_down() { # IP
  vm "$1" "sudo bash -s" <<'EOF'
systemctl stop n1-lan77 2>/dev/null; systemctl reset-failed n1-lan77 2>/dev/null
ip netns del lan77 2>/dev/null; ip link del v77h 2>/dev/null
[ -e /run/n1-lan77.fwd ] && sysctl -qw net.ipv4.ip_forward="$(cat /run/n1-lan77.fwd)"
rm -f /run/n1-lan77.fwd /run/n1-lan77.py
! ip netns list | grep -q lan77 && ! ip link show v77h >/dev/null 2>&1
EOF
}

# s08b_ping IP -> "<received>/<sent> <avg rtt ms>" to the LAN host, from the VM itself
s08b_ping() { vm "$1" "ping -c 3 -W 2 -q $S08B_HOST 2>&1" | awk -F'[ /,]+' '/packets transmitted/ {r=$4; t=$1} /rtt/ {a=$8} END {printf "%s/%s %s", r+0, t+0, (a == "" ? "-" : a)}'; }
# s08b_banner IP -> the TCP banner of the LAN host, or "-"
s08b_banner() {
  vm "$1" "python3 -c 'import socket,sys
try:
    c = socket.create_connection((\"$S08B_HOST\", 7777), timeout=4); print(c.recv(200).decode().strip())
except Exception as e:
    print(\"-\", type(e).__name__)'" 2>/dev/null | strip_cr
}
s08b_route_seen() { # CLIENT_IP: does it see 192.168.77.0/24 as a primary route of a peer?
  field "$1" "any('$S08B_NET' in (q.get('PrimaryRoutes') or []) for q in (d.get('Peer') or {}).values())" | grep -q True
}

t_s08b() {
  local p="" ida idb r b i t0 tr s
  ida=$(node_id c1 a n1-a2); idb=$(node_id c1 b n1-b2)
  [ -n "$ida" ] && [ -n "$idb" ] || { record S08b FAIL "node ids a2=[$ida] b2=[$idb]"; return; }

  s08b_lan_up $A2 A || p="$p; LAN behind a2"
  s08b_lan_up $B2 B || p="$p; LAN behind b2"
  # The LAN host answers on the router itself (sanity of the namespace).
  b=$(s08b_banner $A2); [[ $b == LAN-A* ]] || p="$p; a2 cannot reach its own LAN host [$b]"
  b=$(s08b_banner $B2); [[ $b == LAN-B* ]] || p="$p; b2 cannot reach its own LAN host [$b]"
  # Without a route the client has no path at all.
  r=$(s08b_ping $A1); [[ $r == 0/* ]] || p="$p; a1 reached $S08B_HOST before any route [$r]"

  TS $A2 "set --advertise-routes=$S08B_NET" >/dev/null 2>&1 || p="$p; a2 advertise"
  TS $B2 "set --advertise-routes=$S08B_NET" >/dev/null 2>&1 || p="$p; b2 advertise"
  TS $A1 "set --accept-routes" >/dev/null 2>&1 || p="$p; a1 accept-routes"
  TS $B1 "set --accept-routes" >/dev/null 2>&1 || p="$p; b1 accept-routes"
  sleep 4
  # 1. Advertised, not approved: no route, no answer.
  s08b_route_seen $A1 && p="$p; route offered to a1 before approval"
  r=$(s08b_ping $A1); [[ $r == 0/* ]] || p="$p; a1 reached $S08B_HOST before approval [$r]"

  # 2. The tenant approves with its own API key; B's key cannot touch A's device.
  for s in a b; do
    cli c1 "tailnet apikey create --slug $s --user n1-$s --expiration 1h" >"$TMPD/s08b-key-$s.json" || p="$p; apikey $s"
    py 'import json; d = json.load(open(sys.argv[1])); print(d["key"])' "$TMPD/s08b-key-$s.json" | strip_cr >"$TMPD/api-$s"
  done
  r=$(api_v2 b POST "/api/v2/device/$ida/routes" "{\"routes\":[\"$S08B_NET\"]}"); [ "${r%% *}" = 404 ] || p="$p; B's key on A's device -> ${r%% *}"
  t0=$(date +%s%3N)
  r=$(api_v2 a POST "/api/v2/device/$ida/routes" "{\"routes\":[\"$S08B_NET\"]}"); [ "${r%% *}" = 200 ] || p="$p; A's key approving its route -> ${r%% *}"
  for i in $(seq 1 40); do s08b_route_seen $A1 && break; sleep 0.5; done
  s08b_route_seen $A1 || p="$p; a1 does not see the approved route"

  # 3. Data path: ICMP and TCP through a2 to the LAN host.
  for i in $(seq 1 20); do r=$(s08b_ping $A1); [[ $r == 3/3* ]] && break; sleep 1; done
  tr=$(( $(date +%s%3N) - t0 ))
  [[ $r == 3/3* ]] || p="$p; a1 ping $S08B_HOST [$r]"
  b=$(s08b_banner $A1); [[ $b == LAN-A* ]] || p="$p; a1 TCP to $S08B_HOST got [$b]"
  printf 'S08b a1 -> %s: approval->first 3/3 ping %d ms; ping %s (recv/sent avg-rtt ms); TCP banner [%s]\n' "$S08B_HOST" "$tr" "$r" "$b" >>"$ART_DIR/timings.txt"

  # 4. B: nothing approved yet -> no answer, and never A's LAN host.
  s08b_route_seen $B1 && p="$p; b1 got a route approved in A"
  r=$(s08b_ping $B1); [[ $r == 0/* ]] || p="$p; b1 reached $S08B_HOST before B approved [$r]"
  b=$(s08b_banner $B1); [[ $b == LAN-A* ]] && p="$p; b1 reached A's LAN host"
  live c1 routes approve --slug b --id "$idb" --routes "$S08B_NET" >>"$ART_DIR/logs/routes.log" 2>&1 || p="$p; approve b2"
  for i in $(seq 1 40); do s08b_route_seen $B1 && break; sleep 0.5; done
  for i in $(seq 1 20); do r=$(s08b_ping $B1); [[ $r == 3/3* ]] && break; sleep 1; done
  [[ $r == 3/3* ]] || p="$p; b1 ping $S08B_HOST [$r]"
  b=$(s08b_banner $B1); [[ $b == LAN-B* ]] || p="$p; b1 TCP to $S08B_HOST got [$b] (want LAN-B)"
  printf 'S08b b1 -> %s (same address behind b2): ping %s; TCP banner [%s]\n' "$S08B_HOST" "$r" "$b" >>"$ART_DIR/timings.txt"
  b=$(s08b_banner $A1); [[ $b == LAN-A* ]] || p="$p; a1 lost LAN-A after B's approval [$b]"

  # 5. Policy: A's grants to tailnet addresses only -> no subnet; restore.
  set_policy a '{"grants":[{"src":["*"],"dst":["100.64.0.0/10","fd7a:115c:a1e0::/48"],"ip":["*"]}]}' || p="$p; policy a (no subnet)"
  for i in $(seq 1 30); do r=$(s08b_ping $A1); [[ $r == 0/* ]] && break; sleep 1; done
  [[ $r == 0/* ]] || p="$p; a1 still reaches $S08B_HOST under a policy without the subnet [$r]"
  b=$(s08b_banner $A1); [[ $b == LAN-A* ]] && p="$p; a1 TCP still reaches the LAN host under a policy without the subnet"
  TS $A1 "ping -c 1 --timeout 5s n1-a2" >/dev/null 2>&1 || p="$p; a1 lost a2 under the tailnet-only policy"
  printf 'S08b policy without the subnet: a1 ping %s, TCP [%s]\n' "$r" "$b" >>"$ART_DIR/timings.txt"
  set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}],"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; restore policy a"

  # Undo.
  TS $A2 "set --advertise-routes=" >/dev/null 2>&1; TS $B2 "set --advertise-routes=" >/dev/null 2>&1
  TS $A1 "set --accept-routes=false" >/dev/null 2>&1; TS $B1 "set --accept-routes=false" >/dev/null 2>&1
  s08b_lan_down $A2 || p="$p; LAN behind a2 not removed"
  s08b_lan_down $B2 || p="$p; LAN behind b2 not removed"
  for s in a b; do
    i=$(py 'import json; print(json.load(open(sys.argv[1])).get("id", ""))' "$TMPD/s08b-key-$s.json" 2>/dev/null | strip_cr)
    [ -n "$i" ] && cli c1 "tailnet apikey revoke --slug $s --id $i" >/dev/null 2>&1
    rm -f "$TMPD/s08b-key-$s.json" "$TMPD/api-$s"
  done
  wait_peers $A1 n1-a2 30 || p="$p; a1 lost a2 after S08b"

  [ -z "$p" ] && record S08b PASS "subnet route $S08B_NET data path: unapproved -> no path; tenant approved with its API key (B's key 404); a1 ping 3/3 + TCP banner LAN-A via a2; same subnet behind b2: b1 none before approval, LAN-B after, never LAN-A; policy without the subnet -> a1 no path, a2 still reachable" || record S08b FAIL "${p#; }"
}
