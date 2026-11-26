#!/usr/bin/env bash
# Read-only health check of the lab PostgreSQL HA cluster (P13a) and its
# PgBouncer + HAProxy path (P13b). Runs from the laptop over SSH; prints no
# secrets (SQL runs as postgres over the local socket with peer auth;
# patronictl through the root-only wrapper; the headscale password for the
# PgBouncer round trips goes to a 0600 tmpfs file on mc-ci through stdin).
# The P13b round trips write nothing: `txid_current()` only needs a
# read-write server (a replica refuses it).
#
#   bash infra/lab/pg/check-pg.sh        exit 0 only if every check passes
set -uo pipefail

MESH_LAB_DIR=${MESH_LAB_DIR:-$HOME/.mesh-lab}
SSH=(ssh -i "$MESH_LAB_DIR/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
PG1=mesh@192.168.1.247
PG2=mesh@192.168.1.248
DB=mesh@192.168.1.243
CI=mesh@192.168.1.240
EP=http://192.168.1.247:2379,http://192.168.1.248:2379,http://192.168.1.243:2379
fail=0
ok() { echo "PASS $*"; }
bad() { echo "FAIL $*"; fail=1; }
run() { "${SSH[@]}" "$@"; }
# rjq FILTER <json: jq on mc-pg-01 (cloud-init installs it), so the laptop
# needs nothing but ssh.
rjq() { run $PG1 "jq -r $(printf '%q' "$1")"; }
probe() { # host target port -> open|closed
	run "$1" "timeout 3 bash -c '</dev/tcp/$2/$3' 2>/dev/null && echo open || echo closed"
}

echo "== etcd"
run $PG1 "etcdctl --endpoints=$EP endpoint health" && ok "etcd: 3/3 endpoints healthy" || bad "etcd health"
run $PG1 "etcdctl --endpoints=$EP member list -w table"
st=$(run $PG1 "etcdctl --endpoints=$EP endpoint status -w json")
terms=$(rjq '[.[].Status.header.raft_term] | unique | length' <<<"$st")
leaders=$(rjq '[.[].Status.leader] | unique | length' <<<"$st")
[ "$terms" = 1 ] && [ "$leaders" = 1 ] && ok "etcd: one raft term, one leader on all members" || bad "etcd: members disagree (terms=$terms leaders=$leaders)"

echo "== patroni"
run $PG1 "sudo mc-patronictl list"
cl=$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster")
leader=$(rjq '[.members[] | select(.role=="leader") | .name] | join(" ")' <<<"$cl")
sync=$(rjq '[.members[] | select(.role=="sync_standby") | .name] | join(" ")' <<<"$cl")
syncstate=$(rjq '[.members[] | select(.role=="sync_standby") | .state] | join(" ")' <<<"$cl")
nlead=$(rjq '[.members[] | select(.role=="leader")] | length' <<<"$cl")
[ "$nlead" = 1 ] && ok "patroni: one leader ($leader)" || bad "patroni: $nlead leaders"
[ -n "$sync" ] && [ "$syncstate" = streaming ] &&
	ok "patroni: sync standby $sync streaming" || bad "patroni: no streaming sync standby"
case $leader in mc-pg-01) LH=$PG1 ;; mc-pg-02) LH=$PG2 ;; *) LH= ;; esac
dcs=$(run $PG1 "curl -fsS http://127.0.0.1:8008/config")
[ "$(rjq '.synchronous_mode == true and (.synchronous_mode_strict // false) == false and .ttl == 20 and .loop_wait == 5 and .retry_timeout == 5' <<<"$dcs")" = true ] &&
	ok "patroni DCS: synchronous_mode=true strict=false ttl=20 loop_wait=5 retry_timeout=5" || bad "patroni DCS config: $dcs"

echo "== postgresql (leader $leader)"
if [ -n "$LH" ]; then
	q() { run "$LH" "sudo -u postgres psql -X -At -d postgres -c \"$1\""; }
	[ "$(q 'SHOW data_checksums')" = on ] && ok "data_checksums=on" || bad "data_checksums"
	[ "$(q 'SHOW wal_log_hints')" = on ] && ok "wal_log_hints=on" || bad "wal_log_hints"
	[ "$(q 'SHOW synchronous_commit')" = on ] && ok "synchronous_commit=on" || bad "synchronous_commit"
	echo "synchronous_standby_names = $(q 'SHOW synchronous_standby_names')"
	rep=$(q "SELECT application_name||' '||state||' '||sync_state FROM pg_stat_replication")
	echo "pg_stat_replication: $rep"
	grep -q "streaming sync" <<<"$rep" && ok "replica streaming with sync_state=sync" || bad "no sync replica in pg_stat_replication"
	echo "archive_mode=$(q 'SHOW archive_mode') archive_command=$(q 'SHOW archive_command') (pgBackRest: P13c)"
	echo "server_version=$(q 'SHOW server_version') max_connections=$(q 'SHOW max_connections') shared_buffers=$(q 'SHOW shared_buffers')"
	[ "$(q "SELECT count(*) FROM pg_roles WHERE rolname='headscale' AND rolcanlogin")" = 1 ] && ok "role headscale exists" || bad "role headscale"
	echo "headscale role settings: $(q "SELECT array_to_string(setconfig, ',') FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname='headscale'")"
