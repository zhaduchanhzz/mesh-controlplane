# Source inventory: P13 lease attempt timeout (drill K3)

- **Base:** `feat/P13-integration` `0f5f363` (P12d merged into the P13 integration). Branch `feat/P13-lease-attempt`.
- **Trigger:** review of the P13 drill harness (`feat/P13-drill-f05`, `infra/lab/e2e/p13/README.md` assumption 6). PgBouncer kill case K3 (tcp/6432 dropped, no RST) would make the active process self-fence. D12 allows 0 cluster failovers when a PgBouncer is killed.

## Paths and symbols read

| Path | Symbols | Relevant fact |
|---|---|---|
| `internal/lease/manager.go` | `Config`, `DefaultConfig`, `Validate`, `Manager.hold`, `Run`, `acquire`, `deadlineFrom`, `extend` | Before this change `hold` ran one renewal at a time, with context deadline = self-fence deadline. While it was in flight, later ticks did nothing (`if inFlight { continue }`). After a failure nothing ran until the next tick. |
| `internal/lease/lease.go` | `Store.Renew`, `Store.Release`, `AcquireAbove`, `nowPlus`, `now` | Renew is one `UPDATE … WHERE holder, epoch, expires_at > now()`. Its expiry runs from the database clock at execution, never earlier than the attempt's start. |
| `internal/lease/lease_test.go` | `openStore`, `fast`, `stuckStore`, `TestManagerSelfFencesWhenRenewHangs`, `TestManagerSelfFencesDuringSlowOnAcquire` | Existing safety tests; they must still pass. |
| `hscontrol/db/platform.go` | `OpenPlatform`, `leaseDatabase`, `LeaseStore`, `writableDatabase` | The lease pool is a separate `*gorm.DB` (PostgreSQL), with the same hosts and settings as the data pool, plus `lease_name` and 2 connections (idle = open). On SQLite the lease store shares the data pool. |
| `hscontrol/types/config.go` | `PostgresConfig.Hosts`, `ConnectTimeoutSecs`, `LeaseName`, `DSN()` | `connect_timeout` is per host; pgx tries the next host after it. Default `connect_timeout_secs: 2` (`hscontrol/config/default.go:58`). |
| `hscontrol/cluster.go:327` | `lease.NewManager(platform.LeaseStore(), …)` | The only user of the lease pool. |
| pgx v5.11.0 (`pgconn/config.go:472`, `stdlib/sql.go:566`) | `DeadlineContextWatcherHandler`, `Conn.ResetSession` | When the context ends, pgx sets the net.Conn deadline to now, and the connection is closed (`IsClosed`). database/sql then discards it (`driver.ErrBadConn` in `ResetSession`), so a cancelled attempt never leaves a dead connection in the pool. |
| `design/adr/0006-*.md` §4, `design/adr/0011-*.md` §D | | `query_wait_timeout` stays below the self-fence deadline. Clients use a host list with `connect_timeout=2`. |
| `infra/lab/pgbouncer/pgbouncer-clusters.ini`, `infra/lab/pg/patroni.yml.tmpl` | | Lease aliases per cluster; Patroni `ttl 20`. |

## Constraints

- The safety of ADR 0010 / P11b does not change. The self-fence deadline is the **start** of the last successful renewal + TTL − SafetyMargin, on the monotonic clock. It never moves back and never moves beyond that.
- No migration, no Noise change, no new configuration key (the lease pool caps the existing `connect_timeout_secs`). Generated docs are regenerated with `tools/configgen`, not edited by hand.

## Assumptions

- One second covers TCP connect + authentication to a PgBouncer on the same network (LAN lab: milliseconds).
- Killing a PgBouncer process (K1, K2) gives an RST or a refused connection, so the error is immediate and the retry follows after 250 ms. A silent drop (K3) costs one attempt timeout, 2.5 s by default.
