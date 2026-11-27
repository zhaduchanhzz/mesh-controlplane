#!/usr/bin/env bash
# P14f F5 driver (tickets/P14f.md §F5): measure the capacity limit of ONE
# `headscale cluster serve` process with no ceiling, per policy shape, and the
# small/medium matrix for the cost model. Runs on an operator machine that
# can ssh to the server host, every load host, the DB host and mc-ci.
#
#   ramp.sh limit  SHAPE...       ramp 5k,10k,15k,… until a step fails, then
#                                 bisect between the last pass and the first
#                                 fail down to RES devices; prints LIMIT
#   ramp.sh step   SHAPE N        one step (fresh DB, seed, enroll N, +REGISTER)
#   ramp.sh matrix                MATRIX_N × MATRIX_SHAPES × MATRIX_INC, one
#                                 step each (model points, verdict recorded too)
#   ramp.sh keys-up|keys-down     run key for running this driver ON a lab host
#                                 (see "Operator host" below)
#
# A step PASSES when all of (ticket §F5, owner 2026-11-26):
#   - every one of the N devices got its first map within the enroll phase
#     (capload: failed = 0, deadline DEADLINE, per node TIMEOUT);
#   - all REGISTER (+500) extra devices registered and got a first map;
#   - peak server RSS < 90 % of the server VM's MemTotal;
#   - the server process neither died nor restarted, and no OOM kill in the
#     kernel log of the server host during the step;
#   - headscale_nodestore_netmap_divergence_total == 0 (incremental on);
#   - no isolation violation (capload exit 3 = BLOCK, the driver stops).
# The limit is the largest passing N. The blocker of the failing step is
# classified (server RAM / server CPU / load generator / timeouts /
# divergence) from the samples; if a load host saturated first, the limit is
# reported as a lower bound.
#
# Environment (defaults are the lab of 2026-11-27):
#   SERVER        mesh@192.168.1.242 (mc-bench)   SERVER_ADDR 192.168.1.242:8080
#   LOAD_HOSTS    "mesh@192.168.1.249:12 mesh@192.168.1.240:16"  host:weight
#   DB_SSH        mesh@192.168.1.243 (mc-db: docker mc-pg, PgBouncer :6432)
#   CI_SSH        mesh@192.168.1.240             CI_BINDIR (headscale, capload,
#                 capseed built for this commit; fetched over the LAN, sha256 checked)
#   INC           1 (incremental netmap on) | 0   RECONCILE_INTERVAL FULL_MAP_INTERVAL
#   REGISTER      500   DEADLINE 20m   TIMEOUT 120s   CONCURRENCY 256 (per host)
#   OBSERVE       10 per host (decoded, isolation-checked streams)
#   HOLD          60s   START 5000   STEP 5000   RES 1000
#   OUT           artifacts/<commit>/P14f/F5   (results only; no key ever lands here)
#   SSH_KEY       ~/.mesh-lab/mesh-lab
#   COMMIT        commit of the binaries (manifest)
#
# Operator host: from the laptop the lab key is used. For long runs start it
# ON mc-load with a run-scoped key: `ramp.sh keys-up` (from the laptop) makes
# a key on mc-load and authorises it on the other hosts (comment p14f-ramp);
# `ramp.sh keys-down` removes it everywhere.
#
# Secrets: pre-auth keys (plan.tsv) are generated on the server host, cut and
# piped host to host and deleted after each step; the PostgreSQL password is
# piped from mc-db.env (laptop) or an existing 0600 file into a 0600 file on
# the server host and referenced as `pass: file:`. Nothing secret is printed or
# written under OUT.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
key=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
SERVER=${SERVER:-mesh@192.168.1.242}
SERVER_ADDR=${SERVER_ADDR:-192.168.1.242:8080}
LOAD_HOSTS=${LOAD_HOSTS:-"mesh@192.168.1.249:12 mesh@192.168.1.240:16"}
DB_SSH=${DB_SSH:-mesh@192.168.1.243}
DB_PORT=${DB_PORT:-6432}
CI_SSH=${CI_SSH:-mesh@192.168.1.240}
CI_BINDIR=${CI_BINDIR:-}
INC=${INC:-1}
REGISTER=${REGISTER:-500}
DEADLINE=${DEADLINE:-20m}
TIMEOUT=${TIMEOUT:-120s}
CONCURRENCY=${CONCURRENCY:-256}
OBSERVE=${OBSERVE:-10}
HOLD=${HOLD:-60s}
START=${START:-5000}
STEP=${STEP:-5000}
RES=${RES:-1000}
COMMIT=${COMMIT:-unknown}
OUT=${OUT:-artifacts/$COMMIT/P14f/F5}
HOSTNAME_=bench.capload.internal
RUN=$(date -u +%Y%m%dT%H%M%SZ)
RB=p14f5 # remote base dir under ~ on every host
PY=${PYTHON:-python3}
"$PY" -c "" 2>/dev/null || PY=python # Windows operator: python3 may be a Store alias