fi

echo "== watchdog, time, units (both nodes)"
for h in $PG1 $PG2; do
	n=$(run "$h" hostname)
	wd=$(run "$h" 'sudo fuser /dev/watchdog 2>/dev/null | wc -w; ps -o comm= -p $(sudo fuser /dev/watchdog 2>/dev/null | tr -d " ") 2>/dev/null; cat /sys/class/watchdog/watchdog0/identity /sys/class/watchdog/watchdog0/state /sys/class/watchdog/watchdog0/timeout 2>/dev/null' | tr '\n' ' ')
	echo "$n watchdog: $wd"
	# Patroni arms the watchdog only while it is the leader.
	if [ "$n" = "$leader" ]; then
		[[ $wd == "1 patroni Software Watchdog active "* ]] && ok "$n (leader): softdog active, held by Patroni" || bad "$n (leader): watchdog not armed"
	else
		[[ $wd == *"Software Watchdog"* ]] && [ "$(run "$h" 'stat -c %U /dev/watchdog')" = postgres ] &&
			ok "$n (replica): softdog loaded, /dev/watchdog owned by postgres, armed on promotion" || bad "$n: softdog missing"
	fi
	ch=$(run "$h" "chronyc -n tracking | grep -E '^(Leap status|System time|Last offset)'" | tr '\n' ';')
	echo "$n chrony: $ch"
	grep -q "Normal" <<<"$ch" && ok "$n: chrony synchronised" || bad "$n: chrony"
	for u in mc-etcd mc-patroni nftables chrony; do
		[ "$(run "$h" "systemctl is-active $u; systemctl is-enabled $u" | tr '\n' ' ')" = "active enabled " ] || bad "$n: $u not active+enabled"
	done
done
offs=$(for h in $PG1 $PG2; do run "$h" 'python3 -c "import time; print(repr(time.time()))"'; done)
echo "wall clock read back-to-back over SSH (includes SSH latency): $(tr '\n' ' ' <<<"$offs")"

