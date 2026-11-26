#!/usr/bin/env bash
# pgBackRest repository host for the lab HA cluster mc-pg (P13c, ADR 0012).
# Run as root on mc-db (7704, 192.168.1.243):
#
#   sudo bash setup-pgbackrest-repo.sh [PUBKEY_FILE]
#
# The repository lives outside the PostgreSQL failure domain, on mc-db's
# disk, and both PG nodes reach it over SFTP (pgBackRest repo1-type=sftp):
# whichever node is primary archives WAL and takes backups, and a restore
# can run on either node (F08 restores on the replica). mc-db gets no
# pgBackRest package and no PostgreSQL change; Docker, mc-pg, mc-pgb and the
# etcd witness are not touched.
#
# - System user `pgbackrest` (home /var/lib/pgbackrest-mc-pg, 0750), repo in
#   /var/lib/pgbackrest-mc-pg/repo.
# - sshd drop-in: that user gets SFTP only (ForceCommand internal-sftp, no
#   TTY, no forwarding); `sshd -t` before the reload; reload keeps existing
#   sessions.
# - PUBKEY_FILE (optional): public keys of the PG nodes' postgres user, one
#   per line; installed as authorized_keys with from="<PG nodes>",restrict.
#   The file replaces the previous key list (deploy.sh sends both keys).
#
# Size: retention 2 full backups + the WAL they need (pgbackrest.conf on the
# PG nodes); the cluster is < 1 GB today, mc-db has > 50 GB free.
set -euo pipefail

USER_NAME=pgbackrest
HOME_DIR=/var/lib/pgbackrest-mc-pg
PG_NODES=192.168.1.247,192.168.1.248
[ "$(hostname -s)" = mc-db ] || { echo "run on mc-db" >&2; exit 1; }

if ! id "$USER_NAME" >/dev/null 2>&1; then
	useradd --system --home-dir "$HOME_DIR" --create-home --shell /bin/sh "$USER_NAME"
	# No password login at all (keys only, SFTP only).
	passwd -l "$USER_NAME" >/dev/null
fi
install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$HOME_DIR" "$HOME_DIR/repo"
install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$HOME_DIR/.ssh"

if [ -n "${1:-}" ]; then
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT
	while read -r t k _; do
		[[ $t == ssh-ed25519 && $k =~ ^[A-Za-z0-9+/=]+$ ]] || { echo "unexpected key line" >&2; exit 1; }
		printf 'from="%s",restrict %s %s pgbackrest-mc-pg\n' "$PG_NODES" "$t" "$k"
	done <"$1" >"$tmp"
	[ -s "$tmp" ] || { echo "no keys" >&2; exit 1; }
	install -m 0600 -o "$USER_NAME" -g "$USER_NAME" "$tmp" "$HOME_DIR/.ssh/authorized_keys"
fi

conf=/etc/ssh/sshd_config.d/70-pgbackrest-repo.conf
cat >"$conf.new" <<EOF
# Managed by infra/lab/pg/setup-pgbackrest-repo.sh (P13c): pgBackRest SFTP
# repository of the mc-pg cluster. SFTP only for this user.
Match User $USER_NAME
	ForceCommand internal-sftp
	PermitTTY no
	AllowTcpForwarding no
	AllowAgentForwarding no
	X11Forwarding no
	PermitTunnel no
EOF
if ! cmp -s "$conf.new" "$conf"; then
	mv "$conf.new" "$conf"
	chmod 0644 "$conf"
	sshd -t
	systemctl reload ssh.service
else
	rm -f "$conf.new"
fi
echo "mc-db: pgBackRest SFTP repo $HOME_DIR/repo for $USER_NAME ($( (wc -l <"$HOME_DIR/.ssh/authorized_keys") 2>/dev/null || echo 0) keys); free: $(df -h --output=avail "$HOME_DIR" | tail -1 | tr -d ' ')"
