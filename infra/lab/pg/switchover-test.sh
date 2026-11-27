#!/usr/bin/env bash
# One controlled switchover with a test database round-tripping through it
# (P13a verification; NOT the F05 drill, which runs later with the cluster
# processes and real clients).
#
#   bash infra/lab/pg/switchover-test.sh [ARTIFACT_DIR]
#
# - Creates database p13a_rt (owner headscale) on the leader.
# - mc-ci writes one row every 200 ms through the multi-host DSN that clients
#   will use (host=.247,.248, target_session_attrs=read-write,
#   connect_timeout=2), a new connection per write, for WRITE_SECONDS.
# - After 10 s: `patronictl switchover --leader <current> --candidate <sync>`.
# - Checks: new leader = candidate, old leader back as streaming sync standby,
#   timeline +1, every acknowledged write present on the new primary AND on
#   the new replica (no lost commit), write gap measured.
# - Drops p13a_rt and the temporary pgpass at the end.
#
# The headscale password goes from %USERPROFILE%\.mesh-lab\mc-pg.env to a
# 0600 file in mc-ci's /dev/shm through stdin; it is never printed.
set -euo pipefail

MESH_LAB_DIR=${MESH_LAB_DIR:-$HOME/.mesh-lab}
SSH=(ssh -i "$MESH_LAB_DIR/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
PG1=mesh@192.168.1.247
PG2=mesh@192.168.1.248
CI=mesh@192.168.1.240
WRITE_SECONDS=${WRITE_SECONDS:-60}
ART=${1:-$(mktemp -d)}
mkdir -p "$ART"
run() { "${SSH[@]}" "$@"; }
ms() { date +%s%3N; }

cl=$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster")
leader=$(run $PG1 "jq -r '.members[] | select(.role==\"leader\") | .name'" <<<"$cl")
cand=$(run $PG1 "jq -r '.members[] | select(.role==\"sync_standby\") | .name'" <<<"$cl")
tl0=$(run $PG1 "jq -r '.members[] | select(.role==\"leader\") | .timeline'" <<<"$cl")
case $leader in mc-pg-01) LH=$PG1 NH=$PG2 ;; mc-pg-02) LH=$PG2 NH=$PG1 ;; *) echo "no leader" >&2; exit 1 ;; esac
[ -n "$cand" ] || { echo "no sync standby" >&2; exit 1; }
echo "before: leader=$leader sync_standby=$cand timeline=$tl0"
run $PG1 "sudo mc-patronictl list" | tee "$ART/patroni-before.txt"

# Test database (peer auth on the leader).
run "$LH" "sudo -u postgres psql -X -q -v ON_ERROR_STOP=1 -d postgres" <<'SQL'
DROP DATABASE IF EXISTS p13a_rt;
CREATE DATABASE p13a_rt OWNER headscale;
\c p13a_rt
SET ROLE headscale;
CREATE TABLE rt (seq int PRIMARY KEY, server inet NOT NULL, ts timestamptz NOT NULL DEFAULT clock_timestamp());
SQL

# Temporary pgpass on mc-ci (tmpfs, 0600), password via stdin only.
pgpass=$(grep '^MC_PG_HEADSCALE_PASSWORD=' "$MESH_LAB_DIR/mc-pg.env" | cut -d= -f2- | tr -d '\r' |
	run $CI 'umask 077; f=$(mktemp -p /dev/shm p13a-pgpass.XXXXXX); IFS= read -r p; printf "192.168.1.247:5432:*:headscale:%s\n192.168.1.248:5432:*:headscale:%s\n" "$p" "$p" >"$f"; echo "$f"')
cleanup() { run $CI "rm -f '$pgpass'" || true; }
trap cleanup EXIT
DSN="host=192.168.1.247,192.168.1.248 port=5432 dbname=p13a_rt user=headscale target_session_attrs=read-write connect_timeout=2 passfile=$pgpass"
echo "client DSN (no password): $DSN" | sed "s#passfile=.*#passfile=<tmpfs 0600>#"

# Writer on mc-ci: "seq status epoch_ms server_or_error".
run $CI "DSN='$DSN' END=\$((\$(date +%s)+$WRITE_SECONDS)) bash -s" >"$ART/writes.txt" 2>&1 <<'EOF' &
i=0
while [ "$(date +%s)" -lt "$END" ]; do
	i=$((i + 1))
	t=$(date +%s%3N)
	if out=$(psql "$DSN" -X -q -At -v ON_ERROR_STOP=1 -c "INSERT INTO rt(seq, server) VALUES ($i, inet_server_addr()) RETURNING host(server)" 2>&1); then
		echo "$i ok $t $out"
	else
		echo "$i fail $t $(echo "$out" | head -1 | tr ' ' '_' | cut -c1-120)"
	fi
	sleep 0.2
