#!/usr/bin/env bash
# P13 operational drills: helpers shared by canary.sh (P13d), noise-rotate.sh
# (P13e), relay-restart.sh (P13g) and f08.sh (P13c). Sourced, never run.
#
# 1. p13_overlay: the canary, Noise-rotation and relay drills run inside the
#    N1 E2E harness (infra/lab/e2e/n1/run.sh: two clusters c1/c2 with active
#    and standby processes, own DERP relay, client VMs, cleanup that restores
#    the P02 POC and the long-lived c2) without editing it. p13_overlay copies
#    the N1 directory to infra/lab/e2e/.p13-<drill>-<pid>/ (same depth, so
#    N1's REPO path still resolves), adds the drill file as cases-p13.sh,
#    sources it before CASES_ALL, lists the drill's cases in CASES_ALL, calls
#    p13_cleanup (defined by the drill) first thing in N1's cleanup, and moves
#    the artifacts to artifacts/<commit>/P13/<drill>-<run>-<mode>-<V>/. The
#    copy is removed when the run ends (and stale copies of dead runs at the
#    next start).
# 2. Helpers for the drill cases sourced into that run: docker clients on
#    mc-ci, pingers on mc-ci and on the client VMs, gap statistics.
#
# The N1 operating rules hold: never edit a file of a running drill (bash
# reads scripts incrementally; the overlay copy protects the repo files, not
# the copy); run in the foreground of a background task.

