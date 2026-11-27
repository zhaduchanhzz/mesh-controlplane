#!/usr/bin/env python3
"""M06x client-side watcher (P14f F6), run on the observing client (a1).

One ssh session for the whole of t_m06x. It polls the local
`tailscale status --json` every 0.1 s and answers MARK lines from the harness.
Every time below is this client's monotonic clock, so no clock offset between
the harness host and the client enters a measurement, and an ssh stall on the
harness side delays the harness, not the measurement.

    argv: TAILSCALE_BIN SOCKET DEADLINE_SECONDS
    in:   MARK <id> <kind> <host> [prefix]   kind: present|absent|route|noroute
          QUIT
    out:  READY                               after the first good poll
          ACK <id>                            as soon as the MARK is read
          SEEN <id> <ms> <poll_ms>            ms: MARK read -> end of the first
                                              poll showing the state
          MISS <id> <ms>                      not within DEADLINE_SECONDS

The harness sends MARK right after the API call returns; it adds the
MARK->ACK round trip it measures, so the reported time is an upper bound of
"API returned -> visible at this client". Output holds host names and
prefixes only: no keys.
"""
import json
import subprocess
import sys
import threading
import time

TS_BIN, SOCK, DEADLINE = sys.argv[1], sys.argv[2], float(sys.argv[3])
lock = threading.Lock()
pending = {}  # id -> (t_mark, kind, host, prefix)
ready = threading.Event()


def emit(line):
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def holds(d, kind, host, prefix):
    peers = [p for p in (d.get("Peer") or {}).values() if p.get("HostName") == host]
    if kind == "present":
        return bool(peers)
    if kind == "absent":
        return not peers
    has = any(prefix in ((p.get("PrimaryRoutes") or []) + (p.get("AllowedIPs") or [])) for p in peers)
    return has if kind == "route" else not has


def poll():
    while True:
        t0 = time.monotonic()
        try:
            r = subprocess.run([TS_BIN, "--socket=" + SOCK, "status", "--json"],
                               stdin=subprocess.DEVNULL, capture_output=True, timeout=30, check=False)
            d = json.loads(r.stdout)
        except (subprocess.SubprocessError, ValueError, OSError):
            d = None
        t1 = time.monotonic()
        with lock:
            if d is not None and not ready.is_set():
                ready.set()
                emit("READY")
            for i, (tm, kind, host, prefix) in list(pending.items()):
                if d is not None and holds(d, kind, host, prefix):
                    del pending[i]
                    emit("SEEN %s %d %d" % (i, max(0.0, t1 - tm) * 1000, (t1 - t0) * 1000))
                elif t1 - tm > DEADLINE:
                    del pending[i]
                    emit("MISS %s %d" % (i, (t1 - tm) * 1000))
        time.sleep(max(0.0, 0.1 - (time.monotonic() - t0)))


threading.Thread(target=poll, daemon=True).start()
for line in sys.stdin:
    f = line.split()
    if not f:
        continue
    if f[0] == "QUIT":
        break
    if f[0] == "MARK" and len(f) >= 4:
        with lock:  # ACK before any SEEN of this id
            pending[f[1]] = (time.monotonic(), f[2], f[3], f[4] if len(f) > 4 else "")
            emit("ACK " + f[1])
