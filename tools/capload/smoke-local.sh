#!/usr/bin/env bash
# P14a.0 smoke run of the cluster-mode harness on ONE host (mc-ci), tiny
# scale: throwaway PostgreSQL + PgBouncer (pgstack.sh), one active and one
# standby `headscale cluster serve` on loopback, capseed provisioning, capload
# enrolment over Noise with -resolve, churn, probes, a register phase,
# policy-change probes (capseed -mode policy-flip), then a reconnect cell of
# the saved node keys after a cluster restart. Not a benchmark: server and
# load share the host. Everything is removed at the end (KEEP=1 keeps it).
#
#   smoke-local.sh BINDIR OUTDIR
#
# BINDIR holds headscale (build with -tags benchpprof to probe pprof too),
# capload and capseed. OUTDIR gets the results (keys are deleted).
#
# Env: TAILNETS (2), DEVICES (50), POLICY (allow_all), EXTRA (20),
#      STEADY (60s), PROBES (6), BIG (0) / BIG_POLICY (tag_heavy).
set -euo pipefail

bin=$(cd "$1" && pwd)
out=$2
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out" && chmod 700 "$out"
out=$(cd "$out" && pwd)
base=$out/run
pg=$out/pg
host=bench.capload.internal
port=${PORT:-18080}
export HS=$bin/headscale CLUSTER_ID=${CLUSTER_ID:-bench-c1}
export PG_HOST=127.0.0.1 PG_PORT=${PG_LISTEN_PORT:-16432} PG_DB=headscale PG_USER=headscale PG_PASSFILE=$pg/pg.pass
export CLASS=shared MAX_DEVICES=${MAX_DEVICES:-2000} NOISE_KEY=$base/noise_private.key GOMAXPROCS=${GOMAXPROCS:-4}
export PGSTACK_NAME=p14smoke

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$out/smoke.log" >&2; }

