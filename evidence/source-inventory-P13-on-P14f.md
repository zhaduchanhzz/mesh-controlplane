# Source inventory: P13 code on top of P14f (`feat/P13-on-P14f`)

Status: **Implemented** (code only). Not `Verified` for P13: no drill, no
lab run, no `p05cluster` shard and no integration suite on this branch (out
of scope by instruction: mc-ci is shared with timing-sensitive E2E and
capacity runs). Not merged into `master`; P13 resumes from here once P14f is
accepted.

## Commits

| What | Commit |
|---|---|
| Base: `feat/P14f-int` (mcci; origin was behind at `d2a4bc7`) | `003d47c` |
| Merged: `feat/P13-integration` | `0f5f363` |
| Merged: `feat/P13-lease-attempt` (builds on `0f5f363`) | `0442a5d` |
| Common ancestor of both sides | `06c9f7b` (P12d, before `master` `483afb0`) |
| Merge of P13 integration | `4c7969b` |
| Merge of the lease attempt (no conflicts) | `2d3ffe6` |
| `lease_name` doc keeps P14f's PgBouncer note | `ae4f7e8` |
| gofmt + regenerated config docs (mc-ci) | `08d1435` |
| `max_open_conns` example 50 (was the default 100) | `78e5073` |
| Regenerated config docs for that example | `cd077d7` |
| Lint fixes in the lease attempt tests | `dfd4219`, `ba031ac` |
| This inventory | the commit adding this file (branch tip) |

## Left out on purpose

These paused branches are **not** merged (instruction 2026-11-27); each is
one commit on `0f5f363` and can be merged later on top of this branch:

| Branch | Tip | Content |
|---|---|---|
| `feat/P13-drill-f05` | `46e7ed5` | F05 and PgBouncer-kill drill harnesses (not run) |
| `feat/P13-drill-ops` | `7241997` | F08 backup/restore, canary, Noise rotation, relay restart drill harnesses (not run) |
| `feat/P13b-infra` | `0d28360` | PgBouncer + HAProxy on mc-pg-01/02, pgBackRest repo (lab infra, paused) |

## Paths and symbols read

