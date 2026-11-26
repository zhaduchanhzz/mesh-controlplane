# Source inventory — P13b/P13c lab infra (PgBouncer + HAProxy, pgBackRest)

- **Branch / base:** `feat/P13b-infra` from `feat/P13-integration` `0f5f363` (2026-11-26).
- **Status:** PAUSED (owner changed direction 2026-11-26 23:04). What is applied on the lab: `evidence/P13b-pgbouncer-ha.md` §7.
- **Scope:** lab files only (`infra/lab/pg/`, `infra/lab/pgbouncer/`) and evidence. No Go code, no migration, no `gen/`.

## Read

| Path | What was used |
|---|---|
| `infra/lab/pg/README.md`, `setup-pg.sh`, `deploy.sh`, `check-pg.sh`, `switchover-test.sh`, `patroni.yml.tmpl`, `mc-pg.nft` | P13a topology, secrets layout (`/etc/mc-pg`), pg_hba for role headscale (.240/31, .242, .244/31, .246/31, .248), nft (6432 already open to the cluster VMs, mc-ci, mc-bench), Patroni DCS (ttl 20, archive_mode on) |
| `infra/lab/db/setup-db.sh` | mc-db PgBouncer settings to copy (transaction pool, `max_prepared_statements = 200`, `query_wait_timeout = 5`, `idle_transaction_timeout = 5`, SCRAM, userlist) |
| `infra/lab/pgbouncer/README.md`, `pgbouncer-clusters.ini` | per-cluster aliases (D9b), lease aliases, `max_connections` bound |
| `evidence/P13b-pgbouncer-ha.md` §2–§5 | client side: host list, `target_session_attrs`, lease pool, self-check, renew-on-dead-connection limitation |
| `tickets/P13.md` §P13a, §P13b, §P13c, D12 | drill thresholds (kill PgBouncer: write errors ≤ 10 s) |
| `hscontrol/db/pooler.go`, `hscontrol/db/platform.go` (symbols `checkPooler`, `poolerReadOnlyQuery`, `TargetSessionAttrs` default) | what the process checks at open time (not per transaction) |
| `infra/lab/e2e/p13/f08.sh` (`feat/P13-drill-ops`), `precheck()` | preconditions the F08 drill expects: stanza `mc-pg` status 0, `archive_command` with pgbackrest, PgBouncer from mc-cp, scratch port 5439 / `pg_ctl` / `pg_dump` on the replica |

## Constraints applied

- AGENTS rules 2–4: secrets only on the nodes (`/etc/mc-pg`, root 0600) and in `%USERPROFILE%\.mesh-lab\mc-pg.env`; never printed or committed. Only lab-pool VMs mc-pg-01/02 changed, plus a dedicated SFTP user and an sshd drop-in on mc-db (no restart; Docker, mc-pg, mc-pgb and etcd untouched).
- The busy E2E path (mc-db PgBouncer :6432 and its PostgreSQL, mc-cp, mc-c2, client VMs) was not touched.

## Assumptions

- PgBouncer 1.26 lets user `pgbouncer` into the admin console over the unix socket from its own uid; `peer` in `auth_hba_file` for a user missing from `auth_file` is rejected. Verified on the nodes.
- pgBackRest 2.60 SFTP repo needs `repo1-sftp-host-key-hash-type` even with `strict` known_hosts checking. Verified (exit 37 without it).
