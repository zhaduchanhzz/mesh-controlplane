#!/usr/bin/env bash
# Throwaway PostgreSQL + PgBouncer servers for the P13b tests (hscontrol/db
# pooler and multi-host tests). Nothing here is a lab service: every server
# runs as the calling user in a temporary directory, listens on 127.0.0.1
# (and a unix socket) only, uses trust authentication and no password, and is
# removed by `throwaway.sh stop <dir>`.
#
#   throwaway.sh start        prints the directory; <dir>/env holds the
#                             MESH_TEST_* variables for `set -a; . <dir>/env`
#   throwaway.sh stop <dir>   stops every server and removes <dir>
#
# Servers:
#   rw       PostgreSQL, writable          MESH_TEST_POSTGRES_DSN (socket), MESH_TEST_PG_RW
#   ro       PostgreSQL, default_transaction_read_only = on (stands in for a
#            replica behind a pooler)      MESH_TEST_PG_RO
#   pgb      PgBouncer -> rw, transaction mode, max_prepared_statements = 200
#                                          MESH_TEST_PGBOUNCER
#   pgb0     PgBouncer -> rw, transaction mode, max_prepared_statements = 0
#                                          MESH_TEST_PGBOUNCER_NOPREP
#   pgbro    PgBouncer -> ro, as pgb       MESH_TEST_PGBOUNCER_RO
#   pgbtrack PgBouncer -> rw, as pgb with track_extra_parameters including
#            statement_timeout             MESH_TEST_PGBOUNCER_TRACK
#
# Needs PostgreSQL binaries (/usr/lib/postgresql/*/bin, as CI) and pgbouncer
# on PATH (e.g. nix shell --inputs-from . nixpkgs#pgbouncer).
set -euo pipefail

user=$(id -un)

freeport() {
  python3 - <<'EOF'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
EOF
}

start_pg() { # name port extra-conf
  local dir=$1/$2 port=$3
  mkdir -p "$dir/sock"
  "$pgbin/initdb" -D "$dir/data" -U "$user" --auth=trust >/dev/null
  {
    echo "listen_addresses = '127.0.0.1'"
    echo "port = $port"
    echo "unix_socket_directories = '$dir/sock'"
    echo "fsync = off"
    echo "max_connections = 200"
    [ -n "${4:-}" ] && echo "$4"
  } >>"$dir/data/postgresql.conf"
  "$pgbin/pg_ctl" -D "$dir/data" -l "$dir/log" -w start >/dev/null
}

start_pgb() { # dir name port backend-port maxprep [extra]
  local dir=$1/$2 port=$3
  mkdir -p "$dir"
  printf '"%s" ""\n' "$user" >"$dir/users.txt"
  cat >"$dir/pgbouncer.ini" <<EOF
[databases]
* = host=127.0.0.1 port=$4

[pgbouncer]
listen_addr = 127.0.0.1
listen_port = $port
unix_socket_dir =
auth_type = trust
auth_file = $dir/users.txt
pool_mode = transaction
max_prepared_statements = $5
default_pool_size = 20
max_client_conn = 400
query_wait_timeout = 5
idle_transaction_timeout = 5
logfile = $dir/log
pidfile = $dir/pid
${6:-}
EOF
  pgbouncer -d "$dir/pgbouncer.ini"
  for _ in $(seq 50); do
    [ -s "$dir/pid" ] && return 0
    sleep 0.1
  done
  echo "pgbouncer $2 did not start" >&2
  return 1
}

case "${1:-}" in
start)
  pgbin=$(find /usr/lib/postgresql -mindepth 2 -maxdepth 2 -name bin -type d | sort -V | tail -1)
  command -v pgbouncer >/dev/null || { echo "pgbouncer not on PATH" >&2; exit 1; }
  dir=$(mktemp -d /tmp/p13b-pg.XXXXXX)
  prw=$(freeport) pro=$(freeport) p1=$(freeport) p0=$(freeport) p2=$(freeport) p3=$(freeport)
  start_pg "$dir" rw "$prw"
  start_pg "$dir" ro "$pro" "default_transaction_read_only = on"
  start_pgb "$dir" pgb "$p1" "$prw" 200
  start_pgb "$dir" pgb0 "$p0" "$prw" 0
  start_pgb "$dir" pgbro "$p2" "$pro" 200
  start_pgb "$dir" pgbtrack "$p3" "$prw" 200 "track_extra_parameters = IntervalStyle, statement_timeout"
  cat >"$dir/env" <<EOF
MESH_TEST_POSTGRES_DSN="host=$dir/rw/sock port=$prw user=$user dbname=postgres sslmode=disable"
MESH_TEST_REQUIRE_POSTGRES=1
MESH_TEST_PG_USER=$user
MESH_TEST_PG_RW=127.0.0.1:$prw
MESH_TEST_PG_RO=127.0.0.1:$pro
MESH_TEST_PGBOUNCER=127.0.0.1:$p1
MESH_TEST_PGBOUNCER_NOPREP=127.0.0.1:$p0
MESH_TEST_PGBOUNCER_RO=127.0.0.1:$p2
MESH_TEST_PGBOUNCER_TRACK=127.0.0.1:$p3
EOF
  echo "$dir"
  ;;
stop)
  dir=${2:?usage: throwaway.sh stop <dir>}
  case "$dir" in /tmp/p13b-pg.*) ;; *) echo "refusing to remove $dir" >&2; exit 1 ;; esac
  pgbin=$(find /usr/lib/postgresql -mindepth 2 -maxdepth 2 -name bin -type d | sort -V | tail -1)
  for b in pgb pgb0 pgbro pgbtrack; do
    [ -s "$dir/$b/pid" ] && kill "$(cat "$dir/$b/pid")" 2>/dev/null || true
  done
  for p in rw ro; do
    [ -d "$dir/$p/data" ] && "$pgbin/pg_ctl" -D "$dir/$p/data" -m immediate stop >/dev/null 2>&1 || true
  done
  rm -rf "$dir"
  ;;
*)
  echo "usage: throwaway.sh start | stop <dir>" >&2
  exit 2
  ;;
esac
