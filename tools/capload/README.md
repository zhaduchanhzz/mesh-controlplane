# Server-only capacity harness (P00c; cluster mode P14a.0)

Measures one Headscale process alone: the server runs on one host, the
load (real `controlclient.Direct` clients, one streaming map session per
node) comes from `capload` on another host. Unlike `TestScale`
(`hscontrol/servertest`), server RSS/CPU are not mixed with client cost.

| File | Runs on | Role |
|---|---|---|
| `main.go` … (`capload`) | load host | enrolls or reconnects N nodes over Noise (pre-auth keys per tailnet from `-plan`, P00c `-keys`, or pre-registered `-node-keys`); per node time-to-register and time-to-first-map; then the steady/probe/register/policy/hold phases below; `summary.json`, `nodes.csv`, `frames.csv`, `probes.csv`, `manifest-load.json` |
| `../capseed` | server host | `-mode serve` (P00c): seeded SQLite tailnet; `-mode cluster` (P14a): T tailnets × M devices with users, pre-auth keys (tagged keys for `tag_heavy`) and policy on a running cluster, `plan.tsv` + `tailnets.json`; `-mode policy-flip`: add/remove a probe grant in one tailnet's policy |
| `server.sh` | server host | P00c: `headscale serve` + SQLite (`seed`, `start`, `setup`, `stop`); `cluster-<sub>` runs `cluster.sh <sub>` |
| `cluster.sh` | server host(s) | `headscale cluster serve` on PostgreSQL via PgBouncer: `config`, `migrate` (P13d: once on a fresh database; serve, register and capseed never migrate), `register`, `start active|standby` (the first active creates a missing Noise key with `--init-noise-key`, P13e) (waits for `/ready` 200/503), `restart`, `stop`, `manifest`; 1 s RSS/CPU sampler, 10 s loopback `/metrics` scrape |
| `pgstack.sh` | one host | throwaway PostgreSQL 16 + PgBouncer 1.24.1 (transaction pool, as `infra/lab/db`) with `pg_stat_statements`, for dev/smoke runs |
| `pgsample.sh` | DB host | 1 s `pg_stat_database`/`pg_stat_activity` (`pg.csv`) and PgBouncer `SHOW POOLS/STATS` (`pgb.csv`); at stop, top statements and table sizes |
| `summarize-cell.py` | anywhere | joins one cell (capload + server + standby + PG samples) into `cell.json` per phase window, with both manifests; prints a markdown table |
| `smoke-local.sh` | mc-ci | the whole cluster path on one host at tiny scale (not a benchmark) |
| `summarize.py`, `run-cell.sh`, `run-seeded.sh`, `sync.sh` | | P00c, unchanged |

## Cluster mode (P14a)

Policy shapes (`evidence/P14-benchmark-plan.md` §3): `allow_all`,
`isolated`, `personal_devices`, `tag_heavy` (100 role tags + 20 env tags,
70 % of nodes tagged through tagged pre-auth keys, 300 role grants, 50
groups of 2 users → 5 role tags each) and `mix` (per tailnet 70/20/10
personal/tag_heavy/allow_all, by seed). Ten devices per user.

Phases of one `capload` run, each stamped `PHASE <name>_start|_end <unix>`:

1. `enroll` (from `-plan`: registration over Noise with the tailnet's
   key, so `TailnetForAuthKey`, the device quota and the enroll gate are
   on the path) or `connect` (`-node-keys`: re-login with the stored node
   key, as tailscaled after a restart);
2. `steady` (`-steady`): endpoint churn, `-churn-pct-per-min` of the
   nodes per minute, as non-streaming "lite" MapRequests;
3. `probe` (`-probes`): alternating endpoint and Hostinfo (service list —
   the part of Hostinfo peers receive) changes of a node that some
   observer holds as peer; change → frame latency at every observer
   holding it (`probes.csv`, p50/p95/p99, last observer);
