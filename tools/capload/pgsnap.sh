#!/usr/bin/env bash
# Database snapshots of a P14a cell (evidence/P14-benchmark-plan.md §4,
# step 1): seed once, then start every cell from a copy in seconds.
#
#   pgsnap.sh save    DB SNAP   CREATE DATABASE SNAP TEMPLATE DB
#   pgsnap.sh restore SNAP DB   DROP DATABASE DB; CREATE DATABASE DB TEMPLATE SNAP; VACUUM ANALYZE
#   pgsnap.sh drop    DB
#
# PG_ADMIN_PSQL is a psql command prefix with the right to create and drop
# databases, connected to a maintenance database (e.g. `docker exec -i
# mc-pg psql -U postgres -d postgres` on mc-db). The cluster processes of
# the cell must be stopped: the backends of DB/SNAP are terminated (a
# template needs no other session; PgBouncer reconnects on next use).
#
# Only names matching PGSNAP_ALLOW (default ^bench_[a-z0-9_]+$) are touched;
# the lab's long-lived database `meshlab` never is.
set -euo pipefail

PG_ADMIN_PSQL=${PG_ADMIN_PSQL:?PG_ADMIN_PSQL is required}
ALLOW=${PGSNAP_ALLOW:-^bench_[a-z0-9_]+$}

guard() {
	for n in "$@"; do
		if [ "$n" = meshlab ] || ! [[ $n =~ ^[a-z][a-z0-9_]*$ ]] || ! [[ $n =~ $ALLOW ]]; then
			echo "pgsnap: refusing database name '$n' (PGSNAP_ALLOW=$ALLOW)" >&2
			exit 2
		fi
	done
}

psql_() { $PG_ADMIN_PSQL -X -q -v ON_ERROR_STOP=1 "$@"; }

terminate() {
	psql_ -c "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = '$1' AND pid <> pg_backend_pid()" >/dev/null
}

case ${1:-} in
save)
	src=$2 snap=$3
	guard "$src" "$snap"
	terminate "$src"
	psql_ -c "DROP DATABASE IF EXISTS $snap" -c "CREATE DATABASE $snap TEMPLATE $src"
	echo "saved $src -> $snap ($(psql_ -At -c "SELECT pg_size_pretty(pg_database_size('$snap'))"))"
	;;
restore)
	snap=$2 dst=$3
	guard "$snap" "$dst"
	terminate "$dst"
	terminate "$snap"
	owner=$(psql_ -At -c "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '$snap'")
	t0=$(date +%s.%N)
	psql_ -c "DROP DATABASE IF EXISTS $dst" -c "CREATE DATABASE $dst TEMPLATE $snap OWNER $owner"
	t1=$(date +%s.%N)
	# Fresh planner statistics for the copy, as a seeded database would have.
	${PG_ADMIN_PSQL/-d postgres/-d $dst} -X -q -c 'VACUUM ANALYZE' >/dev/null
	echo "restored $snap -> $dst copy_s=$(echo "$t1 - $t0" | bc) analyze_s=$(echo "$(date +%s.%N) - $t1" | bc)"
	;;
drop)
	guard "$2"
	terminate "$2"
	psql_ -c "DROP DATABASE IF EXISTS $2"
	echo "dropped $2"
	;;
*)
	echo "usage: $0 save DB SNAP | restore SNAP DB | drop DB" >&2
	exit 2
	;;
esac