done
EOF
writer=$!

sleep 10
t0=$(ms)
run $PG1 "sudo mc-patronictl switchover --leader $leader --candidate $cand --force" | tee "$ART/switchover.txt"
sw_rc=${PIPESTATUS[0]}
t1=$(ms)
echo "switchover command exit=$sw_rc, took $((t1 - t0)) ms"

# Wait for: new leader = candidate, old leader streaming as sync standby.
healthy=""
for _ in $(seq 1 60); do
	cl=$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster" || true)
	l=$(run $PG1 "jq -r '.members[] | select(.role==\"leader\" and .state==\"running\") | .name'" <<<"$cl" || true)
	s=$(run $PG1 "jq -r '.members[] | select(.role==\"sync_standby\" and .state==\"streaming\") | .name'" <<<"$cl" || true)
	if [ "$l" = "$cand" ] && [ "$s" = "$leader" ]; then healthy=$(ms); break; fi
	sleep 1
done
[ -n "$healthy" ] && echo "cluster healthy again (leader $cand, sync standby $leader) $((healthy - t0)) ms after switchover start" ||
	echo "cluster NOT healthy within 60 s"
wait "$writer" || true
run $PG1 "sudo mc-patronictl list" | tee "$ART/patroni-after.txt"
tl1=$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster | jq -r '.members[] | select(.role==\"leader\") | .timeline'")

# Results.
acked=$(awk '$2=="ok"{print $1}' "$ART/writes.txt" | sort -n)
nack=$(wc -w <<<"$acked")
nfail=$(awk '$2=="fail"' "$ART/writes.txt" | wc -l)
by_server=$(awk '$2=="ok"{c[$4]++} END{for(s in c) printf "%s=%d ", s, c[s]}' "$ART/writes.txt")
gap=$(awk '$2=="ok"{ if (p) { d=$3-p; if (d>m) m=d }; p=$3 } END{print m+0}' "$ART/writes.txt")
fails=$(awk '$2=="fail"{print $4}' "$ART/writes.txt" | sort | uniq -c | tr '\n' ';')
case $cand in mc-pg-01) CH=$PG1 ;; *) CH=$PG2 ;; esac
rows_new=$(run "$CH" "sudo -u postgres psql -X -At -d p13a_rt -c 'SELECT seq FROM rt ORDER BY seq'")
rows_old=$(run "$LH" "sudo -u postgres psql -X -At -d p13a_rt -c 'SELECT seq FROM rt ORDER BY seq'")
missing_new=$(comm -23 <(sort <<<"$acked") <(sort <<<"$rows_new") | wc -l)
missing_old=$(comm -23 <(sort <<<"$acked") <(sort <<<"$rows_old") | wc -l)
extra_new=$(comm -13 <(sort <<<"$acked") <(sort <<<"$rows_new") | wc -l)
ro=$(run "$LH" "sudo -u postgres psql -X -At -d postgres -c 'SELECT pg_is_in_recovery()'")
cat <<EOF | tee "$ART/summary.txt"
leader before/after: $leader -> $cand (old leader in recovery: $ro); timeline $tl0 -> $tl1
switchover: command exit $sw_rc, $((t1 - t0)) ms; healthy (sync standby streaming) after $([ -n "$healthy" ] && echo $((healthy - t0)) || echo never) ms
writes: acknowledged $nack, failed $nfail ($fails); by server: $by_server
max gap between acknowledged writes: $gap ms (writer interval 200 ms + connect)
rows on new primary $cand: $(wc -w <<<"$rows_new"); acknowledged but missing: $missing_new; present but not acknowledged: $extra_new
rows on new replica $leader: $(wc -w <<<"$rows_old"); acknowledged but missing: $missing_old
EOF

run "$CH" "sudo -u postgres psql -X -q -d postgres -c 'DROP DATABASE p13a_rt'"
ok=1
[ "$sw_rc" = 0 ] && [ -n "$healthy" ] && [ "$missing_new" = 0 ] && [ "$missing_old" = 0 ] && [ "$ro" = t ] && [ "$tl1" -gt "$tl0" ] || ok=0
[ "$ok" = 1 ] && echo "SWITCHOVER ROUND-TRIP PASS" || { echo "SWITCHOVER ROUND-TRIP FAIL"; exit 1; }
