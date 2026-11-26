#!/usr/bin/env bash
# Server half of the P14a cluster-mode harness (tools/capload/README.md):
# `headscale cluster serve` on PostgreSQL behind PgBouncer, one process per
# call (active, or a standby of the same cluster), with the 1 s RSS/CPU
# sampler and a 10 s scrape of the loopback metrics listener. Also reached
# as `server.sh cluster-<sub> …`.
#
#   cluster.sh config   RUNDIR HOST:PORT LISTEN   write RUNDIR/config.yaml
#   cluster.sh migrate  RUNDIR                    create or migrate the schema (P13d: serve, register and
#                                                 capseed never do; run it once on a fresh database)
#   cluster.sh register RUNDIR                    register CLUSTER_ID (CLASS, MAX_DEVICES); exists = ok
#   cluster.sh start    RUNDIR [active|standby]   start serve + samplers; wait /ready 200 (active) or 503 (standby)
#   cluster.sh restart  RUNDIR                    stop, start active (reload users after capseed)
#   cluster.sh stop     RUNDIR                    scrape metrics, SIGTERM, stop samplers
#   cluster.sh manifest RUNDIR                    RUNDIR/manifest-server.json (binary, config, host)
#
# Environment:
#   HS            headscale binary (default ~/p14/bin/headscale)
#   CLUSTER_ID    default bench-c1;  CLASS dedicated|shared (default dedicated); MAX_DEVICES (default 100000)
#   PG_HOST PG_PORT PG_DB PG_USER  PostgreSQL via PgBouncer (default 127.0.0.1 6432 headscale headscale)
#   PG_PASSFILE   file holding the password (0600; referenced as pass: file:…, never copied)
#   METRICS       loopback metrics listener (default 127.0.0.1:9090; a standby on the same host needs another)
#   NOISE_KEY     Noise private key path, shared by active and standby (default RUNDIR/../noise_private.key);
#                 missing: the active creates it with --init-noise-key (P13e: only while the
#                 registry names no key for the cluster), a standby refuses to start
#   MAX_PEER_ENTRIES (default 1000000), INCREMENTAL=1 (+RECONCILE_INTERVAL, FULL_MAP_INTERVAL)
#   PG_MAX_OPEN   database.postgres.max_open_conns (default 10, as production)
#   GOMAXPROCS    default: all CPUs of the host
#   HEADSCALE_BENCH_MUTEX_FRACTION / HEADSCALE_BENCH_BLOCK_RATE  passed through (benchpprof builds)
#
# The listener serves only the hostname of server_url (P08): clients reach
# it as http://HOST:PORT, resolving HOST themselves (capload -resolve).
# Metrics and pprof (benchpprof builds) stay on loopback. No secret is
# printed: the PostgreSQL password is only a file reference in the config.
set -euo pipefail

HS=${HS:-$HOME/p14/bin/headscale}
CLUSTER_ID=${CLUSTER_ID:-bench-c1}
cmd=$1
dir=$2
cfg=$dir/config.yaml

hsc() { "$HS" -c "$cfg" cluster --cluster-id "$CLUSTER_ID" "$@"; }

metrics_addr() { awk '/^metrics_listen_addr:/{print $2}' "$cfg"; }
listen_addr() { awk '/^listen_addr:/{print $2}' "$cfg"; }
server_host() { awk -F'[/:]' '/^server_url:/{print $5}' "$cfg"; }

# ready_code: /ready over the cluster's own hostname, pinned to the listen
# address (the host guard answers 421 to any other Host).
ready_code() {
	local la host port
	la=$(listen_addr)
	host=$(server_host)
	port=${la##*:}
	curl -s -o /dev/null -w '%{http_code}' -m 3 --resolve "$host:$port:${la%:*}" "http://$host:$port/$1" || true
}

case $cmd in
config)
	hostport=$3 listen=$4
	mkdir -p "$dir" && chmod 700 "$dir"
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
	cat >"$cfg" <<EOF
server_url: http://$hostport
listen_addr: $listen
metrics_listen_addr: ${METRICS:-127.0.0.1:9090}
unix_socket: $dir/headscale.sock
unix_socket_permission: "0700"
disable_check_updates: true
noise:
  private_key_path: ${NOISE_KEY:-$(cd "$dir/.." && pwd)/noise_private.key}
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
policy:
  mode: database
