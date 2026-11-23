#!/usr/bin/env python3
"""Summarise server.sh samples.csv: peak/last RSS and CPU use.

usage: summarize.py samples.csv [START_UNIX END_UNIX]

With START/END (capload's PHASE enroll_start/enroll_end lines) it also
reports CPU seconds and mean cores inside that window.
"""
import csv
import sys

TICK = 100.0  # USER_HZ on Linux

rows = []
with open(sys.argv[1]) as f:
    for r in csv.DictReader(f):
        try:
            rows.append((float(r["t"]), int(r["rss_kb"]), int(r["cpu_ticks"])))
        except ValueError:
            continue

if not rows:
    print("no samples")
    sys.exit(0)

peak = max(r[1] for r in rows)
wall = rows[-1][0] - rows[0][0]
cpu = (rows[-1][2] - rows[0][2]) / TICK
best10 = 0.0
for i in range(len(rows)):
    j = i
    while j < len(rows) and rows[j][0] - rows[i][0] < 10:
        j += 1
    if j < len(rows):
        best10 = max(best10, (rows[j][2] - rows[i][2]) / TICK / (rows[j][0] - rows[i][0]))

out = {
    "samples": len(rows),
    "wall_s": round(wall, 1),
    "peak_rss_mb": round(peak / 1024, 1),
    "last_rss_mb": round(rows[-1][1] / 1024, 1),
    "cpu_s": round(cpu, 1),
    "mean_cores": round(cpu / wall, 2) if wall else 0,
    "peak_cores_10s": round(best10, 2),
}

if len(sys.argv) == 4:
    a, b = float(sys.argv[2]), float(sys.argv[3])
    win = [r for r in rows if a <= r[0] <= b]
    if len(win) >= 2:
        c = (win[-1][2] - win[0][2]) / TICK
        out["window_cpu_s"] = round(c, 1)
        out["window_mean_cores"] = round(c / (win[-1][0] - win[0][0]), 2)
        out["window_peak_rss_mb"] = round(max(r[1] for r in win) / 1024, 1)

print(" ".join(f"{k}={v}" for k, v in out.items()))
