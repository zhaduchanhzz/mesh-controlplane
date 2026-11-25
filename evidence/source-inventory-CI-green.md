# Source inventory and evidence: CI green on master (gate N0)

Branch `feat/CI-green` from `master` `fc339e8`. Goal: the GitHub Actions
workflow `.github/workflows/ci.yml` passes on master without changing
behaviour.

## Starting point

- Run 45660843453 on `fc339e8`: `lint` failed with 271 issues;
  `servertest (rest)` failed only `TestPolicyChanges/multiple_policy_changes`;
  every other job passed.
- Lint is the pinned golangci-lint v2.15.0 from `flake.lock`
  (`nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run`),
  configured by `.golangci.yaml` (unchanged by this branch).

## Paths and symbols read

- `.github/workflows/ci.yml` (job commands), `.golangci.yaml`.
- `hscontrol/servertest/policy_test.go` `TestPolicyChanges`;
  `hscontrol/policy/v2` `PolicyManager.SetPolicy` (returns `changed=false`
  for a policy that compiles to the same result); `servertest.TestHarness`
  `ChangePolicy`, `TestClient.UpdateCount`, `WaitForCondition`.
- Every file named by a lint finding (79 files, listed by `git diff --stat
  fc339e8..feat/CI-green`).

## TestPolicyChanges/multiple_policy_changes: test bug

Both rounds applied the same policy bytes. `SetPolicy` reports no change
for an identical policy, so nothing is sent; round 1 passed only when an
unrelated update happened to reach the client within the 10 s wait, which
a loaded CI runner does not provide. Not a product bug and not CPU
starvation.

Fix (commit `7ca5e74`): two different policies, each required to be a
change (`changed == true`) and to reach the client; then an explicit
assertion that re-applying the same bytes is not a change. On mc-ci:
`go test -count=3 -run '^TestPolicyChanges$' ./hscontrol/servertest/` ok.

## Lint: 271 → 0

1. `golangci-lint run --fix` (gci, tagalign, wsl, whitespace, modernize,
   perfsprint, nlreturn, dupword, testifylint), plus `err :=` → `err =`
   where the noinlineerr fix redeclared `err` (commit `aa8dcd7`).
2. By hand (this commit):
   - noinlineerr (67): `if x, err := f(); cond {` split into an assignment
     and an `if`; `err :=` became `err =` only where `err` already existed
     in the same block (all in tests, plus `tools/capload`; in each case
     the earlier `err` had already been checked).
   - err113 (46): package-level sentinels, wrapped with `%w`. Messages keep
     their text, except the lease config errors, now prefixed
     `invalid lease config: ...` (no test or caller matches the old text).
   - thelper, prealloc, errchkjson, noctx, bodyclose, forcetypeassert,
     testifylint, unparam, unused, ineffassign, staticcheck (SA4000 on a
     stateful call written twice), godoclint (a doc comment of
     `createAndSaveNewNode` sat above `EnrollGate`), goconst
     (`dialectPostgres`, `colUpdatedAt`, `useCreate`), noctx
     (`listenAdminSocket` takes the caller's ctx), zerologlint
     (`log.WithLevel`), nonamedreturns (removed where not needed).
   - `capacity_main_test.go`: writes through an `os.Root` on the output
     directory, so no sample/profile/metrics path can leave it.
   - `netmap_golden_test.go` `waitIdle`: `require.Eventually` instead of a
     sleep loop.
   - `capload` hold phase: waits on a timer or the run's context.
3. Per-line `//nolint` with a reason (41 lines, no linter disabled, no
   config change). Grouped:
   - contextcheck (10): `Platform` methods bound their own lookups
     (`lookupContext`, 10 s); cluster runtimes live on the serve context,
     not the lease callback's; shutdown and lease release run after ctx is
     done and bound themselves. Same convention as the 8 existing
     `nolint:contextcheck` lines.
   - forbidigo `time.Sleep` (10, tests): the elapsed time is the subject
     (a lease must expire, a transaction must stay open, writers must reach
     load, a refresh interval must pass, an event must *not* happen).
   - gosec (10): G101 false positives on fake tenant credentials that the
     tests prove are refused, a credential-kind name and a table name; G115
     on values bounded just above; G703 on the operator-chosen output
     directory (MkdirAll/OpenRoot only).
   - gocyclo (4): `config.Validate`, `PolicyManager.IncrementalPeerMap`
     (hot path), `NodeStore.applyBatch`, `TenantDNS.Validate`. Splitting
     them is a refactor with behaviour risk, out of scope for a CI fix.
   - exhaustive (5): switches whose `default` is the intended branch
     (fail-closed refusal for unknown bootstrap outcomes, reflect kinds).
   - nonamedreturns (1): `recover` must set the result.

Behaviour changes: none intended. Two observable details: lease config
error text (above); `bootstrap` answers 503 instead of a 200 with an empty
body if encoding the success body ever failed (it cannot for these types).

## Verification

See the PR / report for the exact commands, exit codes and CI run URL.

## Still assumed

- GitHub runners behave like mc-ci for the timing-sensitive tests that
  passed there.
