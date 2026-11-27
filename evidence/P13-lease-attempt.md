# PR P13 lease attempt timeout: evidence (drill K3, D12 "0 failovers when a PgBouncer is killed")

- **Status: PAUSED (2026-11-26 23:04, owner changed direction).** `Implemented`; `internal/lease` tests (SQLite + PostgreSQL) `Verified` on mc-ci. The repo-wide race run, lint and `p05cluster` shard were **not run** (stopped before they started). Not `Reviewed`. Branch `feat/P13-lease-attempt` from `feat/P13-integration` `0f5f363`; not merged.
- **Source inventory:** `evidence/source-inventory-P13-lease-attempt.md`.

## 1. Commits

| Commit | Content |
|---|---|
| `1ed96f7` | RED: `internal/lease/attempt_test.go` |
| `9f752f1` | GREEN: `Manager.hold` (attempt timeout, retry with backoff, at most 2 in flight, deadline only from a success's start), `Store.dropIdle`, lease pool `connect_timeout` ≤ 1 s, `TestLeaseDatabase`, PostgreSQL test `blackhole_pg_test.go` |
| `1a11006` | Config docs regenerated (`go generate ./hscontrol/types/`, tools/configgen) |
| (this commit) | ADR 0011 §D note, source inventory, this evidence |

## 2. Problem

Before this change, `hold` ran one renewal at a time, with context deadline = self-fence deadline, and later ticks skipped while it was in flight. A renewal sent on a connection whose packets are dropped without an RST (a PgBouncer behind nft drop, a VM stopped hard, a cable pulled) waited until the deadline. The process then self-fenced and the standby took over: one dead connection cost a cluster failover. The second PgBouncer in the host list was never tried.

## 3. Design

- **Per-attempt timeout:** `(TTL − SafetyMargin − RenewInterval) / 2`, floor `(TTL − margin) / 8`. Default: (10 − 2 − 3) / 2 = **2.5 s**. When less than half an attempt timeout would remain after this attempt, the last attempt that fits gets all the time left until the deadline instead.
- **Retry after a failure or timeout:** `RenewInterval / 12` (250 ms by default), doubling up to `RenewInterval`; no waiting for the next tick. At most **2** renewals in flight. A second one starts only when the newest has passed its own deadline (a driver that does not honour the context).
- **Safety unchanged:** the deadline only moves to `start of a successful attempt + TTL − margin`, and never back. A late answer from an older attempt changes nothing. A renewal's database expiry is `now() at execution + TTL`, which is ≥ its start + TTL, so the process always stops before the row expires (as in P11b/ADR 0010). `ErrLost` from any attempt still ends the lease at once.
- **Connections:** pgx closes a connection whose context ends mid-statement (`DeadlineContextWatcherHandler`), and database/sql discards it (`ResetSession` → `ErrBadConn`). After a failed renewal the lease pool also closes its idle connections (`Store.dropIdle`, PostgreSQL only), because they may lead to the same dead server. The retry therefore opens a fresh connection.
- **Lease pool `connect_timeout` at most 1 s** (`leaseDatabase`; the data pool keeps its value, default 2 s). A fresh connection gives up on a dead first host after 1 s and goes to the next host in the list, within one attempt.
- **Worst-case K3 timeline** (default timing, last success at t = 0): attempt at t = 3 s hangs → abandoned at 5.5 s → retry at 5.75 s → host 1 gives up at 6.75 s → host 2 answers ≈ 6.8 s < deadline 8 s. Margin ≈ 1.2 s.

## 4. RED → GREEN

| Test | RED (`1ed96f7`) | GREEN (`9f752f1`) |
|---|---|---|
| `TestManagerRetriesAfterHungRenewal` (first renewal hangs; one variant honours the context, the other ignores it; scaled default timing) | FAIL: "lease ended although only one renewal hung: self-fence deadline passed (renewals tried: 1)", on SQLite and PostgreSQL | PASS: epoch 1 kept for 3 × TTL, row live, at most 2 in flight |
| `TestRenewSkipsBlackholedPgBouncer` (PostgreSQL, real pgx; a TCP proxy that drops every byte without RST stands in for the first PgBouncer of a two-host DSN; `DefaultConfig`) | FAIL on the old manager (test file applied to `1ed96f7`): "lease ended 7.506s after the first PgBouncer was blackholed: self-fence deadline passed" | PASS: "blackholed the first PgBouncer for 20s: lease kept at epoch 1, 1 connection(s) to the second, row live for 7.5s more" |
| `TestManagerSelfFencesWhenEveryRenewalHangs` (safety: every renewal hangs) | PASS (1 attempt) | PASS: self-fence on time, row still live, 2 attempts, never more than 2 in flight |
| `TestManagerDeadlineNeverPassesASuccess` (late successes of older attempts; the deadline is sampled every 2 ms against the newest successful start + window) | PASS | PASS |
| `TestLeaseDatabase` (lease pool `connect_timeout` 1 s when unset or higher; data pool unchanged) | – | PASS |
| Existing manager tests (`TestManagerSelfFencesWhenRenewHangs`, `…DuringSlowOnAcquire`, `…StepDown`, `…StandbyTakesOverAfterCrash`, …) | PASS | PASS |

## 5. Commands and exit codes (mc-ci, `~/src/p13lease`, Go 1.27.4)

```
# RED, SQLite then PostgreSQL 16 (throwaway, socket)
go test -race -count=1 -run 'TestManagerRetriesAfterHungRenewal|TestManagerSelfFencesWhenEveryRenewalHangs|TestManagerDeadlineNeverPassesASuccess' ./internal/lease/   # exit 1 (RED as expected)
MESH_TEST_POSTGRES_DSN=… go test -race … (same)                                                                                                             # exit 1
# RED of the PostgreSQL test on the old manager (1ed96f7 + blackhole_pg_test.go)
MESH_TEST_POSTGRES_DSN=… go test -race -count=1 -run TestRenewSkipsBlackholedPgBouncer ./internal/lease/                                                 # exit 1
# GREEN
go test -race -count=1 ./internal/lease/                                                         # exit 0 (SQLite)
MESH_TEST_POSTGRES_DSN=… go test -race -count=1 -run 'TestManager|TestRenewSkips' ./internal/lease/   # exit 0
go vet ./internal/lease/ ./hscontrol/db/                                                          # exit 0
```

Not run (paused): the repo-wide race run with throwaway PostgreSQL + PgBouncer (`~/p13lease-verify.sh` was stopped while starting the servers; `~/p13lease-race-1a11006.log` holds only its header), `golangci-lint run`, and the `p05cluster` shard run. To resume: `~/p13lease-verify.sh` on mc-ci (race, lint, then shard).

## 6. Risks and limits

- A renewal that is slow but alive (a lease alias pool saturated and queueing up to `query_wait_timeout` 5 s) is now abandoned after 2.5 s and retried, where before it could still succeed at 4–5 s. Retrying with a new request does not help a pool that is really saturated. The lease alias pool is separate and small (ADR 0006 §3.6), and its queueing is alerted (`PgBouncerClientsWaiting`), so this case should be rare. It is better covered by sizing the pool than by a longer attempt.
- `connect_timeout` 1 s on the lease pool assumes a PgBouncer on the same network. A WAN or TLS path slower than 1 s per connection would fail every connection. That would show as renew errors well before production, because the startup self-check uses the same pool.
- Load added on failures: at most about 5 extra renewals per process per self-fence window (backoff 250 ms doubling). Each renewal is one indexed UPDATE.
- F05.3 / F05.4 (PostgreSQL primary hard-stopped or partitioned): Patroni needs ≥ 20 s to fail over, which is more than the 8 s window, so the active process still self-fences there, as D12 (≤ 45 s) expects. This change only removes failovers caused by a dead **path** when a healthy one exists (the second PgBouncer).
- Not yet proven on the lab: drill K3 (`infra/lab/e2e/p13/pgb-kill.sh`, branch `feat/P13-drill-f05`) with this binary.
