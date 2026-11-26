#!/usr/bin/env python3
"""Machine-readable result of one P14a cell.

usage: summarize-cell.py --load DIR [--server DIR] [--standby DIR] [--pg DIR] [--label L] [--out cell.json]

  --load     capload -out directory (summary.json, manifest-load.json)
  --server   cluster.sh RUNDIR of the active (samples.csv, metrics.log, manifest-server.json)
  --standby  cluster.sh RUNDIR of the standby (samples.csv)
  --pg       pgsample.sh DIR (pg.csv, pgb.csv, pgb-columns.csv)

Every capload phase (enroll|connect, steady, probe, register, policy, hold)
becomes a window: server CPU (mean and best 10 s, in cores) and RSS, the
NodeStore/mapper metric deltas, PostgreSQL commits/s, row writes/s, WAL/s,
backends and lock waits, PgBouncer client/server connections and waits.
Writes cell.json (with both manifests embedded) and prints one markdown
table row per window. Numbers are measured; nothing is extrapolated.
"""
import argparse
import csv
import json
import os
import re
import sys

TICK = 100.0  # USER_HZ


def load_json(path):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return None


def windows(summary):
    pt = summary.get("phase_times_unix", {})
    out = []
    for k, start in sorted(pt.items(), key=lambda kv: kv[1]):
        if k.endswith("_start"):
            name = k[: -len("_start")]
            end = pt.get(name + "_end")
            if end:
                out.append((name, start, end))
    return out


def read_samples(path):
    rows = []
    try:
        with open(path) as f:
            for r in csv.DictReader(f):
                try:
                    rows.append((float(r["t"]), int(r["rss_kb"]), int(r["cpu_ticks"])))
                except (ValueError, KeyError):
                    continue
    except FileNotFoundError:
        pass
    return rows


def server_window(rows, a, b):
    win = [r for r in rows if a <= r[0] <= b]
    if len(win) < 2:
        return None
    cpu = (win[-1][2] - win[0][2]) / TICK
    wall = win[-1][0] - win[0][0]
    best = 0.0
    j = 0
    for i in range(len(win)):
        while j < len(win) and win[j][0] - win[i][0] < 10:
            j += 1
        if j < len(win):
            best = max(best, (win[j][2] - win[i][2]) / TICK / (win[j][0] - win[i][0]))
    if wall and wall < 10:
        best = cpu / wall  # window shorter than 10 s: its mean is its peak
    return {
        "cpu_s": round(cpu, 1),
        "mean_cores": round(cpu / wall, 2) if wall else 0,
        "peak_cores_10s": round(best, 2),
        "peak_rss_mb": round(max(r[1] for r in win) / 1024, 1),
        "end_rss_mb": round(win[-1][1] / 1024, 1),
    }


METRIC_RE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)")


def read_metrics(path):
    """metrics.log: '# T <unix>' then exposition lines. Label sets are summed."""
    snaps = []
    cur = None
    try:
        with open(path) as f:
            for line in f:
                if line.startswith("# T "):
                    cur = (float(line.split()[2]), {})
                    snaps.append(cur)
                    continue
                m = METRIC_RE.match(line)
                if cur is None or not m:
                    continue
                name, labels, val = m.group(1), m.group(2) or "", m.group(3)
                if "_bucket" in name:
                    continue
                try:
                    v = float(val)
                except ValueError:
                    continue
                cur[1][name] = cur[1].get(name, 0.0) + v
    except FileNotFoundError:
        pass
    return snaps


def nearest(snaps, t, before):
    cands = [s for s in snaps if (s[0] <= t if before else s[0] >= t)]
    if not cands:
        return None
    return max(cands, key=lambda s: s[0]) if before else min(cands, key=lambda s: s[0])


HISTS = [
    "headscale_nodestore_snapshot_build_duration_seconds",
    "headscale_nodestore_peers_calculation_duration_seconds",
    "headscale_nodestore_batch_duration_seconds",
]
COUNTERS = ["headscale_mapresponse_generated_total", "headscale_nodestore_snapshot_builds_total"]


def metrics_window(snaps, a, b):
    s0, s1 = nearest(snaps, a, True), nearest(snaps, b, False) or nearest(snaps, b, True)
    if not s0 or not s1 or s1[0] <= s0[0]:
        return None
    out = {"from_unix": round(s0[0], 1), "to_unix": round(s1[0], 1)}
    for h in HISTS:
        c = s1[1].get(h + "_count", 0) - s0[1].get(h + "_count", 0)
        s = s1[1].get(h + "_sum", 0) - s0[1].get(h + "_sum", 0)
        short = h.replace("headscale_nodestore_", "").replace("_duration_seconds", "")
        out[short + "_count"] = int(c)
        out[short + "_mean_ms"] = round(s / c * 1000, 2) if c else None
    for name in COUNTERS:
        out[name.replace("headscale_", "") + "_delta"] = int(s1[1].get(name, 0) - s0[1].get(name, 0))
    out["heap_inuse_mb_end"] = round(s1[1].get("go_memstats_heap_inuse_bytes", 0) / 2**20, 1)
    out["goroutines_end"] = int(s1[1].get("go_goroutines", 0))
    return out


def read_pg(path):
    rows = []
    cols = None
    try:
        with open(path) as f:
            for r in csv.reader(f):
                if cols is None:
                    cols = r
                    continue
                if len(r) != len(cols):
                    continue
                try:
                    rows.append({k: float(v) for k, v in zip(cols, r)})
                except ValueError:
                    continue
    except FileNotFoundError:
        pass
    return rows


