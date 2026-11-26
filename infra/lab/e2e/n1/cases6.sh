# shellcheck shell=bash
# N1 E2E batch 6, sourced by run.sh (uses its helpers and those of cases2-5).
#
# S08d (P12d): Tailscale-equivalent exit nodes
# (https://tailscale.com/docs/features/exit-nodes) driven ONLY by the tenant
# API (/api/v2 with the tailnet's API key; docs/tenant-api/subnet-routers.md
# for the contract: policy with ETag/If-Match, device routes, tagged keys).
# The operator CLI only issues and revokes the two API keys and is the
# last-resort fallback of the policy restore.
#
# Topology. a2 (tailnet A) and b2 (tailnet B) each get a fake "internet":
#   bridge x77br on the VM: 203.0.113.1/24, 2001:db8:77::1/64 (TEST-NET-3 and
#     the documentation prefix: both inside autogroup:internet, routed nowhere
#     in the lab, so a client reaches them only through an exit node);
#   netns x77inet: 203.0.113.10, 2001:db8:77::10, no default route; TCP 7780
#     answers "INET-<A|B> src=<address it saw>" (the exit node's SNAT address).
# a2 also runs a LAN witness on its LAN address 10.77.20.12:7781 that answers
# "LANSEEN src=<address it saw>": src 10.77.20.11 means a1 came over the LAN,
# a tailnet address means through the tunnel.
# The auto-approved exit node n1-ax1 lives in netns x77r1 on a2 (own
# tailscaled $V, tun x77ts1, port 41645, 203.0.113.2 / 2001:db8:77::2 on the
# bridge, uplink 10.177.78.2/30 NATed out of a2 by nft table ip n1s08d).
#
# With an exit node set, a client's replies to the harness (mc-ci) leave
# through the exit node and SSH hangs (S08, cases2.sh): every client step
# with an exit node set runs DETACHED on the client (s08d_run: a transient
# unit whose EXIT trap unsets the exit node, plus a fallback timer that unsets
# it anyway); the harness reads the results afterwards.
#
#   S08d.1 manual approval through the API: policy (tagOwners tag:exit,
#          autoApprovers.exitNode, grant to autogroup:internet) written with
#          If-Match; n1-a2 advertises; nothing enabled and no exit node offered
#          before approval (untagged: autoApprovers do not apply); B's key on
#          a2's routes -> 404; A POSTs {"routes":["0.0.0.0/0","::/0"]};
#          approval -> a1 offered n1-a2 (time).
#   S08d.2 data path: a1 --exit-node=<a2's IP>: ping + TCP to 203.0.113.10
#          (banner shows a2's SNAT address), IPv6 2001:db8:77::10;
#          --exit-node-allow-lan-access=false vs true: route, gateway ping
#          and LAN witness recorded as they are.
#   S08d.3 policy without autogroup:internet: what a1 gets (cannot select, or
#          no traffic) recorded; the grant back -> offered again.
#   S08d.4 autoApprovers.exitNode: a tagged key minted by the API; n1-ax1
#          joins with it and advertises -> 0.0.0.0/0, ::/0 enabled with no
#          approval call; a1 through n1-ax1 -> INET-A src=203.0.113.2.
#   S08d.5 B: its own exit node n1-b2 (approved by B's key, B's policy with
#          autogroup:internet written by B's key) and the same fake internet
#          address answering INET-B; b1 can never select A's exit nodes (by
#          name or address), never sees them; keys across tailnets -> 404.
#   S08d.6 `tailscale exit-node list` / `exit-node suggest` on a1 and b1:
#          only their own tailnet's exit nodes.
#   S08d.0 setup and undo: exit node unset on clients, advertise off,
#          approvals removed through each tenant's API, n1-ax1 deleted and
#          its key revoked through the API, policies restored with If-Match,
#          API keys revoked, netns/bridge/nft/forwarding/units removed.

S08D_INET=203.0.113.10
S08D_INET6=2001:db8:77::10
S08D_PORT=7780
S08D_LANW=10.77.20.12 # a2's LAN address: the LAN witness
S08D_LPORT=7781
S08D_GW=10.77.20.1
S08D_EXITR="0.0.0.0/0,::/0" # sorted, as s08c_enabled prints them
S08D_POL_HEAD='{
  // S08d: tenant A, exit nodes (written through /api/v2 with If-Match)
  "tagOwners": {"tag:exit": ["n1-a@"]},
  "autoApprovers": {"exitNode": ["tag:exit"]},
  "grants": [
    {"src": ["*"], "dst": ["100.64.0.0/10", "fd7a:115c:a1e0::/48"], "ip": ["*"]},'
S08D_POL_INET="$S08D_POL_HEAD"'
    {"src": ["*"], "dst": ["autogroup:internet"], "ip": ["*"]},
  ],
}'
S08D_POL_NOINET="$S08D_POL_HEAD"'
  ],
}'
# B keeps its legacy acls form and adds the internet.
S08D_POL_B='{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]},{"action":"accept","src":["*"],"dst":["autogroup:internet:*"]}]}'
S08D_KEYBODY='{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":false,"preauthorized":true,"tags":["tag:exit"]}}},"expirySeconds":1800}'
S08D_ORIG_A="" S08D_ORIG_B="" S08D_TOUCHED_A="" S08D_TOUCHED_B="" S08D_KID="" S08D_IDA="" S08D_IDB="" S08D_IDX="" S08D_SETUP="" S08D_V6A=""

# ------------------------------------------------------------- the VMs -----
# s08d_vm_undo IP: remove every S08d object from the VM (idempotent; also
# called by run.sh's cleanup); true when nothing is left.
s08d_vm_undo() {
  vm "$1" "sudo bash -s" <<'EOF'
systemctl stop 'n1-s08d-*' 2>/dev/null; systemctl reset-failed 'n1-s08d-*' 2>/dev/null
for n in x77r1 x77inet; do ip netns del $n 2>/dev/null; rm -rf /etc/netns/$n; done
rmdir /etc/netns 2>/dev/null
for l in x77br x77ib x77l1b x77u1; do ip link del $l 2>/dev/null; done
nft delete table ip n1s08d 2>/dev/null
[ -e /run/n1-s08d.fwd ] && sysctl -qw net.ipv4.ip_forward="$(cat /run/n1-s08d.fwd)"
[ -e /run/n1-s08d.fwd6 ] && sysctl -qw net.ipv6.conf.all.forwarding="$(cat /run/n1-s08d.fwd6)"
rm -rf /run/n1-s08d.fwd /run/n1-s08d.fwd6 /run/n1-s08d-srv.py /var/lib/n1-s08d-r1 /tmp/n1-s08d-*
! ip netns list | grep -q x77 && ! ip link show x77br >/dev/null 2>&1 && ! nft list table ip n1s08d >/dev/null 2>&1 \
  && [ -z "$(systemctl list-units --no-legend 'n1-s08d-*' 2>/dev/null)" ]
EOF
}