echo "== pgbouncer + haproxy (P13b)"
for h in $PG1 $PG2; do
	n=$(run "$h" hostname)
	[ "$(run "$h" "systemctl is-active pgbouncer haproxy; systemctl is-enabled pgbouncer haproxy" | tr '
' ' ')" = "active active enabled enabled " ] &&
		ok "$n: pgbouncer and haproxy active+enabled" || bad "$n: pgbouncer/haproxy not active+enabled"
	st=$(run "$h" "echo 'show stat' | sudo socat - UNIX-CONNECT:/run/haproxy/admin.sock | awk -F, '\$1==\"pg_primary\" && \$2 ~ /^mc-pg/ {printf \"%s=%s \", \$2, \$18}'")
	echo "$n haproxy pg_primary: $st"
	nup=$(grep -o '=UP' <<<"$st" | wc -l)
	[ "$nup" = 1 ] && grep -q "$leader=UP" <<<"$st" &&
		ok "$n: haproxy sends to the leader $leader only" || bad "$n: haproxy backends $st (leader $leader)"
	cfg=$(run "$h" "sudo -u postgres psql -X -At -h /var/run/postgresql -p 6432 -U pgbouncer pgbouncer -c 'SHOW CONFIG'" |
		awk -F'|' '$1 ~ /^(pool_mode|max_prepared_statements|query_wait_timeout|idle_transaction_timeout|server_login_retry|server_check_query)$/ {printf "%s=%s; ", $1, $2}')
	echo "$n pgbouncer: $cfg"
	grep -q "pool_mode=transaction;" <<<"$cfg" && grep -q "max_prepared_statements=200;" <<<"$cfg" && grep -q "query_wait_timeout=5;" <<<"$cfg" &&
		ok "$n: pgbouncer transaction pooling, max_prepared_statements=200, query_wait_timeout=5" || bad "$n: pgbouncer config"
	# Columns by header name (their order changes between PgBouncer versions).
	pools=$(run "$h" "sudo -u postgres psql -X -A -F' ' -h /var/run/postgresql -p 6432 -U pgbouncer pgbouncer -c 'SHOW POOLS'" |
		awk 'NR==1 {for (i=1; i<=NF; i++) c[$i]=i; next} NF>4 && $1 !~ /^\(/ {print $c["database"], $c["user"], "cl_active=" $c["cl_active"], "cl_waiting=" $c["cl_waiting"], "sv_active=" $c["sv_active"], "sv_idle=" $c["sv_idle"]}')
	echo "$n SHOW POOLS:"; sed 's/^/  /' <<<"$pools"
	waiting=$(awk '{split($4,a,"="); s+=a[2]} END{print s+0}' <<<"$pools")
	[ "$waiting" = 0 ] && ok "$n: no client waiting for a server connection" || bad "$n: $waiting clients waiting (cl_waiting)"
done
for src in $CI mesh@192.168.1.241; do
	for t in 192.168.1.247 192.168.1.248; do
		[ "$(probe "$src" $t 6432)" = open ] && ok "6432 on $t open to ${src#mesh@}" || bad "6432 on $t closed to ${src#mesh@}"
	done
done
if [ -r "$MESH_LAB_DIR/mc-pg.env" ]; then
	pgpass=$(grep '^MC_PG_HEADSCALE_PASSWORD=' "$MESH_LAB_DIR/mc-pg.env" | cut -d= -f2- | tr -d '
' |
		run $CI 'umask 077; f=$(mktemp -p /dev/shm check-pg.XXXXXX); IFS= read -r p; printf "192.168.1.247:6432:*:headscale:%s
192.168.1.248:6432:*:headscale:%s
" "$p" "$p" >"$f"; echo "$f"')
	for dsn in "host=192.168.1.247 port=6432" "host=192.168.1.248 port=6432" "host=192.168.1.247,192.168.1.248 port=6432 target_session_attrs=read-write"; do
		out=$(run $CI "psql 'dbname=postgres user=headscale connect_timeout=3 passfile=$pgpass $dsn' -X -At -c 'SELECT host(inet_server_addr()), pg_is_in_recovery(), txid_current() > 0' 2>&1" | head -1)
		[ "$out" = "$(cut -d' ' -f2 <<<"$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster | jq -r '.members[] | select(.role==\"leader\") | .name + \" \" + .host'")")|f|t" ] &&
			ok "read-write round trip via $dsn -> $out" || bad "round trip via $dsn: $out"
	done
	run $CI "rm -f '$pgpass'"
else
	echo "SKIP PgBouncer round trips: $MESH_LAB_DIR/mc-pg.env not readable"
fi

echo "== pgbackrest (P13c)"
if [ -n "$LH" ]; then
	code=$(run "$LH" "sudo -u postgres pgbackrest --stanza=mc-pg --output=json info" | rjq '.[0].status.code')
	[ "$code" = 0 ] && ok "pgbackrest stanza mc-pg: status ok (leader $leader)" || bad "pgbackrest stanza mc-pg status code '$code'"
	run "$LH" "sudo -u postgres pgbackrest --stanza=mc-pg info" | grep -E 'full backup|incr backup|diff backup|wal archive' | sed 's/^ */  /'
	[ "$(q 'SHOW archive_command')" = "pgbackrest --stanza=mc-pg archive-push %p" ] && ok "archive_command = pgbackrest archive-push" || bad "archive_command: $(q 'SHOW archive_command')"
	arch=$(q "SELECT archived_count, coalesce(last_archived_wal,'-'), failed_count, (last_failed_time IS NULL OR last_failed_time < last_archived_time) FROM pg_stat_archiver")
	echo "pg_stat_archiver (archived, last, failed, last failure older than last success): $arch"
	[ "${arch##*|}" = t ] && ok "WAL archiving: last attempt succeeded" || bad "WAL archiving failing: $arch"
	for h in $PG1 $PG2; do
		[ "$h" = "$LH" ] && continue
		[ "$(run "$h" "sudo -u postgres pgbackrest --stanza=mc-pg --output=json info" | rjq '.[0].status.code')" = 0 ] &&
			ok "$(run "$h" hostname) (replica) reads and decrypts the repository" || bad "replica cannot read the repository"
	done
fi

echo "== mc-db witness"
run $DB "systemctl is-active mc-etcd mc-etcd-nft | tr '\n' ' '; echo; sudo docker ps --format '{{.Names}} {{.Status}}'"
[ "$(run $DB 'sudo docker inspect -f {{.State.Running}} mc-pg mc-pgb' | tr '\n' ' ')" = "true true " ] && ok "mc-db: mc-pg and mc-pgb still running" || bad "mc-db containers"

echo "== firewall (probes from mc-ci .240 and mc-cp .241)"
[ "$(probe $CI 192.168.1.247 2379)" = closed ] && ok "etcd 2379 on mc-pg-01 closed to mc-ci" || bad "etcd open to mc-ci"
[ "$(probe $CI 192.168.1.243 2379)" = closed ] && ok "etcd 2379 on mc-db closed to mc-ci" || bad "mc-db etcd open to mc-ci"
[ "$(probe $CI 192.168.1.248 8008)" = closed ] && ok "patroni 8008 closed to mc-ci" || bad "8008 open to mc-ci"
[ "$(probe $CI 192.168.1.247 5432)" = open ] && ok "5432 open to mc-ci (operator/tests)" || bad "5432 closed to mc-ci"
[ "$(probe mesh@192.168.1.241 192.168.1.247 5432)" = closed ] && ok "5432 closed to mc-cp (clients go through PgBouncer, P13b)" || bad "5432 open to mc-cp"
[ "$(probe $CI 192.168.1.243 6432)" = open ] && ok "mc-db PgBouncer 6432 still reachable from mc-ci" || bad "mc-db PgBouncer unreachable"

echo
[ "$fail" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