| Path | Symbols | Fact used |
|---|---|---|
| `AGENTS.md`, `hscontrol/db/AGENTS.md` (rules) | | append-only migrations, no AutoMigrate, never edit `gen/` by hand, isolation = BLOCK |
| `hscontrol/db/platform.go` (both sides) | `OpenPlatform`, `openPlatformChain`, `leaseDatabase`, `leaseConnectTimeoutSecs`, `closePool`, `writableDatabase`, `checkPooler`, `LeaseStore`, `Close` | P14f `003d47c` is a minimal port of P13b `4556c82` (same `leaseDatabase` / `closePool`, default 2 lease connections). P13 adds host list, pooler self-check on the alias, read-write target, and does not migrate (P13d). Only `003d47c` touched this file on the P14f side. |
| `hscontrol/db/lease_pool_test.go` (P14f), `hscontrol/db/pooler_test.go` (P13) | `TestLeaseStoreHasOwnPool`, `TestLeaseDatabase` | Same names, same assertions; P13's run `migrateP13b` first and check the host list and `target_session_attrs`. |
| `hscontrol/db/migrate.go` | `Migrate`, `MigrateAndOpenPlatform` | Callers: `headscale cluster migrate`, `mesh-placement migrate`, tests. |
| `hscontrol/types/config.go` | `PostgresConfig` (`LeaseName`, `LeaseMaxOpenConnections`, `MaxOpenConnections`, `ConnectTimeoutSecs`), `Tuning.MaxPeerEntries`, `IncrementalNetmapConfig` | Auto-merge left `LeaseName`/`LeaseMaxOpenConnections` twice (P13 position after `StatementTimeout`, P14f position after `QueryExecMode`). |
| `hscontrol/config/default.go` | `Default()` | P14f: `max_open_conns` 100, idle 10, incremental netmap on. P13: `connect_timeout_secs` 2. Both have `lease_max_open_conns` 2. |
| `hscontrol/config/validate.go` | `validateDatabase`, `validatePostgres` | P14f refuses `max_open_conns < 1`; P13 moved Postgres checks into `validatePostgres`. |
| `hscontrol/config/default_test.go:186,194`, `schema_test.go` `TestExamples` | | Reason text "database.postgres.max_open_conns must be at least 1" asserted; examples must differ from defaults. |
| `internal/lease/manager.go` | `Config.Validate`, `attemptTimeout`, `hold` | Lease attempt (`0442a5d`): bounded renewal + retry; validation unchanged (TTL ≥ 1 s, margin ≤ TTL/2, renew < TTL − margin), so `ramp.sh`'s `LEASE_*` knobs stay valid. |
| `hscontrol/state/state.go`, `node_store.go` | `ReloadTenantPolicy`, `ReloadTenantDNS`, `NewStateWithDB`, `Close`, `SetMetricsTailnet`, `WithPeerEntriesBound` call | P13f L4/L9 changes merged without conflict; P14f R2b `WithPeerEntriesBound(MaxPeerEntries, PeerEntries())` kept. |
| `cmd/headscale/cli/cluster.go` | `tailnetPolicyCmd`, `setTailnetPolicyCLI`, `validateTailnetPolicy` | P14f's peer bound on the operator path kept; P13's migrate/noise-key commands kept. |
| `hscontrol/servertest/cluster_p05.go` | `NewTestClusterWith`, `seed`, `Platform`, `ClusterOptions.Netmap` | Test clusters create the schema with `NewHeadscaleDatabase` (single-tenant path), so P14f's servertests do not depend on `OpenPlatform` migrating. |
| `tools/capload/cluster.sh`, `ramp.sh`, `smoke-local.sh`, `README.md` | `config`, `migrate`, `register`, `start` | P14f knobs (`PG_MAX_OPEN` 100, `PG_MAX_IDLE`, `LEASE_NAME`, `LEASE_MAX_OPEN`, `LEASE_TTL`/`RENEW`/`ACQUIRE`/`MARGIN`, `INCREMENTAL=0` writes `enabled: false`). P13: `migrate` sub-command, `--init-noise-key` for the first active. `ramp.sh` (P14f F5 driver) made a fresh DB per step and went straight to `register`. |
| `infra/lab/e2e/n1/run.sh`, `cfg-n1.py`, `cases6.sh`, `cases7.sh` | | P13's migrate step (`cli c1 migrate`) and P14f's `--reconcile/--fullmap` both present after auto-merge; cases6/7 start no process. |
| `tools/pgtest/throwaway.sh` | `start`, `stop` | Throwaway PG + PgBouncer, trust auth, loopback only, no password. |

## Conflicts and resolution

