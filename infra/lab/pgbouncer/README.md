# Per-cluster PostgreSQL connection limits (P13f, D9b)

**State (2026-11-26, P13b):** the aliases are loaded on the PgBouncers of mc-pg-01/02 (`pgbouncer.ini.tmpl` + `pgbouncer-clusters.ini`, installed by `infra/lab/pg/setup-pgbouncer.sh`). The cluster configs still point at mc-db (`meshlab` there); they switch in the `meshlab` move window (`infra/lab/pg/README.md`). mc-db's `mc-pgb` is unchanged.

| File | Installed as (mc-pg-01/02) |
|---|---|
| `pgbouncer.ini.tmpl` | `/etc/pgbouncer/pgbouncer.ini` (`@IP@` = node address): ADR 0006 settings as on mc-db, plus failover settings (`server_login_retry = 1`, `server_connect_timeout = 2`, keepalive, `server_check_query` that fails on a replica) |
| `pgbouncer-clusters.ini` | `/etc/pgbouncer/pgbouncer-clusters.ini` (`%include`) |
| `haproxy-pg.cfg` | `/etc/haproxy/haproxy.cfg`: `127.0.0.1:5000` → node whose Patroni `GET /primary` = 200; `inter 1s fall 2 rise 2`, `on-marked-down shutdown-sessions`, `option redispatch 1` |

ADR 0005 keeps one application role for every cluster (no per-cluster role, no RLS), so PostgreSQL cannot cap connections per cluster. PgBouncer can: each cluster connects to its own **alias** of the same database (`pgbouncer-clusters.ini`), and each alias has its own `pool_size` and `max_db_connections`. A cluster that saturates its alias queues in PgBouncer (`cl_waiting`, alert `PgBouncerClientsWaiting`); other clusters keep their server connections.

## Changes when applied (lab; after P13a/P13b, or on mc-db before)

| Where | Change |
|---|---|
| PgBouncer (mc-pg-01, mc-pg-02; or mc-db's `mc-pgb` container) | `[databases]`: `%include /etc/pgbouncer/pgbouncer-clusters.ini`; `RELOAD` on the admin console (no restart, existing clients keep their pools) |
| Cluster c1 (`mc-cp`, `mc-c1b`) | `database.postgres.name: mesh_c1`, `max_open_conns: 16`, `max_idle_conns: 4`; lease pool (P13b) `database.postgres.lease_name: mesh_c1_lease` |
| Cluster c2 (`mc-c2`, `mc-c2b`) | `database.postgres.name: mesh_c2`, same sizes; lease pool `database.postgres.lease_name: mesh_c2_lease` |
| mesh-placement, mesh-bootstrap, CLI | `database.postgres.name: mesh_ops` |
| Then | remove the `*`/`meshlab` catch-all entries so nothing bypasses the caps |

No code change is needed: the database name is only used in the DSN (no `current_database()` check in the server). Prepared statements: the aliases inherit `max_prepared_statements` from `[pgbouncer]` (P12c/P13b self-check).

## max_connections

Per PgBouncer instance the aliases hold at most `20 + 20 + 3 + 3 + 6 = 52` server connections. Clients list both PgBouncers (P13b DSN host list) and normally use the first; after a PgBouncer failover both may hold pools for a while, so size PostgreSQL for both:

```
max_connections >= 2 × 52            (two PgBouncer instances)
                 + superuser_reserved_connections (3)
                 + max_wal_senders (10: replicas, pg_basebackup, pgBackRest)
                 + Patroni, postgres_exporter ×3, pgBackRest, operator psql (≈ 10)
               = 127  →  set 150 (lab today: 200)
```

Each new cluster adds an alias pair (`mesh_cN` 20 + `mesh_cN_lease` 3) and 46 to the bound; its config names both (`database.postgres.name: mesh_cN`, `lease_name: mesh_cN_lease`). The Go lease pool holds at most `lease_max_open_conns` (default 2) connections per process, so an active and a standby fit `max_db_connections=3` with one spare.

Go side per process: `max_open_conns` ≤ the alias `pool_size` (16) so a single process does not queue on itself; the standby of a cluster uses one or two connections (lease polls).

## Observing it

`pgbouncer_exporter` per PgBouncer (monitoring stack, `infra/lab/monitoring`): `pgbouncer_pools_client_waiting_connections{database="mesh_c2"}`, `pgbouncer_pools_client_maxwait_seconds`, `pgbouncer_pools_server_active_connections`; process side `headscale_db_pool_*{pool="platform"}` on each cluster's operator metrics listener.
