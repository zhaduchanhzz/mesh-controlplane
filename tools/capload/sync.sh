#!/usr/bin/env bash
# Ship the current branch to both lab hosts as a git bundle (never a copy of
# a working tree) and fast-forward their clones. Builds capload on the load
# host; headscale binaries are built by the caller (see README.md).
set -euo pipefail
key=${SSH_KEY:-$HOME/.mesh-lab/mesh-lab}
server=${SERVER_HOST:-mesh@192.168.1.242}
load=${LOAD_HOST:-mesh@192.168.1.240}
branch=$(git rev-parse --abbrev-ref HEAD)
tmp=$(mktemp -d)
git bundle create "$tmp/p00c.bundle" master "$branch" >/dev/null 2>&1
for h in "$server:p00c/src" "$load:src/p00c/src"; do
	host=${h%%:*} path=${h#*:}
	scp -q -i "$key" "$tmp/p00c.bundle" "$host:/tmp/p00c.bundle"
	ssh -i "$key" "$host" "cd ~/$path && git fetch -q /tmp/p00c.bundle $branch:refs/remotes/b/$branch master:refs/remotes/b/master && git checkout -q -B $branch b/$branch && git log --oneline -1"
done
ssh -i "$key" "$load" 'cd ~/src/p00c/src && PATH=/usr/local/go/bin:$PATH go build -o ~/src/p00c/bin/capload ./tools/capload'
rm -rf "$tmp"
