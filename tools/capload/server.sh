#!/usr/bin/env bash
# Server half of the P00c server-only capacity harness (see README.md).
# Runs on the server host (mc-bench). Subcommands:
#
#   server.sh seed  SNAPDIR NODES ADDR   seeded snapshot via capseed: db.sqlite,
#                                        nodes.keys, users.keys, db.sha256
#   server.sh start RUNDIR POLICY ADDR [SNAPDIR]
#                                        start headscale (from a copy of the
#                                        snapshot's db) + 1 s RSS/CPU sampler
#   server.sh setup RUNDIR USERS         create users + reusable keys -> RUNDIR/keys.txt (0600)
#   server.sh stop  RUNDIR               scrape metrics, stop, summarise samples
#
# POLICY is allow_all | isolated | personal_devices (TestScale's policies).
# INCREMENTAL=1 (with RECONCILE_INTERVAL, FULL_MAP_INTERVAL) turns on
# tuning.incremental_netmap (P00d).
# ADDR is the listen address clients reach, e.g. 192.168.1.242:8080.
# Metrics stay on 127.0.0.1. Keys are lab-only, short-lived test credentials
# and are never printed.
set -euo pipefail

HS=${HS:-$HOME/p00c/bin/headscale}
cmd=$1
dir=$2

policy_json() {
	case $1 in
	allow_all) echo '{"grants": [{"src": ["*"], "dst": ["*"], "ip": ["tcp:443"]}]}' ;;
	isolated) echo '{"acls": []}' ;;
	personal_devices) echo '{"grants": [{"src": ["autogroup:member"], "dst": ["autogroup:self"], "ip": ["tcp:443"]}]}' ;;
	*)
		echo "unknown policy $1" >&2
		exit 2
		;;
	esac
}

hs() { "$HS" -c "$dir/config.yaml" "$@"; }

write_config() { # dir policy addr
	local dir=$1 policy=$2 addr=$3
	policy_json "$policy" >"$dir/policy.json"
	cat >"$dir/derp.yaml" <<'EOF'
regions:
  900:
    regionid: 900
    regioncode: lab
    regionname: Lab placeholder
    nodes:
      - name: 900a
        regionid: 900
        hostname: derp.invalid
        stunport: -1
EOF
	cat >"$dir/config.yaml" <<EOF
server_url: http://$addr
disable_check_updates: true
listen_addr: $addr
metrics_listen_addr: 127.0.0.1:9090
unix_socket: $dir/headscale.sock
unix_socket_permission: "0700"
noise:
  private_key_path: $dir/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
policy:
  mode: file
  path: $dir/policy.json
log:
  level: warn
derp:
  server:
    enabled: false
  urls: []
  paths: [$dir/derp.yaml]
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: $dir/db.sqlite
    write_ahead_log: true
dns:
  magic_dns: true
  base_domain: cap.internal
  override_local_dns: false
logtail:
  enabled: false
cli:
  timeout: 300s
EOF
	# P00d: INCREMENTAL=1 turns the incremental netmap on.
	if [ "${INCREMENTAL:-0}" = 1 ]; then
		cat >>"$dir/config.yaml" <<EOF
tuning:
  incremental_netmap:
    enabled: true
    reconcile_interval: ${RECONCILE_INTERVAL:-1m}
    full_map_interval: ${FULL_MAP_INTERVAL:-15m}
EOF
	fi
}

case $cmd in
seed)
	nodes=$3 addr=$4
	rm -rf "$dir" && mkdir -p "$dir" && chmod 700 "$dir"
	write_config "$dir" isolated "$addr"
	"${CAPSEED:-$HOME/p00c/bin/capseed}" -config "$dir/config.yaml" -out "$dir" -nodes "$nodes" | tee "$dir/seed.txt"
	sha256sum "$dir/db.sqlite" | tee "$dir/db.sha256"
	if ls "$dir"/db.sqlite-* >/dev/null 2>&1; then echo "unexpected WAL left behind" >&2; exit 1; fi
	;;
