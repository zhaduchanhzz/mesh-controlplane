# PR P14a.0 Evidence — harness tải cluster mode, `benchpprof`, microbench P14b

- **Trạng thái:** harness `Implemented`, chạy thử nhỏ trên `mc-ci` (`Verified` ở mức "đường đo chạy được và cho số", **không** phải số capacity); `benchpprof` `Verified` (test ở cả hai chế độ build, thử trên process thật); microbench P14b `Verified` (số `mc-ci`, **chỉ mang tính chỉ dẫn**). Chưa `Reviewed`. **Không** chạy benchmark lab, **không** bật `mc-bench`, không đụng `mc-db`.
- **Repo commit before / after:** `458697e` (`feat/P14-plan`) → tip `feat/P14a-capload` (§1). Số chạy thử và microbench ở §5–§6 chạy trên `e206681` (cây sạch, `git status --porcelain` rỗng).
- **Verified source paths & symbols:** `evidence/source-inventory-P14a0.md`.
- **Ticket / kế hoạch:** `tickets/P14.md` (P14a.0, P14b, Q7), `evidence/P14-benchmark-plan.md`.

## 1. Commit

| Commit | Nội dung |
|---|---|
| `b4a1516` | Source inventory (trước code) |
| `8dc2c89` | `benchpprof` (Q7): pprof trên listener metrics cluster, chỉ peer loopback; test hai chế độ build |
| `806031d` | `capseed -mode cluster` (T tailnet × M thiết bị, user, key, policy qua đường operator) và `-mode policy-flip`; shape `tag_heavy`, `mix` |
| `0aeda12` | `capload` cluster mode: `-plan`, observer + kiểm cô lập, churn, probe thay đổi → frame, probe policy, `-resolve`, manifest |
| `7ec1c8f` | Script: `cluster.sh` (+ `server.sh cluster-*`), `pgstack.sh`, `pgsample.sh`, `pgsnap.sh`, `summarize-cell.py`, `smoke-local.sh`, README |
| `c5a4abf` | Lint; `capseed` từ chối dòng register khi tailnet không có user |
| `e206681` | Microbench P14b: `BenchmarkP14bPerRecipientMap`, `BenchmarkP14bNodeStoreEndpointBatch`, `BenchmarkP14bFenceRoundTrips` (chỉ test; helper `openDBWith`, `benchDBOn`) |
| (commit này) | Evidence |

Code chạy thật đổi duy nhất: `hscontrol/cluster.go` (`MetricsHandler` bọc `withBenchPprof`, `Serve` gọi `logBenchPprof`) và hai file build-tag `hscontrol/pprof_bench.go` / `pprof_nobench.go`. Không migration, không đổi schema, không đổi API.

## 2. Harness làm được gì (hợp đồng)