derp:
  server:
    enabled: false
  urls: []
  paths: [$dir/derp.yaml]
  auto_update_enabled: false
database:
  type: postgres
  postgres:
    host: ${PG_HOST:-127.0.0.1}
    port: ${PG_PORT:-6432}
    name: ${PG_DB:-headscale}
    user: ${PG_USER:-headscale}
    pass: file:${PG_PASSFILE:?PG_PASSFILE is required}
    ssl: disable
    max_open_conns: ${PG_MAX_OPEN:-10}
    max_idle_conns: ${PG_MAX_OPEN:-10}
dns:
  magic_dns: true
  base_domain: bench.tailnet.internal
  override_local_dns: false
log:
  level: warn
logtail:
  enabled: false
cli:
  timeout: 300s
tuning:
  max_peer_entries: ${MAX_PEER_ENTRIES:-1000000}
EOF
	if [ "${INCREMENTAL:-0}" = 1 ]; then
		cat >>"$cfg" <<EOF
  incremental_netmap:
    enabled: true
    reconcile_interval: ${RECONCILE_INTERVAL:-1m}
    full_map_interval: ${FULL_MAP_INTERVAL:-15m}
EOF
	fi
	echo "config $cfg (server_url http://$hostport, listen $listen, metrics ${METRICS:-127.0.0.1:9090})"
	;;
migrate)
	# P13d (ADR 0013): the explicit schema step; JSON of what it applied.
	hsc migrate
	;;
register)
	# The registry wants an https endpoint (metadata for bootstrap); the
	# benchmark listener is plain HTTP on the lab LAN.
	endpoint=${ENDPOINT:-https://$(awk -F'//' '/^server_url:/{print $2}' "$cfg")}
	out=$(hsc register --class "${CLASS:-dedicated}" --max-devices "${MAX_DEVICES:-100000}" --endpoint "$endpoint" 2>&1) && echo "$out" && exit 0
	grep -qi 'exist' <<<"$out" && { echo "cluster $CLUSTER_ID already registered"; exit 0; }
	echo "$out" >&2
	exit 1
	;;
start)
	role=${3:-active}
	if [ -f "$dir/server.pid" ] && kill -0 "$(cat "$dir/server.pid")" 2>/dev/null; then
		echo "a server of $dir is already running" >&2
		exit 1
	fi
	# P13e (ADR 0014 §3): a cluster process never creates its Noise key
	# unless asked; the first active of a new bench cluster does.
	init=()
	keyfile=$(awk '/private_key_path:/{print $2}' "$cfg")
	if [ ! -s "$keyfile" ]; then
		[ "$role" = active ] || { echo "no Noise key at $keyfile: start the active first" >&2; exit 1; }
		init=(--init-noise-key)
		echo "no Noise key yet: the active creates $keyfile (--init-noise-key)"
	fi
	ulimit -n 200000 2>/dev/null || ulimit -n "$(ulimit -Hn)"
	log=$dir/server-$(date -u +%Y%m%dT%H%M%SZ).log
	GOMAXPROCS=${GOMAXPROCS:-$(nproc)} nohup "$HS" -c "$cfg" cluster --cluster-id "$CLUSTER_ID" serve ${init[@]+"${init[@]}"} >"$log" 2>&1 &
	pid=$!
	echo $pid >"$dir/server.pid"
	ln -sf "$(basename "$log")" "$dir/server.log"
	# Sampler: unix time, RSS KiB, cumulative user+sys CPU ticks, threads.
	nohup bash -c '
		pid=$1 out=$2
		[ -f "$out" ] || echo "t,rss_kb,cpu_ticks,threads" >"$out"
		while kill -0 "$pid" 2>/dev/null; do
			rss=$(awk "/^VmRSS/{print \$2}" /proc/$pid/status 2>/dev/null || echo 0)
			thr=$(awk "/^Threads/{print \$2}" /proc/$pid/status 2>/dev/null || echo 0)
			cpu=$(awk "{print \$14+\$15}" /proc/$pid/stat 2>/dev/null || echo 0)
			echo "$(date +%s.%N),$rss,$cpu,$thr" >>"$out"
			sleep 1
		done' _ "$pid" "$dir/samples.csv" >/dev/null 2>&1 &
	echo $! >"$dir/sampler.pid"
	# Metrics every 10 s (loopback): headscale_*, Go runtime, process.
	nohup bash -c '
		pid=$1 out=$2 addr=$3
		while kill -0 "$pid" 2>/dev/null; do
			{ echo "# T $(date +%s.%N)"
			  curl -fsS -m 5 "http://$addr/metrics" 2>/dev/null | grep -E "^(headscale_|go_memstats_heap_(alloc|inuse)_bytes|go_gc_duration_seconds|go_goroutines|process_)" | grep -v "tailnet=\"" ; } >>"$out"
			sleep 10
		done' _ "$pid" "$dir/metrics.log" "$(metrics_addr)" >/dev/null 2>&1 &
	echo $! >"$dir/scraper.pid"
	t0=$(date +%s.%N)
	want=200
	[ "$role" = standby ] && want=503
	for _ in $(seq 1 1200); do
		code=$(ready_code ready)
		if [ "$code" = "$want" ]; then
			echo "$role up pid=$pid ready=$code startup_s=$(echo "$(date +%s.%N) - $t0" | bc)"
			exit 0
		fi
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.5
	done
	echo "$role did not reach /ready $want (last $code)" >&2
	tail -20 "$log" >&2
	exit 1
	;;
