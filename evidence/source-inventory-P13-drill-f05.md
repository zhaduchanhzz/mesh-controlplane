# Source inventory: P13 drill harnesses F05 and PgBouncer kill

- **Branch:** `feat/P13-drill-f05`, from `feat/P13-integration` at `0f5f363` (P13 integration with P12d merged in).
- **Scope:** only `infra/lab/e2e/p13/` (`lib.sh`, `jobs.sh`, `f05.sh`, `pgb-kill.sh`, `README.md`) and this file.
- **Not touched:**
  - no product code;
  - `infra/lab/pg/` and `infra/lab/pgbouncer/` belong to the P13b infra work;
  - no lab state was changed: the scripts were not run.
- **Status:** `Implemented` (scripts written; `bash -n` and shellcheck `-S warning` with 0 findings, run on mc-ci with `nix shell nixpkgs#shellcheck`, shellcheck 0.11.0). **Not run yet**: the lab was busy with the P12d E2E.

## Read

| Path | What was used |
|---|---|
| `tickets/P13.md` §LAB-P13, §P13a, §P13b, D12, Risks | F05.1–F05.4 and the PgBouncer kill cases (kill -9, stop, network drop without RST), what each case measures, the negative checks, and the thresholds: F05 ≤ 45 s, ping gap ≤ 10 s, 0 manual interventions, 0 failovers on a PgBouncer kill, write errors ≤ 10 s |
| `infra/lab/e2e/p11b/run.sh` | Active/standby c1 on mc-cp + mc-c1b behind a transient HAProxy with a `/ready` check; Noise key and TLS copied through pipes; mc-c1b nft; lease epoch checks; L01 from the journal; S01 secret scan |
| `infra/lab/e2e/p12b/run.sh`, `jobs.sh` | `CI_BINDIR` LAN fetch with sha256 check, transient units on mc-ci, linger check, ProxyCommand to the client VMs, a second tailscaled on the VMs, keepalives, the operating rules |
| `infra/lab/pg/README.md`, `switchover-test.sh`, `check-pg.sh` | Patroni scope `mc-pg`, `mc-patronictl`, `/cluster` on 8008, PGDATA, peer auth as `postgres`, `mc-pg.env` key `MC_PG_HEADSCALE_PASSWORD`, client DSN with `target_session_attrs=read-write`, writer pattern, "acknowledged but missing" check |
| `infra/lab/pgbouncer/pgbouncer-clusters.ini`, README | Aliases for each cluster. The drill uses the wildcard (`*`) for its own database |
| `infra/lab/c2/setup-c2.sh` | c2 layout: listen 0.0.0.0:8443, metrics 127.0.0.1:9095, front door on mc-cp:8445 |
| `infra/lab/scripts/pve_api.py` | VM power through the Proxmox API: pinned cert, credentials from `~/.mesh-lab/proxmox.env`, never printed. The harness only lets VMID 7708/7709 in pool `meshcontrol-lab` through |
| `cmd/headscale/cli/cluster.go`, `cluster_apikey.go` | `cluster migrate` (operator step; also works through PgBouncer), `register`, `tailnet create/status/policy/authkey/nodes`, `tailnet apikey create --expiration` |
| `internal/lease/manager.go`, `metrics.go` | Log line `cluster lease acquired: active` (cluster, holder, lease_epoch); metrics `headscale_cluster_lease_{held,epoch,events_total{event=self_fence/lost/renew_error}}`; `hold` keeps one renewal in flight until the deadline |
| `docs/ref/config-example.yaml` | `database.postgres.hosts`, `target_session_attrs`, `connect_timeout_secs`, `lease_name`; lease TTL 10 s / renew 3 s / margin 2 s; tenant API rate limits |
| `hscontrol/db/schema_postgres.sql` | `tailnets.owner_epoch`, `cluster_leases(cluster_id, holder, epoch)`, the trigger that keeps the lease epoch monotonic |
| `evidence/P08a-endpoint-inventory.md` | `POST /api/v2/device/{id}/name` on the public tenant API |

## Read-only checks on the lab (2026-11-26)

- **mc-pg-01:**
  - `pgbouncer.service` is active (`Restart=on-failure`, 1 s);
  - `[databases] * = host=127.0.0.1 port=5000` plus `%include pgbouncer-clusters.ini`;
  - `haproxy.service` `listen pg_primary` on 127.0.0.1:5000 checks `/primary` on 8008 for both nodes, with `on-marked-down shutdown-sessions`;
  - `psql` and `jq` are present; passwordless sudo works.
- **mc-cp:** the long-lived `mesh-c2-ha` front door (8445, `/ready` over mc-c2 and mc-c2b) is active. During the check the N1 run was using `mesh-c1-ha` and `mesh-n1-e2e`; the drill refuses to start while those run.
- **mc-c2 / mc-c2b:**
  - live config uses `server_url https://c2.cp.gta5web.online:8445`, listen 0.0.0.0:8443, metrics 127.0.0.1:9095, database `meshlab` on mc-db;
  - `headscale-cluster` was inactive during the N1 run.
- **mc-ci:** `psql` is present; linger for `mesh` = yes.

## Constraints kept

- **AGENTS 2:**
  - the PG password goes from `mc-pg.env` into files with mode 0600/0640 through stdin only;
  - the Proxmox credentials stay inside `pve_api.py`;
  - artifacts are scanned for keys and for the password itself (S01).
- **AGENTS 3/4:** VM power actions are limited to VMID 7708/7709, and the harness checks the pool `meshcontrol-lab` before acting. There is no destroy, and no firewall change outside the lab VMs: the nft tables `inet p13_drill` are transient, inside the PG VMs, with a dead-man timer.
- **AGENTS 13:** a write acknowledged by an isolated old primary after promotion, or an acknowledged write that is missing afterwards, is reported as a BLOCKER.
- **Standing permission:** stopping the long-lived c2 and the P02 POC for an E2E run is covered. The harness restarts exactly the services it found active.

## Assumptions (to check on the first run)

See `infra/lab/e2e/p13/README.md`, "Assumptions to check on the first run" (items 1–8). The main ones:
- the PgBouncer wildcard and auth for `headscale`;
- `cluster migrate` from mc-ci with a config that only has the database part valid;
- a device rename is a fenced write;
- likely RED on K3 (a renewal in flight on a silently dropped connection → self-fence).