| Thành phần | Chạy ở | Làm gì | Kết quả |
|---|---|---|---|
| `capseed -mode cluster` | host server (cùng config với cluster) | Cấp phát T tailnet (+ tùy chọn một tailnet lớn `-big`) trên cluster **đang giữ lease**, qua đúng đường operator của `headscale cluster tailnet …`: `placement.CreateTailnetOn` → `SetTailnetStatus(ACTIVE)` → `TenantDB` → `CreateUser` + `CreatePreAuthKey` (key có tag cho node tag của `tag_heavy`) → `policy.ValidatePolicy(…, WithEmptyPolicyDenies(true))` → `SetPolicy` → `BumpPolicyRevision`. **Không** ghi node | `plan.tsv` (0600: tailnet, phase `enroll`/`register`, hostname, pre-auth key), `tailnets.json` + `policies/<slug>.json` (không secret: shape, số user/tag/key, sha256 policy, thời gian cấp phát) |
| `capseed -mode policy-flip` | host server | Thêm/bớt grant thăm dò `autogroup:member → autogroup:self tcp:9999` trong policy của một tailnet, cùng kiểm tra + bump revision như CLI | 1 dòng trạng thái |
| `capload` | host tải | Đăng ký qua **Noise thật** bằng key của tailnet (`TailnetForAuthKey` → quota → enroll gate), hoặc kết nối lại node có sẵn (`-node-keys`, cột tailnet); một map stream mỗi node; các pha `enroll|connect`, `steady` (churn endpoint, `-churn-pct-per-min`), `probe` (đổi endpoint / danh sách service của Hostinfo, đo tới frame ở mọi observer đang giữ node đó), `register`, `policy` (`-policy-cmd`), `hold`; `-observe K` stream giải mã mọi frame, ghi dấu thời gian, **kiểm cô lập**; `-resolve host=ip` (DNS trong process) | `summary.json` (pha, p50/p95/p99/max register, first map, probe, policy, churn, cô lập, số theo tailnet, mốc pha), `nodes.csv`, `frames.csv`, `probes.csv`, `manifest-load.json`; exit 3 nếu có lỗi cô lập |
| `cluster.sh` (`server.sh cluster-*`) | host server, host standby | `config` (PostgreSQL qua PgBouncer, mật khẩu chỉ là tham chiếu `file:`), `register` (`--class`, `--max-devices`), `start active|standby` (chờ `/ready` 200/503 qua hostname cluster), `restart`, `stop`, `manifest` | `samples.csv` (RSS/CPU 1 s), `metrics.log` (scrape loopback 10 s), `metrics-final.txt`, `manifest-server.json` (sha256 binary, config, host) |
| `pgsample.sh` | host DB | `pg_stat_database` + `pg_stat_activity` mỗi 1 s (một kết nối giữ), PgBouncer `SHOW POOLS/STATS` mỗi 1 s; lúc dừng: top 30 câu theo `pg_stat_statements`, kích thước bảng | `pg.csv`, `pgb.csv`, `pgb-columns.csv`, `pg-final.txt` |
| `pgsnap.sh` | host DB | `save`/`restore` bằng `CREATE DATABASE … TEMPLATE` + `VACUUM ANALYZE`; chỉ tên khớp `PGSNAP_ALLOW` (mặc định `^bench_…`), không bao giờ `meshlab` | thời gian copy/analyze |
| `summarize-cell.py` | bất kỳ | Ghép một ô: mỗi pha của capload là một cửa sổ → CPU server (TB, đỉnh 10 s), RSS, delta metric NodeStore/mapper, PG commit/s, ghi dòng/s, WAL/s, backend, chờ khóa, PgBouncer `cl_waiting`/`maxwait` | `cell.json` (kèm cả hai manifest) + bảng markdown |
| `pgstack.sh`, `smoke-local.sh` | một host | PostgreSQL 16 + PgBouncer 1.24.1 tạm; chạy toàn bộ đường trên một host ở quy mô nhỏ, dọn sạch | |

Cờ `capload` mới: `-plan`, `-resolve`, `-save-node-keys`, `-observe` (mặc định 200), `-steady`, `-churn-pct-per-min` (mặc định 1), `-probes`, `-probe-interval` (10 s), `-probe-timeout` (30 s), `-policy-cmd`, `-policy-tailnet`, `-policy-changes` (3), `-policy-interval` (30 s), `-seed`, `-commit`, `-label`. Cờ P00c giữ nguyên (`-keys`, `-node-keys`, `-nodes`, `-register`, `-per-user`, `-concurrency`, `-timeout`, `-hold`, `-deadline`, `-decode-every`, `-out`); khác P00c: `-nodes 0` = mọi dòng của nguồn, và observer mặc định 200 (đặt `-observe 0` để giống P00c).

Thiết kế đáng chú ý:
- **Kiểm cô lập:** khóa node được sinh **trước** khi client nào chạy (`Persist.PrivateNodeKey` + auth key: client đăng ký đúng khóa đó), nên ánh xạ khóa → tailnet là bất biến và đầy đủ; mọi peer trong `Peers`/`PeersChanged` của frame đã giải mã phải thuộc tailnet của observer. `unknown_peers` > 0 cũng là dấu hiệu bất thường (một peer capload không tạo ra).
- **Probe Hostinfo** đổi **danh sách service**: peer chỉ nhận một phần Hostinfo (`types.PeerHostinfo`: hostname, OS, services, SSH host key, app connector); đổi `OSVersion` được lưu nhưng không gửi cho peer (lần chạy thử đầu: 59/59 observation "missed" vì lý do này — đã sửa).
- **Probe policy** tính từ lúc chạy lệnh (gồm cả CLI và vòng reconcile 2 s của cluster, `hscontrol/cluster.go:77`) tới frame đầu có packet filter; `other_tailnet_filter_frames` đếm frame có packet filter mà observer của tailnet **khác** nhận trong cửa sổ (phải = 0). Pha policy chờ 5 s trước lần đổi đầu để frame của pha register không bị tính nhầm (lần chạy thử đầu: 20 frame `PeersChanged` muộn của pha register bị đếm khi còn đếm mọi frame — đã đổi sang đếm frame có filter + chờ ổn định).
- **`-resolve`:** listener cluster chỉ trả lời hostname của `server_url` (P08), lab không có DNS cho tên benchmark; `capload` trỏ `dnscache.Get().Forward` và `net.DefaultResolver` tới một DNS nhỏ trong process (A record cho tên đã ánh xạ, NXDOMAIN cho tên khác). Không sửa `/etc/hosts` của host tải dùng chung.
- **User mới chỉ được nạp khi runtime nạp tailnet** (`ReloadPolicy` không nạp lại user): sau `capseed -mode cluster` phải khởi động lại active (standby dừng trong lúc đó, nếu không nó lấy lease), hoặc chạy ô từ snapshot DB.
- **Quota:** tenant trên cluster `shared` bị chặn ở `DefaultSharedTenantLimit = 5000`; ô một tailnet > 5k dùng cluster `dedicated` (`CLASS=dedicated`, `MAX_DEVICES`). `allow_all` > ~1.000 cần `MAX_PEER_ENTRIES=0`.

