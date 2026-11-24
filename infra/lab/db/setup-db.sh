#!/usr/bin/env bash
# Long-lived PostgreSQL 16 + PgBouncer 1.24.1 on mc-db (ADR 0005, ADR 0006).
# Idempotent: run again to apply a changed config; data stays in the
# mc-pg-data volume. Run as root on mc-db:
#
#   sudo bash setup-db.sh           two lines on stdin: postgres superuser
#                                   password, then the headscale role's
#
# - PostgreSQL is reachable only on the container network; clients go
#   through PgBouncer on LISTEN (lab LAN), transaction mode,
#   max_prepared_statements = 200, SCRAM.
# - Databases: `meshlab`, long-lived (lab stage 2, manual tests, long runs);
#   per-run E2E databases e2e_* are created and dropped by the harness
#   (`db.sh`). Every database is owned by role `headscale`.
# - Passwords live in /opt/mc-db (root, 0700) and in the operator's
#   %USERPROFILE%\.mesh-lab\mc-db.env; never on a command line or in a log.
set -euo pipefail

LISTEN=${LISTEN:-192.168.1.243:6432}
PG_IMAGE=postgres:16
PGB_IMAGE=edoburu/pgbouncer:v1.24.1-p1
DIR=/opt/mc-db
NET=mc-db-net

umask 077
install -d -m 0700 "$DIR"

read -r super_pass
read -r app_pass
# Letters and digits only: they go into a psql \set and the userlist,
# quoted, never through a shell word or a command line.
for p in "$super_pass" "$app_pass"; do
	[[ ${#p} -ge 24 && $p =~ ^[A-Za-z0-9]+$ ]] || { echo "passwords: 24+ letters and digits" >&2; exit 1; }
done

# Readable by the container users through bind mounts; /opt/mc-db (0700)
# keeps other host users out.
printf '%s' "$super_pass" >"$DIR/postgres.pass"
printf '"headscale" "%s"\n' "$app_pass" >"$DIR/userlist.txt"
cat >"$DIR/pgbouncer.ini" <<'EOF'
[databases]
; Any database name: meshlab and the per-run e2e_* databases.
* = host=mc-pg port=5432

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction
max_prepared_statements = 200
default_pool_size = 20
max_client_conn = 2000
max_db_connections = 80
ignore_startup_parameters = extra_float_digits
EOF
chmod 644 "$DIR/postgres.pass" "$DIR/userlist.txt" "$DIR/pgbouncer.ini"

docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
docker image inspect "$PGB_IMAGE" >/dev/null 2>&1 || docker pull -q "$PGB_IMAGE" >/dev/null

if ! docker container inspect mc-pg >/dev/null 2>&1; then
	docker run -d --name mc-pg --restart unless-stopped --network "$NET" \
		-e POSTGRES_PASSWORD_FILE=/run/mc-db/postgres.pass -e "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --auth-local=trust" \
		-v mc-pg-data:/var/lib/postgresql/data -v "$DIR/postgres.pass:/run/mc-db/postgres.pass:ro" \
		"$PG_IMAGE" -c max_connections=200 >/dev/null
fi

# Over TCP: the first start runs a temporary socket-only server, then
# restarts (see infra/lab/e2e/p05/pg.sh).
ready=0
for _ in $(seq 1 90); do
	if docker exec mc-pg pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then
		ready=$((ready + 1))
		[ "$ready" -ge 2 ] && break
	else
		ready=0
	fi
	sleep 1
done
[ "$ready" -ge 2 ] || { echo "postgres not ready" >&2; exit 1; }

# The superuser keeps the password given now (also after a re-run); the
# headscale role is created or gets the new password; meshlab exists.
{
	printf "\\set super '%s'\n\\set app '%s'\n" "$super_pass" "$app_pass"
	cat <<'SQL'
SET client_min_messages = warning;
ALTER ROLE postgres PASSWORD :'super';
SELECT format('CREATE ROLE headscale LOGIN PASSWORD %L', :'app')
  WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'headscale') \gexec
ALTER ROLE headscale PASSWORD :'app';
SELECT 'CREATE DATABASE meshlab OWNER headscale'
  WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'meshlab') \gexec
SQL
} | docker exec -i mc-pg psql -v ON_ERROR_STOP=1 -q -U postgres -d postgres >/dev/null

docker rm -f mc-pgb >/dev/null 2>&1 || true
docker run -d --name mc-pgb --restart unless-stopped --network "$NET" -p "$LISTEN:6432" \
	-v "$DIR/pgbouncer.ini:/etc/pgbouncer/pgbouncer.ini:ro" \
	-v "$DIR/userlist.txt:/etc/pgbouncer/userlist.txt:ro" "$PGB_IMAGE" >/dev/null
for _ in $(seq 1 30); do
	docker logs mc-pgb 2>&1 | grep -q "process up" && break
	sleep 1
done
docker logs mc-pgb 2>&1 | grep -q "process up"

echo "postgres $(docker exec mc-pg postgres --version | awk '{print $3}'), pgbouncer $(docker exec mc-pgb pgbouncer --version 2>/dev/null | head -1 | awk '{print $NF}') on $LISTEN; databases: $(docker exec mc-pg psql -At -U postgres -c "SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1')")"
