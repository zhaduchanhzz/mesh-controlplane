# P02 E2E — one Noise key per tailnet, tailnet chosen by Host

Real-client harness for `tickets/P02.md`. The tests assert the **target** P02
behaviour. Against a single-tailnet server most of them FAIL. That is the
expected RED state, recorded in `evidence/P02-e2e-red-baseline.md`.

## Run

```bash
# From Git Bash, WSL or Linux, at the repo root.
bash infra/lab/e2e/p02/run.sh                 # all tests
ONLY="K01 K05 R02" bash infra/lab/e2e/p02/run.sh
```

Exit code: `0` = no FAIL, `1` = at least one FAIL, `2` = harness/setup error.
Every line is `PASS|FAIL|SKIP <id> <mapping> <reason>`. Results also go to
`artifacts/<gitsha>/P02/e2e-<UTC>/results.tsv`, with redacted client logs.
`artifacts/` is gitignored.

| Variable | Default | Meaning |
|---|---|---|
| `SSH_KEY` | `~/.mesh-lab/mesh-lab` | SSH key for `mc-cp` and `mc-ci` |
| `CP_SSH` / `CI_SSH` | `mesh@192.168.1.241` / `mesh@192.168.1.240` | control plane / Docker host |
| `HOST_A`, `HOST_B`, `HOST_UNKNOWN` | `a.`, `b.`, `zz.cp.gta5web.online` | tailnet hostnames and an unknown one |
| `PORT`, `PUBLIC_IP` | `8443`, `116.118.3.46` | public endpoint (VPS forwarder) |
| `IMAGE_NEW`, `IMAGE_MIN` | `v1.104.7`, `v1.84.3` | `tailscale/tailscale` tags (latest pinned and minimum capver 116) |
| `KEYCMD_A`, `KEYCMD_B` | `headscale preauthkeys create --user lab-test --expiration 30m -o json` | mint a key for tailnet A / B. The baseline has one tailnet, so both use the same user. Point `KEYCMD_B` at tailnet B once it exists |
| `KEEP` | `0` | `1` skips cleanup (debug only; the temp dir then still holds keys) |

## What it touches

- `mc-cp`: read-only, except that it creates pre-auth keys (≤30 min) and
  deletes the nodes and keys this run created. Nodes are deleted only if they did
  not exist before the run **and** are named `p02-*`.
- `mc-ci`: containers `p02-*` with label `p02-e2e=<run id>` and volumes `p02-*`,
  all removed on exit. `ts-home1` and other containers are never touched.
- Pre-auth keys sit in a mode-700 temp dir (files mode 600). They are piped to
  `tailscale up|login --auth-key=file:/dev/stdin` and never printed. CLI and
  tailscaled logs pass through `redact()`, which removes `tskey-`/`hskey-` tokens and
  `/register/<id>` URLs and shortens public keys.

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