## 3. `benchpprof` (Q7)

- `hscontrol/pprof_bench.go` (`//go:build benchpprof`): `withBenchPprof` đặt **trước** `operatorOnly` trong `MetricsHandler`; đường `/debug/pprof/*` chỉ phục vụ khi peer TCP thật là loopback (kiểm trước khi `operatorOnly` đổi `RemoteAddr` của operator thành loopback), nên `metrics_operator_cidrs` đọc được `/metrics` nhưng **không** đọc được profile. Mutex/block profiling chỉ khi đặt `HEADSCALE_BENCH_MUTEX_FRACTION` / `HEADSCALE_BENCH_BLOCK_RATE`. Log cảnh báo lúc khởi động.
- `hscontrol/pprof_nobench.go` (`!benchpprof`): `withBenchPprof` trả nguyên handler → không có route pprof nào trong cluster mode.
- Listener public (`HTTPHandler`) không bao giờ có pprof, có tag hay không.
- `.goreleaser.yml` build không có tag; `TestBenchPprofNotInReleaseBuilds` ghim rằng `.goreleaser.yml`, `Makefile`, `flake.nix`, `Dockerfile.integration` không nhắc `benchpprof`.

## 4. Tests — lệnh chính xác và exit code

Môi trường: `mc-ci` (16 vCPU Xeon E5-2696 v3, 24 GiB, kernel 6.8.0-149), Go 1.27.4 linux/amd64, `GOFLAGS=-buildvcs=false`, worktree `~/src/p14a` detached ở `e206681`, `git status --porcelain` rỗng. Host dùng chung với agent khác (load average lúc chạy 2–19 theo `uptime`).

| Lệnh | Exit | Kết quả |
|---|---:|---|
| `go build ./...` | 0 | |
| `go vet ./...`; `go vet -tags benchpprof ./hscontrol/` | 0; 0 | |
| `go test -count=1 -v -run 'BenchPprof\|TestP08ClusterMetricsOperatorsOnly\|TestP08aPublicRouterHasNoDiagnosticRoutes\|TestP08ClusterHostGuard' ./hscontrol/` | 0 | 113 PASS, 0 FAIL (không tag: `/debug/pprof/*` 404 ở loopback, 403/404 nơi khác, public 404/421) |
| cùng lệnh với `-tags benchpprof` | 0 | 113 PASS, 0 FAIL (loopback 200; operator CIDR, LAN, CGNAT, ULA, Internet 403; public 404/421; `/metrics` không đổi) |
| `go test -count=1 ./tools/...` (gồm `TestInProcessResolver`, `TestShapesValidateAsTenantPolicies`, `TestTagHeavyDimensions`) | 0 | |
| `MESH_TEST_POSTGRES_DSN=<PostgreSQL 16 tạm, unix socket> go test -count=1 ./hscontrol/db/tenantscope/` | 0 | test fence/lease không đổi hành vi sau khi tách `openDBWith`/`benchDBOn` |
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --concurrency 4 ./tools/... ./hscontrol/ ./hscontrol/mapper/ ./hscontrol/state/ ./hscontrol/db/tenantscope/` | 0 | 0 issues (lần đầu 15 + 2 issue wsl/goconst/contextcheck/prealloc/omitzero/gosec → sửa ở `c5a4abf`, `e206681`) |
| cùng lệnh `--build-tags benchpprof ./hscontrol/` | 0 | 0 issues |

Không chạy `go test ./...` đầy đủ và không chạy `-race` toàn bộ: code chạy thật chỉ đổi ở `MetricsHandler` (đã có test riêng); phần còn lại là `tools/` và file `_test.go`.

## 5. Chạy thử trên `mc-ci` (quy mô nhỏ, không phải số capacity)

**Lệnh** (cây `e206681` sạch; binary build trên `mc-ci`, `CGO_ENABLED=0 -trimpath`; `headscale` với `-tags benchpprof`):

```sh
go build -tags benchpprof -o ~/p14a-final/bin/headscale ./cmd/headscale
go build -o ~/p14a-final/bin/capload ./tools/capload
go build -o ~/p14a-final/bin/capseed ./tools/capseed
# sha256: headscale dbb08d5f…241180d, capload e39d2c33…6824965, capseed abf9a93a…b6096
COMMIT=$(git rev-parse HEAD) STEADY=60s bash tools/capload/smoke-local.sh ~/p14a-final/bin ~/p14a-final/allow_all          # exit 0
POLICY=personal_devices TAILNETS=1 BIG=50 BIG_POLICY=tag_heavy COMMIT=… STEADY=60s \
  bash tools/capload/smoke-local.sh ~/p14a-final/bin ~/p14a-final/mixed                                                   # exit 0
