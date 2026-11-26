# Rewrite a cluster config for the N1 E2E (runs on the cluster host):
#   python3 cfg-n1.py [--reconcile=DUR] [--fullmap=DUR] SRC DST DERPMAP VERIFY_SECRET_PATH INCREMENTAL(0|1) [DBNAME]
# --reconcile/--fullmap: tuning.incremental_netmap reconcile_interval and
# full_map_interval (default 1h each: out of every case's window).
# Replaces the top-level `derp:` block (own relay region, no public DERP,
# external-DERP verify secret), sets tuning.incremental_netmap.enabled and,
# when DBNAME is given, the postgres database name. Text-based on purpose
# (no PyYAML on the hosts); refuses a config it does not understand.
import re
import sys

opts = {"reconcile": "1h", "fullmap": "1h"}
args = []
for a in sys.argv[1:]:
    m = re.match(r"^--(reconcile|fullmap)=([0-9]+[smh])$", a)
    if m:
        opts[m.group(1)] = m.group(2)
    elif a.startswith("--"):
        sys.exit(f"cfg-n1: bad option {a!r} (want --reconcile=/--fullmap=<n>[smh])")
    else:
        args.append(a)
src, dst, derpmap, secret, inc = args[0:5]
dbname = args[5] if len(args) > 5 else ""
lines = open(src, encoding="utf-8").read().splitlines()


def drop_block(lines, key):
    out, skip = [], False
    for ln in lines:
        if re.match(rf"^{key}:\s*$", ln):
            skip = True
            continue
        if skip and ln and not ln[0].isspace() and not ln.startswith("#"):
            skip = False
        if not skip:
            out.append(ln)
    return out


lines = drop_block(lines, "derp")
lines += [
    "derp:",
    "  server:",
    "    enabled: false",
    "  urls: []",
    f"  paths: [{derpmap}]",
    "  auto_update_enabled: false",
    f"  verify_secret_path: {secret}",
]

inc_block = [
    "  incremental_netmap:",
    f"    enabled: {'true' if inc == '1' else 'false'}",
    f"    reconcile_interval: {opts['reconcile']}",
    f"    full_map_interval: {opts['fullmap']}",
]
if any(re.match(r"^tuning:\s*$", ln) for ln in lines):
    if any("incremental_netmap:" in ln for ln in lines):
        sys.exit("cfg-n1: tuning.incremental_netmap already set in the source config")
    i = next(i for i, ln in enumerate(lines) if re.match(r"^tuning:\s*$", ln))
    lines[i + 1:i + 1] = inc_block
else:
    lines += ["tuning:"] + inc_block

if dbname:
    n = 0
    for i, ln in enumerate(lines):
        if re.match(r"^    name: ", ln):
            lines[i] = f"    name: {dbname}"
            n += 1
    if n != 1:
        sys.exit(f"cfg-n1: expected one database name line, found {n}")

open(dst, "w", encoding="utf-8").write("\n".join(lines) + "\n")
