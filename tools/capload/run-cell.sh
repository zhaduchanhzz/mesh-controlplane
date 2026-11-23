#!/usr/bin/env bash
# Driver for one cell of the P00c server-only capacity harness: one fresh
# server on SERVER_HOST, one capload run on LOAD_HOST. Runs on an operator
# machine that can ssh to both. Keys go host to host through a pipe and are
# deleted on both hosts afterwards; nothing secret is written locally.
#
#   run-cell.sh LABEL NODES POLICY
#
# Results: SERVER_HOST:~/p00c/results/LABEL-NODES-POLICY/ (samples.csv,
# metrics.txt, setup.txt, server.log) and LOAD_HOST:~/src/p00c/results/
# LABEL-NODES-POLICY/ (summary.json, nodes.csv, capload.log).
set -euo pipefail

label=$1 nodes=$2 policy=$3
key=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
server=${SERVER_HOST:-mesh@192.168.1.242}
load=${LOAD_HOST:-mesh@192.168.1.240}
addr=${SERVER_ADDR:-192.168.1.242:8080}
hold=${HOLD:-60s}
deadline=${DEADLINE:-40m}
cell=$label-$nodes-$policy
sdir="\$HOME/p00c/results/$cell"
ldir="\$HOME/src/p00c/results/$cell"
users=$(((nodes + 9) / 10))

s() { ssh -i "$key" -o BatchMode=yes "$server" "$@"; }
l() { ssh -i "$key" -o BatchMode=yes "$load" "$@"; }

s "bash ~/p00c/src/tools/capload/server.sh start $sdir $policy $addr"
s "bash ~/p00c/src/tools/capload/server.sh setup $sdir $users"
l "mkdir -p $ldir && chmod 700 $ldir && umask 077 && cat > $ldir/keys.txt" < <(s "cat $sdir/keys.txt")
s "rm -f $sdir/keys.txt"

set +e
l "ulimit -n 200000; cd $ldir && ~/src/p00c/bin/capload -server http://$addr -keys keys.txt \
	-nodes $nodes -hold $hold -deadline $deadline -out . >capload.log 2>&1; echo exit=\$? >>capload.log; rm -f keys.txt"
rc=$?
set -e

s "bash ~/p00c/src/tools/capload/server.sh stop $sdir"
s "python3 ~/p00c/src/tools/capload/summarize.py $sdir/samples.csv" | tee /dev/stderr >/dev/null || true
l "tail -n 60 $ldir/capload.log"
exit $rc