```

Mỗi lần: PostgreSQL 16.16 + PgBouncer 1.24.1 tạm (`pgstack.sh`, transaction pool 20) trên `127.0.0.1:16432`; cluster `bench-c1` (`shared`, `max_devices` 2000) đăng ký, **active** (`127.0.0.1:18080`, metrics `127.0.0.1:19090`) và **standby** (`:18081`, `/ready` 503 suốt lần chạy) cùng Noise key, `server_url` `http://bench.capload.internal:18080` (capload `-resolve bench.capload.internal=127.0.0.1`); `capseed -mode cluster` → restart active → ô `enroll` → `pgsnap save/restore` → start → ô `reconnect` (`-node-keys` đã lưu). Server và tải **cùng host**, `GOMAXPROCS=4`: CPU/RSS dưới đây chỉ chứng minh sampler chạy.

| Ô | Tailnet (shape × thiết bị) | Pha → kết quả | Probe thay đổi → frame (p50 / p99, observation, missed) | Cô lập |
|---|---|---|---|---|
| allow_all / enroll | 2 × 50 `allow_all` (+ 2 × 10 register) | enroll 100/100 trong 14,0 s; steady 60 s churn 10/10; register 20/20 trong 3,8 s | endpoint 1.002 / 1.208 ms (59, 0); Hostinfo 997 / 999 ms (59, 0); policy 599 / 801 ms (40, 0), `other_tailnet_filter_frames` = 0 | 803 frame giải mã, 0 vi phạm, 0 peer lạ |
| allow_all / reconnect | như trên, từ snapshot | connect 120/120 trong 4,1 s | endpoint 996 / 997 ms (39, 0); Hostinfo 999 / 1.002 ms (39, 0) | 267 frame, 0 / 0 |
| mixed / enroll | `smoke-big` 50 `tag_heavy` (35 node tag, 2 user, 37 key) + `smoke-0000` 50 `personal_devices` (+ 20 register) | enroll 100/100 trong 14,0 s; churn 10/10; register 20/20 trong 3,9 s | endpoint 1.006 / 1.142 ms (34, 0); Hostinfo 995 / 998 ms (34, 0); policy (trên `tag_heavy`) 2.121 / 3.208 ms (40, 0), khác tailnet 0 | 600 frame, 0 / 0 |
| mixed / reconnect | như trên, từ snapshot | connect 120/120 trong 4,0 s | endpoint 873 / 1.001 ms (24, 0); Hostinfo 214 / 1.000 ms (18, 0) | 209 frame, 0 / 0 |

`summarize-cell.py` (trích ô allow_all/enroll; đủ trường trong `cell.json`):

| window | s | server cores TB/đỉnh 10 s | RSS đỉnh MB | PG commit/s | PG ghi dòng/s | PG backend max | PgB `cl_waiting` max | PgB `maxwait` s | snapshot build (TB ms) |
|---|---:|---|---:|---:|---:|---:|---:|---:|---|
| enroll | 14,0 | 0,13 / 0,15 | 84,4 | 84,9 | 22,1 | 12 | 0 | 0 | 34 (0,15) |
| steady | 60,1 | 0,00 / 0,01 | 85,0 | 6,5 | 1,3 | 12 | 0 | 0 | 23 (0,15) |
| probe | 21,2 | 0,01 / 0,01 | 85,0 | 5,3 | 0,5 | 12 | 0 | 0 | 17 (0,20) |
| register | 3,8 | 0,17 / 0,17 | 95,6 | 41,5 | 11,3 | 12 | 0 | 0 | 15 (0,25) |
| policy | 11,4 | 0,02 / 0,02 | 96,8 | 17,6 | 2,6 | 12 | 0 | 0 | 4 (0,35) |
| hold | 10,0 | 0,00 / 0,00 | 98,1 | 5,1 | 0,3 | 12 | 0 | 0 | 1 (0,37) |

