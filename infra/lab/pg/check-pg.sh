#!/usr/bin/env bash
# Read-only health check of the lab PostgreSQL HA cluster (P13a). Runs from
# the laptop over SSH; prints no secrets (SQL runs as postgres over the local
# socket with peer auth; patronictl through the root-only wrapper).
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

echo "== mc-db witness"
run $DB "systemctl is-active mc-etcd mc-etcd-nft | tr '\n' ' '; echo; sudo docker ps --format '{{.Names}} {{.Status}}'"
[ "$(run $DB 'sudo docker inspect -f {{.State.Running}} mc-pg mc-pgb' | tr '\n' ' ')" = "true true " ] && ok "mc-db: mc-pg and mc-pgb still running" || bad "mc-db containers"

echo "== firewall (probes from mc-ci .240 and mc-cp .241)"
probe() { # host target port -> open|closed
	run "$1" "timeout 3 bash -c '</dev/tcp/$2/$3' 2>/dev/null && echo open || echo closed"
}
[ "$(probe $CI 192.168.1.247 2379)" = closed ] && ok "etcd 2379 on mc-pg-01 closed to mc-ci" || bad "etcd open to mc-ci"
[ "$(probe $CI 192.168.1.243 2379)" = closed ] && ok "etcd 2379 on mc-db closed to mc-ci" || bad "mc-db etcd open to mc-ci"
[ "$(probe $CI 192.168.1.248 8008)" = closed ] && ok "patroni 8008 closed to mc-ci" || bad "8008 open to mc-ci"
[ "$(probe $CI 192.168.1.247 5432)" = open ] && ok "5432 open to mc-ci (operator/tests)" || bad "5432 closed to mc-ci"
[ "$(probe mesh@192.168.1.241 192.168.1.247 5432)" = closed ] && ok "5432 closed to mc-cp (clients go through PgBouncer, P13b)" || bad "5432 open to mc-cp"
[ "$(probe $CI 192.168.1.243 6432)" = open ] && ok "mc-db PgBouncer 6432 still reachable from mc-ci" || bad "mc-db PgBouncer unreachable"

echo
[ "$fail" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"