start)
	policy=$3 addr=$4 snap=${5:-}
	# One cell at a time: a second server would share the port and host.
	if pgrep -f "$HS.* serve" >/dev/null; then
		echo "a harness server is already running" >&2
		exit 1
	fi
	rm -rf "$dir" && mkdir -p "$dir" && chmod 700 "$dir"
	write_config "$dir" "$policy" "$addr"
	if [ -n "$snap" ]; then
		cp "$snap/db.sqlite" "$dir/db.sqlite"
	fi
	ulimit -n 200000
	GOMAXPROCS=${GOMAXPROCS:-8} nohup "$HS" -c "$dir/config.yaml" serve >"$dir/server.log" 2>&1 &
	pid=$!
	echo $pid >"$dir/server.pid"
	# Sampler: unix time, RSS KiB, cumulative user+sys CPU ticks, threads.
	nohup bash -c '
		pid=$1 out=$2
		echo "t,rss_kb,cpu_ticks,threads" >"$out"
		while kill -0 "$pid" 2>/dev/null; do
			rss=$(awk "/^VmRSS/{print \$2}" /proc/$pid/status 2>/dev/null || echo 0)
			thr=$(awk "/^Threads/{print \$2}" /proc/$pid/status 2>/dev/null || echo 0)
			cpu=$(awk "{print \$14+\$15}" /proc/$pid/stat 2>/dev/null || echo 0)
			echo "$(date +%s.%N),$rss,$cpu,$thr" >>"$out"
			sleep 1
		done' _ "$pid" "$dir/samples.csv" >/dev/null 2>&1 &
	echo $! >"$dir/sampler.pid"
	# Startup includes the first full peer-map build.
	t0=$(date +%s.%N)
	for _ in $(seq 1 1200); do
		if curl -fsS "http://$addr/health" >/dev/null 2>&1; then
			echo "server up pid=$pid startup_s=$(echo "$(date +%s.%N) - $t0" | bc)"
			exit 0
		fi
		sleep 0.5
	done
	echo "server did not come up" >&2
	tail -20 "$dir/server.log" >&2
	exit 1
	;;
setup)
	users=$3
	t0=$(date +%s.%N)
	# Users, then one reusable key per user, 32 CLI calls at a time.
	seq 0 $((users - 1)) | xargs -P 32 -I{} \
		"$HS" -c "$dir/config.yaml" users create "cap-user-{}" >/dev/null
	t1=$(date +%s.%N)
	mkdir -p "$dir/keys.d"
	seq 0 $((users - 1)) | xargs -P 32 -I{} sh -c \
		"'$HS' -c '$dir/config.yaml' -o json preauthkeys create -u cap-user-{} --reusable -e 24h > '$dir/keys.d/{}.json'"
	t2=$(date +%s.%N)
	umask 077
	: >"$dir/keys.txt"
	for i in $(seq 0 $((users - 1))); do
		jq -r .key "$dir/keys.d/$i.json" >>"$dir/keys.txt"
	done
	rm -rf "$dir/keys.d"
	echo "setup users=$users create_users_s=$(echo "$t1 - $t0" | bc) create_keys_s=$(echo "$t2 - $t1" | bc)" | tee "$dir/setup.txt"
	;;
stop)
	curl -fsS http://127.0.0.1:9090/metrics 2>/dev/null | grep -E '^(headscale_|go_memstats_heap|go_gc|process_)' >"$dir/metrics.txt" || true
	pid=$(cat "$dir/server.pid")
	kill "$pid" 2>/dev/null || true
	for _ in $(seq 1 60); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
	kill -9 "$pid" 2>/dev/null || true
	kill "$(cat "$dir/sampler.pid")" 2>/dev/null || true
	rm -f "$dir/keys.txt"
	;;
*)
	echo "usage: $0 start|setup|stop RUNDIR ..." >&2
	exit 2
	;;
esac
