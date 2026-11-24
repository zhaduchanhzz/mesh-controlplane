#!/usr/bin/env python3
"""Point a copied tailscaled state file at another control server.

Used by the two-cluster E2E (F07): a node registered with cluster 1 keeps its
machine and node keys but talks to cluster 2, as a client with an old
bootstrap answer or a hand-edited ControlURL would. Runs on mc-ci only, on a
mode-600 copy that is deleted right after; never prints keys.

Usage: retarget-state.py STATE_FILE OLD_URL NEW_URL  -> prints the count of
ControlURL values changed (0 = nothing to do, exit 1).
"""
import base64
import json
import sys


def walk(v, old, new):
    n = 0
    if isinstance(v, dict):
        for k, x in v.items():
            if k == "ControlURL" and x == old:
                v[k] = new
                n += 1
            else:
                n += walk(x, old, new)
    elif isinstance(v, list):
        for x in v:
            n += walk(x, old, new)
    return n


def main():
    path, old, new = sys.argv[1:4]
    with open(path) as f:
        state = json.load(f)
    changed = 0
    for key, val in state.items():
        try:
            inner = json.loads(base64.b64decode(val))
        except Exception:
            continue
        n = walk(inner, old, new)
        if n:
            state[key] = base64.b64encode(json.dumps(inner).encode()).decode()
            changed += n
    with open(path, "w") as f:
        json.dump(state, f)
    print(changed)
    return 0 if changed else 1


if __name__ == "__main__":
    sys.exit(main())
