# P02 E2E — one Noise key per tailnet, tailnet chosen by Host

Real-client harness for `tickets/P02.md`. The tests assert the **target** P02
behaviour. Against a single-tailnet server most of them FAIL. That is the
expected RED state, recorded in `evidence/P02-e2e-red-baseline.md`.

## Run

```bash
# From Git Bash, WSL or Linux, at the repo root.
bash infra/lab/e2e/p02/run.sh                 # all tests (~40 s)
bash infra/lab/e2e/p02/run.sh --fast          # HTTP/TLS only, K01-K09 (~4 s)
ONLY="K01 K05 R02" bash infra/lab/e2e/p02/run.sh
JOBS=1 bash infra/lab/e2e/p02/run.sh          # one test at a time
```

Lab invocation with the P02 POC (one admin CLI per tailnet):

```bash
HS='sudo -u headscale /usr/local/bin/headscale'
HS_A="$HS -c /etc/headscale/tailnets/a.yaml" HS_B="$HS -c /etc/headscale/tailnets/b.yaml"   HEADSCALE="$HS -c /etc/headscale/tailnets/hs.yaml" bash infra/lab/e2e/p02/run.sh [--fast]
```

Exit code: `0` = no FAIL, `1` = at least one FAIL, `2` = harness/setup error,
interrupt, or a cleanup that could not prove it removed everything it created.
Each result line is `PASS|FAIL|SKIP <id> <mapping> <seconds> <reason>`, printed as
tests finish. The summary at the end always uses the same order.

Artifacts go to `artifacts/<gitsha>/P02/e2e-<UTC>/` (gitignored):

- `results.tsv`: `id, mapping, status, reason, duration_s`, in fixed order.
- `results.json`: the same rows plus `run_id`, `repo`, `mode`, `only`, `jobs`,
  `wall_time_s` (whole run including cleanup), `tests_wall_time_s`, `exit_code`,
  `cleanup_ok` and PASS/FAIL/SKIP counts.
- `logs/<slot>-<id>.log`: one redacted log per test. Also redacted client logs
  (`p02-*.cli.log`, `*.tailscaled.log`, `*.noise-dials.log`, `K10-*.log`),
  `env.txt` and `keys.txt`.

### Modes and speed

- `--fast` runs only K01–K09 (curl against `/key` and `/ts2021`). It does not
  touch `mc-ci` and creates nothing on `mc-cp`; it reads only the server version
  for `env.txt`. `ONLY=` filters within the mode.
- Tests run concurrently, at most `JOBS` at a time (default 4). The two `/key`
  fetches that K01/K04/K09 need, the node snapshot used for cleanup, and the
  image check run concurrently during setup. Every client scenario (R01,
  R02/`v1.84.3`, R02/`v1.104.7` + N8, R03) has its own containers, volumes,
  hostnames, nodes and keys (`p02-r01-a|b`, `p02-r02-<tag>`, `p02-r03`), so the
  scenarios share no state. K10 probes A and B at once.
- Images are checked once per run and pulled only if missing.
- Readiness waits poll up to a timeout instead of sleeping a fixed time
  (container start, state after `switch`, state after `docker restart`). The
  only fixed wait left is R01's 5 s observation window. R01 is a negative test,
  since a leaked peer would arrive asynchronously. The window ends early only
  if a leak is seen.
- SSH: the harness opens one ControlMaster per host in a private temp dir
  (removed on exit) and probes it first. Git Bash's msys OpenSSH starts a
  master, but its sessions fail and fall back to new connections, so there the
  probe turns multiplexing off and plain connections are used (~0.35 s each on
  the LAN). On WSL/Linux it is used.

Timings on 2026-11-22 against the live endpoint (Git Bash, `JOBS=4`): the full
suite went from 98 s (sequential) to 40 s, and `--fast` takes 3–4 s. See
`evidence/source-inventory-P02.md` §7.

| Variable | Default | Meaning |
|---|---|---|
| `SSH_KEY` | `~/.mesh-lab/mesh-lab` | SSH key for `mc-cp` and `mc-ci` |
| `CP_SSH` / `CI_SSH` | `mesh@192.168.1.241` / `mesh@192.168.1.240` | control plane / Docker host |
| `HOST_A`, `HOST_B`, `HOST_UNKNOWN` | `a.`, `b.`, `zz.cp.gta5web.online` | tailnet hostnames and an unknown one |
| `PORT`, `PUBLIC_IP` | `8443`, `116.118.3.46` | public endpoint (VPS forwarder) |
| `IMAGE_NEW`, `IMAGE_MIN` | `v1.104.7`, `v1.84.3` | `tailscale/tailscale` tags (latest pinned and minimum capver 116) |
| `HEADSCALE`, `HS_A`, `HS_B` | `sudo -u headscale /usr/local/bin/headscale` | admin CLI (server version) and the CLI of tailnet A / B. Node and key IDs are tagged `A:<id>` / `B:<id>` |
| `KEYCMD_A`, `KEYCMD_B` | `$HS_A` / `$HS_B preauthkeys create --user lab-test --expiration 30m -o json` | mint a key for tailnet A / B |
| `ONLY` | empty | space-separated test IDs |
| `JOBS` | `4` | max tests running at once |
| `KEEP` | `0` | `1` skips cleanup (debug only; the temp dir then still holds keys) |

