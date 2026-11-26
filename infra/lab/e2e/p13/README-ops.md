# P13 operational drills: F08, canary, Noise rotation, relay restart

Harnesses for the P13 drills that are not about PostgreSQL failover (F05 and kill PgBouncer live in `f05.sh` / `pgb-kill.sh` on another branch). **Written, not run yet** (lab busy 2026-11-26). Each first run is also the harness's own test: read §4 before it.

| Script | Ticket | Drill | Threshold (D12) |
|---|---|---|---|
| `f08.sh` | P13c, ADR 0012 | backup B → F01 c1 → move m c1→c2 → PITR to B → fence → recovery | 0 stale authority after fence; recovery ≤ 5 min after restore |
| `canary.sh` | P13d, ADR 0013 | c2: expand migrate (N+1) → standby N+1 → switchover → observe → both N+1 → rollback to N; concurrent migrate | every switchover ≤ 15 s; same minor only |
| `noise-rotate.sh` | P13e, ADR 0014, `docs/ops/noise-key-rotation.md` | c2: planned rotation (§2) and emergency (§3) | measured and reported (no threshold); unrecovered after `tailscaled` restart = stop condition |
| `relay-restart.sh` | P13g | derper n1-derp-c1: graceful restart, kill -9, VM reboot (opt-in) | no peer lost; ping gap ≤ `RELAY_GAP_MAX_MS` (proposal 20 s) |
| `lib-ops.sh` | — | shared helpers, N1 overlay | — |

## 1. How the drills run

- **canary, noise, relay run inside the N1 E2E harness** without changing it: `p13_overlay` (lib-ops.sh) copies `infra/lab/e2e/n1/` to `infra/lab/e2e/.p13-<drill>-<pid>/`, adds the drill file as `cases-p13.sh`, lists the drill's cases in `CASES_ALL`, calls the drill's `p13_cleanup` first in N1's cleanup, and writes artifacts to `artifacts/<commit>/P13/<drill>-<run>-<MODE>-<V>/`. N1 provides c1 (mc-cp + mc-c1b, transient HAProxy), c2 (mc-c2 + mc-c2b behind the long-lived `mesh-c2-ha`), the relay (regions 901/902), the client VMs and the full cleanup (P02 POC and long-lived c2 restarted). If N1's `run.sh` changes shape, the overlay refuses to start (anchors: `CASES_ALL="`, `  log "cleanup"`, the `ART_DIR` line).
- **f08 is standalone** (P11b/N1 patterns): its own binary, config, unit and password file names (`-p13f08`), the HA cluster mc-pg through both PgBouncers, its own transient HAProxy for c1.
- Same rules as N1/P12b: one harness on the lab at a time; start in the foreground of a background task; never edit a file of a running drill; `CI_BINDIR` (binaries built on mc-ci, fetched over the LAN, sha256 checked).

## 2. Commands

```sh
# relay (×2: V=1.104.7 and V=1.84.3); RELAY_REBOOT=1 adds the VM reboot case
CI_BINDIR=/home/mesh/<bin> MODE=off V=1.104.7 bash infra/lab/e2e/p13/relay-restart.sh

# Noise rotation (×2); containers always run both NOISE_IMAGES; NOISE_CHECKPOINTS="60 300 900 3600" for the 60-min point
CI_BINDIR=/home/mesh/<bin> MODE=off V=1.104.7 bash infra/lab/e2e/p13/noise-rotate.sh

# canary: build N+1 once (same commit, pushed to mcci, + one drill-only expand migration), then run (×2)
CI_BINDIR2=/home/mesh/<bin>-next bash infra/lab/e2e/p13/canary.sh build
CI_BINDIR=/home/mesh/<bin> CI_BINDIR2=/home/mesh/<bin>-next MODE=off V=1.104.7 bash infra/lab/e2e/p13/canary.sh

# F08 (×2); exits 2 without touching the lab when the infrastructure is missing (precondition.txt)
CI_BINDIR=/home/mesh/<bin> bash infra/lab/e2e/p13/f08.sh
```

`P13_CASES="t_e00 t_s01 t_r01"` (overlay drills) selects cases as N1's `CASES` does.

## 3. Infrastructure the drills need (state 2026-11-26)

| Need | Drill | State |
|---|---|---|
| pgBackRest stanza `mc-pg` (repo, `repo1-cipher-type`, passphrase outside Git), `archive_command = pgbackrest … archive-push`, `stanza-create` + `check` | F08 | **Missing**: package 2.60.1 installed on mc-pg-01/02, `/etc/pgbackrest.conf` has only `repo1-path`; `archive_command` is `/bin/true` (`patroni.yml.tmpl`) |
| PgBouncer on mc-pg-01/02 :6432 → local HAProxy :5000 → Patroni primary, with a route for `e2e_*` databases (catch-all `*` or an alias) | F08 | **Missing** (other P13b work item); nothing listens on 6432/5000 on the PG nodes |
| Role `headscale` on the HA cluster (password `MC_PG_HEADSCALE_PASSWORD`, `~/.mesh-lab/mc-pg.env`) with CREATEDB not needed (the drill creates the database as `postgres`) | F08 | Password present in the env file; role assumed created by P13a |
| `/usr/local/go/bin/go` and `/home/mesh/git/mesh.git` on mc-ci | canary build | Present |
| `/tmp/n1probe derp-check` | relay | As N1 S15 |

## 4. Assumptions to verify on the first run

- **F08**
  - `pgbackrest restore --pg1-path=<scratch> --type=name --target-action=promote --archive-mode=off` of a Patroni data directory starts as a standalone instance with the `-c` overrides used (port 5439, socket dir in the scratch dir, no `primary_conninfo`, no sync standby). Patroni-written `postgresql.conf`/`pg_hba.conf` live in the data directory (no `config_dir`).
  - `pg_restore --no-owner --role=headscale` recreates the schema, triggers and functions with `headscale` as owner.
  - `cluster migrate` and `cluster epoch-fence` work through PgBouncer in transaction mode (one transaction each). The ADR prefers a direct connection to the primary, but 5432 is closed to the cluster hosts.
  - Console log format: the lease and runtime lines carry `lease_epoch=N`, `epoch=N` and `cluster=…`. The audits parse these; a JSON log format would make the stale-authority check pass vacuously.
  - c2's pre-restore holder logs `cluster lease ended` after the fence (the holder name changed).
  - Recovery time includes the deliberate 40 s pre-fence window.
- **canary**
  - `go build` on mc-ci of a patched clone gives a binary whose `headscale version` shares major.minor with N. Pseudo-versions or `dev` pass, and the INFO row records that the binaries skip versioncheck.
  - The patch anchors hold: the end of `v01Migrations` and the end of the `migrationCompat` map.
  - `systemctl stop` (SIGTERM) of the active hands over within 15 s, including the graceful shutdown.
- **noise**
  - c2's run config has exactly one `  private_key_path:` line, the Noise key. The drill points it at a copy.
  - `noise-key install --from /dev/stdin` reads the piped key.
  - Recovery is detected when an old client sees a probe node enrolled after the rotation. A client stuck on the old key does not receive that peer.
  - Offline probe nodes from N01 stay in the tailnet during N02, which is harmless.
- **relay**
  - The N1 derper units are transient. They are restarted with N1's exact flags, and after a VM reboot the cert, key and secret are installed again from the run's copies.
  - `ping` on the VM reaches the peer's tailnet IP through `tailscale0`.
  - The derper journal names verify failures with "verify".
- **All overlay drills**
  - N1's `TMPD` key files (`key-c`, `vsecret-c1`) and helper functions keep their names.