4. `register` (`-register`): new devices from the plan's register lines;
5. `policy` (`-policy-cmd`, e.g. `capseed -mode policy-flip` over ssh):
   change → first frame with a packet filter at every observer of that
   tailnet; observers of other tailnets must get no filter frame
   (`other_tailnet_filter_frames`). The time includes the CLI path and
   the cluster's 2 s reconcile poll, as for an operator edit;
6. `hold`.

`-observe K` streams (default 200; round-robin over tailnets, consecutive
devices within each, so a user's devices are observed together) decode
every frame: timestamps (`frames.csv`), peer sets, and an **isolation
check** — every decoded peer key must belong to the observer's tailnet
(capload generates every node key before the run). A violation makes
capload exit 3: stop the benchmark, BLOCK MERGE (AGENTS rule 13). Lower
`-observe` for large `allow_all` cells: each observer decodes full maps.

The cluster listener answers only its `server_url` hostname (P08), and the
lab has no DNS for it: `-resolve host=ip` maps it inside capload (an
in-process DNS answer through Go's resolver); the shared load host's
`/etc/hosts` is not touched.

A running cluster loads users only when it loads a tailnet; after
`capseed -mode cluster` restart the active (`cluster.sh restart`, standby
stopped meanwhile) before enrolling, or start from a database snapshot.

Quota: a tenant on a `shared` cluster is capped at 5000 devices
(`placement.DefaultSharedTenantLimit`); one-tailnet cells above that use a
`dedicated` cluster (`CLASS=dedicated`, `MAX_DEVICES`). `allow_all` above
~1000 devices needs `MAX_PEER_ENTRIES=0`.

```sh
# dev/smoke on mc-ci (tiny, everything local, removed afterwards)
go build -tags benchpprof -o ~/p14a-smoke/bin/headscale ./cmd/headscale
go build -o ~/p14a-smoke/bin/capload ./tools/capload
go build -o ~/p14a-smoke/bin/capseed ./tools/capseed
COMMIT=$(git rev-parse HEAD) bash tools/capload/smoke-local.sh ~/p14a-smoke/bin ~/p14a-smoke/out
```

Profiles: a `-tags benchpprof` build serves `/debug/pprof/` on the
metrics listener to loopback peers only (`ssh server curl
127.0.0.1:9090/debug/pprof/profile?seconds=30`); mutex/block profiling via
`HEADSCALE_BENCH_MUTEX_FRACTION` / `HEADSCALE_BENCH_BLOCK_RATE`. Never a
release build.

## Notes

- Clients other than observers do not decode maps (frames are counted and
  discarded), except the first map of every `-decode-every`th client,
  which is also isolation-checked. The load generator does not model
  tailscaled's own CPU.
- No retries: a node that has no first map within `-timeout` (120 s) is a
  failure; `over_10s` counts nodes slower than TestScale's 10 s limit.
- The server listens on the lab LAN only; metrics (and pprof) stay on
  127.0.0.1.
- Pre-auth keys (`plan.tsv`, `keys.txt`) and node keys (`nodes.keys`) are
  short-lived lab test credentials in mode-0600 files; they are piped
  between hosts and deleted after the run, never printed or committed.
  The PostgreSQL password is only a `file:` reference in the config.

## P14f F5: no-ceiling ramp (`ramp.sh`)

`ramp.sh limit SHAPE…` finds the largest N one cluster process serves for a
policy shape. It runs steps of `STEP` (5000) from `START` until a step
fails, then bisects down to `RES` (1000). `ramp.sh step SHAPE N` runs one
step, and `ramp.sh matrix` runs the small/medium points for the cost model.

- **Settings:** the header of `ramp.sh` lists the pass criteria, the
  blocker classes and the environment variables.
- **Results:** each step writes
  `$OUT/<shape>/inc<0|1>/<N>/verdict.json` (`ramp-verdict.py`).
- **Load:** can be split over several hosts with
  `LOAD_HOSTS="host:weight …"`.
- **Database:** each step gets a fresh `capf5_*` database on mc-db.
- **Shapes:** `allow_all`, `isolated`, `personal_devices`,
  `shared_service`, `tag_heavy`.

Evidence: `evidence/P14f-f5-prep.md`.
