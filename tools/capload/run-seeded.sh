#!/usr/bin/env bash
# Tier-2 cell from a seeded snapshot (see README.md): fresh server from a
# copy of SNAPDIR's db, then capload connects the NODES pre-registered nodes
# (steady-state capacity) and registers REGISTER new ones on top
# (registration speed and fan-out under churn).
#
#   run-seeded.sh LABEL NODES POLICY REGISTER
#
# Key fixtures go host to host through a pipe into the cell's directory on
# the load host and are deleted after the run.
set -euo pipefail

label=$1 nodes=$2 policy=$3 register=$4
key=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
server=${SERVER_HOST:-mesh@192.168.1.242}
load=${LOAD_HOST:-mesh@192.168.1.240}
addr=${SERVER_ADDR:-192.168.1.242:8080}
hold=${HOLD:-60s}
deadline=${DEADLINE:-20m}
conc=${CONCURRENCY:-256}
snap="\$HOME/p00c/snap/$nodes"
cell=$label-$nodes-$policy
sdir="\$HOME/p00c/results/$cell"
ldir="\$HOME/src/p00c/results/$cell"

s() { ssh -i "$key" -o BatchMode=yes "$server" "$@"; }
l() { ssh -i "$key" -o BatchMode=yes "$load" "$@"; }

t0=$(date +%s)
s "MAX_PEER_ENTRIES=${MAX_PEER_ENTRIES:-1000000} INCREMENTAL=${INCREMENTAL:-0} RECONCILE_INTERVAL=${RECONCILE_INTERVAL:-1m} FULL_MAP_INTERVAL=${FULL_MAP_INTERVAL:-15m} HS=${HS:-\$HOME/p00c/bin/headscale} bash ~/p00c/src/tools/capload/server.sh start $sdir $policy $addr $snap"
l "rm -rf $ldir && mkdir -p $ldir && chmod 700 $ldir"
l "umask 077 && cat > $ldir/users.keys" < <(s "cat $snap/users.keys")
l "umask 077 && cat > $ldir/nodes.keys" < <(s "cat $snap/nodes.keys")
t1=$(date +%s)

set +e
l "ulimit -n 200000; cd $ldir && ~/src/p00c/bin/capload -server http://$addr -keys users.keys -node-keys nodes.keys \
	-nodes $nodes -register $register -concurrency $conc -hold $hold -deadline $deadline -out . >capload.log 2>&1; \
	echo exit=\$? >>capload.log; rm -f users.keys nodes.keys"
rc=$?
set -e

s "bash ~/p00c/src/tools/capload/server.sh stop $sdir"
t2=$(date +%s)
echo "cell=$cell setup_s=$((t1 - t0)) run_s=$((t2 - t1)) total_s=$((t2 - t0))"
ph=$(l "grep -h PHASE $ldir/capload.log" | awk '{print $3}' | tr '\n' ' ')
# connect window = first two PHASE stamps, register window = next two.
set -- $ph
s "python3 ~/p00c/src/tools/capload/summarize.py $sdir/samples.csv ${1:-} ${2:-}" | sed 's/^/server(all, window=connect): /'
[ -n "${4:-}" ] && s "python3 ~/p00c/src/tools/capload/summarize.py $sdir/samples.csv $3 $4" | sed 's/^/server(window=register): /'
l "grep -E '^(connect|register) wall|exit=' $ldir/capload.log"
exit $rc