# p13_overlay DRILL SELF ADD_CASES CASES: run N1 with the drill's cases.
p13_overlay() {
  local drill=$1 self=$2 add=$3 cases=$4
  local here repo n1 tmp d pid rc child
  here=$(cd "$(dirname "$self")" && pwd)
  repo=$(cd "$here/../../../.." && pwd)
  n1=$repo/infra/lab/e2e/n1
  [ -f "$n1/run.sh" ] || { echo "p13: $n1/run.sh missing" >&2; return 2; }
  for d in "$repo"/infra/lab/e2e/.p13-*; do
    [ -d "$d" ] || continue
    pid=${d##*-}
    [[ $pid =~ ^[0-9]+$ ]] && ! kill -0 "$pid" 2>/dev/null && rm -rf "$d"
  done
  tmp=$repo/infra/lab/e2e/.p13-$drill-$$
  mkdir -p "$tmp" || return 2
  cp "$n1"/*.sh "$n1"/*.py "$tmp"/ || { rm -rf "$tmp"; return 2; }
  cp "$self" "$tmp/cases-p13.sh" && cp "$here/lib-ops.sh" "$tmp/lib-ops.sh" || { rm -rf "$tmp"; return 2; }
  # Anchors the overlay relies on; a changed N1 harness stops the drill
  # before anything on the lab is touched.
  if ! grep -q '^CASES_ALL="' "$n1/run.sh" || ! grep -q '^  log "cleanup"$' "$n1/run.sh" \
    || ! grep -q '/N1/e2e-\$RUN_ID' "$n1/run.sh"; then
    echo "p13: infra/lab/e2e/n1/run.sh changed shape (CASES_ALL, cleanup log or ART_DIR anchor missing)" >&2
    rm -rf "$tmp"; return 2
  fi
  awk -v add="$add" -v drill="$drill" '
    /^CASES_ALL="/ { print ". \"$(dirname \"$0\")/cases-p13.sh\""; sub(/^CASES_ALL="/, "CASES_ALL=\"" add " ") }
    /^  log "cleanup"$/ { print; print "  declare -F p13_cleanup >/dev/null && p13_cleanup"; next }
    { gsub(/\/N1\/e2e-\$RUN_ID/, "/P13/" drill "-$RUN_ID"); print }' "$n1/run.sh" >"$tmp/run.sh" || { rm -rf "$tmp"; return 2; }
  grep -q 'cases-p13.sh' "$tmp/run.sh" && grep -q 'p13_cleanup' "$tmp/run.sh" && grep -q "/P13/$drill-" "$tmp/run.sh" \
    || { echo "p13: overlay of run.sh failed" >&2; rm -rf "$tmp"; return 2; }
  export P13_DRILL=$drill
  CASES=$cases bash "$tmp/run.sh" </dev/null &
  child=$!
  trap 'kill -TERM $child 2>/dev/null' INT TERM
  rc=0; wait "$child" || rc=$?
  while kill -0 "$child" 2>/dev/null; do wait "$child"; rc=$?; done
  trap - INT TERM
  rm -rf "$tmp"
  return "$rc"
}

# ------------------------------------------------- helpers inside a run ----
# They use the N1 harness's functions and variables (ssh_, vm, log, record,
# redact, py, strip_cr, now_ms, CI_SSH, TMPD, ART_DIR, URL1, URL2, ...).

P13CI=/dev/shm/p13x            # work dir on mc-ci (linger: see N1 notes)
P13VM=/run/p13x                # work dir on a client VM
P13CIRUN="sudo systemd-run --quiet --collect --uid=mesh --gid=mesh -p SupplementaryGroups=docker -p RuntimeMaxSec=7200"

ci_() { ssh_ "$CI_SSH" "$@"; }

# The pinger script (one probe every 200 ms, lines "<epoch ms> <rc>"), as
# P12b's jobs.sh ping: a script file with plain arguments because systemd
# expands $VARS of an inline command.
p13_pinger_src() {
  cat <<'EOF'
#!/usr/bin/env bash
# p13 pinger: ping.sh docker CONTAINER IP OUT STOP [MAXSEC] | ping.sh host IP OUT STOP [MAXSEC]
set -u
kind=$1; shift
if [ "$kind" = docker ]; then c=$1; shift; fi
ip=$1 out=$2 stop=$3 max=${4:-1800}
end=$(($(date +%s) + max))
while [ ! -e "$stop" ] && [ "$(date +%s)" -lt "$end" ]; do
  t=$(date +%s%3N)
  if [ "$kind" = docker ]; then timeout 3 docker exec "$c" ping -c 1 -W 1 "$ip" >/dev/null 2>&1
  else timeout 3 ping -c 1 -W 1 "$ip" >/dev/null 2>&1; fi
  echo "$t $?"
  sleep 0.2
done >"$out" 2>&1
EOF
}
p13_ci_prepare() {
  ci_ "mkdir -p $P13CI && chmod 700 $P13CI && cat >$P13CI/ping.sh && chmod 755 $P13CI/ping.sh" < <(p13_pinger_src) || return 1
  ci_ "cat >$P13CI/peers.py" <<'EOF' || return 1
# peers.py NAME...: per docker container "<name> <BackendState> <peer,peer|->"
import json, subprocess, sys
for n in sys.argv[1:]:
    try:
        out = subprocess.run(["docker", "exec", n, "tailscale", "status", "--json"],
                             capture_output=True, timeout=10).stdout
        d = json.loads(out or b"{}")
        peers = sorted(p.get("HostName", "?") for p in (d.get("Peer") or {}).values())
        print(n, d.get("BackendState", "?"), ",".join(peers) or "-")
    except Exception as e:  # noqa: BLE001
        print(n, "error", "-")
EOF
}
# ci_pinger_start NAME CONTAINER IP / ci_pinger_stop NAME (-> $ART_DIR/logs/NAME.ping)
ci_pinger_start() {
  ci_ "rm -f $P13CI/$1.stop $P13CI/$1.ping; sudo systemctl stop p13x-ping-$1 2>/dev/null; sudo systemctl reset-failed p13x-ping-$1 2>/dev/null; \
    $P13CIRUN --unit=p13x-ping-$1 bash $P13CI/ping.sh docker $2 $3 $P13CI/$1.ping $P13CI/$1.stop"
}
ci_pinger_stop() {
  ci_ "touch $P13CI/$1.stop; for i in \$(seq 1 20); do systemctl is-active -q p13x-ping-$1 || break; sleep 0.25; done; \
    sudo systemctl stop p13x-ping-$1 2>/dev/null; cat $P13CI/$1.ping" | strip_cr >"$ART_DIR/logs/$1.ping"
}
# ci_pinger_snap NAME OUT: the pinger file so far; ci_now_ms: mc-ci's clock
# (pinger timestamps are mc-ci's: windows must use it, not the laptop's).
ci_pinger_snap() { ci_ "cat $P13CI/$1.ping" | strip_cr >"$2"; }
ci_now_ms() { ci_ 'date +%s%3N' | strip_cr; }
# vm_pinger_start IP NAME TARGET_IP / vm_pinger_stop IP NAME
vm_pinger_start() {
  vm "$1" "sudo install -d -m 0755 $P13VM && sudo tee $P13VM/ping.sh >/dev/null && sudo chmod 755 $P13VM/ping.sh && \
    sudo rm -f $P13VM/$2.stop $P13VM/$2.ping; sudo systemctl stop p13x-ping-$2 2>/dev/null; sudo systemctl reset-failed p13x-ping-$2 2>/dev/null; \
    sudo systemd-run --quiet --collect --unit=p13x-ping-$2 -p RuntimeMaxSec=3600 bash $P13VM/ping.sh host $3 $P13VM/$2.ping $P13VM/$2.stop" < <(p13_pinger_src)
}
vm_pinger_stop() {
  vm "$1" "sudo touch $P13VM/$2.stop; for i in \$(seq 1 20); do systemctl is-active -q p13x-ping-$2 || break; sleep 0.25; done; \
    sudo systemctl stop p13x-ping-$2 2>/dev/null; sudo cat $P13VM/$2.ping" | strip_cr >"$ART_DIR/logs/$2.ping"
}
# gap_stats FILE [FROM_MS] [TO_MS]: "<max gap ms between answered probes>
# <answered> <lost>" from a pinger file, counting only probes in
# [FROM_MS, TO_MS] (default all).
gap_stats() {
  py 'import sys
frm = int(sys.argv[1]) if len(sys.argv) > 1 and sys.argv[1] else 0
to = int(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2] else 1 << 62
ok, lost = [], 0
for l in sys.stdin:
    f = l.split()
    if len(f) != 2 or not f[0].isdigit():
        continue
    t, rc = int(f[0]), f[1]
    if t < frm or t > to:
        continue
    if rc == "0":
        ok.append(t)
    else:
        lost += 1
g = max((b - a for a, b in zip(ok, ok[1:])), default=-1)
print(g, len(ok), lost)' "${2:-}" "${3:-}" <"$1" | strip_cr
}

# Docker clients on mc-ci (as P11b/P12b): p13_ts_start NAME IMAGE LABEL,
# p13_ts_up NAME URL KEYFILE, p13_peers NAME... (lines from peers.py).
p13_ts_start() {
  ci_ "docker run -d --name $1 --hostname $1 --label $3 --cap-add=NET_ADMIN --cap-add=NET_RAW --device=/dev/net/tun \
    --mount type=volume,src=$1,dst=/var/lib/tailscale,volume-label=$3 $2 \
    tailscaled --state=/var/lib/tailscale/tailscaled.state >/dev/null && \
    for i in \$(seq 1 60); do docker exec $1 tailscale status >/dev/null 2>&1; [ \$? -le 1 ] && exit 0; sleep 0.5; done; exit 1"
}
p13_ts_up() {
  ci_ "docker exec -i $1 timeout 60 tailscale up --login-server=$2 --auth-key=file:/dev/stdin --hostname=$1 ${4:-}" \
    <"$3" 2>&1 | redact >>"$ART_DIR/logs/$1.cli.log"
  return "${PIPESTATUS[0]}"
}
p13_peers() { ci_ "python3 $P13CI/peers.py $*" 2>/dev/null | strip_cr; }
p13_ts_ip() { ci_ "docker exec $1 tailscale ip -4" 2>/dev/null | strip_cr | head -1; }
p13_needs_login() { ci_ "docker logs $1 2>&1" | grep -c 'NeedsLogin' || true; }
p13_ci_cleanup() { # LABEL
  local c
  for c in $(ci_ "docker ps -a --filter label=$1 --format '{{.Names}}'" 2>/dev/null | strip_cr); do
    ci_ "docker logs $c 2>&1" | redact >"$ART_DIR/logs/$c.tailscaled.log" 2>/dev/null || true
  done
  ci_ "for u in \$(systemctl list-units --plain --no-legend 'p13x-ping-*' | awk '{print \$1}'); do sudo systemctl stop \$u; done; \
    docker ps -aq --filter label=$1 | xargs -r docker rm -f >/dev/null; \
    docker volume ls -q --filter label=$1 | xargs -r docker volume rm >/dev/null; rm -rf $P13CI; \
    test -z \"\$(docker ps -aq --filter label=$1)\""
}
# node_map CLUSTER SLUG: "hostname=id/ipv4 ..." of the tailnet (operator CLI).
p13_node_map() {
  cli "$1" "tailnet nodes --slug $2" 2>/dev/null \
    | py "import json; print(' '.join(f\"{n['hostname']}={n['id']}/{n['ipv4']}\" for n in sorted(json.load(sys.stdin), key=lambda n: n['hostname'])))" | strip_cr
}
# server_key URL: hex of the public Noise key a cluster URL serves (/key,
# "mkey:" stripped; public, not a secret).
p13_server_key() { curl -s -m 5 "$1/key?v=88" | py 'import json; print(json.load(sys.stdin).get("publicKey", "").removeprefix("mkey:"))' 2>/dev/null | strip_cr; }
# set_policy2 SLUG JSON: policy of a tailnet owned by c2 (N1's set_policy
# knows only a, b and c).
p13_policy_c2() {
  local pf=/var/lib/headscale/cluster/p13-policy.hujson
  printf '%s\n' "$2" | on c2 "sudo install -o headscale -g headscale -m 0640 /dev/stdin $pf && $CLI2 tailnet policy --slug $1 --file $pf; rc=\$?; sudo rm -f $pf; exit \$rc" \
    > >(redact >>"$ART_DIR/logs/policy.log") 2>&1
}
