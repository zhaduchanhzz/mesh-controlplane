# infra/lab/e2e/p13 — P13 drills on the lab (F05, PgBouncer kill)

Harnesses for the drills of `tickets/P13.md` §P13a (F05) and §P13b (kill PgBouncer under load). Thresholds come from D12, approved by the owner on 2026-11-26. Written on 2026-11-26, branch `feat/P13-drill-f05`.

**Status: not run yet.** The lab was busy with the P12d E2E when they were written. Only `bash -n` and shellcheck (`-S warning`, 0 findings) were run.

| File | Purpose |
|---|---|
| `lib.sh` | Shared part: topology, setup/cleanup, client rounds, probes, the checks every case makes, run-wide L01/S01 |
| `jobs.sh` | Probes that run on mc-ci as transient units: pings, `/ready`, a fenced write every 500 ms per cluster (tenant API device rename), and a SQL writer through the client DSN every 200 ms |
| `f05.sh` | F05.1 switchover, F05.2 kill -9 of the postmaster, F05.3 hard stop of the primary VM (Proxmox API), F05.4 nft partition of the primary |
| `pgb-kill.sh` | K1 kill -9 of PgBouncer (held down), K2 clean stop, K3 silent drop of tcp/6432 |

## Run

Run one harness at a time, with the lab free (no N1/P12b/S08d run). Start each run in the foreground of a background task.

```bash
CI_BINDIR=/home/mesh/<dir with a linux headscale built on mc-ci> bash infra/lab/e2e/p13/f05.sh </dev/null
CI_BINDIR=... CASES="f05_1" IMAGES=tailscale/tailscale:v1.104.7 bash infra/lab/e2e/p13/f05.sh </dev/null   # one case, one round
CI_BINDIR=... bash infra/lab/e2e/p13/pgb-kill.sh </dev/null
```

| Variable | Meaning |
|---|---|
| `CASES` | Which cases to run, in that order |
| `IMAGES` | One round per client image. Default: v1.104.7, then v1.84.3. This gives the ticket's "each case × 2" |
| `VM_TARGETS` | Client VMs. Default: a1 10.77.20.11 in tailnet `fa` (c1) and b1 10.77.30.11 in tailnet `fb` (c2). Each gets a second tailscaled from `/opt/tailscale/<version>`. Empty means no VM |
| `RECOVER_MAX_MS` | 45000 (F05: fenced writes of both clusters are back within this time) |
| `GAP_MAX_MS` | 10000 (largest ping gap) |
| `WERR_MAX_MS` | 10000 (PgBouncer kill: write outage, and the SQL writer's largest gap) |
| `MANUAL_MAX` | 0 (manual interventions on clients) |
| `PART_MAX` | 180 (F05.4 dead-man switch, in seconds) |
| `HOLD_S` | 30 (how long the PgBouncer stays down) |
| `PGB_TARGET` | `pg1` or `pg2` |
| `MIGRATE_VIA` | `direct`: the migrate step runs on mc-ci against 5432 of the primary (ADR 0006 §4). `pgbouncer`: it runs on mc-cp through PgBouncer |
| `KEEP=1` | Skip cleanup |

Exit code:
- 0: no case failed.
- 1: at least one case failed.
- 2: setup error, or cleanup could not be proven.

Artifacts go to `artifacts/<commit>/P13/<drill>-<run>/` (gitignored):
- `results.tsv`, `run.log`;
- `timeline.txt`: Patroni state, leases, owner epochs and lease holders before and after each case;
- `timings.txt`;
- `probes/r<N>/probe-*.txt`: the raw probe lines, all on the mc-ci clock;
- `logs/`: process journals, metrics snapshots, chrony offsets, the configs used.

## What the run changes, and how it is undone

- **mc-cp:**
  - stops the P02 POC (`headscale-tailnets`), and any `headscale-cluster` or `mesh-bootstrap-e2e`;
  - runs c1 (`mesh-p13-e2e`) and a transient HAProxy `mesh-c1-ha` on `10.99.77.2:8443` (`/ready` check over mc-cp and mc-c1b);
  - the long-lived `mesh-c2-ha` (8445) is used as it is.
- **mc-c1b:**
  - runs the c1 standby;
  - receives c1's TLS and Noise key through SSH pipes;
  - the Noise key is removed at cleanup, as P11b does.
- **mc-c2 and mc-c2b:** the long-lived c2 (`headscale-cluster` on `meshlab`) is stopped, and c2 runs on the drill database instead.
- **Restart at cleanup:** only the services found active at setup are started again (`WAS_ACTIVE`).
- **PG nodes:**
  - setup creates database `e2e_p13<run>` (owner `headscale`) on the leader, with a table `p13_probe`, and cleanup drops it;
  - every fault has a dead-man timer on the VM (`p13-undo-*`);
  - cleanup deletes `inet p13_drill`, starts `pgbouncer` on both nodes, starts any stopped PG VM (7708/7709) through the Proxmox API, and waits for Patroni to be healthy.
- **mc-ci:**
  - docker clients `p13-r<N>-*` (label `mesh-e2e=p13`);
  - probe units `p13-*`;
  - `/dev/shm/p13`, which holds the API keys, the passfile and the probe outputs, and is removed at cleanup. It needs linger, which setup checks.

## Assumptions to check on the first run

1. **PgBouncer on both PG nodes routes the e2e database.** `pgbouncer.ini` on mc-pg-01 has `* = host=127.0.0.1 port=5000` (read-only check, 2026-11-26), and `userlist.txt`/`pg_hba` accept `headscale` with `MC_PG_HEADSCALE_PASSWORD` from `mc-pg.env`. If the wildcard goes away, add an alias for the run.
2. **The migrate step on mc-ci** (`headscale -c <c1 config, database direct> cluster migrate`) does not trip over files that exist only on cluster hosts: TLS, Noise key, unix socket. If it does, use `MIGRATE_VIA=pgbouncer`.
3. **A device rename** (`POST /api/v2/device/{id}/name`, tenant API key) is a fenced write of the active process (handle with owner/epoch/lease). It is limited by `key_rate` (10/s), not by `config_write_rate`. The probe sends 2/s.
4. **mc-c2b's live config** has the same layout as mc-c2's (`metrics_listen_addr 127.0.0.1:9095`, shared Noise key of c2). mc-c1b already has nft table `mc_c1b` from the P11b setup.
5. **Promotion time.** For F05.1 and F05.3 it is the first time the harness sees the new leader, which is an upper bound. For F05.4 it is polled on the promoted node itself every 100 ms. Probe times use the mc-ci clock, and promotion times use the PG node's clock. Chrony offsets are recorded before each case.
6. **F05.2:** Patroni may restart PostgreSQL in place rather than fail over. The case records which one happened. Both are valid.
7. **Expected RED to watch** (ticket P13a "code"; risk 6 in the ticket):
   - **K3 (silent drop):** a lease renewal in flight on a hung connection keeps `inFlight` set until the self-fence deadline (8 s), because `internal/lease/manager.go` `hold` sends no second attempt. That would be a cluster failover, which D12 does not allow.
   - **F05.3 and F05.4:** writes may hang on the old primary's PgBouncer until `query_wait_timeout` (5 s) or the client's own deadline.

   Fix only when the drill shows it (RED → GREEN).
8. **"Before HA" RED baseline of F05** (a single PG): not part of these harnesses. Run it separately if it is still wanted.
