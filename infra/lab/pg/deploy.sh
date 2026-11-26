#!/usr/bin/env bash
# Operator-side deploy of the lab PostgreSQL HA cluster (P13a). Runs from the
# laptop (Git Bash or WSL) over SSH as user `mesh`:
#
#   bash infra/lab/pg/deploy.sh etcd      etcd on mc-pg-01, mc-pg-02, mc-db
#   bash infra/lab/pg/deploy.sh pg        Patroni on mc-pg-01 (gen secrets),
#                                         copy secrets, then mc-pg-02
#   bash infra/lab/pg/deploy.sh pgbouncer PgBouncer + local HAProxy on both
#                                         PG nodes (P13b), one node at a time
#   bash infra/lab/pg/deploy.sh pgbackrest pgBackRest (P13c): SFTP repo on
#                                         mc-db, client config + cipher pass on
#                                         both PG nodes, stanza-create, Patroni
#                                         archive_command, check (no backup)
#   bash infra/lab/pg/deploy.sh status    etcd + Patroni status (no secrets)
#
# Secrets move only through pipes between the two PG nodes and into
# $MESH_LAB_DIR/mc-pg.env on the laptop; nothing is echoed (AGENTS.md rule 2).
# mc-db gets only the etcd witness (no restart of Docker, mc-pg or mc-pgb).
set -euo pipefail

MESH_LAB_DIR=${MESH_LAB_DIR:-$HOME/.mesh-lab}
SSH=(ssh -i "$MESH_LAB_DIR/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
SCP=(scp -q -i "$MESH_LAB_DIR/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
PG1=mesh@192.168.1.247
PG2=mesh@192.168.1.248
DB=mesh@192.168.1.243
HERE=$(cd "$(dirname "$0")" && pwd)
REMOTE=/tmp/mc-pg-setup

push() { # host files...
	local h=$1; shift
	"${SSH[@]}" "$h" "rm -rf $REMOTE && mkdir -p $REMOTE"
	"${SCP[@]}" "$@" "$h:$REMOTE/"
}

case ${1:-} in
etcd)
	for h in $PG1 $PG2 $DB; do
		push "$h" "$HERE/setup-etcd.sh"
		"${SSH[@]}" "$h" "sudo bash $REMOTE/setup-etcd.sh"
	done
	for _ in $(seq 1 30); do
		"${SSH[@]}" $PG1 "etcdctl --endpoints=http://192.168.1.247:2379,http://192.168.1.248:2379,http://192.168.1.243:2379 endpoint health" && break
		sleep 2
	done
	;;
pg)
	push $PG1 "$HERE/setup-pg.sh" "$HERE/patroni.yml.tmpl" "$HERE/mc-pg.nft"
	"${SSH[@]}" $PG1 "sudo SECRETS=gen bash $REMOTE/setup-pg.sh"
	# Same secrets on mc-pg-02 (root-only files; tar over the SSH pipe).
	"${SSH[@]}" $PG1 "sudo tar -C /etc/mc-pg -cf - patroni.env headscale.pass" |
		"${SSH[@]}" $PG2 "sudo install -d -m 0700 /etc/mc-pg && sudo tar -C /etc/mc-pg --no-same-owner -xf - && sudo chmod 0600 /etc/mc-pg/patroni.env /etc/mc-pg/headscale.pass"
	# Operator copy (laptop), never printed.
	(
		umask 077
		"${SSH[@]}" $PG1 'sudo cat /etc/mc-pg/patroni.env; printf "MC_PG_HEADSCALE_PASSWORD=%s\n" "$(sudo cat /etc/mc-pg/headscale.pass)"' >"$MESH_LAB_DIR/mc-pg.env"
	)
	if command -v icacls.exe >/dev/null 2>&1; then
		icacls.exe "$(cygpath -w "$MESH_LAB_DIR/mc-pg.env" 2>/dev/null || wslpath -w "$MESH_LAB_DIR/mc-pg.env")" /inheritance:r /grant:r "$USERNAME:F" >/dev/null
	fi
	push $PG2 "$HERE/setup-pg.sh" "$HERE/patroni.yml.tmpl" "$HERE/mc-pg.nft"
	"${SSH[@]}" $PG2 "sudo SECRETS=keep bash $REMOTE/setup-pg.sh"
	;;