## What it touches

- `mc-cp`: read-only, except that it creates pre-auth keys (≤30 min) and
  deletes the nodes and keys this run created. Nodes are deleted only if they did
  not exist before the run **and** are named `p02-*`, per tailnet via the
  `A:`/`B:` tagged IDs (one SSH call per tailnet, IDs validated as `[AB]:<digits>`).
- `mc-ci`: containers `p02-*` (and K10's `--rm` containers) with label
  `p02-e2e=<run id>` and volumes `p02-*`, all removed on exit. `ts-home1` and
  other containers are never touched.
- Pre-auth keys sit in a mode-700 temp dir (files mode 600). They are piped to
  `tailscale up|login --auth-key=file:/dev/stdin` and never printed. CLI and
  tailscaled logs, and every per-test log, pass through `redact()`, which removes
  `tskey-`/`hskey-` tokens and `/register/<id>` URLs and shortens public keys.
- Cleanup runs on normal exit, on FAIL, and on Ctrl-C/TERM. It first kills
  every running test (each runs in its own process group). Then it removes
  containers, volumes, new `p02-*` nodes and the run's keys (expire, then
  delete), and **verifies** that none of them is left: no labelled container,
  none of the run's volumes, no new `p02-*` node, none of the run's key IDs in
  `preauthkeys list`. Stragglers (for example a `docker run` still in flight at
  the interrupt) are retried for up to 90 s, after which the run exits `2`.
  Known limit: if an interrupt cuts the SSH reply of a `preauthkeys create`, the
  harness never learns that key's ID. Such a key is unused and expires within
  30 min.

## Tests

| ID | Mapping | Check (target behaviour) |
|---|---|---|
| K01 | S11, N2 (precondition), I01 | `/key?v=116` on Host A and Host B returns two **different** keys |
| K02 | S11, N3 | `/key` with unknown Host `zz.…` → refused, no default key |
| K03 | S11, N3 | `/key` with a raw-IP Host (IP URL without SNI, and SNI A + `Host: <ip>`) → refused |
| K04 | N4 | `Host: a…:8443`, `a…`, `A…` (upper case), `a….` (trailing dot) all give A's key. SKIP while A==B (the check cannot tell them apart) |
| K05 | S11, N1 | `/key` with SNI A + Host B (and the reverse) → refused |
| K06 | S11, N1 | `POST /ts2021` upgrade with SNI A + Host B → refused before `101` |
| K07 | S11, N3 | `/ts2021` upgrade with unknown Host / raw IP → refused before `101` |
| K08 | S11, N9 | WebSocket `/ts2021` with SNI A + Host B → refused before `101` |
| K09 | S11 | `X-Tailnet-ID`, `X-Forwarded-Host`, `Forwarded: host=` set to B on Host A do not change the key. SKIP while A==B |
| K10 | P02 POC step 1 | Positive control: `tailscale debug ts2021 --host <h>:8443` (v1.104.7) completes a Noise handshake on A and B. It also records whether port 80 was tried first |
| K10m | P02 POC step 1 | SKIP: the v1.84.3 `debug ts2021` cannot use `host:port`. v1.84 handshakes are covered by R02 |
| K11 | S11, N2 | SKIP: Host B + an initiation encrypted to A's key needs a custom Noise client |
| N5, N7 | N5, N7, I01 | SKIP: need a custom Noise client (inner `:authority`, foreign node key on a map request) |
| N10 | N10 | SKIP: port 80 of the public IP is not the control plane |
| S10w | S10 | SKIP: no OIDC/web approval on the lab server |
| R01 | S11, N7, I01 | Client registered via A and client registered via B do not see each other in `tailscale status` |
| R02/`<tag>` | S05, I03 | One device, both images: `up` via A, `login` via B, `switch` back to A, `docker restart`. Expect 2 local profiles, 2 node keys, **2 server nodes with the same machine key**, A resumes without re-auth (same node key) after the switch and after the restart, and Noise dials go to both hosts |
| N8 | N8 | Part of R02 (latest image): deleting A's node leaves B's node. SKIP when the server has only one node |
| R03 | S10, N6 | Pre-auth key minted for A, used at B → registration refused and no node created |

The K06/K07/K08 probes send a bogus handshake (`AAAA`). A server that serves the
tailnet answers `101` and then fails the Noise handshake, so a `101` already shows
that the server did not refuse the Host/SNI before the handshake. "Refused" means
HTTP ≥400, or no HTTP answer at all (`000`, for example a TLS alert on an unknown
SNI). K01 first proves that the path works.

## Limits

- No test here proves Noise/tenant binding with a forged initiation (N2, N5, N7).
  That needs a small custom client. Go race/unit tests do not replace it (AGENTS.md).
- The control plane sits behind `systemd-socket-proxyd` (TCP passthrough). SNI
  and Host reach the server unchanged, but the client source IP is always
  `10.99.77.1`.
- The client tries `http://<host>:80/ts2021` first even with `:8443` in the URL
  (K10, R02 logs). In this lab port 80 on the VPS is an unrelated HAProxy that
  returns an empty reply, so the client falls back to 8443.
