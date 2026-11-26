#!/usr/bin/env bash
# P13b lab measurement: writes through the two PgBouncers (mc-pg-01/02 :6432
# -> local HAProxy -> Patroni primary) while the primary switches over and
# while one PgBouncer dies. NOT the F05 drill (no cluster processes, no
# clients): it measures the database path the drills stand on.
#
#   bash infra/lab/pg/pgbouncer-failover-test.sh [ARTIFACT_DIR]
#
# Scratch database p13b_rt on the Patroni cluster (owner headscale), dropped
# at the end. Three writers on mc-ci, one INSERT every 200 ms, a new client
# connection per write (PgBouncer reuses its pooled server connections, so
# a server connection left on a demoted primary would show up as a
# read-only error):
#   w247   host=192.168.1.247 port=6432            (PgBouncer of mc-pg-01 only)
#   w248   host=192.168.1.248 port=6432            (PgBouncer of mc-pg-02 only)
#   wmulti host=192.168.1.247,192.168.1.248 port=6432 target_session_attrs=read-write
#          connect_timeout=2 (the DSN of the cluster processes, P13b)
# Phases (events.txt):
#   P1 switchover  `patronictl switchover` leader -> sync standby
#   P2 kill9       SIGKILL PgBouncer on mc-pg-01 (systemd restarts it after 1 s)
#   P3 down        PgBouncer on mc-pg-01 stopped for DOWN_SECONDS (refused)
#   P4 blackhole   nft drops tcp/6432 to mc-pg-01 for DOWN_SECONDS (no RST)
# Per phase and writer: failures, error window (first failure -> next
# success), max gap between acknowledged writes, read-only errors.
# D12 (tickets/P13.md): kill PgBouncer -> wmulti write errors <= 10 s.
# At the end: every acknowledged write is on the primary (no lost commit).
#
# The headscale password goes from %USERPROFILE%\.mesh-lab\mc-pg.env to a
# 0600 file in mc-ci's /dev/shm through stdin; it is never printed.
set -euo pipefail

MESH_LAB_DIR=${MESH_LAB_DIR:-$HOME/.mesh-lab}
SSH=(ssh -i "$MESH_LAB_DIR/mesh-lab" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
PG1=mesh@192.168.1.247
PG2=mesh@192.168.1.248
CI=mesh@192.168.1.240
DOWN_SECONDS=${DOWN_SECONDS:-15}
# PHASES: subset of "P1 P2 P3 P4" (default all), e.g. PHASES=P1 for
# switchovers only.
PHASES=${PHASES:-P1 P2 P3 P4}
SETTLE=${SETTLE:-12}
ART=${1:-$(mktemp -d)}
mkdir -p "$ART"
: >"$ART/events.txt"
run() { "${SSH[@]}" "$@"; }
ms() { date +%s%3N; }
event() { echo "$(ms) $*" | tee -a "$ART/events.txt"; }

leader_name() { run $PG1 "curl -fsS http://127.0.0.1:8008/cluster | jq -r '.members[] | select(.role==\"leader\") | .name'"; }
host_of() { case $1 in mc-pg-01) echo $PG1 ;; mc-pg-02) echo $PG2 ;; *) return 1 ;; esac; }

leader=$(leader_name)
LH=$(host_of "$leader")
cand=$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster | jq -r '.members[] | select(.role==\"sync_standby\") | .name'")
[ -n "$cand" ] || { echo "no sync standby" >&2; exit 1; }
echo "before: leader=$leader sync_standby=$cand"
run $PG1 "sudo mc-patronictl list" | tee "$ART/patroni-before.txt"

run "$LH" "sudo -u postgres psql -X -q -v ON_ERROR_STOP=1 -d postgres" <<'SQL'
DROP DATABASE IF EXISTS p13b_rt WITH (FORCE);
CREATE DATABASE p13b_rt OWNER headscale;
\c p13b_rt
SET ROLE headscale;
CREATE TABLE rt (w text NOT NULL, seq int NOT NULL, server inet NOT NULL,
  ts timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY (w, seq));
SQL

