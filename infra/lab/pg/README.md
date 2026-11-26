# infra/lab/pg — PostgreSQL HA cluster `mc-pg` (LAB-P13, P13a)

Patroni + etcd, synchronous replication (not strict), as decided in ADR 0011 (owner-approved 2026-11-26, D1/D2/D7). This is a **fresh cluster**. The long-lived `meshlab` database still runs on mc-db (`mc-pg`/`mc-pgb` containers) until the announced move window (see the end of this file).

## Topology

| Host | VMID | Address | Runs |
|---|---|---|---|
| `mc-pg-01` | 7708 | 192.168.1.247 | PostgreSQL 16.16 (PGDG) under Patroni 4.1.5, etcd 3.6.17, chrony, softdog, nftables |
| `mc-pg-02` | 7709 | 192.168.1.248 | same |
| `mc-db` | 7704 | 192.168.1.243 | etcd 3.6.17 only (third member, witness). No Patroni PostgreSQL. Docker `mc-pg`/`mc-pgb` are not touched |

- Patroni scope `mc-pg`, namespace `/service/`, DCS `etcd3` (all three members listed).
- DCS settings: `ttl 20`, `loop_wait 5`, `retry_timeout 5`, `synchronous_mode true`, `synchronous_mode_strict false`, `use_pg_rewind`, `use_slots`.
- PostgreSQL: `initdb --data-checksums`, locale `C.UTF-8`, `wal_log_hints on`, `synchronous_commit on`, `max_connections 200`, `shared_buffers 1GB`. Data directory: `/var/lib/postgresql/16/mc-pg`.
- `archive_mode on` with `archive_command /bin/true`. This only prepares for pgBackRest: switching on `archive_mode` needs a restart, while changing `archive_command` needs only a reload. **WAL is not archived and there is no backup until P13c.**
- Watchdog: `softdog`, `mode: required`, 15 s timeout (ttl − safety_margin 5). Patroni arms it only while it is the leader.
- Roles:
  - `postgres` (superuser), `replicator`, `rewinder`, created by Patroni.
  - `headscale` (application role, `idle_in_transaction_session_timeout = 5s`, like mc-db). There are no application databases yet.

## Files