Kiểm thêm trong cùng lần chạy:
- `benchpprof` trên process thật: `GET 127.0.0.1:19090/debug/pprof/` → 200, CPU profile 1 s tải được; listener public `/debug/pprof/` (đúng hostname) → **404**.
- `pgsnap.sh`: save 8,8 MB, restore (copy 0,36–0,71 s + `VACUUM ANALYZE` 0,32–0,45 s).
- Probe ~1 s ≈ tick batch 800 ms (`hscontrol/config/default.go:84`) + xử lý; probe policy gồm CLI (`capseed -mode policy-flip`, ~0,3 s) + poll reconcile 2 s.
- Manifest: `manifest-load.json` (commit `e2066815…`, sha256 capload, host `mc-ci` 16 CPU, `max open files` 65536, `ip_local_port_range` 32768–60999), `manifest-server.json` (sha256 headscale, config — dòng `pass` chỉ là `file:…`, host).
- Dọn dẹp: container còn 0; file khóa (`plan.tsv`, `nodes.keys`, `pg.pass`, `pgpass`, `noise_private.key`) còn 0; `grep` mẫu `hskey-…`, `privkey:`, `nodekey:`/`mkey:` 64 hex trên toàn bộ kết quả: không khớp (exit 1). `server-errors.txt` rỗng.
- Artifact: `artifacts/e206681/P14a0/p14a0-smoke.tgz` (laptop, thư mục bị ignore), sha256 `bd0e4acc3d0397b0a5f08bc96a81c4e8ccb4158df8391869fe6853bb55527606`.

Lỗi gặp khi chạy thử và đã sửa trước commit: `cluster register` đòi endpoint `https` (truyền `--endpoint https://<host>`); probe Hostinfo đổi `OSVersion` không tới peer (đổi sang danh sách service); đếm "frame khác tailnet" bắt nhầm `PeersChanged` muộn của pha register (đổi sang frame có filter + chờ 5 s); log debug của thư viện policy trong `capseed` (đặt mức warn).

## 6. Microbench P14b — baseline (**mc-ci, shared, indicative only**)

**Đọc trước khi dùng:** `mc-ci` dùng chung (agent khác build/test song song; load average 18,9 lúc bắt đầu, 5,1 lúc kết thúc), `-cpu 4`, từng package một (`-p 1`), `-count 6`, số trong bảng là **trung vị của 6 mẫu**; cột "spread" = (max − min)/trung vị của ns/op. Chỉ dùng để so trong cùng lần chạy và làm số "before" thô cho P14b; số before/after kết luận phải chạy xen kẽ trên cùng máy (P14b quy tắc). Không có `benchstat` trên `mc-ci`; trung vị tính bằng script nhỏ (trong artifact).

Lệnh (cây `e206681` sạch; PostgreSQL 16.15 tạm, `initdb` + unix socket + trust như `.github/workflows/ci.yml`, xóa sau khi chạy):

```sh
MESH_TEST_POSTGRES_DSN="host=<tmp>/sock port=5432 user=ci dbname=postgres sslmode=disable" \
go test -p 1 -cpu 4 -run '^$' -bench BenchmarkP14bFenceRoundTrips -benchmem -count 6 ./hscontrol/db/tenantscope/   # exit 0, 72 s
go test -p 1 -cpu 4 -run '^$' -bench BenchmarkP14bPerRecipientMap -benchmem -count 6 ./hscontrol/mapper/              # exit 0, 1081 s
HEADSCALE_CAPACITY_NODES=1000,5000,10000 \
go test -p 1 -cpu 4 -run '^$' -bench BenchmarkP14bNodeStoreEndpointBatch -benchmem -count 6 ./hscontrol/state/       # exit 0, 156 s
```

Raw: `artifacts/e206681/P14a0/p14a0-bench.tgz` (laptop), sha256 `ad73e60565cc1ef3122639e9ce60231d6da2705832d5b1d187d7771ad6426eb8`.

