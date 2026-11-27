#!/usr/bin/env bash
# PostgreSQL and PgBouncer samplers of a P14a cell (1 s), on the database
# host. PG_PSQL and PGB_PSQL are psql command prefixes for the cell's
# database and the PgBouncer admin console (pgstack.sh up prints them for a
# throwaway stack; on mc-db, `docker exec -i mc-pg psql -U postgres -d DB`).
#
#   pgsample.sh start DIR    pg.csv (one held connection) and pgb.csv
#   pgsample.sh stop  DIR    stop; pg-final.txt (top statements, table sizes)
#   pgsample.sh reset        pg_stat_statements_reset() and pg_stat_reset() of the cell database
#
# The sampler's own connection is one backend (numbackends counts it).
set -euo pipefail

cmd=$1

PG_PSQL=${PG_PSQL:?PG_PSQL is required}

pg_query() {
	cat <<'SQL'
SELECT extract(epoch FROM clock_timestamp())::numeric(16,3), d.xact_commit, d.xact_rollback,
  d.tup_inserted, d.tup_updated, d.tup_deleted, d.tup_returned, d.tup_fetched, d.numbackends,
  (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = current_database() AND a.state = 'active'),
  (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = current_database() AND a.state = 'idle'),
  (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = current_database() AND a.state LIKE 'idle in transaction%'),
  (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = current_database() AND a.wait_event_type = 'Lock'),
  pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint
FROM pg_stat_database d WHERE d.datname = current_database();
SQL
}

case $cmd in
start)
	dir=$2
	mkdir -p "$dir"
	echo "t,xact_commit,xact_rollback,tup_inserted,tup_updated,tup_deleted,tup_returned,tup_fetched,numbackends,active,idle,idle_in_tx,lock_wait,wal_bytes" >"$dir/pg.csv"
	q=$(pg_query)
	nohup bash -c '
		q=$1; while :; do printf "%s\n" "$q"; sleep 1; done' _ "$q" |
		nohup $PG_PSQL -X -q -At -F, >>"$dir/pg.csv" 2>"$dir/pg.err" &
	echo $! >"$dir/pgsample.pid"
	if [ -n "${PGB_PSQL:-}" ]; then
		nohup bash -c '
			out=$1; shift
			while :; do
				t=$(date +%s.%N)
				$@ -X -At -F, -c "SHOW POOLS" 2>/dev/null | sed "s/^/pools,$t,/" >>"$out"
				$@ -X -At -F, -c "SHOW STATS" 2>/dev/null | sed "s/^/stats,$t,/" >>"$out"
				sleep 1
			done' _ "$dir/pgb.csv" $PGB_PSQL >/dev/null 2>&1 &
		echo $! >"$dir/pgbsample.pid"
		# Column names, once (they differ between PgBouncer versions).
		{ $PGB_PSQL -X -F, -c "SHOW POOLS" 2>/dev/null | sed -n 1p | sed 's/^ *//; s/ *| */,/g; s/^/pools,t,/'
		  $PGB_PSQL -X -F, -c "SHOW STATS" 2>/dev/null | sed -n 1p | sed 's/^ *//; s/ *| */,/g; s/^/stats,t,/'; } >"$dir/pgb-columns.csv" || true
	fi
	echo "pg samplers started in $dir"
	;;
stop)
	dir=$2
	for p in pgsample pgbsample; do
		if [ -f "$dir/$p.pid" ]; then
			pkill -P "$(cat "$dir/$p.pid")" 2>/dev/null || true
			kill "$(cat "$dir/$p.pid")" 2>/dev/null || true
			rm -f "$dir/$p.pid"
		fi
	done
	# Statement texts are parameterised ($1…): no values, no secrets.
	$PG_PSQL -X -q <<'SQL' >"$dir/pg-final.txt" 2>&1 || true
\pset pager off
SELECT version();
SELECT name, setting FROM pg_settings WHERE source NOT IN ('default', 'override') ORDER BY name;
SELECT calls, round(total_exec_time::numeric, 1) AS total_ms, round(mean_exec_time::numeric, 3) AS mean_ms,
  round(max_exec_time::numeric, 1) AS max_ms, rows, left(regexp_replace(query, '\s+', ' ', 'g'), 160) AS query
FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY total_exec_time DESC LIMIT 30;
SELECT relname, n_tup_ins, n_tup_upd, n_tup_hot_upd, n_tup_del, n_live_tup,
  pg_size_pretty(pg_total_relation_size(relid)) AS total_size
FROM pg_stat_user_tables ORDER BY pg_total_relation_size(relid) DESC LIMIT 15;
SQL
	echo "pg samplers stopped; $dir/pg-final.txt"
	;;
reset)
	$PG_PSQL -X -q -c 'SELECT pg_stat_statements_reset()' -c 'SELECT pg_stat_reset()' >/dev/null
	echo "statistics reset"
	;;
*)
	echo "usage: $0 start|stop DIR | reset" >&2
	exit 2
	;;
esac