def pg_window(rows, a, b):
    win = [r for r in rows if a <= r["t"] <= b]
    if len(win) < 2:
        return None
    d = lambda k: win[-1][k] - win[0][k]  # noqa: E731
    dt = win[-1]["t"] - win[0]["t"]
    return {
        "commits_per_s": round(d("xact_commit") / dt, 1),
        "rollbacks_per_s": round(d("xact_rollback") / dt, 2),
        "row_writes_per_s": round((d("tup_inserted") + d("tup_updated") + d("tup_deleted")) / dt, 1),
        "rows_read_per_s": round((d("tup_returned") + d("tup_fetched")) / dt, 1),
        "wal_kb_per_s": round(d("wal_bytes") / dt / 1024, 1),
        "backends_max": int(max(r["numbackends"] for r in win)),
        "active_max": int(max(r["active"] for r in win)),
        "idle_in_tx_max": int(max(r["idle_in_tx"] for r in win)),
        "lock_wait_max": int(max(r["lock_wait"] for r in win)),
    }


def read_pgb(dirpath):
    cols = {}
    try:
        with open(os.path.join(dirpath, "pgb-columns.csv")) as f:
            for r in csv.reader(f):
                if len(r) > 2:
                    cols[r[0]] = [c.strip() for c in r[2:]]
    except FileNotFoundError:
        return []
    rows = []
    try:
        with open(os.path.join(dirpath, "pgb.csv")) as f:
            for r in csv.reader(f):
                kind = r[0]
                if kind not in cols or len(r) - 2 != len(cols[kind]):
                    continue
                d = dict(zip(cols[kind], r[2:]))
                if d.get("database") != "headscale":
                    continue
                d["_kind"], d["_t"] = kind, float(r[1])
                rows.append(d)
    except FileNotFoundError:
        pass
    return rows


def pgb_window(rows, a, b):
    pools = [r for r in rows if r["_kind"] == "pools" and a <= r["_t"] <= b]
    stats = [r for r in rows if r["_kind"] == "stats" and a <= r["_t"] <= b]
    if not pools:
        return None
    mx = lambda k: max(int(float(r.get(k, 0) or 0)) for r in pools)  # noqa: E731
    out = {
        "cl_active_max": mx("cl_active"),
        "cl_waiting_max": mx("cl_waiting"),
        "sv_active_max": mx("sv_active"),
        "sv_idle_max": mx("sv_idle"),
        "maxwait_s_max": mx("maxwait"),
    }
    if len(stats) >= 2 and "total_xact_count" in stats[0]:
        dt = stats[-1]["_t"] - stats[0]["_t"]
        out["xact_per_s"] = round((float(stats[-1]["total_xact_count"]) - float(stats[0]["total_xact_count"])) / dt, 1)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--load", required=True)
    ap.add_argument("--server")
    ap.add_argument("--standby")
    ap.add_argument("--pg")
    ap.add_argument("--label", default="")
    ap.add_argument("--out")
    a = ap.parse_args()

    summary = load_json(os.path.join(a.load, "summary.json"))
    if summary is None:
        sys.exit("no summary.json in " + a.load)

    samples = read_samples(os.path.join(a.server, "samples.csv")) if a.server else []
    standby = read_samples(os.path.join(a.standby, "samples.csv")) if a.standby else []
    snaps = read_metrics(os.path.join(a.server, "metrics.log")) if a.server else []
    pg = read_pg(os.path.join(a.pg, "pg.csv")) if a.pg else []
    pgb = read_pgb(a.pg) if a.pg else []

    cell = {
        "label": a.label,
        "load": {k: summary.get(k) for k in ("observers", "phases", "steady", "churn", "probes", "policy",
                                             "isolation", "tailnets", "hold_s", "hold_frames", "hold_bytes")},
        "windows": [],
        "manifest_load": load_json(os.path.join(a.load, "manifest-load.json")),
        "manifest_server": load_json(os.path.join(a.server, "manifest-server.json")) if a.server else None,
    }

    print("| window | s | server cores mean/peak10s | RSS peak MB | PG commit/s | PG row writes/s | PG backends max | PgB cl_waiting max | PgB maxwait s | snapshot builds (mean ms) |")
    print("|---|---:|---|---:|---:|---:|---:|---:|---:|---|")
    for name, s, e in windows(summary):
        w = {"name": name, "start_unix": s, "end_unix": e, "seconds": round(e - s, 1),
             "server": server_window(samples, s, e), "standby": server_window(standby, s, e),
             "metrics": metrics_window(snaps, s, e), "pg": pg_window(pg, s, e), "pgbouncer": pgb_window(pgb, s, e)}
        cell["windows"].append(w)
        sv, pw, bw, mw = w["server"] or {}, w["pg"] or {}, w["pgbouncer"] or {}, w["metrics"] or {}
        print(f"| {name} | {w['seconds']} | {sv.get('mean_cores', '–')}/{sv.get('peak_cores_10s', '–')} | "
              f"{sv.get('peak_rss_mb', '–')} | {pw.get('commits_per_s', '–')} | {pw.get('row_writes_per_s', '–')} | "
              f"{pw.get('backends_max', '–')} | {bw.get('cl_waiting_max', '–')} | {bw.get('maxwait_s_max', '–')} | "
              f"{mw.get('snapshot_build_count', '–')} ({mw.get('snapshot_build_mean_ms', '–')}) |")

    out = a.out or os.path.join(a.load, "cell.json")
    with open(out, "w") as f:
        json.dump(cell, f, indent=2)
    iso = summary.get("isolation", {})
    print(f"\nisolation: violations={iso.get('violations')} unknown={iso.get('unknown_peers')} "
          f"frames_decoded={iso.get('frames_decoded')}; cell.json: {out}")


if __name__ == "__main__":
    main()