**#4 — câu lệnh mỗi ghi có fence** (`BenchmarkP14bFenceRoundTrips`, PostgreSQL 16 qua unix socket, `synchronous_commit` mặc định; `stmts/op` đếm ở driver, gồm BEGIN/COMMIT):

| Ghi | nofence: stmts/op, ms/op | fence (owner + bound): stmts/op, ms/op | fence + lease: stmts/op, ms/op |
|---|---|---|---|
| update 1 dòng | 3, 1,84 (spread 49 %) | 5, 2,08 (41 %) | 6, 1,69 (10 %) |
| create 1 dòng | 3, 1,98 (35 %) | 5, 2,46 (44 %) | 6, 1,74 (23 %) |
| transaction 2 update | 4, 1,67 (24 %) | 8, 2,33 (68 %) | **10**, 2,14 (11 %) |

`stmts/op` là số đo ổn định: mỗi câu ghi tenant thêm 2 câu (fence; bound transaction) và 3 câu khi có lease (+ đọc lease), như ticket đọc từ code (2 ghi = 10). Thời gian/ghi ở đây bị fsync COMMIT (~1,5 ms) và tải nền chi phối, chênh lệch giữa các chế độ nằm trong nhiễu: chi phí round trip phải đo qua PgBouncer + mạng (P14d), không ở socket cục bộ.

**#1, #6, #2 — một người nhận trong tailnet allow-all** (`BenchmarkP14bPerRecipientMap`, State thật trên SQLite, 10 node/user, node giữa danh sách):

