#!/usr/bin/env bash
# Throwaway PostgreSQL + PgBouncer for the P14a harness on one host (dev
# and smoke runs; the lab benchmark uses mc-db, infra/lab/db). Same PgBouncer
# settings as infra/lab/db/setup-db.sh and the P05 E2E (ADR 0006: transaction
# pool, max_prepared_statements 200, SCRAM), plus pg_stat_statements and a
# stats user for SHOW POOLS/STATS.
#
#   pgstack.sh up   DIR LISTEN_ADDR   fresh containers; password generated into DIR (0600 / 0700 dir)
#   pgstack.sh down DIR               remove containers, network and DIR
#
# DIR/pg.pass holds the password (for PG_PASSFILE), DIR/pgpass a libpq
# passfile mounted read-only into the PostgreSQL container for the samplers.
# Nothing secret goes on a command line or into a log.
set -euo pipefail

NAME=${PGSTACK_NAME:-p14}
NET=$NAME-pgnet
LABEL=mesh-bench=$NAME
PG_IMAGE=${PG_IMAGE:-postgres:16}
PGB_IMAGE=${PGB_IMAGE:-edoburu/pgbouncer:v1.24.1-p1}
POOL=${PGB_POOL:-20}

down() {
	docker rm -fv "$NAME-pgb" "$NAME-pg" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	[ -n "${1:-}" ] && rm -rf "$1"
	test -z "$(docker ps -aq --filter "label=$LABEL")"
}

case ${1:-} in
up)
	dir=$2 listen=$3
	down "$dir"
	umask 077
	mkdir -p "$dir"
	chmod 700 "$dir"
	head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32 >"$dir/pg.pass"
	pass=$(cat "$dir/pg.pass")
	printf '"headscale" "%s"\n' "$pass" >"$dir/userlist.txt"
	printf '*:*:*:headscale:%s\n' "$pass" >"$dir/pgpass"
	unset pass
	cat >"$dir/pgbouncer.ini" <<EOF
[databases]
headscale = host=$NAME-pg port=5432 dbname=headscale

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction
max_prepared_statements = 200
default_pool_size = $POOL
max_db_connections = 80
max_client_conn = 4000
query_wait_timeout = 5
idle_transaction_timeout = 5
stats_users = headscale
ignore_startup_parameters = extra_float_digits
EOF
	# Readable by the container users through the bind mounts; the mode-700
	# directory keeps other host users out.
	chmod 644 "$dir/pg.pass" "$dir/userlist.txt" "$dir/pgbouncer.ini"
	docker image inspect "$PGB_IMAGE" >/dev/null 2>&1 || docker pull -q "$PGB_IMAGE" >/dev/null
	docker image inspect "$PG_IMAGE" >/dev/null 2>&1 || docker pull -q "$PG_IMAGE" >/dev/null
	docker network create --label "$LABEL" "$NET" >/dev/null
	docker run -d --name "$NAME-pg" --label "$LABEL" --network "$NET" \
		-e POSTGRES_USER=headscale -e POSTGRES_DB=headscale \
		-e POSTGRES_PASSWORD_FILE=/run/bench/pass -e POSTGRES_INITDB_ARGS=--auth=scram-sha-256 \
		-v "$dir/pg.pass:/run/bench/pass:ro" -v "$dir/pgpass:/run/bench/pgpass:ro" \
		"$PG_IMAGE" -c shared_preload_libraries=pg_stat_statements -c max_connections=200 >/dev/null
	# Over TCP, twice in a row: the image's first start runs a temporary
	# server on the unix socket only, then restarts (see infra/lab/e2e/p05/pg.sh).
	ready=0
	for _ in $(seq 1 90); do
		if docker exec "$NAME-pg" pg_isready -h 127.0.0.1 -U headscale -d headscale >/dev/null 2>&1; then
			ready=$((ready + 1))
			[ "$ready" -ge 2 ] && break
		else
			ready=0
		fi
		sleep 1
	done
	[ "$ready" -ge 2 ] || { echo "postgres not ready" >&2; exit 1; }
	# The passfile is mounted 0644 for the container user; libpq wants 0600
	# from its owner, so copy it inside the container.
	docker exec "$NAME-pg" sh -c 'install -m 0600 -o postgres /run/bench/pgpass /tmp/pgpass'
	docker exec -e PGPASSFILE=/tmp/pgpass -u postgres "$NAME-pg" \
		psql -q -h 127.0.0.1 -U headscale -d headscale -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements' >/dev/null
	docker run -d --name "$NAME-pgb" --label "$LABEL" --network "$NET" -p "$listen:6432" \
		-v "$dir/pgbouncer.ini:/etc/pgbouncer/pgbouncer.ini:ro" \
		-v "$dir/userlist.txt:/etc/pgbouncer/userlist.txt:ro" "$PGB_IMAGE" >/dev/null
	for _ in $(seq 1 30); do
		docker logs "$NAME-pgb" 2>&1 | grep -q "process up" && break
		sleep 1
	done
	chmod 600 "$dir/pg.pass"
	echo "postgres $(docker exec "$NAME-pg" postgres --version | awk '{print $3}'), pgbouncer $(docker exec "$NAME-pgb" pgbouncer --version 2>/dev/null | head -1 | awk '{print $NF}') on $listen (pool $POOL)"
	echo "samplers: PG_PSQL='docker exec -i -e PGPASSFILE=/tmp/pgpass -u postgres $NAME-pg psql -h 127.0.0.1 -U headscale -d headscale'"
	echo "          PGB_PSQL='docker exec -i -e PGPASSFILE=/tmp/pgpass -u postgres $NAME-pg psql -h $NAME-pgb -p 6432 -U headscale -d pgbouncer'"
	;;
down)
	down "${2:-}"
	echo "removed $NAME containers"
	;;
*)
	echo "usage: $0 up DIR LISTEN_ADDR | down DIR" >&2
	exit 2
	;;
esac