| File | Purpose |
|---|---|
| `main.tf` | OpenTofu root: the two VMs + cloud-init snippets (`../cloud-init/mc-pg.yaml.tftpl`: no Docker). State lives in the **main checkout** `infra/lab/pg/terraform.tfstate` (gitignored) with a dated copy in `%USERPROFILE%\.mesh-lab\tofu-state\` |
| `setup-etcd.sh` | etcd member (all three hosts). On mc-db it adds only nft table `inet mc_etcd` (tcp/2379–2380), persisted by `mc-etcd-nft.service` |
| `setup-pg.sh` | PG node: PGDG packages (pinned + `apt-mark hold`), chrony, softdog, nftables (`mc-pg.nft`), Patroni (`patroni.yml.tmpl`, unit `mc-patroni.service`), role `headscale` |
| `deploy.sh` | Laptop-side order: `etcd`, then `pg` (secrets generated on mc-pg-01, copied to mc-pg-02 through an SSH pipe), `status` |
| `check-pg.sh` | Read-only health check (etcd quorum, Patroni, sync replica, checksums, watchdog, chrony, firewall probes). Exit 0 = all pass |
| `switchover-test.sh` | One controlled switchover, with a test database written through the client DSN while it runs. Not the F05 drill |

All scripts are idempotent: re-running `deploy.sh pg` restarts nothing when nothing changed.

## Secrets

- On each PG node, in `/etc/mc-pg/` (root, 0700):
  - `patroni.env` holds `PATRONI_{SUPERUSER,REPLICATION,REWIND,RESTAPI}_{USERNAME,PASSWORD}` and is loaded by systemd and by `mc-patronictl`.
  - `headscale.pass` holds the application role's password.
- Operator copy: `%USERPROFILE%\.mesh-lab\mc-pg.env` (ACL: owner only). It has the same keys plus `MC_PG_HEADSCALE_PASSWORD`.
- No password ever appears in a command line, the Patroni YAML, or this repository.

## Operating

```bash
bash infra/lab/pg/check-pg.sh                     # health; exit 0 = all pass
ssh mesh@192.168.1.247 sudo mc-patronictl list    # topology
ssh mesh@192.168.1.247 sudo mc-patronictl switchover --leader <cur> --candidate <sync> --force
ssh mesh@192.168.1.247 sudo mc-patronictl edit-config   # DCS settings (stored in etcd)
ssh mesh@192.168.1.247 'etcdctl --endpoints=http://192.168.1.247:2379,http://192.168.1.248:2379,http://192.168.1.243:2379 endpoint status -w table'
```

Firewall (`mc-pg.nft`, policy drop):

| Port | Allowed from |
|---|---|
| 22 | 192.168.1.0/24 |
| 5432 | the two PG nodes, mc-ci (.240), mc-db (.243) |
| 6432 | mc-ci, mc-cp, mc-bench, mc-c2, mc-c1b, mc-c2b (prepared for P13b; nothing listens yet) |
| 2379/2380 | the three etcd members |
| 8008 | the PG nodes and mc-db |

On mc-db, only 2379/2380 are filtered (to the members).

## Client endpoint (ADR 0011 §D, decision D3)

**Now (P13a, direct PostgreSQL):** a libpq/pgx multi-host DSN that only accepts a read-write session:

```
host=192.168.1.247,192.168.1.248 port=5432 dbname=<db> user=headscale
target_session_attrs=read-write connect_timeout=2
# URL form: postgres://headscale@192.168.1.247:5432,192.168.1.248:5432/<db>?target_session_attrs=read-write&connect_timeout=2
```

- The client tries the hosts in order and skips one that refuses the connection or is in recovery. That is how it finds the leader.
- It was verified through a switchover: every write landed on the current primary, with a 1.67 s write gap (`evidence/P13a-ha-postgres.md`).
- 5432 is open only to the PG nodes, mc-ci and mc-db. This form is for the operator, tests, and the `meshlab` dump/restore. It is not for the cluster processes.

**Target for the cluster processes (P13b):**

```
host=192.168.1.247,192.168.1.248 port=6432 ... target_session_attrs=read-write
```

- One PgBouncer (ADR 0006 settings; mc-db runs 1.24.1, PGDG now ships 1.26.x) runs on each PG node and sends to a local HAProxy `127.0.0.1:5000`, which picks the node whose `GET :8008/primary` returns 200 (`on-marked-down shutdown-sessions`). Both PgBouncers therefore always reach the current primary.
- A pool alias `mesh_lease` serves the lease store (ADR 0006 §3.6).
- The code change is P13b: `PostgresConfig` must take a host list.

**Fallback (if the multi-host DSN does not work through PgBouncer with pgx):**
- keepalived VRRP VIP **192.168.1.249** (kept free; it may sit inside the router's DHCP pool, so reserve it first), held by the node whose Patroni is the leader (`/primary` check script);
- clients use `host=192.168.1.249 port=6432`, with no code change.
- This is not installed.

## `meshlab` move window (later, announced; not done here)

Needs:
1. P13b PgBouncer + HAProxy on mc-pg-01/02, or an explicit decision to point c2/c1 at 5432 temporarily. 5432 is not open to .241/.244–.246 today: that needs a `mc-pg.nft` change.
2. A stop of the long-lived c2 (mc-c2/mc-c2b) and the c1 POC. This is covered by the standing permission.
3. Dump and restore:
   - `pg_dump -Fc` from mc-db (`docker exec mc-pg pg_dump -U postgres -Fc meshlab`);
   - `CREATE DATABASE meshlab OWNER headscale` on the Patroni leader. The new cluster's locale is C.UTF-8, while mc-db is the image's en_US.utf8, so do **not** use `pg_restore -C`;
   - `pg_restore --no-owner --role=headscale -d meshlab`;
   - check `migrations` and row counts.
4. Point the configs of c2/c1 at the new DSN and restart them. Keep `mc-pg`/`mc-pgb` stopped, without deleting the volume, until the owner approves.
5. Before or with the move: decide whether to enable pgBackRest first (P13c). Until then the new cluster has **no backup**, while the old container has none either.