| File | Resolution |
|---|---|
| `hscontrol/db/platform.go` | P13's version taken as a whole (`--theirs`): one lease-pool implementation, P13's. P14f's port dropped. |
| `hscontrol/db/lease_pool_test.go` | Deleted: `pooler_test.go` defines `TestLeaseStoreHasOwnPool` and `TestLeaseDatabase` (duplicate symbols otherwise). |
| `hscontrol/types/config.go` (auto-merged, duplicate fields) | P14f's second copy of `LeaseName`/`LeaseMaxOpenConnections` removed; keys stay `lease_name` / `lease_max_open_conns`. P14f's sentence "behind PgBouncer an empty name shares the data pool's server connections, so set an alias there" appended to P13's doc (`ae4f7e8`). P14f's `MaxOpenConnections` doc kept; its example changed 100 → 50 (see §Tests: `TestExamples` refuses an example equal to the default; the failure is already on `feat/P14f-int` `003d47c`). |
| `hscontrol/config/default.go` | Both: `MaxOpenConnections: 100`, `MaxIdleConnections: 10` (P14f) and `ConnectTimeoutSecs: 2` (P13); `LeaseMaxOpenConnections: 2`. gofmt alignment fixed. |
| `hscontrol/config/validate.go` | P13's `validatePostgres` kept; P14f's refusal of `max_open_conns < 1` (same reason, hint, docs link) moved to its top, so host-list checks still run when the pool size is wrong. |
| `tools/capload/cluster.sh` | Header lists both sides' knobs; body auto-merged (P14f config knobs + P13 `migrate` + `--init-noise-key`). |
| `tools/capload/ramp.sh` (no textual conflict) | Each F5 step now runs `cluster.sh migrate` between `config` and `register`: with P13d, `register` on the fresh step database would stop with "schema is behind this binary; run migrate". |
| `PLAN.md` | P12d row from P14f; P13 row from P13 (`Planned`) plus a note pointing to this branch and evidence. |
| Generated (`config_docs_generated.go`, `docs/ref/*`, `nix/options.generated.nix`) | Not hand-merged: regenerated on mc-ci with `tools/configgen`. Diff = only the `lease_name` and `max_open_conns` doc/example text. |
| Compat goldens (`hscontrol/config/testdata/compat/*.golden.json`) | `HEADSCALE_UPDATE_GOLDEN=1 go test ./hscontrol/config/ -run TestConfigCompat`: no diff (the goldens project only fields that are equal on both sides: `MaxOpenConnections` 100, idle 10). |

Not changed: migrations (none added, none reordered; both sides' chains
are identical below `hscontrol/db`, `git diff feat/P13-lease-attempt HEAD --
hscontrol/db internal` is only the deleted duplicate test), Noise model
(P13e as on its branch), `gen/`.

## Constraints

- One lease pool implementation (P13b). Default `lease_max_open_conns` 2,
  lease `connect_timeout` at most 1 s (lease attempt).
- `max_open_conns` default 100, idle 10, `< 1` refused (P14f F5).
- Incremental netmap on by default, `MaxPeerEntries` refuses policy writes
  past the bound (P14f R2b) — unchanged.
- `OpenPlatform` never migrates (P13d): every harness runs the migrate step
  on a fresh database.

## Assumptions / open points

- `p05cluster` servertests were only compiled (`go vet -tags p05cluster`),
  not run, on this branch. P13 ran them at `00138ae`/`e1a3519` (225/226 +
  fix) and P14f at its own tip; the combination is unverified.
- `ramp.sh` with the migrate step was not run (no lab).
- The lease attempt branch was "paused by the owner" with lease tests
  verified only (`evidence/P13-lease-attempt.md`).

## Tests (mc-ci, shared host: every command under `nice -n 19`, `go test -p 2`)

Throwaway PostgreSQL + PgBouncer from the branch's own
`tools/pgtest/throwaway.sh` (trust auth, loopback, no password), env file
sourced, `MESH_TEST_REQUIRE_POSTGRES=1`. Harness note: `$(nix shell … start
| tail -1)` never returns on mc-ci (nix's `crashpad_handler` keeps the pipe
open); the runner writes `start`'s output to a file and reads the last line.
No lab VM, no PgBouncer/DB of the lab, no `p05cluster` shard, no
integration suite.

Packages: `./hscontrol/db/... ./hscontrol/config/... ./hscontrol/types/...
./internal/lease/... ./internal/placement/... ./hscontrol/state/...
./hscontrol/mapper/... ./cmd/headscale/...`

