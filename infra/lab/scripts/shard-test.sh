#!/usr/bin/env bash
# shard-test.sh — run the top-level tests of ONE Go package in N parallel
# shards (P12: the p05cluster servertest suite takes ~42 min serially; the
# other packages already run in parallel under `go test ./...`).
#
# Runs on the CI host (mc-ci) from the repository root:
#
#   infra/lab/scripts/shard-test.sh [-n SHARDS] [-race] [-tags TAGS] [-o OUTDIR] PKG
#
# Shards are balanced by the durations of the previous run
# (OUTDIR/timings.tsv, longest first onto the least loaded shard); tests
# without a timing count as 30 s. Each shard is a separate `go test -json
# -count=1 -run '^(A|B|...)$'`; the build cache is shared, so the package
# is compiled once. Exit 0 only if every shard passed and every listed test
# reported a result. Writes OUTDIR/shard-<i>.json, OUTDIR/summary.txt and
# refreshes OUTDIR/timings.tsv. Prints no environment and no test output
# beyond failures.
set -euo pipefail

shards=6 race="" tags="" out=""

while [ $# -gt 1 ]; do
  case "$1" in
    -n) shards=$2; shift 2 ;;
    -race) race=-race; shift ;;
    -tags) tags=$2; shift 2 ;;
    -o) out=$2; shift 2 ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
done

pkg=${1:?package}
out=${out:-$HOME/shard-$(basename "$pkg")}
mkdir -p "$out"
rm -f "$out"/shard-*.json "$out"/shard-*.run

tagargs=()
[ -n "$tags" ] && tagargs=(-tags "$tags")

start=$(date +%s)

# Top-level tests of the package (Example/Benchmark/Fuzz excluded).
go test "${tagargs[@]}" -list '^Test' "$pkg" | grep -E '^Test' | sort >"$out/tests.txt"
total=$(wc -l <"$out/tests.txt")
[ "$total" -gt 0 ] || { echo "no tests in $pkg" >&2; exit 2; }

# Greedy longest-first assignment by previous timings.
python3 - "$out" "$shards" <<'PY'
import os, sys
out, n = sys.argv[1], int(sys.argv[2])
tests = [l.strip() for l in open(os.path.join(out, "tests.txt")) if l.strip()]
t = {}
p = os.path.join(out, "timings.tsv")
if os.path.exists(p):
    for l in open(p):
        name, _, sec = l.strip().partition("\t")
        try: t[name] = float(sec)
        except ValueError: pass
load = [0.0] * n
groups = [[] for _ in range(n)]
for name in sorted(tests, key=lambda x: -t.get(x, 30.0)):
    i = load.index(min(load))
    groups[i].append(name)
    load[i] += t.get(name, 30.0)
for i, g in enumerate(groups):
    with open(os.path.join(out, f"shard-{i}.run"), "w") as f:
        f.write("^(" + "|".join(g) + ")$" if g else "")
    print(f"shard {i}: {len(g)} tests, ~{load[i]:.0f}s by previous timings")
PY

pids=()
for i in $(seq 0 $((shards - 1))); do
  re=$(cat "$out/shard-$i.run")
  [ -n "$re" ] || continue
  go test $race "${tagargs[@]}" -count=1 -json -timeout 45m -run "$re" "$pkg" >"$out/shard-$i.json" 2>"$out/shard-$i.err" &
  pids+=($!)
done

rc=0
for p in "${pids[@]}"; do wait "$p" || rc=1; done

# Summary and new timings from the JSON events of top-level tests.
python3 - "$out" "$start" <<'PY' || rc=1
import glob, json, os, sys, time
out, start = sys.argv[1], int(sys.argv[2])
res, el = {}, {}
for f in sorted(glob.glob(os.path.join(out, "shard-*.json"))):
    for line in open(f, errors="replace"):
        try: e = json.loads(line)
        except ValueError: continue
        name = e.get("Test", "")
        if not name or "/" in name: continue
        if e.get("Action") in ("pass", "fail", "skip"):
            res[name] = e["Action"]; el[name] = e.get("Elapsed", 0.0)
tests = [l.strip() for l in open(os.path.join(out, "tests.txt")) if l.strip()]
missing = [x for x in tests if x not in res]
fail = sorted(x for x, a in res.items() if a == "fail")
with open(os.path.join(out, "timings.tsv"), "w") as f:
    for x in sorted(el): f.write(f"{x}\t{el[x]:.1f}\n")
wall = int(time.time()) - start
lines = [f"tests={len(tests)} pass={sum(a=='pass' for a in res.values())} fail={len(fail)} "
         f"skip={sum(a=='skip' for a in res.values())} missing={len(missing)} wall={wall}s "
         f"serial_sum={sum(el.values()):.0f}s"]
lines += [f"FAIL {x}" for x in fail] + [f"MISSING {x}" for x in missing]
open(os.path.join(out, "summary.txt"), "w").write("\n".join(lines) + "\n")
print("\n".join(lines))
sys.exit(1 if fail or missing else 0)
PY

exit $rc