| n | `ListPeers` (#6) | full map: build | + JSON | + JSON + zstd (#1) | JSON / wire | policy change + JSON + zstd (#2) |
|---:|---:|---:|---:|---:|---:|---:|
| 1.000 | 22,9 µs (8 KB, 1 alloc) | 2,45 ms (15 %) | 6,46 ms | **10,0 ms** (6 %; 3,8 MB, 21k alloc) | 421 KB / 91,7 KB | 9,66 ms |
| 2.500 | 62,0 µs | 9,07 ms (9 %) | 19,2 ms | **28,3 ms** (11 %; 7,7 MB) | 1.060 KB / 230 KB | 27,6 ms |
| 5.000 | 109 µs | 15,8 ms (50 %) | 41,8 ms | **52,8 ms** (11 %; 14,9 MB, 106k alloc) | 2.131 KB / 460 KB | 63,7 ms (46 %) |

Đọc (tỉ số của số đo; **ngoại suy** nếu nhân lên): chi phí mỗi người nhận tuyến tính theo n (10–11 µs/peer ở cả ba cỡ); trong tổng: build ≈ 25–30 %, JSON ≈ 35–50 %, zstd ≈ 20–35 %; zstd giảm byte ~4,6×. Hiện **không chia sẻ gì giữa người nhận**: một full map / policy change tới mọi node của tailnet 5k ≈ 5.000 × 53–64 ms ≈ 260–320 CPU-giây (**ngoại suy**, n lần số đo một người nhận) — khớp hướng với P00c (allow_all 10k không đạt) và là lý do ứng viên #1/#2/#3 đứng đầu. `ListPeers` (#6) ở 5k: 109 µs × 5.000 người nhận ≈ 0,55 CPU-giây cho **mỗi** patch broadcast (ngoại suy).

**#7 — một batch NodeStore chỉ đổi endpoint** (`BenchmarkP14bNodeStoreEndpointBatch`, writer thật, batch size 1, `peersFunc` production):

| n | allow_all | isolated | personal_devices | B/op, alloc/op |
|---:|---:|---:|---:|---|
| 1.000 | 4,05 ms (24 %) | 2,19 ms | 2,14 ms | 1,69 MB, 4,6k |
| 5.000 | 13,0 ms (168 %) | 12,8 ms | 11,8 ms (6 %) | 8,0 MB, 22,7k |
| 10.000 | 26,0 ms (38 %) | 28,1 ms | 28,3 ms (6 %) | 16,0 MB, 45,3k |

Đọc: chi phí không phụ thuộc policy (adjacency được giữ lại) và tuyến tính theo n — chính là bản sao mọi node + dựng lại index của `applyBatch`. Ở 10k: ~28 ms và 16 MB rác mỗi batch; writer NodeStore tuần tự nên trần ≈ 35 batch/s mỗi tailnet (**ngoại suy** 1/28 ms, chưa tính gộp batch). Churn 1 %/phút ở 10k (100 đổi/phút) chỉ chiếm ~5 % một core nếu mỗi đổi một batch, nhưng một bão reconnect/endpoint vài nghìn đổi/s sẽ xếp hàng — đo ở P14c.

## 7. Lần đo P14a thật cần gì (chưa làm; cần cửa sổ lab độc quyền)

**VM (Q2 đã duyệt: đo cả hai cỡ):**

| Vai trò | Cỡ | VM đề xuất | Ghi chú |
|---|---|---|---|
| Active, cỡ dedicated | 8 vCPU / 24 GiB | `mc-bench` (7703) | bật bằng plan đã lưu, chỉ update trong pool `meshcontrol-lab` (AGENTS 3), sau khi tắt VM chỉ dùng cho E2E (quyết định RAM 2026-11-26) |
| Active, cỡ shared | 4 vCPU / 8 GiB | `mc-bench` đổi cỡ (plan update: cores/memory, VM phải khởi động lại) **hoặc** VM cluster lab 4/6–4/8 | quota ghi theo cỡ VM; cần người chọn |
| Standby | ≥ 4/6 | `mc-c1b` (7706) | chạy `cluster.sh start … standby`; noise key chép từ active (cùng một key cluster, ADR 0005) |
| PostgreSQL + PgBouncer | 4 vCPU / 8 GiB | `mc-db` (7704), DB `bench_*` riêng | PgBouncer đã có `* = host=mc-pg` nên DB `bench_*` đi qua được; `meshlab` không bao giờ bị chạm (`pgsnap.sh` từ chối) |
| Tải | 16 vCPU | `mc-ci` (7701) | `ip_local_port_range` = 32768–60999 (≈ 28.200 cổng tới một đích): ô > ~28k stream cần IP nguồn thứ hai hoặc nới sysctl — **cần người duyệt** vì `mc-ci` dùng chung |

**Việc chuẩn bị còn thiếu (theo thứ tự):**
1. Driver điều phối từ máy operator (`run-cluster-cell.sh`: khóa độc quyền, `pgsnap restore` → `pgsample start` → `cluster.sh start standby`/`active` → `capload` → `stop` → chép artifact về, kiểm redact `nodekey:`/`mkey:`/`privkey:`/`hskey-`) — chưa viết trong PR này (§8); ước 0,5–1 ngày, ghép từ các lệnh đã chạy thử ở §5.
2. `mc-db`: `stats_users` cho PgBouncer (`SHOW POOLS/STATS`) và `pg_stat_statements` (`shared_preload_libraries`, **khởi động lại** `mc-pg`) — thay đổi hạ tầng dùng chung với E2E/P13, cần chủ dự án duyệt và hẹn giờ. Không có thì `pgsample.sh` vẫn chạy phần `pg_stat_database`/`pg_stat_activity`, thiếu top câu và số liệu PgBouncer.
3. Binary: build trên `mc-ci` từ cây sạch, `-tags benchpprof` (đường code giống release khi không gọi pprof; mutex/block tắt), sha256 trong manifest; chép qua LAN có kiểm sha256 (như `CI_BINDIR` của N1).
4. Hostname benchmark (vd. `bench.lab.internal`) trong `server_url`; `capload -resolve bench.lab.internal=192.168.1.242`.
5. Seed: mỗi (shape, N) một snapshot `bench_seed_<shape>_<N>`: `capseed -mode cluster -extra 500` → restart active → `capload -plan … -save-node-keys` → dừng → `pgsnap save`. Ô sau đó: `pgsnap restore` + `capload -node-keys … -plan … -register 500`.

**Thứ tự chạy** (theo `evidence/P14-benchmark-plan.md` §4): giai đoạn 0 (P00c trên `serve` + SQLite, 3 ô) → giai đoạn 1 trên 8/24 (một tailnet: `personal_devices`, `tag_heavy` ×5 cỡ, `allow_all` ×3, `isolated` ×2; 3 lần/ô; chia đôi tới bước 500 quanh biên) → giai đoạn 2 trên 8/24 (nhiều tailnet, hỗn hợp shared) → giai đoạn 1–2 rút gọn quanh biên trên 4/8 → (sau P14b) before/after xen kẽ.

**Ước lượng thời gian** (ngoại suy từ pha của một ô, không phải đo): một ô ≈ 12–14 phút (restore + khởi động ~1–2 phút, connect N ≈ 1–3 phút ở 10k theo P00c, steady 5 phút, 20 probe ≈ 3,5 phút, register 500 ≈ 1 phút, 3 policy ≈ 1,6 phút, hold 1 phút). Giai đoạn 1: 15 ô × 3 lần ≈ 10 h + biên ≈ 1,5 h; seed ≈ 75k lượt đăng ký ≈ 2–3 h; giai đoạn 2: ≈ 30 lần ≈ 7 h + seed ≈ 3–4 h; cỡ 4/8 rút gọn ≈ 8–10 h; giai đoạn 0 ≈ 1 h. **Tổng ≈ 33–40 h lab độc quyền ≈ 4–5 ngày** cửa sổ 8 h (ticket ghi 3–4 ngày cho phần chạy).

## 8. Lệch so với yêu cầu, giới hạn, rủi ro

**Lệch:**
- Chế độ cluster nằm ở `tools/capload/cluster.sh`, `server.sh` chỉ chuyển tiếp `cluster-<sub>` (giữ nguyên đường P00c của `server.sh`).
- **Chưa có driver lab** (`run-cluster-cell.sh` điều phối nhiều host qua ssh, khóa `/var/lock/mesh-bench`, chép artifact về laptop). Mọi lệnh con nó cần đã chạy thử ở §5 trên một host; driver nhiều host chưa chạy được thì không viết (không có lab để thử). Việc đầu tiên của P14a.
- Metric nhỏ trong code (histogram gia hạn lease, thời gian transaction có fence — P14a.0 gạch đầu dòng cuối, "PR riêng, cần duyệt") **chưa làm**; harness đọc `nodestore_*`, `mapresponse_generated_total` và log.
- CPU cgroup của container PostgreSQL/PgBouncer chưa thu (`pgsample.sh` chỉ có số phía PostgreSQL/PgBouncer); trên `mc-db` dùng `docker stats`/cgroup ở driver.
- `BenchmarkScale_AddToBatch_Targeted` (lỗi đo, ticket "ngoài code") chưa sửa.

**Giới hạn của harness:**
- `capload` không có data plane và không mô phỏng CPU của tailscaled; observer giải mã mọi frame — với `allow_all` lớn nên giảm `-observe` (mỗi full map ở 5k peer ≈ vài MB JSON).
- Probe policy đo đường operator (CLI + poll reconcile 2 s), không phải API tenant v2.
- Probe chọn node nguồn trong tập peer của observer: với `personal_devices` chỉ ≤ 9 observer (cùng user) thấy một thay đổi; số observation mỗi probe nhỏ hơn `allow_all`.
- Churn bỏ qua tick khi đã có 64 request đang bay (tính là `failed`): con số này là tín hiệu server không theo kịp, không phải tải đã gửi.
- Một nguồn tải: > ~28k stream tới một `IP:port` chạm `ip_local_port_range` của `mc-ci`.
- `summarize-cell.py` ghép metric theo mẫu 10 s gần mốc pha nhất: cửa sổ < 10 s có delta metric thô.

**Rủi ro:**
- Số microbench (§6) lấy trên `mc-ci` dùng chung (load 6–19 lúc chạy, agent khác build/test song song): chỉ dùng để so trong cùng lần chạy, không dùng làm số tuyệt đối.
- Thay đổi hạ tầng cần người duyệt trước lần đo thật: `stats_users` + `pg_stat_statements` trên `mc-db` (khởi động lại `mc-pg`, ảnh hưởng E2E/P13), cỡ 4/8 cho active, nới cổng nguồn trên `mc-ci`.
- Trong lúc phát triển, agent này một lần chạy nhầm script đồng bộ của agent P13 (trùng tên file `sync.sh` trong scratchpad dùng chung): script đó reset `~/src/p13be` trên `mc-ci` về đúng tip nhánh của nó và chép lại file đang sửa của chính nó (thao tác của script đó, không mất dữ liệu nhưng có thể chen vào một lần build/test đang chạy của agent kia). Từ đó dùng tên riêng `p14a-sync.sh`.

## 9. Negative tenant isolation, migration, reviewer

- **Cô lập:** mọi frame giải mã ở §5 (1.879 frame, 4 ô) kiểm từng peer theo tailnet: 0 vi phạm, 0 peer lạ; policy đổi ở một tailnet: 0 frame có filter ở tailnet kia. pprof: 0 route trên listener public ở cả hai build; `metrics_operator_cidrs`/LAN/CGNAT/ULA/Internet bị từ chối (§4).
- **DB migrations:** không có.
- **Reviewer approval:** chưa (cần người duyệt: thay đổi `MetricsHandler` + build tag theo AGENTS rule 9).
