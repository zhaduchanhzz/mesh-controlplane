#!/usr/bin/env bash
# PostgreSQL + PgBouncer for the P05 cluster E2E (DB=postgres in run.sh), on
# the lab host that runs it (mc-bench). ADR 0005 (shared PostgreSQL) and ADR
# 0006 (PgBouncer, transaction mode, max_prepared_statements=200, SCRAM).
#
#   pg.sh up LISTEN_ADDR   password on stdin; fresh containers each run
#   pg.sh down             remove containers, network and files
#
# The password is a short-lived test credential of this run: it lives only in
# a mode-700 directory here (and in a mode-0640 file on mc-cp), never on the
# command line, in an environment listing or in a log.
set -euo pipefail

DIR=$HOME/p05pg
NET=p05pg-net
LABEL=mesh-e2e=p05pg
PG_IMAGE=${PG_IMAGE:-postgres:16}
PGB_IMAGE=${PGB_IMAGE:-edoburu/pgbouncer:v1.24.1-p1}

down() {
	docker rm -fv p05-pgb p05-pg >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	rm -rf "$DIR"
	test -z "$(docker ps -aq --filter label=$LABEL)" && ! test -e "$DIR"
}

case ${1:-} in
up)
	listen=$2
	down
	umask 077
	mkdir -p "$DIR"
	chmod 700 "$DIR"
	pass=$(cat)
	[ -n "$pass" ] || { echo "no password on stdin" >&2; exit 1; }
	# Readable by the container users (postgres, pgbouncer) through the
	# bind mounts; the mode-700 directory keeps other host users out.
	printf '%s' "$pass" >"$DIR/pass"
	printf '"headscale" "%s"\n' "$pass" >"$DIR/userlist.txt"
	cat >"$DIR/pgbouncer.ini" <<'EOF'
[databases]
headscale = host=p05-pg port=5432 dbname=headscale

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction
max_prepared_statements = 200
default_pool_size = 20
max_client_conn = 2000
ignore_startup_parameters = extra_float_digits
EOF
	chmod 644 "$DIR/pass" "$DIR/userlist.txt" "$DIR/pgbouncer.ini"
	docker image inspect "$PGB_IMAGE" >/dev/null 2>&1 || docker pull -q "$PGB_IMAGE" >/dev/null
	docker network create --label "$LABEL" "$NET" >/dev/null
	docker run -d --name p05-pg --label "$LABEL" --network "$NET" \
		-e POSTGRES_USER=headscale -e POSTGRES_DB=headscale \
		-e POSTGRES_PASSWORD_FILE=/run/p05/pass -e POSTGRES_INITDB_ARGS=--auth=scram-sha-256 \
		-v "$DIR/pass:/run/p05/pass:ro" "$PG_IMAGE" >/dev/null
	# Over TCP: the image's first start runs a temporary server on the unix
	# socket only (initdb scripts), then restarts; a socket check can pass
	# against it and fail right after. Two passes in a row, a second apart.
	ready=0
	for _ in $(seq 1 90); do
		if docker exec p05-pg pg_isready -h 127.0.0.1 -U headscale -d headscale >/dev/null 2>&1; then
			ready=$((ready + 1))
			[ "$ready" -ge 2 ] && break
		else
			ready=0
		fi
		sleep 1
	done
	[ "$ready" -ge 2 ] || { echo "postgres not ready" >&2; exit 1; }
	docker run -d --name p05-pgb --label "$LABEL" --network "$NET" -p "$listen:6432" \
		-v "$DIR/pgbouncer.ini:/etc/pgbouncer/pgbouncer.ini:ro" \
		-v "$DIR/userlist.txt:/etc/pgbouncer/userlist.txt:ro" "$PGB_IMAGE" >/dev/null
	for _ in $(seq 1 30); do
		docker logs p05-pgb 2>&1 | grep -q "process up" && break
		sleep 1
	done
	echo "postgres $(docker exec p05-pg postgres --version | awk '{print $3}'), pgbouncer $(docker exec p05-pgb pgbouncer --version 2>/dev/null | head -1 | awk '{print $NF}') on $listen"
	;;
down)
	down
	;;
*)
	echo "usage: $0 up LISTEN_ADDR | down" >&2
	exit 2
	;;
esac
