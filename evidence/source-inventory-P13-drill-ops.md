# Source inventory — P13 drill harnesses: F08, canary, Noise rotation, relay restart

- **Branch:** `feat/P13-drill-ops` from `feat/P13-integration` `0f5f363` (P13 integration with P12d merged).
- **Scope:** harness files only (`infra/lab/e2e/p13/{lib-ops,f08,canary,noise-rotate,relay-restart}.sh`, `README-ops.md`). No product code, no `infra/lab/pg` or `pgbouncer` change. **Not run** (lab busy). Status: `Implemented` (harness), drills `Not started`.

## Read (paths and symbols)

| Path | Symbols / what was used |
|---|---|
| `tickets/P13.md` | §P13c (F08 steps, stop conditions), §P13d (canary, N+1 = same tree + one drill-only expand migration, rollback), §P13e (drill on c2, ≥ 2 tenants, both clients, emergency variant), §P13g (relay restart drill), D12 thresholds, owner decisions (canary within a minor, VPS after green) |
| `docs/ops/noise-key-rotation.md` | §2 planned steps 2–12, §3 emergency (step 4: rotate before a new-key process runs), §6 measures |
| `cmd/headscale/cli/cluster.go` | `cluster serve --init-noise-key`, `cluster migrate` (`db.Migrate`, JSON `MigrateResult`), `cluster epoch-fence --reason --operator [--external-hwm]`, `tailnet reassign --to`, `register` |
| `cmd/headscale/cli/cluster_noisekey.go` | `noise-key show` (`match`), `install --generate / --from`, `rotate [--public-key]` |
| `cmd/mesh-placement/main.go` | `migrate`, `epoch-fence` (same functions; not used by the drills) |
| `hscontrol/db/epoch_fence.go` | `EpochFenceResult{floor, holder, leases, tailnets, moves_cancelled}`, floor = unix s × 2^20, trigger refusing DELETE/TRUNCATE of `cluster_leases`, `platform_epoch_fences` |
| `hscontrol/db/migrate.go`, `schema_compat.go`, `platform.go` | `MigrateResult{fresh, applied, latest}`, `migrationCompat` (expand min_reader = `SchemaCompatMigrationID`), `v01Migrations()` order, schema files `schema.sql` / `schema_postgres.sql` (go:embed) |
| `hscontrol/db/versioncheck.go`, `hscontrol/types/version.go` | same-minor rule, `dev` skips the check, version from build info |
| `internal/lease/manager.go`, `metrics.go` | `hold` (renew, self-fence deadline), `AcquireAbove(HWM)`, `ErrEpochRegression` log "CRITICAL epoch regression", "cluster lease acquired: active" / "ended: standby", metrics `headscale_cluster_lease_*` |
| `hscontrol/cluster.go` | `ownerEpochRegressed` (owner_epoch HWM), "loaded tailnet runtime" (`slug`, `epoch`), "tailnet handed over: no longer served here" |
| `hscontrol/db/tenantscope/tenantscope.go` | `headscale_tenant_fence_refusals_total{reason}` |
| `hscontrol/types/config.go` | `database.postgres.hosts`, `target_session_attrs`, `lease_name` |
| `infra/lab/e2e/n1/run.sh`, `cfg-n1.py` | overlay anchors (`CASES_ALL="`, `  log "cleanup"`, `ART_DIR` line), helpers (`direct`, `active_of`, `start_proc`, `cli`, `TS`, `vm`, `relay`, `record`), derper unit flags, relay cert/secret install, `TMPD/key-*`, `vsecret-*` |
| `infra/lab/e2e/p11b/run.sh`, `p12b/run.sh`, `p12b/jobs.sh` | HAProxy /ready config, F01 kill pattern, lease audit parse, `CI_BINDIR` LAN fetch, pinger format `<ms> <rc>`, docker clients |
| `infra/lab/pg/{README.md,patroni.yml.tmpl,setup-pg.sh}`, `infra/lab/pgbouncer/*` | Patroni REST 8008 `/primary` `/replica`, `archive_command: /bin/true` (pgBackRest not wired), PgBouncer → HAProxy :5000 design (not deployed) |

## Read-only lab inspection (2026-11-26)

- mc-pg-01 / mc-pg-02: `mc-patroni` and `mc-etcd` active, leader mc-pg-02, sync standby mc-pg-01, TL 2. `pgbackrest` installed, `/etc/pgbackrest.conf` has only `[global] repo1-path`. Nothing listens on 6432 or 5000.
- mc-ci: `/usr/local/go/bin/go`, `/home/mesh/git/mesh.git`.

## Constraints kept

- AGENTS 2: no secret in argv, logs or artifacts.
  - Passwords and keys move through stdin or 0600 files only.
  - Each harness ends with an artifact secret scan.
  - The relay drill only counts derper processes with the secret in argv; it never prints the secret.
- AGENTS 3/4: lab VMs only. No firewall change outside the VMs (`nft` inside the client VM a2, as N1 S15). No VM is deleted. The relay VM reboot is opt-in.
- AGENTS 11/12: the canary N+1 migration exists only in a throwaway clone on mc-ci (never committed). No Noise model change: the drill uses the ADR 0014 CLI.
- F08 never writes meshlab or any database other than `e2e_p13f08<run>`. The PITR target is a scratch instance, deleted at cleanup.

## Assumptions

See `infra/lab/e2e/p13/README-ops.md` §4. The checks are `bash -n` clean, and shellcheck 0.11.0 (nix, mc-ci) reports nothing at `-S warning`.