pgpass=$(grep '^MC_PG_HEADSCALE_PASSWORD=' "$MESH_LAB_DIR/mc-pg.env" | cut -d= -f2- | tr -d '\r' |
	run $CI 'umask 077; f=$(mktemp -p /dev/shm p13b-pgpass.XXXXXX); IFS= read -r p; printf "192.168.1.247:6432:*:headscale:%s\n192.168.1.248:6432:*:headscale:%s\n" "$p" "$p" >"$f"; echo "$f"')
nft_cleanup() { run $PG1 "sudo nft -a list chain inet mc_pg input | awk '/p13b-blackhole/{print \$NF}' | xargs -r -n1 sudo nft delete rule inet mc_pg input handle" >/dev/null 2>&1 || true; }
cleanup() {
	nft_cleanup
	l=$(leader_name 2>/dev/null) && h=$(host_of "$l") &&
		run "$h" "sudo -u postgres psql -X -q -d postgres -c 'DROP DATABASE IF EXISTS p13b_rt WITH (FORCE)'" >/dev/null 2>&1 || true
	run $PG1 "sudo systemctl start pgbouncer" >/dev/null 2>&1 || true
	run $CI "rm -f '$pgpass'; pkill -f p13b-writer" >/dev/null 2>&1 || true
}
trap cleanup EXIT

common="dbname=p13b_rt user=headscale passfile=$pgpass"
declare -A DSN=(
	[w247]="host=192.168.1.247 port=6432 connect_timeout=2 $common"
	[w248]="host=192.168.1.248 port=6432 connect_timeout=2 $common"
	[wmulti]="host=192.168.1.247,192.168.1.248 port=6432 target_session_attrs=read-write connect_timeout=2 $common"
)
echo "client DSNs (no password): ${DSN[wmulti]}" | sed "s#passfile=[^ ]*#passfile=<tmpfs 0600>#"

# Writer on mc-ci: "seq ok|fail epoch_ms server|error" until the stop file.
stopf=/dev/shm/p13b-stop.$$
run $CI "rm -f $stopf"
pids=()
for w in w247 w248 wmulti; do
	run $CI "W=$w STOP=$stopf DSN='${DSN[$w]}' bash -c 'exec -a p13b-writer bash -s'" >"$ART/writes-$w.txt" 2>&1 <<'EOF' &
i=0
while [ ! -e "$STOP" ]; do
	i=$((i + 1))
	t=$(date +%s%3N)
	if out=$(psql "$DSN" -X -q -At -v ON_ERROR_STOP=1 -c "INSERT INTO rt(w, seq, server) VALUES ('$W', $i, inet_server_addr()) RETURNING host(server)" 2>&1); then
		echo "$i ok $t $out"
	else
		echo "$i fail $t $(echo "$out" | grep -v '^$' | tail -1 | tr ' ' '_' | cut -c1-140)"
	fi
	sleep 0.2
done
EOF
	pids+=($!)
done

want() { [[ " $PHASES " == *" $1 "* ]]; }
sleep "$SETTLE"
if want P1; then
event "P1 switchover $leader -> $cand"
run $PG1 "sudo mc-patronictl switchover --leader $leader --candidate $cand --force" >"$ART/switchover.txt" 2>&1 || event "switchover command failed"
for _ in $(seq 1 60); do
	cl=$(run $PG1 "curl -fsS http://127.0.0.1:8008/cluster" || true)
	l=$(run $PG1 "jq -r '.members[] | select(.role==\"leader\" and .state==\"running\") | .name'" <<<"$cl" || true)
	s=$(run $PG1 "jq -r '.members[] | select(.role==\"sync_standby\" and .state==\"streaming\") | .name'" <<<"$cl" || true)
	[ "$l" = "$cand" ] && [ "$s" = "$leader" ] && break
	sleep 1
done
event "P1 healthy leader=$l sync_standby=$s"
for h in $PG1 $PG2; do
	run "$h" "echo 'show stat' | sudo socat - UNIX-CONNECT:/run/haproxy/admin.sock | awk -F, '\$1==\"pg_primary\" && \$2 ~ /^mc-pg/ {printf \"%s=%s \", \$2, \$18}'; echo"
done | tee "$ART/haproxy-after-switchover.txt"
sleep "$SETTLE"
fi

