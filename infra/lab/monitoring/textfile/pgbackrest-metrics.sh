#!/usr/bin/env bash
# P13f: pgBackRest metrics for node_exporter's textfile collector on mc-db
# (the repository host, P13c). NOT INSTALLED: README.md "Backups".
#
# Run as the pgbackrest repository user from a systemd timer (every 5 min):
#   pgbackrest-metrics.sh <stanza> [--check]
# --check also runs `pgbackrest check` (forces a WAL switch and waits for
# its archive), every 15 min is enough.
#
# Writes /var/lib/mc-mon/textfile/pgbackrest.prom atomically. Only times and
# status codes; no path, host, credential or WAL content.
set -euo pipefail

stanza=${1:?usage: pgbackrest-metrics.sh <stanza> [--check]}
check=${2:-}
out_dir=${OUT_DIR:-/var/lib/mc-mon/textfile}
repo=${PGBACKREST_REPO_PATH:-/var/lib/pgbackrest}

tmp=$(mktemp "$out_dir/.pgbackrest.prom.XXXXXX")
trap 'rm -f "$tmp"' EXIT

info=$(pgbackrest --stanza="$stanza" --output=json info)

{
	echo '# HELP pgbackrest_backup_last_completed_timestamp_seconds Stop time of the newest backup of each type.'
	echo '# TYPE pgbackrest_backup_last_completed_timestamp_seconds gauge'
	jq -r --arg s "$stanza" '
		.[] | select(.name == $s) | .backup // [] | group_by(.type)[] | max_by(.timestamp.stop)
		| "pgbackrest_backup_last_completed_timestamp_seconds{stanza=\"\($s)\",type=\"\(.type)\"} \(.timestamp.stop)"
	' <<<"$info"

	echo '# HELP pgbackrest_stanza_status_code pgBackRest stanza status code (0 = ok).'
	echo '# TYPE pgbackrest_stanza_status_code gauge'
	jq -r --arg s "$stanza" '.[] | select(.name == $s)
		| "pgbackrest_stanza_status_code{stanza=\"\($s)\"} \(.status.code)"' <<<"$info"

	# Newest archived WAL segment in the repository (mtime).
	newest=$(find "$repo/archive/$stanza" -type f -name '0*' -printf '%T@\n' 2>/dev/null | sort -n | tail -1)
	echo '# HELP pgbackrest_archive_last_wal_timestamp_seconds Modification time of the newest archived WAL file.'
	echo '# TYPE pgbackrest_archive_last_wal_timestamp_seconds gauge'
	echo "pgbackrest_archive_last_wal_timestamp_seconds{stanza=\"$stanza\"} ${newest%.*}"

	if [ "$check" = --check ]; then
		ok=0
		if pgbackrest --stanza="$stanza" --log-level-console=off check >/dev/null 2>&1; then
			ok=1
		fi

		echo '# HELP pgbackrest_archive_check_ok 1 when the last `pgbackrest check` succeeded.'
		echo '# TYPE pgbackrest_archive_check_ok gauge'
		echo "pgbackrest_archive_check_ok{stanza=\"$stanza\"} $ok"
	fi
} >"$tmp"

chmod 0644 "$tmp"
mv -f "$tmp" "$out_dir/pgbackrest.prom"
trap - EXIT
