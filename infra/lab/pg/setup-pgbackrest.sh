#!/usr/bin/env bash
# pgBackRest client side on a PG node of the lab HA cluster (P13c, ADR 0012).
# Run as root on mc-pg-01 and mc-pg-02 (deploy.sh pgbackrest does the whole
# order, including the repository host mc-db):
#
#   sudo SECRETS=gen  bash setup-pgbackrest.sh   first node: create the cipher
#                                                passphrase
#   sudo SECRETS=keep bash setup-pgbackrest.sh   default: it must exist
#                                                (copied from the first node)
#
# Files expected next to this script: pgbackrest.conf, known_hosts_pgbackrest
# (mc-db's host keys, "192.168.1.243 <type> <key>" lines, from deploy.sh).
#
# Idempotent. Does NOT create the stanza or change archive_command (deploy.sh
# does that once, on the leader, after both nodes and the repository are
# ready).
#
# Secrets (AGENTS.md rule 2): /etc/mc-pg/pgbackrest.pass (root 0600, letters
# and digits, 48 chars) -> /etc/pgbackrest/conf.d/cipher.conf (root:postgres
# 0640). The postgres user's SFTP key (/var/lib/postgresql/.ssh, 0600) never
# leaves the node; only its public half goes to mc-db. Nothing is printed.
set -euo pipefail

SECRETS=${SECRETS:-keep}
HERE=$(cd "$(dirname "$0")" && pwd)
PGHOME=/var/lib/postgresql
name=$(hostname -s)
case $name in mc-pg-01 | mc-pg-02) ;; *) echo "host $name is not a PG node" >&2; exit 1 ;; esac
for f in pgbackrest.conf known_hosts_pgbackrest; do
	[ -s "$HERE/$f" ] || { echo "$HERE/$f missing" >&2; exit 1; }
done
command -v pgbackrest >/dev/null || { echo "pgbackrest missing (setup-pg.sh installs it)" >&2; exit 1; }

# --- cipher passphrase ---------------------------------------------------------
install -d -m 0700 -o root -g root /etc/mc-pg
if [ ! -s /etc/mc-pg/pgbackrest.pass ]; then
	[ "$SECRETS" = gen ] || { echo "/etc/mc-pg/pgbackrest.pass missing (SECRETS=gen on the first node, then copy)" >&2; exit 1; }
	(
		umask 077
		s=$(head -c 8192 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')
		[ "${#s}" -ge 48 ] || exit 1
		printf '%s' "${s:0:48}" >/etc/mc-pg/pgbackrest.pass
	)
fi
chmod 0600 /etc/mc-pg/pgbackrest.pass
grep -qE '^[A-Za-z0-9]{48}$' /etc/mc-pg/pgbackrest.pass || { echo "pgbackrest.pass malformed" >&2; exit 1; }

# --- configuration -------------------------------------------------------------
install -d -m 0750 -o root -g postgres /etc/pgbackrest /etc/pgbackrest/conf.d
install -m 0640 -o root -g postgres "$HERE/pgbackrest.conf" /etc/pgbackrest/pgbackrest.conf
(
	umask 027
	printf '[global]\nrepo1-cipher-pass=%s\n' "$(cat /etc/mc-pg/pgbackrest.pass)" >/etc/pgbackrest/conf.d/cipher.conf.new
)
chown root:postgres /etc/pgbackrest/conf.d/cipher.conf.new
chmod 0640 /etc/pgbackrest/conf.d/cipher.conf.new
mv /etc/pgbackrest/conf.d/cipher.conf.new /etc/pgbackrest/conf.d/cipher.conf
# The package's sample at the old default path is only read when the new
# default is missing; keep it out of the way so nobody edits the wrong file.
[ -e /etc/pgbackrest.conf ] && mv /etc/pgbackrest.conf /etc/pgbackrest.conf.package-sample
install -d -m 0770 -o postgres -g postgres /var/log/pgbackrest

# --- SFTP key and mc-db's host keys ---------------------------------------------
install -d -m 0700 -o postgres -g postgres "$PGHOME/.ssh"
if [ ! -s "$PGHOME/.ssh/pgbackrest_ed25519" ]; then
	runuser -u postgres -- ssh-keygen -q -t ed25519 -N '' -C "pgbackrest@$name" -f "$PGHOME/.ssh/pgbackrest_ed25519"
fi
while read -r h t k; do
	[ "$h" = 192.168.1.243 ] && [[ $t =~ ^(ssh-ed25519|ecdsa-sha2-nistp256|ssh-rsa)$ ]] && [[ $k =~ ^[A-Za-z0-9+/=]+$ ]] ||
		{ echo "known_hosts_pgbackrest: unexpected line" >&2; exit 1; }
done <"$HERE/known_hosts_pgbackrest"
install -m 0644 -o postgres -g postgres "$HERE/known_hosts_pgbackrest" "$PGHOME/.ssh/known_hosts_pgbackrest"

echo "$name: pgbackrest $(pgbackrest version | awk '{print $2}'), repo sftp://pgbackrest@192.168.1.243/var/lib/pgbackrest-mc-pg/repo, key $(ssh-keygen -lf "$PGHOME/.ssh/pgbackrest_ed25519.pub" | awk '{print $2}')"