| Command | Commit | Exit | Result |
|---|---|---|---|
| `go build ./...` | `ae4f7e8`, `ba031ac` | 0 | |
| `go vet ./...`; `go vet -tags p05cluster ./hscontrol/servertest/`; `go vet -tags benchpprof,benchdb ./...` | `ae4f7e8` | 0, 0, 0 | p05cluster servertests compile (not run) |
| `go run ../../tools/configgen table . config_docs_generated.go && go run ../../tools/configgen/files ../..` (in `hscontrol/types`) | `ae4f7e8`, `78e5073` | 0 | diff: `lease_name` and `max_open_conns` doc/example only |
| `HEADSCALE_UPDATE_GOLDEN=1 go test ./hscontrol/config/ -run TestConfigCompat` | `ae4f7e8`, `cd077d7` | 0 | no golden diff |
| `go test -count=1 -p 2 -json <packages>` | `08d1435` | 1 | 4713 pass, 33 skip, **3 fail**: `hscontrol/config` `TestExamples`, `TestExamples/database.postgres.max_open_conns`; `cmd/headscale/cli` `TestGenerateConfigRunningRoundTrip` — all "example equals the default" for `max_open_conns`. Same failure on `feat/P14f-int` `003d47c` alone (`go test ./hscontrol/config/ -run TestExamples`: FAIL), i.e. from P14f `0b8bff0`, not from the merge. Fixed in `78e5073` + `cd077d7`. |
| `go test -count=1 -p 2 ./hscontrol/config/... ./cmd/headscale/...` | `cd077d7` | 0 | |
| `go test -race -count=1 -p 2 -json <packages>` | `cd077d7` | 0 | 14/14 packages ok; 4714 pass, 34 skip, 0 fail |
| `golangci-lint run` (nix `nixpkgs#golangci-lint`) | `cd077d7` | 1 | 9 issues, all in the lease attempt's new tests (`internal/lease/attempt_test.go`, `blackhole_pg_test.go`): contextcheck 1, noctx 3, noinlineerr 3, unparam 1, wsl 1 (that branch was paused before lint ran) |
| `golangci-lint run` | `dfd4219` | 1 | 2 × wsl in `blackhole_pg_test.go` |
| `golangci-lint run` | `ba031ac` | 0 | 0 issues |
| `go test -race -count=1 -p 2 ./internal/lease/...` (with PG) | `ba031ac` | 0 | 26 pass, 0 fail; `TestRenewSkipsBlackholedPgBouncer` PASS |

Skips are the existing designed ones (P13d helper process, `TestRoundTrip`
cases with a fatal config, `TestNodeNameReplayCompat` N11–N13 pdb compat
notes, `TestHeadscaleProcess`; under race also
`TestScale1000_AllToAll_FullPipeline`). No PostgreSQL test skipped: the
P13b/P13c/P13d tests (`TestOpenPlatform*`, `TestLeaseStoreHasOwnPool`,
`TestLeaseDatabase`, `TestMigrateTwoProcessesAtOnce`, `TestEpochFence*`,
`TestLeaseFence*`) and `TestRenewSkipsBlackholedPgBouncer` ran and passed.

## Negative tenant isolation

No isolation code changed by the merge beyond what each side already had:
`hscontrol/db` and `internal/` equal `feat/P13-lease-attempt` (plus lint-only
test edits); `hscontrol/state`, `mapper`, `policy` equal `feat/P14f-int` plus
P13f's metric-label and reload-retry changes (auto-merged). The fence and
isolation tests in the packages above pass (`TestFence*`, `TestLeaseFence*`,
`TestFenceOtherTenantUnaffected`, tenantscope). The `p05cluster` cross-tenant
servertests were compiled, not run: **not verified on this branch**.

## Open problems

1. `p05cluster` shard (`infra/lab/scripts/shard-test.sh`) not run on the
   combined branch — needed before P13 can claim `Verified` here.
2. `feat/P14f-int` itself still fails `TestExamples` and
   `TestGenerateConfigRunningRoundTrip` (example = default 100); the fix here
   (`78e5073`, `cd077d7`) can be cherry-picked there.
3. `tools/capload/ramp.sh` now runs the migrate step per F5 step; not run.
   If P14f's F5 capacity measurements continue on `feat/P14f-int` binaries,
   nothing changes for them; this only matters once P13 is on the binary.
4. The three paused drill/infra branches still need merging when P13 drills
   resume (each one commit on `0f5f363`).
