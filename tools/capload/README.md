# Server-only capacity harness (P00c)

Measures one Headscale process alone: the server runs on one host, the
load (real `controlclient.Direct` clients, one streaming map session per
node) comes from `capload` on another host. Unlike `TestScale`
(`hscontrol/servertest`), server RSS/CPU are not mixed with client cost.

| File | Runs on | Role |
|---|---|---|
| `main.go` (`capload`) | load host | enrolls N nodes, 32 registrations at a time; per node records time-to-register (`TryLogin`) and time-to-first-map (map request to first frame); then holds all streams for `-hold` |
| `server.sh` | server host | `start` (config, policy, headscale + 1 s RSS/CPU sampler), `setup` (users + one reusable key per user via the CLI), `stop` (scrape `headscale_*` metrics from 127.0.0.1, stop) |
| `summarize.py` | server host | peak/last RSS, CPU seconds, mean and peak cores from `samples.csv` |
| `run-cell.sh` | operator | one cell: fresh server, setup, keys piped host to host, capload, stop |
| `sync.sh` | operator | ships the branch as a `git bundle` (never a working-tree copy) |

Policies are TestScale's: `allow_all`, `isolated`, `personal_devices`;
ten nodes per user.

```sh
bash tools/capload/sync.sh
# on the server host: build the binary under test
#   go build -o ~/p00c/bin/headscale ./cmd/headscale
HOLD=60s DEADLINE=20m bash tools/capload/run-cell.sh new 5000 personal_devices
```

Notes:

- Clients do not decode maps (frames are counted and discarded), except
  the first map of every 100th client, decoded to check its peer count.
  The load generator therefore does not model tailscaled's own CPU.
- No retries: a node that has no first map within `-timeout` (120 s) is a
  failure; `over_10s` counts nodes slower than TestScale's 10 s limit.
- The server listens on the lab LAN only; metrics stay on 127.0.0.1.
- Pre-auth keys are short-lived lab test credentials; they are piped
  between hosts and deleted after the run, never printed or committed.