# s08d_inet_up IP TAG WITNESS: bridge x77br + fake internet host netns
# x77inet answering "INET-<TAG>"; WITNESS=1 also starts the LAN witness on
# the VM's own LAN address. Forwarding (v4 and v6) on, old values kept.
s08d_inet_up() {
  s08d_vm_undo "$1" >/dev/null 2>&1
  vm "$1" "sudo tee /run/n1-s08d-srv.py >/dev/null" <<'PY' || return 1
import socket, sys
TAG, BIND, PORT = sys.argv[1], sys.argv[2], int(sys.argv[3])
fam = socket.AF_INET6 if ":" in BIND else socket.AF_INET
s = socket.socket(fam, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
if fam == socket.AF_INET6:
    s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
s.bind((BIND, PORT))
s.listen(16)
while True:
    c, a = s.accept()
    src = a[0][7:] if a[0].startswith("::ffff:") else a[0]
    print("tcp from %s" % src, flush=True)
    try:
        c.sendall(("%s src=%s\n" % (TAG, src)).encode())
    finally:
        c.close()
PY
  vm "$1" "sudo bash -s" <<EOF
set -e
ip link add x77br type bridge mcast_snooping 0
sysctl -qw net.ipv6.conf.x77br.disable_ipv6=0 || true
ip addr add 203.0.113.1/24 dev x77br
ip -6 addr add 2001:db8:77::1/64 dev x77br nodad || true
ip link set x77br up
ip netns add x77inet
ip link add x77ih type veth peer name x77ib
ip link set x77ih netns x77inet
ip link set x77ib master x77br up
ip -n x77inet link set lo up
ip -n x77inet addr add $S08D_INET/24 dev x77ih
ip netns exec x77inet sysctl -qw net.ipv6.conf.all.disable_ipv6=0 || true
ip -n x77inet -6 addr add $S08D_INET6/64 dev x77ih nodad || true
ip -n x77inet link set x77ih up
[ -e /run/n1-s08d.fwd ] || cat /proc/sys/net/ipv4/ip_forward >/run/n1-s08d.fwd
[ -e /run/n1-s08d.fwd6 ] || cat /proc/sys/net/ipv6/conf/all/forwarding >/run/n1-s08d.fwd6
sysctl -qw net.ipv4.ip_forward=1
sysctl -qw net.ipv6.conf.all.forwarding=1
systemd-run --quiet --collect --unit=n1-s08d-inet -p RuntimeMaxSec=2400 ip netns exec x77inet python3 -u /run/n1-s08d-srv.py INET-$2 :: $S08D_PORT
if [ "$3" = 1 ]; then systemd-run --quiet --collect --unit=n1-s08d-lan -p RuntimeMaxSec=2400 python3 -u /run/n1-s08d-srv.py LANSEEN $S08D_LANW $S08D_LPORT; fi
set +e
for i in \$(seq 1 20); do ip netns exec x77inet ss -ltnH 'sport = :$S08D_PORT' | grep -q $S08D_PORT && break; sleep 0.25; done
[ "$3" = 1 ] || exit 0
for i in \$(seq 1 20); do ss -ltnH 'sport = :$S08D_LPORT' | grep -q $S08D_LPORT && exit 0; sleep 0.25; done
exit 1
EOF
}

# The auto-approved exit node n1-ax1: netns x77r1 on a2, tailscaled $V in it.
s08d_xr_start() {
  vm "$A2" "sudo bash -s" <<EOF
set -e
ip netns add x77r1
ip link add x77l1 type veth peer name x77l1b
ip link set x77l1 netns x77r1
ip link set x77l1b master x77br up
ip link add x77u1 type veth peer name x77u1n
ip link set x77u1n netns x77r1
ip addr add 10.177.78.1/30 dev x77u1
ip link set x77u1 up
ip -n x77r1 link set lo up
ip netns exec x77r1 sysctl -qw net.ipv6.conf.all.disable_ipv6=0 || true
ip -n x77r1 addr add 203.0.113.2/24 dev x77l1
ip -n x77r1 -6 addr add 2001:db8:77::2/64 dev x77l1 nodad || true
ip -n x77r1 link set x77l1 up
ip -n x77r1 addr add 10.177.78.2/30 dev x77u1n
ip -n x77r1 link set x77u1n up
ip -n x77r1 route add default via 10.177.78.1
ip netns exec x77r1 sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1
printf '%s\n' 'table ip n1s08d {' '  chain post {' '    type nat hook postrouting priority srcnat; policy accept;' \
  '    ip saddr 10.177.78.0/30 ip daddr != 10.177.78.0/30 masquerade' '  }' '}' | nft -f -
mkdir -p /etc/netns/x77r1
cp /run/systemd/resolve/resolv.conf /etc/netns/x77r1/resolv.conf
rm -rf /var/lib/n1-s08d-r1; install -d -m 0700 /var/lib/n1-s08d-r1
systemctl reset-failed n1-s08d-r1 2>/dev/null || true
systemd-run --quiet --unit=n1-s08d-r1 -p RuntimeDirectory=n1-s08d-r1 -p RuntimeMaxSec=1800 \
  ip netns exec x77r1 /opt/tailscale/$V/tailscaled --state=/var/lib/n1-s08d-r1/tailscaled.state \
  --socket=/run/n1-s08d-r1/ts.sock --tun=x77ts1 --port=41645
ip netns exec x77r1 ping -c 1 -W 2 -q $S08D_INET >/dev/null
set +e
for i in \$(seq 1 40); do /opt/tailscale/$V/tailscale --socket=/run/n1-s08d-r1/ts.sock status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.25; done
exit 1
EOF
}
s08d_xts() { vm "$A2" "sudo /opt/tailscale/$V/tailscale --socket=/run/n1-s08d-r1/ts.sock $*"; }
s08d_xr_up() {
  vm "$A2" "sudo timeout 60 /opt/tailscale/$V/tailscale --socket=/run/n1-s08d-r1/ts.sock up --login-server=$URL1 --auth-key=file:/dev/stdin --hostname=n1-ax1 --accept-dns=false" \
    <"$TMPD/key-exit" 2>&1 | redact >>"$ART_DIR/logs/n1-ax1.cli.log"
  return "${PIPESTATUS[0]}"
}
s08d_xr_stop() {
  vm "$A2" "sudo journalctl -u n1-s08d-r1 --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/S08d-ax1-tailscaled.log"
  vm "$A2" "sudo systemctl stop n1-s08d-r1 2>/dev/null; sudo systemctl reset-failed n1-s08d-r1 2>/dev/null; sudo ip netns del x77r1 2>/dev/null; \
    sudo ip link del x77u1 2>/dev/null; sudo nft delete table ip n1s08d 2>/dev/null; sudo rm -rf /etc/netns/x77r1 /var/lib/n1-s08d-r1; \
    ! ip link show x77u1 >/dev/null 2>&1 && ! sudo nft list table ip n1s08d >/dev/null 2>&1"
}

# ------------------------------------------------- client-side, harness ----
# s08d_ping CLIENT DEST -> "<received>/<sent> <avg ms>" (raw output to
# logs/S08d-ping.log). Test the OUTPUT, never "s08d_ping | grep" (pipefail).
s08d_ping() {
  local out; out=$(vm "$1" "ping -c 3 -W 2 $2 2>&1" | strip_cr)
  printf '== %s %s -> %s\n%s\n' "$(date -u +%T)" "$1" "$2" "$out" >>"$ART_DIR/logs/S08d-ping.log"
  awk -F'[ /,]+' '/packets transmitted/ {r=$4; t=$1} /rtt/ {a=$8} END {printf "%s/%s %s", r+0, t+0, (a == "" ? "-" : a)}' <<<"$out"
}
# s08d_banner CLIENT HOST PORT -> the banner, or "- <error>"
s08d_banner() {
  vm "$1" "python3 -c 'import socket, sys
try:
    c = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=4); print(c.recv(200).decode().strip())
except Exception as e:
    print(\"-\", type(e).__name__)' $2 $3" 2>/dev/null | strip_cr
}
# s08d_opt CLIENT HOST -> True|False (the peer's ExitNodeOption) or absent
s08d_opt() { field "$1" "next((str(bool(q.get('ExitNodeOption'))) for q in (d.get('Peer') or {}).values() if q.get('HostName') == '$2'), 'absent')"; }
s08d_opt_is() { [ "$(s08d_opt "$1" "$2")" = "$3" ]; }
# s08d_opts CLIENT -> sorted host names of the peers offered as exit nodes
s08d_opts() { field "$1" "' '.join(sorted(q['HostName'] for q in (d.get('Peer') or {}).values() if q.get('ExitNodeOption')))"; }
# s08d_exitnow CLIENT -> the exit node in use (its addresses) or "none"
s08d_exitnow() { field "$1" "','.join((d.get('ExitNodeStatus') or {}).get('TailscaleIPs') or []) or 'none'"; }
s08d_advertised() { # SLUG ID -> the device's advertised routes, sorted
  s08c_api "$1" GET "/api/v2/device/$2/routes" | py 'import json
st, _, body = sys.stdin.read().partition(" ")
print(",".join(sorted((json.loads(body).get("advertisedRoutes") or []) if st == "200" else [])))' | strip_cr
}
s08d_adv_exit() { [ "$(s08d_advertised "$1" "$2")" = "$S08D_EXITR" ]; }
s08d_enabled_is() { [ "$(s08c_enabled "$1" "$2")" = "$3" ]; }
# s08d_setpol SLUG BODY: GET the ETag, POST the policy with If-Match -> status
s08d_setpol() {
  local e r
  s08c_api "$1" GET /api/v2/tailnet/-/acl >/dev/null; e=$(s08c_etag)
  [ -n "$e" ] || { echo "no-etag"; return; }
  r=$(s08c_api "$1" POST /api/v2/tailnet/-/acl "$2" application/hujson "If-Match: $e")
  echo "${r%% *}"
}
s08d_kv() { sed -n "s/^$2=//p" "$1" | tail -1; } # FILE KEY -> last value
s08d_lan_seen() { s08d_banner "$1" $S08D_LANW $S08D_LPORT; }

# s08d_run CLIENT TAG MAXSEC [VAR=VALUE...] <<'EOF' body EOF: the body runs
# detached on CLIENT as root (transient unit n1-s08d-TAG, at most MAXSEC s),
# after a prelude: T (the client's tailscale CLI), R KEY VALUE (a result
# line), helpers pings/banner/exitst/firstreply/setx; its EXIT trap unsets
# the exit node (and allow-lan-access), and a fallback timer unit
# (n1-s08d-unset) unsets it MAXSEC+30 s later whatever happens. The harness
# waits for the unit, then copies the result lines to $TMPD/s08d-TAG.out and
# logs/S08d-TAG.out (redacted).
s08d_prelude() {
  cat <<'EOF'
R() { printf '%s=%s\n' "$1" "$(printf '%s' "$2" | tr '\r\n' '  ')" >>"$OUT"; }
ms() { date +%s%3N; }
exitst() { $T status --json 2>/dev/null | python3 -c 'import json, sys
e = json.load(sys.stdin).get("ExitNodeStatus") or {}
print(",".join(e.get("TailscaleIPs") or []) + " online=%s" % e.get("Online") if e else "none")'; }
fin() {
  $T set --exit-node= --exit-node-allow-lan-access=false >/dev/null 2>&1; R unset_rc $?
  R exit_after "$(exitst)"
  touch "$DONE"
}
trap fin EXIT
trap 'exit 143' TERM INT HUP
pings() { ping -c "${2:-3}" -W 2 "$1" 2>&1 | awk -F'[ /,]+' '/packets transmitted/ {r=$4; t=$1} /rtt/ {a=$8} END {printf "%s/%s %s", r+0, t+0, (a == "" ? "-" : a)}'; }
banner() { python3 -c 'import socket, sys
try:
    c = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=4); print(c.recv(200).decode().strip())
except Exception as e:
    print("-", type(e).__name__)' "$1" "$2"; }
# firstreply DEST SECONDS -> ms from the last setx to the first echo reply, -1 if none
firstreply() {
  local end=$(( $(ms) + $2 * 1000 ))
  while [ "$(ms)" -lt "$end" ]; do
    ping -c 1 -W 1 -q "$1" >/dev/null 2>&1 && { echo $(( $(ms) - SETAT )); return; }
    sleep 0.2
  done
  echo -1
}
# setx KEY ARGS...: tailscale set ARGS -> KEY_rc, KEY_out; status as tailscale's
setx() { local k=$1 o rc; shift; SETAT=$(ms); o=$($T set "$@" 2>&1); rc=$?; R "${k}_rc" "$rc"; R "${k}_out" "$o"; return $rc; }
SETAT=$(ms)
R start "$(ms)"
EOF
}
s08d_run() {
  local ip=$1 tag=$2 max=$3 f=/tmp/n1-s08d-$2 v end
  shift 3
  {
    printf 'T="/opt/tailscale/%s/tailscale --socket=%s"\nOUT=%s.out\nDONE=%s.done\n' "$V" "$TSS" "$f" "$f"
    for v in "$@"; do printf '%s=%q\n' "${v%%=*}" "${v#*=}"; done
    s08d_prelude; cat
  } | vm "$ip" "cat >$f.sh" || return 1
  vm "$ip" "sudo rm -f $f.out $f.done; for u in n1-s08d-$tag n1-s08d-unset.timer n1-s08d-unset; do sudo systemctl stop \$u 2>/dev/null; sudo systemctl reset-failed \$u 2>/dev/null; done
    sudo systemd-run --quiet --collect --unit=n1-s08d-unset --on-active=$((max + 30)) /opt/tailscale/$V/tailscale --socket=$TSS set --exit-node= --exit-node-allow-lan-access=false
    sudo systemd-run --quiet --collect --unit=n1-s08d-$tag -p RuntimeMaxSec=$max bash $f.sh" || return 1
  sleep 5
  end=$(( $(date +%s) + max + 45 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    timeout 20 ssh "${SSHO[@]}" -o StrictHostKeyChecking=accept-new -o "$PX" "mesh@$ip" "test -e $f.done" 2>/dev/null && break
    sleep 4
  done
  vm "$ip" "sudo systemctl stop n1-s08d-unset.timer n1-s08d-unset 2>/dev/null; sudo systemctl reset-failed n1-s08d-unset.timer n1-s08d-unset 2>/dev/null; cat $f.out 2>/dev/null; test -e $f.done; rc=\$?; sudo rm -f $f.sh $f.out $f.done; exit \$rc" \
    2>/dev/null | strip_cr >"$TMPD/s08d-$tag.out"
  local rc=${PIPESTATUS[0]}
  redact <"$TMPD/s08d-$tag.out" >"$ART_DIR/logs/S08d-$tag.out"
  return "$rc"
}

# s08d_diag LABEL: state snapshot to logs/S08d-diag.log.
s08d_diag() {
  local ip
  {
    printf '=== %s %s\n' "$(date -u +%T)" "$1"
    for ip in $A1 $B1 $A2 $B2; do
      echo "--- $ip: prefs, rules, table 52, peers (HostName Online ExitNodeOption AllowedIPs), forwarding"
      timeout 30 ssh "${SSHO[@]}" -o StrictHostKeyChecking=accept-new -o "$PX" "mesh@$ip" \
        "sudo /opt/tailscale/$V/tailscale --socket=$TSS debug prefs 2>&1 | grep -E '\"(ExitNodeID|ExitNodeIP|ExitNodeAllowLANAccess|AdvertiseRoutes|RouteAll|WantRunning)\"'
         ip rule; ip route show table 52; ip -6 route show table 52; sysctl net.ipv4.ip_forward net.ipv6.conf.all.forwarding
         sudo /opt/tailscale/$V/tailscale --socket=$TSS status --json 2>/dev/null | python3 -c 'import json, sys
d = json.load(sys.stdin)
print(\"ExitNodeStatus\", d.get(\"ExitNodeStatus\"))
for q in (d.get(\"Peer\") or {}).values(): print(q.get(\"HostName\"), q.get(\"Online\"), q.get(\"ExitNodeOption\"), q.get(\"AllowedIPs\"))'" 2>&1
    done
    echo "--- API: enabled routes a2 [$(s08c_enabled a "$S08D_IDA")] ax1 [$(s08c_enabled a "$S08D_IDX")] b2 [$(s08c_enabled b "$S08D_IDB")]"
  } | strip_cr | redact >>"$ART_DIR/logs/S08d-diag.log"
}
s08d_done() { # ID PASS_TEXT: record from the caller's p (FAIL: snapshot first)
  p=${p#; }
  if [ -z "$p" ]; then record "$1" PASS "$2"; return; fi
  s08d_diag "$1"
  printf '%s FAIL: %s (snapshot: logs/S08d-diag.log)\n' "$1" "$p" >>"$ART_DIR/timings.txt"
  record "$1" FAIL "$p"
}

# ------------------------------------------------------------------ cases ---
s08d_setup() {
  local p="" s r b
  for s in a b; do
    cli c1 "tailnet apikey create --slug $s --user n1-$s --expiration 1h" >"$TMPD/s08d-key-$s.json" || p="$p; apikey $s"
    py 'import json; d = json.load(open(sys.argv[1])); print(d["key"])' "$TMPD/s08d-key-$s.json" | strip_cr >"$TMPD/api-$s"
  done
  S08D_IDA=$(s08c_dev a n1-a2); S08D_IDB=$(s08c_dev b n1-b2)
  [ -n "$S08D_IDA" ] && [ -n "$S08D_IDB" ] || p="$p; device ids a2=[$S08D_IDA] b2=[$S08D_IDB]"
  # No approval left by an earlier case (approvals persist).
  r=$(s08c_enabled a "$S08D_IDA"); [ -z "$r" ] || p="$p; a2 has approved routes before S08d [$r]"
  r=$(s08c_enabled b "$S08D_IDB"); [ -z "$r" ] || p="$p; b2 has approved routes before S08d [$r]"
  s08d_inet_up "$A2" A 1 || p="$p; fake internet + LAN witness on a2"
  s08d_inet_up "$B2" B 0 || p="$p; fake internet on b2"
  b=$(s08d_banner "$A2" $S08D_INET $S08D_PORT); [ "$b" = "INET-A src=203.0.113.1" ] || p="$p; a2 to its own fake internet [$b]"
  b=$(s08d_banner "$B2" $S08D_INET $S08D_PORT); [ "$b" = "INET-B src=203.0.113.1" ] || p="$p; b2 to its own fake internet [$b]"
  b=$(s08d_banner "$A2" $S08D_INET6 $S08D_PORT)
  if [ "$b" = "INET-A src=2001:db8:77::1" ]; then S08D_V6A=1; else S08D_V6A="a2 has no IPv6 path to its own fake internet [$b]"; fi
  # Without an exit node the clients have no path to the fake internet (captured output, not a pipeline).
  for s in "$A1:a1" "$B1:b1"; do
    r=$(s08d_ping "${s%%:*}" $S08D_INET); [[ $r == 0/* ]] || p="$p; ${s##*:} reached $S08D_INET without an exit node [$r]"
    b=$(s08d_banner "${s%%:*}" $S08D_INET $S08D_PORT); [[ $b == -* ]] || p="$p; ${s##*:} TCP to $S08D_INET without an exit node [$b]"
    b=$(s08d_banner "${s%%:*}" $S08D_INET6 $S08D_PORT); [[ $b == -* ]] || p="$p; ${s##*:} TCP to $S08D_INET6 without an exit node [$b]"
    [ "$(s08d_exitnow "${s%%:*}")" = none ] || p="$p; ${s##*:} already uses an exit node"
  done
  b=$(s08d_lan_seen "$A1"); [ "$b" = "LANSEEN src=$A1" ] || p="$p; a1 to the LAN witness over the LAN [$b]"
  printf 'S08d setup: a1/b1 without exit node -> %s: no answer; a1 -> LAN witness [%s]; IPv6 on a2: %s\n' "$S08D_INET" "$b" "$S08D_V6A" >>"$ART_DIR/timings.txt"
  S08D_SETUP=${p#; }
  [ -z "$p" ] || s08d_diag setup
  [ -z "$p" ]
}

# Helpers named s08d_<step> below append their failures to the CALLER's p
# (bash dynamic scope): never call them in $(...).

t_s08d_1() {
  local p="" r t0 t1 st
  r=$(s08c_api a GET /api/v2/tailnet/-/acl "" "" "Accept: application/hujson")
  [ "${r%% *}" = 200 ] && S08D_ORIG_A=${r#* } || p="$p; GET acl -> ${r%% *}"
  S08D_TOUCHED_A=1
  st=$(s08d_setpol a "$S08D_POL_INET"); [ "$st" = 200 ] || p="$p; POST policy (internet grant) -> $st"
  TS "$A2" "set --advertise-exit-node" >/dev/null 2>&1 || p="$p; a2 advertise-exit-node"
  s08c_until 30 s08d_adv_exit a "$S08D_IDA" || p="$p; a2 advertised routes [$(s08d_advertised a "$S08D_IDA")]"
  sleep 3
  # Not approved: nothing enabled (n1-a2 is untagged: autoApprovers.exitNode
  # tag:exit does not apply) and a1 is offered no exit node.
  r=$(s08c_enabled a "$S08D_IDA"); [ -z "$r" ] || p="$p; enabled before approval [$r]"
  r=$(s08d_opt "$A1" n1-a2); [ "$r" = False ] || p="$p; a1 offered n1-a2 before approval [$r]"
  # B's key on A's device: 404 (read and write).
  r=$(s08c_api b GET "/api/v2/device/$S08D_IDA/routes"); [ "${r%% *}" = 404 ] || p="$p; B's key GET a2 routes -> ${r%% *}"
  r=$(s08c_api b POST "/api/v2/device/$S08D_IDA/routes" '{"routes":["0.0.0.0/0","::/0"]}'); [ "${r%% *}" = 404 ] || p="$p; B's key approving a2 -> ${r%% *}"
  r=$(s08c_enabled a "$S08D_IDA"); [ -z "$r" ] || p="$p; B's refused call enabled [$r]"
  t0=$(s08c_t)
  r=$(s08c_api a POST "/api/v2/device/$S08D_IDA/routes" '{"routes":["0.0.0.0/0","::/0"]}'); [ "${r%% *}" = 200 ] || p="$p; A approving a2 -> ${r%% *}"
  s08c_until 30 s08d_opt_is "$A1" n1-a2 True && t1=$(s08c_t) || { t1=-1; p="$p; a1 not offered n1-a2 after approval [$(s08d_opt "$A1" n1-a2)]"; }
  s08d_enabled_is a "$S08D_IDA" "$S08D_EXITR" || p="$p; enabled after approval [$(s08c_enabled a "$S08D_IDA")]"
  r=$(s08d_opts "$B1"); [ -z "$r" ] || p="$p; b1 offered exit nodes [$r]"
  printf 'S08d.1 manual approval (POST /device/%s/routes 0.0.0.0/0,::/0 by A'"'"'s key) -> a1 offered n1-a2 after %s ms\n' \
    "$S08D_IDA" "$((t1 < 0 ? -1 : t1 - t0))" >>"$ART_DIR/timings.txt"
  s08d_done S08d.1 "policy (tagOwners, autoApprovers.exitNode, autogroup:internet) by A's key with If-Match; n1-a2 advertised: not enabled, not offered (untagged); B's key on a2's routes 404; A's key approved 0.0.0.0/0,::/0 -> a1 offered n1-a2 after $((t1 < 0 ? -1 : t1 - t0)) ms; b1 offered nothing"
}

t_s08d_2() {
  local p="" f ip6 a2ip r v6
  a2ip=$(selfip "$A2"); ip6=$(field "$A1" "next((a for a in d['Self']['TailscaleIPs'] if ':' in a), '')")
  s08d_run "$A1" data 120 "EXIT=$a2ip" "INET=$S08D_INET" "INET6=$S08D_INET6" "P=$S08D_PORT" "GW=$S08D_GW" "LANW=$S08D_LANW" "LP=$S08D_LPORT" <<'EOF' || p="$p; detached run on a1 did not finish"
setx sel --exit-node="$EXIT" --exit-node-allow-lan-access=false || exit 0
R first_reply_ms "$(firstreply "$INET" 40)"
R exit "$(exitst)"
R ping4 "$(pings "$INET")"
R tcp4 "$(banner "$INET" "$P")"
R ping6 "$(pings "$INET6")"
R tcp6 "$(banner "$INET6" "$P")"
R route_lan_off "$(ip route get "$GW" 2>&1 | head -1)"
R ping_gw_off "$(pings "$GW")"
R lan_off "$(banner "$LANW" "$LP")"
setx lanon --exit-node-allow-lan-access=true
sleep 2
R route_lan_on "$(ip route get "$GW" 2>&1 | head -1)"
R ping_gw_on "$(pings "$GW")"
R lan_on "$(banner "$LANW" "$LP")"
R ping4_on "$(pings "$INET")"
R tcp4_on "$(banner "$INET" "$P")"
EOF
  f=$TMPD/s08d-data.out
  [ "$(s08d_kv "$f" sel_rc)" = 0 ] || p="$p; a1 set --exit-node=$a2ip rc=[$(s08d_kv "$f" sel_rc)] [$(s08d_kv "$f" sel_out)]"
  [[ $(s08d_kv "$f" exit) == *"$a2ip/32"* ]] || p="$p; a1 exit node status [$(s08d_kv "$f" exit)]"
  [[ $(s08d_kv "$f" ping4) == 3/3* ]] || p="$p; a1 ping $S08D_INET [$(s08d_kv "$f" ping4)]"
  [ "$(s08d_kv "$f" tcp4)" = "INET-A src=203.0.113.1" ] || p="$p; a1 TCP $S08D_INET [$(s08d_kv "$f" tcp4)]"
  # IPv6: asserted when the lab has it (a1 has a tailnet IPv6, a2 reaches its fake internet over IPv6).
  if [ -z "$ip6" ]; then v6="skipped: a1 has no tailnet IPv6"
  elif [ "$S08D_V6A" != 1 ]; then v6="skipped: $S08D_V6A"
  else
    v6="ping $(s08d_kv "$f" ping6), TCP [$(s08d_kv "$f" tcp6)]"
    [[ $(s08d_kv "$f" ping6) == 3/3* ]] || p="$p; a1 ping $S08D_INET6 [$(s08d_kv "$f" ping6)]"
    [ "$(s08d_kv "$f" tcp6)" = "INET-A src=2001:db8:77::1" ] || p="$p; a1 TCP $S08D_INET6 [$(s08d_kv "$f" tcp6)]"
  fi
  # allow-lan-access=false: the LAN goes into the tunnel (never direct);
  # true: the LAN is direct again, the internet still through a2.
  [[ $(s08d_kv "$f" route_lan_off) == *tailscale0* ]] || p="$p; allow-lan off: route to $S08D_GW [$(s08d_kv "$f" route_lan_off)]"
  [ "$(s08d_kv "$f" lan_off)" != "LANSEEN src=$A1" ] || p="$p; allow-lan off: LAN witness reached directly"
  [ "$(s08d_kv "$f" lanon_rc)" = 0 ] || p="$p; set allow-lan-access=true rc=[$(s08d_kv "$f" lanon_rc)]"
  [[ $(s08d_kv "$f" route_lan_on) != *tailscale0* ]] || p="$p; allow-lan on: route to $S08D_GW [$(s08d_kv "$f" route_lan_on)]"
  [ "$(s08d_kv "$f" lan_on)" = "LANSEEN src=$A1" ] || p="$p; allow-lan on: LAN witness [$(s08d_kv "$f" lan_on)]"
  [[ $(s08d_kv "$f" ping_gw_on) == 3/3* ]] || p="$p; allow-lan on: ping $S08D_GW [$(s08d_kv "$f" ping_gw_on)]"
  [ "$(s08d_kv "$f" tcp4_on)" = "INET-A src=203.0.113.1" ] || p="$p; allow-lan on: TCP $S08D_INET [$(s08d_kv "$f" tcp4_on)]"
  [ "$(s08d_kv "$f" unset_rc)" = 0 ] && [ "$(s08d_kv "$f" exit_after)" = none ] || p="$p; a1 exit node not unset [$(s08d_kv "$f" exit_after)]"
  r=$(s08d_exitnow "$A1"); [ "$r" = none ] || p="$p; a1 still uses an exit node [$r]"
  printf 'S08d.2 a1 --exit-node=%s (n1-a2): set -> first reply from %s %s ms (a1 clock); ping %s; TCP [%s]; IPv6 %s\n' \
    "$a2ip" "$S08D_INET" "$(s08d_kv "$f" first_reply_ms)" "$(s08d_kv "$f" ping4)" "$(s08d_kv "$f" tcp4)" "$v6" >>"$ART_DIR/timings.txt"
  printf 'S08d.2 allow-lan-access=false: route [%s]; ping %s [%s]; LAN witness [%s]\n' \
    "$(s08d_kv "$f" route_lan_off)" "$S08D_GW" "$(s08d_kv "$f" ping_gw_off)" "$(s08d_kv "$f" lan_off)" >>"$ART_DIR/timings.txt"
  printf 'S08d.2 allow-lan-access=true: route [%s]; ping %s [%s]; LAN witness [%s]; internet still [%s] ping [%s]\n' \
    "$(s08d_kv "$f" route_lan_on)" "$S08D_GW" "$(s08d_kv "$f" ping_gw_on)" "$(s08d_kv "$f" lan_on)" "$(s08d_kv "$f" tcp4_on)" "$(s08d_kv "$f" ping4_on)" >>"$ART_DIR/timings.txt"
  s08d_done S08d.2 "a1 via n1-a2 ($a2ip): ping 3/3 and TCP [INET-A src=203.0.113.1] (a2's SNAT); IPv6 $v6; allow-lan-access=false: LAN into the tunnel, witness [$(s08d_kv "$f" lan_off)], gateway ping [$(s08d_kv "$f" ping_gw_off)]; true: LAN direct (witness src=$A1), internet still via a2; exit node unset after"
}

t_s08d_3() {
  local p="" f st r t0 t1 t2 t3 what
  t0=$(s08c_t)
  st=$(s08d_setpol a "$S08D_POL_NOINET"); [ "$st" = 200 ] || p="$p; POST policy without autogroup:internet -> $st"
  s08c_until 30 s08d_opt_is "$A1" n1-a2 False && t1=$(s08c_t) || t1=-1
  r=$(s08d_opt "$A1" n1-a2)
  s08d_run "$A1" pol 60 "EXIT=n1-a2" "INET=$S08D_INET" "P=$S08D_PORT" <<'EOF' || p="$p; detached run on a1 did not finish"
setx sel --exit-node="$EXIT" || exit 0
R exit "$(exitst)"
R ping4 "$(pings "$INET")"
R tcp4 "$(banner "$INET" "$P")"
EOF
  f=$TMPD/s08d-pol.out
  if [ "$(s08d_kv "$f" sel_rc)" != 0 ]; then
    what="cannot select: offered=$r, set --exit-node=n1-a2 rc=$(s08d_kv "$f" sel_rc) [$(s08d_kv "$f" sel_out)]"
  else
    what="selected (offered=$r, status [$(s08d_kv "$f" exit)]) but ping [$(s08d_kv "$f" ping4)] TCP [$(s08d_kv "$f" tcp4)]"
    [[ $(s08d_kv "$f" ping4) == 0/* ]] && [[ $(s08d_kv "$f" tcp4) == -* ]] || p="$p; internet reached through n1-a2 without autogroup:internet: $what"
  fi
  [ "$(s08d_exitnow "$A1")" = none ] || p="$p; a1 still uses an exit node"
  ping_ok "$A1" n1-a2 || p="$p; a1 lost a2 under the tailnet-only policy"
  t2=$(s08c_t)
  st=$(s08d_setpol a "$S08D_POL_INET"); [ "$st" = 200 ] || p="$p; POST policy with autogroup:internet again -> $st"
  s08c_until 30 s08d_opt_is "$A1" n1-a2 True && t3=$(s08c_t) || { t3=-1; p="$p; a1 not offered n1-a2 after the grant came back"; }
  s08d_enabled_is a "$S08D_IDA" "$S08D_EXITR" || p="$p; a2's approval changed by the policy writes [$(s08c_enabled a "$S08D_IDA")]"
  printf 'S08d.3 policy without autogroup:internet: offer withdrawn after %s ms; a1 %s; grant back -> offered after %s ms\n' \
    "$((t1 < 0 ? -1 : t1 - t0))" "$what" "$((t3 < 0 ? -1 : t3 - t2))" >>"$ART_DIR/timings.txt"
  s08d_done S08d.3 "policy without autogroup:internet (If-Match): a1 $what; a2 still reachable; grant back -> n1-a2 offered again after $((t3 < 0 ? -1 : t3 - t2)) ms, approval kept"
}

t_s08d_4() {
  local p="" r f t0 t1 t2
  r=$(s08c_api a POST /api/v2/tailnet/-/keys "$S08D_KEYBODY"); [ "${r%% *}" = 200 ] || p="$p; tagged auth key -> ${r%% *}"
  py 'import json; print(json.loads(sys.argv[1].split(" ", 1)[1])["key"])' "$r" 2>/dev/null | strip_cr >"$TMPD/key-exit"
  S08D_KID=$(py 'import json; print(json.loads(sys.argv[1].split(" ", 1)[1])["id"])' "$r" 2>/dev/null | strip_cr)
  chmod 600 "$TMPD/key-exit"
  s08d_xr_start || p="$p; n1-ax1 netns/tailscaled"
  s08d_xr_up || p="$p; n1-ax1 up with the tagged key"
  S08D_IDX=$(s08c_dev a n1-ax1); [ -n "$S08D_IDX" ] || p="$p; n1-ax1 not in A's device list"
  [ "$(s08c_devfield a n1-ax1 tags)" = '["tag:exit"]' ] || p="$p; n1-ax1 tags [$(s08c_devfield a n1-ax1 tags)]"
  r=$(s08c_enabled a "$S08D_IDX"); [ -z "$r" ] || p="$p; n1-ax1 enabled before advertising [$r]"
  t0=$(s08c_t)
  s08d_xts "set --advertise-exit-node" >/dev/null 2>&1 || p="$p; n1-ax1 advertise-exit-node"
  # No approval call: autoApprovers.exitNode [tag:exit] enables both routes.
  s08c_until 30 s08d_enabled_is a "$S08D_IDX" "$S08D_EXITR" && t1=$(s08c_t) || { t1=-1; p="$p; n1-ax1 not auto-approved [$(s08c_enabled a "$S08D_IDX")]"; }
  s08c_until 30 s08d_opt_is "$A1" n1-ax1 True && t2=$(s08c_t) || { t2=-1; p="$p; a1 not offered n1-ax1 [$(s08d_opt "$A1" n1-ax1)]"; }
  s08d_run "$A1" auto 90 "EXIT=n1-ax1" "INET=$S08D_INET" "INET6=$S08D_INET6" "P=$S08D_PORT" <<'EOF' || p="$p; detached run on a1 did not finish"
setx sel --exit-node="$EXIT" --exit-node-allow-lan-access=false || exit 0
R first_reply_ms "$(firstreply "$INET" 40)"
R exit "$(exitst)"
R ping4 "$(pings "$INET")"
R tcp4 "$(banner "$INET" "$P")"
R tcp6 "$(banner "$INET6" "$P")"
EOF
  f=$TMPD/s08d-auto.out
  [ "$(s08d_kv "$f" sel_rc)" = 0 ] || p="$p; a1 set --exit-node=n1-ax1 rc=[$(s08d_kv "$f" sel_rc)] [$(s08d_kv "$f" sel_out)]"
  [[ $(s08d_kv "$f" ping4) == 3/3* ]] || p="$p; a1 ping $S08D_INET via n1-ax1 [$(s08d_kv "$f" ping4)]"
  [ "$(s08d_kv "$f" tcp4)" = "INET-A src=203.0.113.2" ] || p="$p; a1 TCP via n1-ax1 [$(s08d_kv "$f" tcp4)]"
  [ "$S08D_V6A" != 1 ] || [ "$(s08d_kv "$f" tcp6)" = "INET-A src=2001:db8:77::2" ] || p="$p; a1 TCP $S08D_INET6 via n1-ax1 [$(s08d_kv "$f" tcp6)]"
  [ "$(s08d_exitnow "$A1")" = none ] || p="$p; a1 still uses an exit node"
  printf 'S08d.4 autoApprovers.exitNode: n1-ax1 advertise -> enabled (no call) %s ms, -> a1 offered %s ms; a1 via n1-ax1: first reply %s ms (a1 clock), ping %s, TCP [%s], IPv6 TCP [%s]\n' \
    "$((t1 < 0 ? -1 : t1 - t0))" "$((t2 < 0 ? -1 : t2 - t0))" "$(s08d_kv "$f" first_reply_ms)" "$(s08d_kv "$f" ping4)" "$(s08d_kv "$f" tcp4)" "$(s08d_kv "$f" tcp6)" >>"$ART_DIR/timings.txt"
  s08d_done S08d.4 "tagged key (tag:exit) by A's API key; n1-ax1 advertised -> 0.0.0.0/0,::/0 enabled by autoApprovers.exitNode with no approval call ($((t1 < 0 ? -1 : t1 - t0)) ms); a1 via n1-ax1: ping 3/3, TCP [INET-A src=203.0.113.2]"
}

t_s08d_5() {
  local p="" r f st t0 t1 xip ka kx k bips what
  r=$(s08c_api b GET /api/v2/tailnet/-/acl "" "" "Accept: application/hujson")
  [ "${r%% *}" = 200 ] && S08D_ORIG_B=${r#* } || p="$p; B GET acl -> ${r%% *}"
  S08D_TOUCHED_B=1
  st=$(s08d_setpol b "$S08D_POL_B"); [ "$st" = 200 ] || p="$p; B POST policy (acls + autogroup:internet) -> $st"
  TS "$B2" "set --advertise-exit-node" >/dev/null 2>&1 || p="$p; b2 advertise-exit-node"
  s08c_until 30 s08d_adv_exit b "$S08D_IDB" || p="$p; b2 advertised routes [$(s08d_advertised b "$S08D_IDB")]"
  sleep 3
  r=$(s08d_opts "$B1"); [ -z "$r" ] || p="$p; b1 offered [$r] before B approved"
  # Keys across tailnets on exit-node devices: 404, nothing changes.
  r=$(s08c_api a POST "/api/v2/device/$S08D_IDB/routes" '{"routes":["0.0.0.0/0","::/0"]}'); [ "${r%% *}" = 404 ] || p="$p; A's key approving b2 -> ${r%% *}"
  r=$(s08c_api b POST "/api/v2/device/$S08D_IDA/routes" '{"routes":[]}'); [ "${r%% *}" = 404 ] || p="$p; B's key unapproving a2 -> ${r%% *}"
  r=$(s08c_api b POST "/api/v2/device/$S08D_IDX/routes" '{"routes":[]}'); [ "${r%% *}" = 404 ] || p="$p; B's key unapproving n1-ax1 -> ${r%% *}"
  r=$(s08c_api b GET "/api/v2/device/$S08D_IDX/routes"); [ "${r%% *}" = 404 ] || p="$p; B's key GET n1-ax1 routes -> ${r%% *}"
  r=$(s08c_enabled b "$S08D_IDB"); [ -z "$r" ] || p="$p; b2 enabled before B approved [$r]"
  t0=$(s08c_t)
  r=$(s08c_api b POST "/api/v2/device/$S08D_IDB/routes" '{"routes":["0.0.0.0/0","::/0"]}'); [ "${r%% *}" = 200 ] || p="$p; B approving b2 -> ${r%% *}"
  s08c_until 30 s08d_opt_is "$B1" n1-b2 True && t1=$(s08c_t) || { t1=-1; p="$p; b1 not offered n1-b2 [$(s08d_opt "$B1" n1-b2)]"; }
  # b1 sees only B: its exit nodes, peers, status and netmap carry nothing of A's.
  r=$(s08d_opts "$B1"); [ "$r" = n1-b2 ] || p="$p; b1 offered [$r]"
  r=$(peers "$B1"); [ "$r" = n1-b2 ] || p="$p; b1 peers [$r]"
  ka=$(selfkey "$A2"); kx=$(s08d_xts "status --json" 2>/dev/null | py 'import json; print(json.load(sys.stdin)["Self"]["PublicKey"][:16])' | strip_cr)
  [ -n "$ka" ] && [ -n "$kx" ] || p="$p; A exit node keys [$ka] [$kx]"
  for k in $ka $kx; do grep -q "$k" <<<"$(ts_json "$B1")$(netmap "$B1")" && p="$p; A key ${k:8:8} in b1's view"; done
  grep -qE 'n1-a2|n1-ax1|tag:exit' <<<"$(ts_json "$B1")$(netmap "$B1")" && p="$p; b1's status/netmap names A's exit nodes"
  r=$(s08d_opts "$A1"); [ "$r" = "n1-a2 n1-ax1" ] || p="$p; a1 offered [$r]"
  s08d_enabled_is a "$S08D_IDA" "$S08D_EXITR" && s08d_enabled_is a "$S08D_IDX" "$S08D_EXITR" || p="$p; A's approvals changed"
  # n1-ax1's address in A, tried by b1 only when no B node has it (per-tailnet IPAM: addresses overlap).
  xip=$(s08d_xts "ip -4" 2>/dev/null | strip_cr | head -1); bips=" $(selfip "$B1") $(selfip "$B2") "
  [[ $bips == *" $xip "* ]] && xip=""
  s08d_run "$B1" b 150 "XA=n1-a2" "XX=n1-ax1" "XIP=$xip" "EXIT=n1-b2" "INET=$S08D_INET" "INET6=$S08D_INET6" "P=$S08D_PORT" <<'EOF' || p="$p; detached run on b1 did not finish"
setx a2 --exit-node="$XA"; R exit_a2 "$(exitst)"; R tcp_a2 "$(banner "$INET" "$P")"
setx ax1 --exit-node="$XX"; R exit_ax1 "$(exitst)"; R tcp_ax1 "$(banner "$INET" "$P")"
if [ -n "$XIP" ]; then setx xip --exit-node="$XIP"; R exit_xip "$(exitst)"; R tcp_xip "$(banner "$INET" "$P")"; fi
$T set --exit-node= >/dev/null 2>&1
setx sel --exit-node="$EXIT" --exit-node-allow-lan-access=false || exit 0
R first_reply_ms "$(firstreply "$INET" 40)"
R exit "$(exitst)"
R ping4 "$(pings "$INET")"
R tcp4 "$(banner "$INET" "$P")"
R tcp6 "$(banner "$INET6" "$P")"
EOF
  f=$TMPD/s08d-b.out
  for k in a2 ax1 ${xip:+xip}; do
    [ "$(s08d_kv "$f" "${k}_rc")" != 0 ] || p="$p; b1 selected A's exit node ($k) [$(s08d_kv "$f" "exit_$k")]"
  done
  grep -q 'INET-A' "$f" && p="$p; b1 reached A's fake internet"
  [ "$(s08d_kv "$f" sel_rc)" = 0 ] || p="$p; b1 set --exit-node=n1-b2 rc=[$(s08d_kv "$f" sel_rc)] [$(s08d_kv "$f" sel_out)]"
  [[ $(s08d_kv "$f" ping4) == 3/3* ]] || p="$p; b1 ping $S08D_INET via n1-b2 [$(s08d_kv "$f" ping4)]"
  [ "$(s08d_kv "$f" tcp4)" = "INET-B src=203.0.113.1" ] || p="$p; b1 TCP via n1-b2 [$(s08d_kv "$f" tcp4)]"
  [ "$(s08d_exitnow "$B1")" = none ] || p="$p; b1 still uses an exit node"
  what="by name n1-a2 rc=$(s08d_kv "$f" a2_rc) [$(s08d_kv "$f" a2_out)], n1-ax1 rc=$(s08d_kv "$f" ax1_rc)"
  if [ -n "$xip" ]; then what="$what, by n1-ax1's address $xip rc=$(s08d_kv "$f" xip_rc) [$(s08d_kv "$f" xip_out)]"
  else what="$what (n1-ax1's address is also a B address: not tried)"; fi
  printf 'S08d.5 B: approval by B'"'"'s key -> b1 offered n1-b2 after %s ms; b1 selecting A'"'"'s exit nodes: %s; b1 via n1-b2: first reply %s ms, ping %s, TCP [%s], IPv6 TCP [%s]\n' \
    "$((t1 < 0 ? -1 : t1 - t0))" "$what" "$(s08d_kv "$f" first_reply_ms)" "$(s08d_kv "$f" ping4)" "$(s08d_kv "$f" tcp4)" "$(s08d_kv "$f" tcp6)" >>"$ART_DIR/timings.txt"
  s08d_done S08d.5 "B's own exit node n1-b2 (policy and approval by B's key; A's key on b2 404, B's key on a2/n1-ax1 404): b1 via n1-b2 gets [INET-B src=203.0.113.1] for the same address, never INET-A; b1 cannot select A's exit nodes ($what); no A exit node name, tag or key in b1's status/netmap; a1 offered n1-a2 n1-ax1 only"
}

t_s08d_6() {
  local p="" la sa lb sb
  la=$(TS "$A1" "exit-node list" 2>&1 | strip_cr); sa=$(TS "$A1" "exit-node suggest" 2>&1 | strip_cr)
  lb=$(TS "$B1" "exit-node list" 2>&1 | strip_cr); sb=$(TS "$B1" "exit-node suggest" 2>&1 | strip_cr)
  { echo "== a1: tailscale exit-node list"; echo "$la"; echo "== a1: tailscale exit-node suggest"; echo "$sa"
    echo "== b1: tailscale exit-node list"; echo "$lb"; echo "== b1: tailscale exit-node suggest"; echo "$sb"; } | redact >"$ART_DIR/logs/S08d-exit-node-list-suggest.txt"
  grep -q 'n1-a2' <<<"$la" && grep -q 'n1-ax1' <<<"$la" || p="$p; a1 list lacks A's exit nodes"
  grep -qE 'n1-b[12]' <<<"$la$sa" && p="$p; a1 list/suggest names a B node"
  grep -qE 'n1-a2|n1-ax1' <<<"$sa" || p="$p; a1 suggest names no A exit node [$(tr '\n' ' ' <<<"$sa" | head -c 160)]"
  grep -q 'n1-b2' <<<"$lb" || p="$p; b1 list lacks n1-b2"
  grep -qE 'n1-a[12]|n1-ax1' <<<"$lb$sb" && p="$p; b1 list/suggest names an A node"
  grep -q 'n1-b2' <<<"$sb" || p="$p; b1 suggest names no B exit node [$(tr '\n' ' ' <<<"$sb" | head -c 160)]"
  printf 'S08d.6 exit-node suggest: a1 [%s]; b1 [%s] (full output: logs/S08d-exit-node-list-suggest.txt)\n' \
    "$(grep -m1 -iE 'suggest|n1-' <<<"$sa" | head -c 160)" "$(grep -m1 -iE 'suggest|n1-' <<<"$sb" | head -c 160)" >>"$ART_DIR/timings.txt"
  s08d_done S08d.6 "exit-node list: a1 n1-a2 + n1-ax1 only, b1 n1-b2 only; exit-node suggest: a1 an A exit node, b1 n1-b2"
}

# S08d.0: undo, in the order the API needs (keys revoked last).
s08d_undo() {
  local s i r st
  for i in "$A1:a1" "$B1:b1"; do
    TS "${i%%:*}" "set --exit-node= --exit-node-allow-lan-access=false" >/dev/null 2>&1 || p="$p; ${i##*:} exit node unset"
    [ "$(s08d_exitnow "${i%%:*}")" = none ] || p="$p; ${i##*:} still uses an exit node"
  done
  TS "$A2" "set --advertise-exit-node=false" >/dev/null 2>&1 || p="$p; a2 advertise off"
  TS "$B2" "set --advertise-exit-node=false" >/dev/null 2>&1 || p="$p; b2 advertise off"
  if [ -n "$S08D_IDA" ]; then r=$(s08c_api a POST "/api/v2/device/$S08D_IDA/routes" '{"routes":[]}'); [ "${r%% *}" = 200 ] || p="$p; A unapprove a2 -> ${r%% *}"
    r=$(s08c_enabled a "$S08D_IDA"); [ -z "$r" ] || p="$p; a2 still approved [$r]"; fi
  if [ -n "$S08D_IDB" ]; then r=$(s08c_api b POST "/api/v2/device/$S08D_IDB/routes" '{"routes":[]}'); [ "${r%% *}" = 200 ] || p="$p; B unapprove b2 -> ${r%% *}"
    r=$(s08c_enabled b "$S08D_IDB"); [ -z "$r" ] || p="$p; b2 still approved [$r]"; fi
  s08d_xr_stop || p="$p; n1-ax1 netns not removed"
  [ -n "$S08D_IDX" ] || S08D_IDX=$(s08c_dev a n1-ax1)
  if [ -n "$S08D_IDX" ]; then r=$(s08c_api a DELETE "/api/v2/device/$S08D_IDX"); [ "${r%% *}" = 200 ] || p="$p; delete n1-ax1 -> ${r%% *}"; fi
  if [ -n "$S08D_KID" ]; then r=$(s08c_api a DELETE "/api/v2/tailnet/-/keys/$S08D_KID"); [ "${r%% *}" = 200 ] || p="$p; delete tagged key -> ${r%% *}"; fi
  if [ -n "$S08D_TOUCHED_A" ]; then
    st=000; [ -n "$S08D_ORIG_A" ] && st=$(s08d_setpol a "$S08D_ORIG_A")
    [ "$st" = 200 ] || { p="$p; policy a not restored by API ($st; operator fallback)"
      set_policy a '{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}],"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; policy a not restored"; }
  fi
  if [ -n "$S08D_TOUCHED_B" ]; then
    st=000; [ -n "$S08D_ORIG_B" ] && st=$(s08d_setpol b "$S08D_ORIG_B")
    [ "$st" = 200 ] || { p="$p; policy b not restored by API ($st; operator fallback)"
      set_policy b '{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' || p="$p; policy b not restored"; }
  fi
  vm "$A2" "sudo journalctl -u n1-s08d-inet -u n1-s08d-lan --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/S08d-inet-a2.log"
  vm "$B2" "sudo journalctl -u n1-s08d-inet --no-pager" 2>/dev/null | redact >"$ART_DIR/logs/S08d-inet-b2.log"
  for i in $A1 $A2 $B1 $B2; do s08d_vm_undo "$i" >/dev/null 2>&1 || p="$p; $i: S08d objects left"; done
  for s in a b; do
    i=$(py 'import json; print(json.load(open(sys.argv[1])).get("id", ""))' "$TMPD/s08d-key-$s.json" 2>/dev/null | strip_cr)
    [ -n "$i" ] && cli c1 "tailnet apikey revoke --slug $s --id $i" >/dev/null 2>&1 || p="$p; API key $s not revoked"
  done
  rm -f "$TMPD"/s08d-key-?.json "$TMPD"/api-? "$TMPD/key-exit" "$TMPD/s08c-hdr" "$TMPD"/s08d-*.out
  wait_peers "$A1" n1-a2 40 || p="$p; a1 peers after S08d [$(peers "$A1")]"
  wait_peers "$B1" n1-b2 40 || p="$p; b1 peers after S08d [$(peers "$B1")]"
  r=$(s08d_opts "$A1")$(s08d_opts "$B1"); [ -z "$r" ] || p="$p; exit nodes still offered [$r]"
}

t_s08d() {
  local p="" c S08C_APILOG=S08d-api.log
  S08D_ORIG_A="" S08D_ORIG_B="" S08D_TOUCHED_A="" S08D_TOUCHED_B="" S08D_KID="" S08D_IDA="" S08D_IDB="" S08D_IDX="" S08D_SETUP="" S08D_V6A=""
  if s08d_setup; then
    t_s08d_1; t_s08d_2; t_s08d_3; t_s08d_4; t_s08d_5; t_s08d_6
  else
    for c in 1 2 3 4 5 6; do record "S08d.$c" FAIL "not run: setup [$S08D_SETUP]"; done
  fi
  s08d_undo; p=${p#; }
  [ -z "$S08D_SETUP$p" ] && record S08d.0 PASS "setup and undo: exit node unset on a1/b1, advertise off, approvals removed by each tenant's API key, n1-ax1 and its tagged key deleted by API, policies A and B restored by API (If-Match), API keys revoked, netns/bridge/nft/forwarding/units removed" \
    || record S08d.0 FAIL "${S08D_SETUP:+setup: $S08D_SETUP; }${p:+undo: $p}"
}