pgbouncer)
	# The replica first: a mistake there never touches the primary's node.
	PGB=$HERE/../pgbouncer
	order=$("${SSH[@]}" $PG1 "curl -fsS http://127.0.0.1:8008/cluster | jq -r '[.members[] | select(.role!=\"leader\")] + [.members[] | select(.role==\"leader\")] | .[].host'")
	for ip in $order; do
		push "mesh@$ip" "$HERE/setup-pgbouncer.sh" "$PGB/haproxy-pg.cfg" "$PGB/pgbouncer.ini.tmpl" "$PGB/pgbouncer-clusters.ini"
		"${SSH[@]}" "mesh@$ip" "sudo bash $REMOTE/setup-pgbouncer.sh"
	done
	;;
pgbackrest)
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT
	# mc-db's host keys over the operator's authenticated SSH (no TOFU).
	"${SSH[@]}" $DB 'cat /etc/ssh/ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ecdsa_key.pub /etc/ssh/ssh_host_rsa_key.pub' |
		awk '{print "192.168.1.243", $1, $2}' >"$work/known_hosts_pgbackrest"
	push $DB "$HERE/setup-pgbackrest-repo.sh"
	"${SSH[@]}" $DB "sudo bash $REMOTE/setup-pgbackrest-repo.sh"
	push $PG1 "$HERE/setup-pgbackrest.sh" "$HERE/pgbackrest.conf" "$work/known_hosts_pgbackrest"
	"${SSH[@]}" $PG1 "sudo SECRETS=gen bash $REMOTE/setup-pgbackrest.sh"
	"${SSH[@]}" $PG1 "sudo tar -C /etc/mc-pg -cf - pgbackrest.pass" |
		"${SSH[@]}" $PG2 "sudo install -d -m 0700 /etc/mc-pg && sudo tar -C /etc/mc-pg --no-same-owner -xf - && sudo chmod 0600 /etc/mc-pg/pgbackrest.pass"
	push $PG2 "$HERE/setup-pgbackrest.sh" "$HERE/pgbackrest.conf" "$work/known_hosts_pgbackrest"
	"${SSH[@]}" $PG2 "sudo SECRETS=keep bash $REMOTE/setup-pgbackrest.sh"
	# Public keys of both nodes -> authorized_keys of the repo user.
	for h in $PG1 $PG2; do "${SSH[@]}" "$h" 'sudo cat /var/lib/postgresql/.ssh/pgbackrest_ed25519.pub'; done >"$work/pubkeys"
	"${SCP[@]}" "$work/pubkeys" "$DB:$REMOTE/pubkeys"
	"${SSH[@]}" $DB "sudo bash $REMOTE/setup-pgbackrest-repo.sh $REMOTE/pubkeys && rm -f $REMOTE/pubkeys"
	# Operator copy of the cipher passphrase (restore without the nodes), never printed.
	(
		umask 077
		grep -v '^MC_PG_PGBACKREST_CIPHER_PASS=' "$MESH_LAB_DIR/mc-pg.env" >"$work/env" || true
		"${SSH[@]}" $PG1 'printf "MC_PG_PGBACKREST_CIPHER_PASS=%s
" "$(sudo cat /etc/mc-pg/pgbackrest.pass)"' >>"$work/env"
		cat "$work/env" >"$MESH_LAB_DIR/mc-pg.env"
	)
	# Stanza and archiving on the leader (archive_mode is already on since
	# P13a: archive_command is a reload-only change in the Patroni DCS).
	leader=$("${SSH[@]}" $PG1 "curl -fsS http://127.0.0.1:8008/cluster | jq -r '.members[] | select(.role==\"leader\") | .host'")
	"${SSH[@]}" "mesh@$leader" "sudo -u postgres pgbackrest --stanza=mc-pg stanza-create"
	"${SSH[@]}" $PG1 "sudo mc-patronictl edit-config --force -s 'postgresql.parameters.archive_command=pgbackrest --stanza=mc-pg archive-push %p'" >/dev/null
	for _ in $(seq 1 30); do
		[ "$("${SSH[@]}" "mesh@$leader" "sudo -u postgres psql -X -At -c 'SHOW archive_command'")" = "pgbackrest --stanza=mc-pg archive-push %p" ] && break
		sleep 1
	done
	"${SSH[@]}" "mesh@$leader" "sudo -u postgres pgbackrest --stanza=mc-pg check"
	"${SSH[@]}" "mesh@$leader" "sudo -u postgres pgbackrest --stanza=mc-pg info"
	;;
status)
	"${SSH[@]}" $PG1 "etcdctl --endpoints=http://192.168.1.247:2379,http://192.168.1.248:2379,http://192.168.1.243:2379 endpoint status -w table"
	"${SSH[@]}" $PG1 "sudo mc-patronictl list"
	;;
*)
	echo "usage: $0 etcd|pg|pgbouncer|pgbackrest|status" >&2
	exit 2
	;;
esac