SSHO=(-i "$key" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=30)
on() { local h=$1; shift; ssh "${SSHO[@]}" "$h" "$@"; }
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$OUT/ramp.log" >&2; }
loads() { for x in $LOAD_HOSTS; do echo "${x%:*}"; done; }

mkdir -p "$OUT"

# ---------------------------------------------------------------- binaries
binsrv_pid=
bins() {
	[ -n "$CI_BINDIR" ] || { log "CI_BINDIR unset: using ~/$RB/bin already on the hosts"; return 0; }
	local port=$((20000 + RANDOM % 20000)) ip=${CI_SSH#*@}
	on "$CI_SSH" "cd $CI_BINDIR && sha256sum headscale capload capseed" >"$OUT/bins.sha256"
	on "$CI_SSH" "nohup python3 -m http.server $port --bind $ip --directory $CI_BINDIR >/dev/null 2>&1 & echo \$!" >"$OUT/.binsrv.pid"
	binsrv_pid=$(cat "$OUT/.binsrv.pid")
	sleep 1
	local h f
	for h in "$SERVER" $(loads); do
		for f in headscale capload capseed; do
			on "$h" "mkdir -p ~/$RB/bin && curl -fsS -o ~/$RB/bin/$f.new http://$ip:$port/$f && chmod 755 ~/$RB/bin/$f.new && mv ~/$RB/bin/$f.new ~/$RB/bin/$f"
		done
		on "$h" "cd ~/$RB/bin && sha256sum -c --quiet" <"$OUT/bins.sha256" || { log "sha256 mismatch on $h"; return 1; }
	done
	on "$CI_SSH" "kill $binsrv_pid" || true
	binsrv_pid=
	log "binaries on server and load hosts ($(awk '{print $2"="substr($1,1,16)}' "$OUT/bins.sha256" | tr '\n' ' '))"
}

tools() { # cluster.sh, pgsample.sh, … onto the server host
	tar -C "$here" -cf - cluster.sh server.sh | on "$SERVER" "mkdir -p ~/$RB/tools && tar -C ~/$RB/tools -xf -"
}

# ---------------------------------------------------------------- database
db_name() { echo "capf5_$(echo "$1" | tr -dc 'a-z0-9')"; }
db_fresh() { # NAME
	[[ $1 =~ ^capf5_[a-z0-9]+$ ]] || return 1
	on "$DB_SSH" "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $1 && sudo docker exec mc-pg createdb -U postgres -O headscale $1"
}
db_drop() { [[ $1 =~ ^capf5_[a-z0-9]+$ ]] && on "$DB_SSH" "sudo docker exec mc-pg dropdb -U postgres --if-exists --force $1" || true; }

# The lease alias (ADR 0006 §3.6, P13b design D9b): a PgBouncer entry of the
# step's database with its own pool of 2, so lease renewals never queue
# behind the data pool's server connections. One line, marked, in mc-db's
# pgbouncer.ini; PgBouncer reloads on SIGHUP. LEASE_ALIAS=0 skips it.
LEASE_ALIAS=${LEASE_ALIAS:-1}
PGB_INI=${PGB_INI:-/opt/mc-db/pgbouncer.ini}
lease_alias_up() { # DB
	[ "$LEASE_ALIAS" = 1 ] || return 0
	[[ $1 =~ ^capf5_[a-z0-9]+$ ]] || return 1
	on "$DB_SSH" "sudo sed -i '/^; p14f5-lease\$/d; /^capf5lease = /d' $PGB_INI && sudo sed -i '/^\[databases\]\$/a capf5lease = host=mc-pg port=5432 dbname=$1 pool_size=2 max_db_connections=2' $PGB_INI && sudo sed -i '/^\[databases\]\$/a ; p14f5-lease' $PGB_INI && sudo grep -q '^capf5lease = ' $PGB_INI && sudo docker kill -s HUP mc-pgb >/dev/null"
}
lease_alias_down() {
	[ "$LEASE_ALIAS" = 1 ] || return 0
	on "$DB_SSH" "sudo sed -i '/^; p14f5-lease\$/d; /^capf5lease = /d' $PGB_INI && sudo docker kill -s HUP mc-pgb >/dev/null" || true
}
db_pass() { # write the app password to the server host (0600), never printed
	on "$SERVER" "test -s ~/$RB/pg.pass" && return 0
	local env=${MC_DB_ENV:-$HOME/.mesh-lab/mc-db.env}
	[ -f "$env" ] || { log "no ~/$RB/pg.pass on the server and no $env"; return 1; }
	grep -E '^MC_DB_APP_PASSWORD=' "$env" | head -1 | cut -d= -f2- | tr -d '\r\n' |
		on "$SERVER" "umask 077; mkdir -p ~/$RB && cat > ~/$RB/pg.pass"
}

# ---------------------------------------------------------------- one step
# step SHAPE N -> writes $OUT/<shape>/<inc>/<N>/verdict.json, returns 0 pass / 1 fail / 3 isolation
step() {
	local shape=$1 n=$2
	local id="${shape//_/}${n}i${INC}${RUN,,}"
	local db sd rd ld cell
	db=$(db_name "$id")
	cell=$OUT/$shape/inc$INC/$n
	mkdir -p "$cell"
	sd="\$HOME/$RB/run/$id"
	log "step $shape N=$n INC=$INC db=$db"
	db_fresh "$db"
	lease_alias_up "$db"
	local leasename=
	[ "$LEASE_ALIAS" = 1 ] && leasename=capf5lease
	local ip=${SERVER_ADDR%:*} port=${SERVER_ADDR#*:}
	# F5 measures capacity, not the quota: no peer bound for any shape. Since
	# review fix R2b the bound also refuses policy writes.
	local maxpeer=${MAX_PEER_ENTRIES:-0}
	local envs="HS=\$HOME/$RB/bin/headscale CLUSTER_ID=f5-$n CLASS=dedicated MAX_DEVICES=$((n * 2 + 10000)) PG_HOST=${DB_SSH#*@} PG_PORT=$DB_PORT PG_DB=$db PG_USER=headscale PG_PASSFILE=\$HOME/$RB/pg.pass NOISE_KEY=$sd/noise_private.key MAX_PEER_ENTRIES=$maxpeer INCREMENTAL=$INC RECONCILE_INTERVAL=${RECONCILE_INTERVAL:-1m} FULL_MAP_INTERVAL=${FULL_MAP_INTERVAL:-15m} PG_MAX_OPEN=${PG_MAX_OPEN:-100} PG_MAX_IDLE=${PG_MAX_IDLE:-10} LEASE_TTL=${LEASE_TTL:-} LEASE_RENEW=${LEASE_RENEW:-3s} LEASE_MARGIN=${LEASE_MARGIN:-5s} LEASE_NAME=$leasename"
	on "$SERVER" "mkdir -p $sd && chmod 700 $sd && $envs bash ~/$RB/tools/cluster.sh config $sd/active $HOSTNAME_:$port $ip:$port >/dev/null && $envs bash ~/$RB/tools/cluster.sh migrate $sd/active && $envs bash ~/$RB/tools/cluster.sh register $sd/active >/dev/null && $envs bash ~/$RB/tools/cluster.sh start $sd/active active" | tee -a "$cell/server-setup.txt"
	local t_seed=$SECONDS
	on "$SERVER" "mkdir -p $sd/seed && ~/$RB/bin/capseed -mode cluster -config $sd/active/config.yaml -cluster-id f5-$n -out $sd/seed -tailnets 1 -devices $n -policy $shape -extra $REGISTER -prefix f5 -seed 7 >$sd/seed/capseed.log 2>&1; rc=\$?; tail -3 $sd/seed/capseed.log; exit \$rc" >>"$cell/server-setup.txt"
	echo "seed_s=$((SECONDS - t_seed))" >>"$cell/server-setup.txt"
	on "$SERVER" "$envs bash ~/$RB/tools/cluster.sh restart $sd/active" >>"$cell/server-setup.txt"
	date -u +%s >"$cell/step-start.txt"

	# Split plan.tsv (enroll + register lines) over the load hosts by weight,
	# in contiguous chunks so a user's devices stay on one host.
	local ne nr wsum=0 x
	ne=$(on "$SERVER" "awk -F'\t' '\$3==\"enroll\"' $sd/seed/plan.tsv | wc -l")
	nr=$(on "$SERVER" "awk -F'\t' '\$3==\"register\"' $sd/seed/plan.tsv | wc -l")
	for x in $LOAD_HOSTS; do wsum=$((wsum + ${x##*:})); done
	local ea=0 ra=0 i=0 k=0 hosts=() pids=()
	for x in $LOAD_HOSTS; do
		k=$((k + 1))
		local h=${x%:*} w=${x##*:} ec rc_
		ec=$((ne * w / wsum)) rc_=$((nr * w / wsum))
		[ $k -eq $(echo $LOAD_HOSTS | wc -w) ] && ec=$((ne - ea)) && rc_=$((nr - ra))
		ld="\$HOME/$RB/run/$id-load$k"
		on "$SERVER" "awk -F'\t' -v a=$ea -v b=$((ea + ec)) -v c=$ra -v d=$((ra + rc_)) '\$3==\"enroll\"{e++; if(e>a&&e<=b)print; next} \$3==\"register\"{r++; if(r>c&&r<=d)print}' $sd/seed/plan.tsv" |
			on "$h" "mkdir -p $ld && chmod 700 $ld && umask 077 && cat > $ld/plan.tsv"
		echo "$h enroll=$ec register=$rc_" >>"$cell/split.txt"
		ea=$((ea + ec)) ra=$((ra + rc_))
		hosts+=("$h:$ld:$rc_")
	done
	on "$SERVER" "rm -f $sd/seed/plan.tsv"

	# Load: every host at once; a 1 s sampler of capload and of the host.
	for x in "${hosts[@]}"; do
		local h=${x%%:*} rest=${x#*:} ld rcnt
		ld=${rest%:*} rcnt=${rest##*:}
		(on "$h" "cd $ld && old=\$(cat /proc/sys/net/ipv4/ip_local_port_range | tr '\t' ' ') && echo \"\$old\" > port_range.before && sudo sysctl -qw net.ipv4.ip_local_port_range='1024 65535'; \
			ulimit -n 1048576 2>/dev/null || ulimit -n \$(ulimit -Hn); \
			~/$RB/bin/capload -server http://$HOSTNAME_:$port -resolve $HOSTNAME_=$ip -plan plan.tsv -register $rcnt \
			-concurrency $CONCURRENCY -timeout $TIMEOUT -deadline $DEADLINE -observe $OBSERVE -decode-every 1000 -hold $HOLD \
			-commit $COMMIT -label f5-$shape-$n-inc$INC -out . >capload.log 2>&1 & cp=\$!; \
			( echo t,cap_rss_kb,cap_cpu_ticks,host_cpu_busy,host_cpu_total,tcp_estab; while kill -0 \$cp 2>/dev/null; do \
				r=\$(awk '/^VmRSS/{print \$2}' /proc/\$cp/status 2>/dev/null); c=\$(awk '{print \$14+\$15}' /proc/\$cp/stat 2>/dev/null); \
				read -r _ u ni sy id io irq sirq st _ < /proc/stat; e=\$(ss -Htn state established | wc -l); \
				echo \"\$(date +%s),\$r,\$c,\$((u+ni+sy+irq+sirq+st)),\$((u+ni+sy+id+io+irq+sirq+st)),\$e\"; sleep 1; done ) > load-samples.csv & \
			wait \$cp; rc=\$?; echo exit=\$rc >> capload.log; rm -f plan.tsv; sudo sysctl -qw net.ipv4.ip_local_port_range=\"\$(cat port_range.before)\"; exit \$rc") &
		pids+=($!)
	done
	local lrc=0 p
	for p in "${pids[@]}"; do wait "$p" || lrc=$?; done

	# Collect: server samples/metrics/log tail, load summaries; drop the DB.
	local alive
	alive=$(on "$SERVER" "test -f $sd/active/server.pid && kill -0 \$(cat $sd/active/server.pid) 2>/dev/null && echo 1 || echo 0")
	on "$SERVER" "$envs bash ~/$RB/tools/cluster.sh stop $sd/active" >/dev/null || true
	on "$SERVER" "cat $sd/active/samples.csv" >"$cell/server-samples.csv" || true
	on "$SERVER" "cat $sd/active/metrics-final.txt" >"$cell/metrics-final.txt" 2>/dev/null || true
	on "$SERVER" "grep -h -E 'divergence|^# T' $sd/active/metrics.log" >"$cell/metrics-divergence.txt" 2>/dev/null || true
	on "$SERVER" "echo server_logs=\$(ls $sd/active/server-*.log | wc -l); echo lease_lost=\$(cat $sd/active/server-*.log | grep 'cluster standby: not serving' | grep -vc 'reason=shutdown'); echo fenced=\$(cat $sd/active/server-*.log | grep -c 'ownership fence failed'); grep -iE 'panic|fatal|out of memory' $sd/active/server.log | head -5" >"$cell/server-errors.txt" 2>/dev/null || true
	# Warn/error lines of the server, counted by message, ids and keys cut.
	on "$SERVER" "cat $sd/active/server-*.log | grep -E ' (WRN|ERR|FTL) ' | sed -E 's/^[^ ]+ //; s/[0-9a-f]{12,}/X/g; s/(mkey|nodekey|discokey|privkey|tskey)[:-][^ \"]+/\1:X/g; s/[0-9]+(\.[0-9]+)?(ns|µs|ms|s)\b/N\2/g; s/=[0-9]+/=N/g' | cut -c1-240 | sort | uniq -c | sort -rn | head -40" >"$cell/server-log-summary.txt" 2>/dev/null || true
	on "$SERVER" "sudo dmesg --time-format iso | grep -iE 'out of memory|oom-kill|killed process' | tail -5" >"$cell/dmesg-oom.txt" 2>/dev/null || true
	on "$SERVER" "awk '/^MemTotal/{print \$2}' /proc/meminfo; nproc" >"$cell/server-vm.txt"
	k=0
	for x in "${hosts[@]}"; do
		k=$((k + 1))
		local h=${x%%:*} rest=${x#*:} ld
		ld=${rest%:*}
		mkdir -p "$cell/load$k"
		on "$h" "cd $ld && tar -cf - summary.json capload.log load-samples.csv nodes.csv 2>/dev/null; nproc >&2" 2>"$cell/load$k/nproc.txt" | tar -C "$cell/load$k" -xf - || true
		on "$h" "rm -rf $ld"
	done
	on "$SERVER" "rm -f $sd/noise_private.key; rm -rf $sd"
	lease_alias_down
	db_drop "$db"
	echo "server_alive_at_end=$alive load_rc=$lrc" >>"$cell/server-setup.txt"
	"$PY" "$here/ramp-verdict.py" "$cell" --shape "$shape" --n "$n" --inc "$INC" --register "$REGISTER" --alive "$alive" | tee -a "$OUT/ramp.log"
	local v
	v=$("$PY" -c "import json,sys;print(json.load(open(sys.argv[1]))['verdict'])" "$cell/verdict.json")
	case $v in
	PASS) return 0 ;;
	ISOLATION) log "ISOLATION violation at $shape N=$n: BLOCK (AGENTS 13), stopping"; return 3 ;;
	*) return 1 ;;
	esac
}

limit() { # SHAPE: ramp then bisect
	local shape=$1 n=$START pass=0 fail=0 rc
	while :; do
		rc=0; step "$shape" "$n" || rc=$?
		[ $rc -eq 3 ] && return 3
		if [ $rc -eq 0 ]; then pass=$n; n=$((n + STEP)); else fail=$n; break; fi
	done
	while [ $((fail - pass)) -gt "$RES" ]; do
		n=$(((pass + fail) / 2 / RES * RES))
		[ "$n" -le "$pass" ] && n=$((pass + RES))
		[ "$n" -ge "$fail" ] && break
		rc=0; step "$shape" "$n" || rc=$?
		[ $rc -eq 3 ] && return 3
		if [ $rc -eq 0 ]; then pass=$n; else fail=$n; fi
	done
	log "LIMIT shape=$shape inc=$INC limit=$pass first_fail=$fail (resolution $RES)"
	echo "$shape inc=$INC limit=$pass first_fail=$fail" >>"$OUT/limits.txt"
}

cleanup() {
	case ${1:-} in step | limit | matrix) lease_alias_down ;; esac
	[ -n "$binsrv_pid" ] && on "$CI_SSH" "kill $binsrv_pid" 2>/dev/null || true
	rm -f "$OUT/.binsrv.pid"
}
trap 'cleanup "${CMD:-}"' EXIT
CMD=${1:-}

case ${1:-} in
keys-up)
	lh=${OPERATOR:-mesh@192.168.1.249}
	pub=$(on "$lh" "test -f ~/.ssh/p14f-ramp || ssh-keygen -q -t ed25519 -N '' -C p14f-ramp -f ~/.ssh/p14f-ramp; cat ~/.ssh/p14f-ramp.pub")
	# The operator host is also a load host: it ssh-es to itself too.
	for h in "$SERVER" $(loads) "$DB_SSH" "$CI_SSH"; do
		on "$h" "grep -q p14f-ramp ~/.ssh/authorized_keys || echo '$pub' >> ~/.ssh/authorized_keys"
	done
	db_pass
	echo "run on $lh with SSH_KEY=~/.ssh/p14f-ramp; the DB password is in the server's ~/$RB/pg.pass"
	;;
keys-down)
	lh=${OPERATOR:-mesh@192.168.1.249}
	for h in "$SERVER" $(loads) "$DB_SSH" "$CI_SSH"; do
		on "$h" "sed -i '/p14f-ramp\$/d' ~/.ssh/authorized_keys" || true
	done
	on "$lh" "rm -f ~/.ssh/p14f-ramp ~/.ssh/p14f-ramp.pub"
	on "$SERVER" "rm -f ~/$RB/pg.pass"
	;;
step)
	db_pass; bins; tools
	step "$2" "$3"
	;;
limit)
	shift
	db_pass; bins; tools
	for s in "$@"; do limit "$s" || [ $? -ne 3 ] || exit 3; done
	cat "$OUT/limits.txt"
	;;
matrix)
	db_pass; bins; tools
	for inc in ${MATRIX_INC:-1 0}; do
		for s in ${MATRIX_SHAPES:-isolated personal_devices shared_service allow_all}; do
			for n in ${MATRIX_N:-1000 2000 5000}; do
				INC=$inc step "$s" "$n" || [ $? -ne 3 ] || exit 3
			done
		done
	done
	;;
*)
	sed -n '2,20p' "$0"
	exit 2
	;;
esac