if want P2; then
event "P2 kill9 pgbouncer mc-pg-01"
# systemctl kill can exit non-zero ("Failed to send signal SIGKILL to
# auxiliary processes") although the main process was killed (seen
# 2026-11-26); the journal line "code=killed, status=9/KILL" is the proof.
run $PG1 "sudo systemctl kill --kill-whom=main -s SIGKILL pgbouncer" || event "systemctl kill exited non-zero"
sleep "$SETTLE"
# "activating" (restart pending) exits non-zero; record it, do not abort.
st=$(run $PG1 "systemctl is-active pgbouncer" || true)
echo "pgbouncer mc-pg-01 ${SETTLE}s after kill -9: $st" | tee -a "$ART/events.txt"
fi

if want P3; then
event "P3 down pgbouncer mc-pg-01 stopped ${DOWN_SECONDS}s"
run $PG1 "sudo systemctl stop pgbouncer"
sleep "$DOWN_SECONDS"
run $PG1 "sudo systemctl start pgbouncer"
event "P3 up"
sleep "$SETTLE"
fi

if want P4; then
event "P4 blackhole tcp/6432 to mc-pg-01 ${DOWN_SECONDS}s"
run $PG1 'sudo nft insert rule inet mc_pg input tcp dport 6432 ip saddr 192.168.1.240 drop comment \"p13b-blackhole\"'
sleep "$DOWN_SECONDS"
nft_cleanup
event "P4 open"
sleep "$SETTLE"
fi
event "end"

run $CI "touch $stopf"
wait "${pids[@]}" || true
run $CI "rm -f $stopf"
run $PG1 "sudo mc-patronictl list" | tee "$ART/patroni-after.txt"

# --- results -----------------------------------------------------------------------
pstart() { awk -v p="$1" '$2==p{print $1; exit}' "$ART/events.txt"; }
next_start() { # phase -> start of the next phase run, or end
	awk -v p="$1" 'f && ($2 ~ /^P[0-9]$/ && $2!=p || $2=="end"){print $1; exit} $2==p{f=1}' "$ART/events.txt"
}
{
	echo "phase writer: fails, error window ms (first failure -> next ok), max gap between acks ms, read-only errors, error kinds"
	for p in $PHASES; do
		a=$(pstart $p)
		b=$(next_start $p)
		for w in w247 w248 wmulti; do
			awk -v a="$a" -v b="$b" -v p="$p" -v w="$w" '
				$3>=a && $3<b {
					if ($2=="fail") { f++; if (!ff) ff=$3; if ($4 ~ /read.only|read-only|recovery/) ro++; k[$4]++ }
					else { if (ff && !fo) fo=$3; if (pk) { d=$3-pk; if (d>g) g=d }; pk=$3 }
				}
				END {
					win = ff ? (fo ? fo-ff : b-ff) : 0
					kinds=""; for (x in k) kinds = kinds sprintf("%dx%s ", k[x], substr(x,1,70))
					printf "%s %s: fails=%d window=%d max_gap=%d read_only=%d %s\n", p, w, f+0, win, g+0, ro+0, kinds
				}' "$ART/writes-$w.txt"
		done
	done
} | tee "$ART/summary.txt"

newl=$(leader_name)
NLH=$(host_of "$newl")
for w in w247 w248 wmulti; do
	acked=$(awk '$2=="ok"{print $1}' "$ART/writes-$w.txt" | sort -n)
	rows=$(run "$NLH" "sudo -u postgres psql -X -At -d p13b_rt -c \"SELECT seq FROM rt WHERE w='$w' ORDER BY seq\"")
	miss=$(comm -23 <(sort <<<"$acked") <(sort <<<"$rows") | grep -c . || true)
	echo "$w: acknowledged $(grep -c . <<<"$acked"), rows on primary $newl $(grep -c . <<<"$rows"), acknowledged but missing $miss" | tee -a "$ART/summary.txt"
done
by_server=$(cat "$ART"/writes-*.txt | awk '$2=="ok"{c[$4]++} END{for(s in c) printf "%s=%d ", s, c[s]}')
echo "acknowledged writes by server: $by_server" | tee -a "$ART/summary.txt"
run "$NLH" "sudo -u postgres psql -X -q -d postgres -c 'DROP DATABASE p13b_rt WITH (FORCE)'"
echo "artifacts: $ART"
