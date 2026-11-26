#!/usr/bin/env python3
"""Verdict of one P14f F5 step (tools/capload/ramp.sh): PASS / FAIL /
ISOLATION, the measurements, and the blocker of a failing step.

    ramp-verdict.py CELLDIR --shape S --n N --inc 1 --register 500 --alive 1

Reads CELLDIR/{server-samples.csv,server-vm.txt,metrics-final.txt,
dmesg-oom.txt,step-start.txt,server-setup.txt,load*/summary.json,
load*/capload.log,load*/load-samples.csv,load*/nproc.txt}; writes
CELLDIR/verdict.json and prints one line.
"""

import argparse
import csv
import glob
import json
import os
import re
from datetime import datetime

CLK_TCK = 100  # Linux USER_HZ on the lab VMs
RSS_LIMIT = 0.90  # peak RSS must stay below 90 % of the VM's RAM
BUSY = 0.90  # a CPU at >= 90 % busy over the enroll window is saturated


def read(path, default=""):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return default


def rows(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return list(csv.DictReader(f))
    except OSError:
        return []


def fnum(x, default=0.0):
    try:
        return float(x)
    except (TypeError, ValueError):
        return default


def window_cpu(samples, t0, t1, tkey, ckey, scale):
    """Average cores used between t0 and t1 from cumulative tick samples."""
    pts = [(fnum(r[tkey]), fnum(r[ckey])) for r in samples if r.get(ckey) not in (None, "")]
    pts = [p for p in pts if t0 <= p[0] <= t1]
    if len(pts) < 2 or pts[-1][0] <= pts[0][0]:
        return None
    return (pts[-1][1] - pts[0][1]) / scale / (pts[-1][0] - pts[0][0])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cell")
    ap.add_argument("--shape", required=True)
    ap.add_argument("--n", type=int, required=True)
    ap.add_argument("--inc", default="1")
    ap.add_argument("--register", type=int, default=500)
    ap.add_argument("--alive", default="1")
    a = ap.parse_args()
    c = a.cell

    vm = read(os.path.join(c, "server-vm.txt")).split()
    mem_kb = fnum(vm[0]) if vm else 0
    cores = int(fnum(vm[1])) if len(vm) > 1 else 0

    srv = rows(os.path.join(c, "server-samples.csv"))
    rss_peak_kb = max((fnum(r.get("rss_kb")) for r in srv), default=0)

    loads = sorted(glob.glob(os.path.join(c, "load*")))
    enroll = {"nodes": 0, "ok": 0, "failed": 0, "wall_s": 0.0, "bytes": 0, "frames": 0, "errors": {}}
    reg = {"nodes": 0, "ok": 0, "failed": 0, "wall_s": 0.0, "bytes": 0, "errors": {}}
    fm_p99 = []
    t_enroll = [None, None]
    isolation = 0
    hold_bytes = 0
    load_hosts = []
    exit_codes = []
    for ld in loads:
        log = read(os.path.join(ld, "capload.log"))
        m = re.findall(r"^exit=(\d+)", log, re.M)
        exit_codes.append(int(m[-1]) if m else -1)
        try:
            s = json.load(open(os.path.join(ld, "summary.json"), encoding="utf-8"))
        except (OSError, ValueError):
            s = {}
        isolation += int(s.get("isolation", {}).get("violations", 0) or 0)
        hold_bytes += int(s.get("hold_bytes", 0) or 0)
        pt = s.get("phase_times_unix", {})
        if pt.get("enroll_start"):
            t_enroll[0] = min(filter(None, [t_enroll[0], pt["enroll_start"]]))
            t_enroll[1] = max(filter(None, [t_enroll[1], pt.get("enroll_end")]))
        for ph in s.get("phases", []):
            d = enroll if ph.get("name") in ("enroll", "connect") else reg if ph.get("name") == "register" else None
            if d is None:
                continue
            d["nodes"] += ph.get("nodes", 0)
            d["ok"] += ph.get("ok", 0)
            d["failed"] += ph.get("failed", 0)
            d["wall_s"] = max(d["wall_s"], ph.get("wall_s", 0.0))
            d["bytes"] += ph.get("bytes_all_clients", 0)
            for k, v in (ph.get("errors") or {}).items():
                d["errors"][k] = d["errors"].get(k, 0) + v
            if d is enroll:
                d["frames"] += ph.get("frames_all_clients", 0)
                if ph.get("first_map", {}).get("p99_ms") is not None:
                    fm_p99.append(ph["first_map"]["p99_ms"])
        # Load host saturation over the enroll window.
        ls = rows(os.path.join(ld, "load-samples.csv"))
        ncpu = int(fnum(read(os.path.join(ld, "nproc.txt")).strip() or 0)) or 1
        host = {"host": os.path.basename(ld), "cpus": ncpu}
        if ls and t_enroll[0]:
            pts = [r for r in ls if t_enroll[0] <= fnum(r["t"]) <= (t_enroll[1] or 1e18)]
            if len(pts) >= 2:
                busy = fnum(pts[-1]["host_cpu_busy"]) - fnum(pts[0]["host_cpu_busy"])
                tot = fnum(pts[-1]["host_cpu_total"]) - fnum(pts[0]["host_cpu_total"])
                host["cpu_busy_frac"] = round(busy / tot, 3) if tot else None
            host["capload_rss_peak_mb"] = round(max(fnum(r.get("cap_rss_kb")) for r in ls) / 1024, 1)
            host["tcp_estab_peak"] = int(max(fnum(r.get("tcp_estab")) for r in ls))
        load_hosts.append(host)

    t0 = t_enroll[0] or 0
    t1 = t_enroll[1] or 1e18
    srv_cores = window_cpu(srv, t0, t1, "t", "cpu_ticks", CLK_TCK)
    cpu_total_s = (fnum(srv[-1]["cpu_ticks"]) - fnum(srv[0]["cpu_ticks"])) / CLK_TCK if len(srv) >= 2 else None

    div = 0.0
    for line in read(os.path.join(c, "metrics-final.txt")).splitlines():
        if line.startswith("headscale_nodestore_netmap_divergence_total"):
            div += fnum(line.rsplit(" ", 1)[-1])

    start = fnum(read(os.path.join(c, "step-start.txt")).strip())
    oom = []
    for line in read(os.path.join(c, "dmesg-oom.txt")).splitlines():
        try:
            ts = datetime.fromisoformat(line.split()[0].replace(",", ".")).timestamp()
        except (ValueError, IndexError):
            continue
        if ts >= start:
            oom.append(line[:160])
    errs = read(os.path.join(c, "server-errors.txt"))
    m = re.search(r"server_logs=(\d+)", errs)
    restarts = max(int(m.group(1)) - 2, 0) if m else 0  # start + one planned restart

    rss_frac = rss_peak_kb / mem_kb if mem_kb else None
    checks = {
        "enroll_all_connected": enroll["nodes"] == a.n and enroll["ok"] == a.n and enroll["failed"] == 0,
        "register_all_ok": reg["nodes"] == a.register and reg["ok"] == a.register and reg["failed"] == 0,
        "rss_below_90pct": rss_frac is not None and rss_frac < RSS_LIMIT,
        "server_alive_no_restart_no_oom": a.alive == "1" and restarts == 0 and not oom,
        "divergence_zero": div == 0,
        "no_isolation_violation": isolation == 0 and 3 not in exit_codes,
    }
    if not checks["no_isolation_violation"]:
        verdict = "ISOLATION"
    elif all(checks.values()):
        verdict = "PASS"
    else:
        verdict = "FAIL"

    blocker = None
    load_sat = [h for h in load_hosts if (h.get("cpu_busy_frac") or 0) >= BUSY]
    err_text = " ".join(list(enroll["errors"]) + list(reg["errors"])).lower()
    if verdict == "FAIL":
        if oom or a.alive != "1" or (rss_frac or 0) >= RSS_LIMIT:
            blocker = "server RAM"
        elif not checks["divergence_zero"]:
            blocker = "netmap divergence (correctness)"
        elif load_sat or re.search(r"too many open files|cannot assign requested address|address already in use", err_text):
            blocker = "load generator (CPU, ports or fds): limit is a lower bound"
        elif srv_cores is not None and cores and srv_cores >= BUSY * cores:
            blocker = "server CPU"
        else:
            blocker = "timeouts/latency (no resource saturated in the samples)"

    out = {
        "shape": a.shape, "n": a.n, "incremental": a.inc, "verdict": verdict, "blocker": blocker, "checks": checks,
        "server": {
            "vm_mem_gib": round(mem_kb / 2**20, 1), "vm_cpus": cores,
            "rss_peak_gib": round(rss_peak_kb / 2**20, 2), "rss_peak_frac": round(rss_frac, 3) if rss_frac else None,
            "cpu_cores_avg_enroll": round(srv_cores, 2) if srv_cores is not None else None,
            "cpu_seconds_total": round(cpu_total_s, 1) if cpu_total_s is not None else None,
            "alive_at_end": a.alive == "1", "restarts": restarts, "oom": oom,
            "netmap_divergence_total": div,
        },
        "enroll": {**enroll, "first_map_p99_ms_max": max(fm_p99) if fm_p99 else None},
        "register": reg,
        "bytes_sent_total": enroll["bytes"] + reg["bytes"] + hold_bytes,
        "isolation_violations": isolation,
        "load_hosts": load_hosts, "capload_exit": exit_codes,
    }
    with open(os.path.join(c, "verdict.json"), "w", encoding="utf-8") as f:
        json.dump(out, f, indent=1)
    print(
        f"VERDICT {verdict} shape={a.shape} n={a.n} inc={a.inc} enroll={enroll['ok']}/{a.n} wall={enroll['wall_s']:.0f}s "
        f"register={reg['ok']}/{a.register} rss={out['server']['rss_peak_gib']}GiB/{out['server']['vm_mem_gib']}GiB "
        f"cpu={out['server']['cpu_cores_avg_enroll']}c div={div:g} bytes={out['bytes_sent_total']} blocker={blocker}"
    )


if __name__ == "__main__":
    main()
