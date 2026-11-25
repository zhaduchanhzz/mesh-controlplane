#!/bin/bash
# LAB-stage2-n1 checks L01-L08 (infra/lab/stage2-n1.md). From the laptop,
# through mc-ci (the only management path into 10.77.0.0/16).
set -u
K=(-i "$HOME/.mesh-lab/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
PX="ProxyCommand=ssh -i $HOME/.mesh-lab/mesh-lab -o IdentitiesOnly=yes -o BatchMode=yes mesh@192.168.1.240 nc -q0 %h %p"
vm() { local ip=$1; shift; ssh "${K[@]}" -o "$PX" "mesh@$ip" "$@"; }
A1=10.77.20.11 A2=10.77.20.12 B1=10.77.30.11 B2=10.77.30.12 R=10.77.40.30
FAILS=0
rec() { printf '%-4s %-4s %s\n' "$2" "$1" "$3"; [ "$2" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# tcp IP PORT from VM: open | refused | timeout
tcp() { vm "$1" "timeout 4 bash -c '</dev/tcp/$2/$3' 2>&1 && echo open || { r=\$?; [ \$r = 124 ] && echo timeout || echo refused; }" 2>/dev/null | tail -1; }
pingok() { vm "$1" "ping -c2 -W2 $2 >/dev/null 2>&1 && echo ok || echo no" 2>/dev/null; }

echo "== hosts"; for ip in $A1 $A2 $B1 $B2 $R; do echo "$ip $(vm $ip hostname 2>&1 | tail -1)"; done

# Host drop counter for A<->B (read before/after).
ctr() { python "${PVE_SHELL:?PVE_SHELL= path to a root-shell helper for the PVE host}" <<'EOF' 2>/dev/null | grep -oE 'packets [0-9]+' | awk '{s+=$2} END {print s+0}'
nft list chain inet mclab forward | grep -E "10.77.(20|30).0/24 ip daddr 10.77.(30|20).0/24 counter"
EOF
}

# L01
p=""; [ "$(pingok $A1 $A2)" = ok ] || p="$p a1->a2"; [ "$(pingok $B1 $B2)" = ok ] || p="$p b1->b2"
[ -z "$p" ] && rec L01 PASS "a1<->a2 and b1<->b2 reach each other on their own underlay" || rec L01 FAIL "no ping:$p"

# L02
c0=$(ctr)
p=""
[ "$(pingok $A1 $B1)" = no ] || p="$p ping a1->b1"
[ "$(pingok $B1 $A1)" = no ] || p="$p ping b1->a1"
t=$(tcp $A1 $B1 22); [ "$t" = timeout ] || p="$p tcp a1->b1:22=$t"
t=$(tcp $B2 $A2 22); [ "$t" = timeout ] || p="$p tcp b2->a2:22=$t"
vm $B1 "timeout 8 nc -u -l 40000 >/tmp/udp-in 2>/dev/null & sleep 1; true" >/dev/null 2>&1
vm $A1 "echo hi | timeout 2 nc -u -w1 $B1 40000" >/dev/null 2>&1
sleep 7; u=$(vm $B1 "cat /tmp/udp-in 2>/dev/null; rm -f /tmp/udp-in" 2>/dev/null)
[ -z "$u" ] || p="$p udp a1->b1 delivered"
c1=$(ctr)
[ "${c1:-0}" -gt "${c0:-0}" ] || p="$p drop counter $c0 -> $c1"
[ -z "$p" ] && rec L02 PASS "A<->B blocked: ping, tcp/22, udp both ways; host drop counter $c0 -> $c1" || rec L02 FAIL "$p"

# L03
p=""
t=$(tcp $A1 $R 443); [ "$t" = refused ] || [ "$t" = open ] || p="$p a1->relay:443=$t"
t=$(tcp $B1 $R 443); [ "$t" = refused ] || [ "$t" = open ] || p="$p b1->relay:443=$t"
t=$(tcp $A1 $R 22); [ "$t" = timeout ] || p="$p a1->relay:22=$t"
vm $R "timeout 8 nc -u -l 3478 >/tmp/stun-in 2>/dev/null & sleep 1; true" >/dev/null 2>&1
vm $A1 "echo stun | timeout 2 nc -u -w1 $R 3478" >/dev/null 2>&1
sleep 7; u=$(vm $R "cat /tmp/stun-in 2>/dev/null; rm -f /tmp/stun-in" 2>/dev/null)
[ "$u" = stun ] || p="$p udp a1->relay:3478 not delivered"
[ -z "$p" ] && rec L03 PASS "clients reach the relay on tcp/443 (refused: nothing listens yet) and udp/3478 (datagram delivered); relay tcp/22 from a client: timeout" || rec L03 FAIL "$p"

# L04
p=""
for v in $A1 $B1 $R; do
  for d in "192.168.1.241 22" "192.168.1.243 6432" "192.168.1.1 80"; do
    set -- $d; t=$(tcp $v $1 $2); [ "$t" = timeout ] || p="$p $v->$1:$2=$t"
  done
done
[ -z "$p" ] && rec L04 PASS "lab VMs reach nothing on the home LAN (mc-cp:22, mc-db:6432, router:80 time out)" || rec L04 FAIL "$p"

# L05
p=""
for v in $A1 $B1 $R; do
  gw=${v%.*}.1
  for d in "$gw 22" "$gw 8006" "192.168.1.14 22" "192.168.1.14 8006"; do
    set -- $d; t=$(tcp $v $1 $2); [ "$t" = timeout ] || p="$p $v->$1:$2=$t"
  done
done
[ -z "$p" ] && rec L05 PASS "lab VMs reach no host service (gateway and 192.168.1.14, tcp/22 and 8006)" || rec L05 FAIL "$p"

# L06
p=""
for v in $A1 $B2 $R; do
  c=$(vm $v "curl -s -o /dev/null -w '%{http_code}' -m 10 https://c2.cp.gta5web.online:8445/ready" 2>/dev/null); [ "$c" = 200 ] || p="$p $v c2=$c"
  c=$(vm $v "curl -s -o /dev/null -w '%{http_code}' -m 10 https://a.cp.gta5web.online:8443/health" 2>/dev/null); [ "$c" = 200 ] || p="$p $v poc=$c"
done
[ -z "$p" ] && rec L06 PASS "lab VMs reach the public control endpoints (c2 :8445/ready, POC :8443/health) via SNAT and the VPS" || rec L06 FAIL "$p"

# L07
p=""; for ip in $A1 $A2 $B1 $B2 $R; do vm $ip true >/dev/null 2>&1 || p="$p $ip"; done
[ -z "$p" ] && rec L07 PASS "mc-ci reaches every lab VM over ssh" || rec L07 FAIL "no ssh:$p"

# L08
p=""
for h in 240 241 243 244 245 246; do ssh "${K[@]}" mesh@192.168.1.$h true >/dev/null 2>&1 || p="$p .$h"; done
c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 https://c2.cp.gta5web.online:8445/ready); [ "$c" = 200 ] || p="$p c2=$c"
c=$(curl -s -o /dev/null -w '%{http_code}' -m 10 https://a.cp.gta5web.online:8443/health); [ "$c" = 200 ] || p="$p poc=$c"
[ -z "$p" ] && rec L08 PASS "existing VMs (ci, cp, db, c2, c1b, c2b) reachable; public c2 and POC 200" || rec L08 FAIL "$p"

echo "FAILS=$FAILS"