cleanup() {
	[ "${KEEP:-0}" = 1 ] && { log "KEEP=1: leaving the stack running"; return; }
	bash "$here/cluster.sh" stop "$base/standby" >/dev/null 2>&1 || true
	bash "$here/cluster.sh" stop "$base/active" >/dev/null 2>&1 || true
	[ -n "${PG_PSQL:-}" ] && bash "$here/pgsample.sh" stop "$out/pgstats" >/dev/null 2>&1 || true
	bash "$here/pgstack.sh" down "$pg" >/dev/null 2>&1 || true
	# Lab test credentials: plan.tsv (pre-auth keys), node keys, noise key.
	rm -f "$base/seed/plan.tsv" "$out"/*/nodes.keys "$NOISE_KEY"
	log "cleanup done: containers $(docker ps -aq --filter label=mesh-bench=$PGSTACK_NAME | wc -l), keys left $(find "$out" -name 'plan.tsv' -o -name 'nodes.keys' -o -name 'noise_private.key' -o -name 'pg.pass' | wc -l)"
}
trap cleanup EXIT

mkdir -p "$base" && chmod 700 "$base"
log "PostgreSQL + PgBouncer (throwaway)"
bash "$here/pgstack.sh" up "$pg" "127.0.0.1:$PG_PORT" | tee -a "$out/smoke.log"
export PG_PSQL="docker exec -i -e PGPASSFILE=/tmp/pgpass -u postgres $PGSTACK_NAME-pg psql -h 127.0.0.1 -U headscale -d headscale"
export PGB_PSQL="docker exec -i -e PGPASSFILE=/tmp/pgpass -u postgres $PGSTACK_NAME-pg psql -h $PGSTACK_NAME-pgb -p 6432 -U headscale -d pgbouncer"

log "cluster config, migrate, register, active + standby"
METRICS=127.0.0.1:19090 bash "$here/cluster.sh" config "$base/active" "$host:$port" "127.0.0.1:$port"
METRICS=127.0.0.1:19091 bash "$here/cluster.sh" config "$base/standby" "$host:$port" "127.0.0.1:$((port + 1))"
bash "$here/cluster.sh" migrate "$base/active" | tee -a "$out/smoke.log"
bash "$here/cluster.sh" register "$base/active" | tee -a "$out/smoke.log"
bash "$here/cluster.sh" start "$base/active" active | tee -a "$out/smoke.log"
bash "$here/cluster.sh" start "$base/standby" standby | tee -a "$out/smoke.log"

log "capseed: ${TAILNETS:-2} tailnets x ${DEVICES:-50} devices (${POLICY:-allow_all}), big ${BIG:-0}, extra ${EXTRA:-20}"
mkdir -p "$base/seed"
"$bin/capseed" -mode cluster -config "$base/active/config.yaml" -cluster-id "$CLUSTER_ID" -out "$base/seed" \
	-tailnets "${TAILNETS:-2}" -devices "${DEVICES:-50}" -policy "${POLICY:-allow_all}" \
	-big "${BIG:-0}" -big-policy "${BIG_POLICY:-tag_heavy}" -extra "${EXTRA:-20}" -prefix smoke -seed 7 | tee -a "$out/smoke.log"
cp "$base/seed/tailnets.json" "$out/"
# The standby is stopped while the active restarts: it would take the lease.
restart_active() {
	bash "$here/cluster.sh" stop "$base/standby" | tee -a "$out/smoke.log"
	bash "$here/cluster.sh" restart "$base/active" | tee -a "$out/smoke.log"
	bash "$here/cluster.sh" start "$base/standby" standby | tee -a "$out/smoke.log"
}

log "restart the active so its runtimes load the new users"
restart_active

flip="'$bin/capseed' -mode policy-flip -config '$base/active/config.yaml' -cluster-id '$CLUSTER_ID' -slug \"\$CAPLOAD_TAILNET\""

cell() { # NAME capload-args…
	local name=$1
	shift
	local d=$out/$name
	mkdir -p "$d" && chmod 700 "$d"
	bash "$here/pgsample.sh" reset >/dev/null
	bash "$here/pgsample.sh" start "$out/pgstats-$name" >/dev/null
	log "cell $name: capload $*"
	set +e
	(ulimit -n 65536; "$bin/capload" -server "http://$host:$port" -resolve "$host=127.0.0.1" -out "$d" \
		-commit "${COMMIT:-unknown}" -label "smoke-$name" "$@" >"$d/capload.log" 2>&1)
	local rc=$?
	set -e
	echo "exit=$rc" >>"$d/capload.log"
	bash "$here/pgsample.sh" stop "$out/pgstats-$name" >/dev/null
	log "cell $name: capload exit $rc"
	grep -E '^(enroll|connect|register) wall|^steady|^probe|^policy|ISOLATION' "$d/capload.log" | tee -a "$out/smoke.log" || true
	python3 "$here/summarize-cell.py" --load "$d" --server "$base/active" --standby "$base/standby" \
		--pg "$out/pgstats-$name" --label "smoke-$name" | tee -a "$out/smoke.log"
	return $rc
}

rc=0
cell enroll -plan "$base/seed/plan.tsv" -register "${EXTRA:-20}" -concurrency 16 -observe 40 \
	-steady "${STEADY:-60s}" -churn-pct-per-min 10 -probes "${PROBES:-6}" -probe-interval 3s -probe-timeout 15s \
	-policy-cmd "$flip" -policy-changes 2 -policy-interval 5s -hold 10s \
	-save-node-keys "$out/enroll/nodes.keys" || rc=$?

if [ -x "$bin/headscale" ] && curl -fsS -m 5 -o /dev/null "http://127.0.0.1:19090/debug/pprof/"; then
	log "benchpprof: loopback metrics /debug/pprof/ 200; cmdline + 1 s CPU profile:"
	curl -fsS -m 10 -o "$out/cpu-1s.pprof" "http://127.0.0.1:19090/debug/pprof/profile?seconds=1" && ls -l "$out/cpu-1s.pprof" | tee -a "$out/smoke.log"
	echo "public listener /debug/pprof/: $(curl -s -o /dev/null -w '%{http_code}' --resolve "$host:$port:127.0.0.1" "http://$host:$port/debug/pprof/")" | tee -a "$out/smoke.log"
else
	log "no pprof on the metrics listener (binary built without -tags benchpprof)"
fi

log "reconnect cell: stop the cluster, snapshot the seeded database and restore it (pgsnap.sh), start, connect the saved node keys"
bash "$here/cluster.sh" stop "$base/standby" | tee -a "$out/smoke.log"
bash "$here/cluster.sh" stop "$base/active" | tee -a "$out/smoke.log"
export PG_ADMIN_PSQL="docker exec -i -e PGPASSFILE=/tmp/pgpass -u postgres $PGSTACK_NAME-pg psql -h 127.0.0.1 -U headscale -d postgres"
PGSNAP_ALLOW='^(headscale|bench_smoke)$' bash "$here/pgsnap.sh" save headscale bench_smoke | tee -a "$out/smoke.log"
PGSNAP_ALLOW='^(headscale|bench_smoke)$' bash "$here/pgsnap.sh" restore bench_smoke headscale | tee -a "$out/smoke.log"
bash "$here/cluster.sh" start "$base/active" active | tee -a "$out/smoke.log"
bash "$here/cluster.sh" start "$base/standby" standby | tee -a "$out/smoke.log"
cell reconnect -node-keys "$out/enroll/nodes.keys" -concurrency 32 -observe 40 -probes 4 -probe-interval 3s \
	-probe-timeout 15s -hold 5s || rc=$?

bash "$here/cluster.sh" manifest "$base/active" >/dev/null
cp "$base/active/manifest-server.json" "$out/" 2>/dev/null || true
for r in active standby; do cp "$base/$r/samples.csv" "$out/samples-$r.csv" 2>/dev/null || true; done
cp "$base/active/metrics-final.txt" "$out/" 2>/dev/null || true
grep -c . "$base/active/server.log" >/dev/null 2>&1 && grep -iE 'error|panic' "$base/active/server.log" | head -20 >"$out/server-errors.txt" || true
log "standby still not ready: /ready $(curl -s -o /dev/null -w '%{http_code}' --resolve "$host:$((port + 1)):127.0.0.1" "http://$host:$((port + 1))/ready" || true)"
exit $rc