restart)
	bash "$0" stop "$dir"
	bash "$0" start "$dir" active
	;;
stop)
	curl -fsS -m 10 "http://$(metrics_addr)/metrics" 2>/dev/null | grep -E '^(headscale_|go_memstats_heap|go_gc|go_goroutines|process_)' >"$dir/metrics-final.txt" || true
	[ -f "$dir/server.pid" ] || exit 0
	pid=$(cat "$dir/server.pid")
	kill "$pid" 2>/dev/null || true
	for _ in $(seq 1 120); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
	kill -9 "$pid" 2>/dev/null || true
	for p in sampler scraper; do kill "$(cat "$dir/$p.pid" 2>/dev/null)" 2>/dev/null || true; rm -f "$dir/$p.pid"; done
	rm -f "$dir/server.pid"
	echo "stopped $pid"
	;;
manifest)
	python3 - "$HS" "$cfg" "$dir/manifest-server.json" <<'PY'
import hashlib, json, os, platform, re, subprocess, sys, time
hs, cfg, out = sys.argv[1:4]
def run(*a):
    try:
        return subprocess.run(a, capture_output=True, text=True, timeout=30).stdout.strip()
    except Exception as e:  # noqa: BLE001
        return f"error: {e}"
def sha(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for b in iter(lambda: f.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()
# The config holds a file reference for the password, never the password.
conf = open(cfg).read()
assert not re.search(r"^\s*pass:\s*(?!file:)\S", conf, re.M), "inline password in config"
meminfo = dict(l.split(":", 1) for l in open("/proc/meminfo") if ":" in l)
cpu = next((l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")), "")
m = {
    "time_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "binary": hs, "binary_sha256": sha(hs), "go_version_m": run("go", "version", "-m", hs)[:4000],
    "headscale_version": run(hs, "version"),
    "config_yaml": conf,
    "host": {"hostname": platform.node(), "nproc": os.cpu_count(), "cpu_model": cpu,
             "mem_total": meminfo.get("MemTotal", "").strip(), "kernel": platform.release(),
             "ulimit_n": run("sh", "-c", "ulimit -n"),
             "uptime": run("uptime"), "top5_cpu": run("sh", "-c", "ps -eo pid,pcpu,rss,comm --sort=-pcpu | head -6")},
    "env": {k: os.environ[k] for k in ("CLUSTER_ID", "CLASS", "MAX_DEVICES", "MAX_PEER_ENTRIES", "INCREMENTAL",
                                       "GOMAXPROCS", "PG_MAX_OPEN", "HEADSCALE_BENCH_MUTEX_FRACTION",
                                       "HEADSCALE_BENCH_BLOCK_RATE") if k in os.environ},
}
json.dump(m, open(out, "w"), indent=2)
print(f"manifest {out}")
PY
	;;
*)
	echo "usage: $0 config|migrate|register|start|restart|stop|manifest RUNDIR ..." >&2
	exit 2
	;;
esac
