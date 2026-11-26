# Source Inventory — P14a.0 (harness tải cho re-baseline capacity) + benchpprof + microbench P14b

- **Commit gốc:** `458697e` (`feat/P14-plan`: P12c tích hợp + kế hoạch P14 + quyết định Q1–Q11 ngày 2026-11-26), ngày 2026-11-26.
- **Nhánh:** `feat/P14a-capload` (worktree `wt-p14-a`). Push chỉ `mcci`; không push `origin`, không merge.
- **Trạng thái khi viết file này:** `Planned` (chưa đổi code).
- **Phạm vi (theo yêu cầu chủ dự án 2026-11-26):** (1) `capload`/`capseed` chế độ cluster: N tailnet × M thiết bị trên `headscale cluster serve` + PostgreSQL qua PgBouncer, đăng ký bằng pre-auth key thật theo tailnet, policy `allow_all`/`personal_devices`/`tag_heavy` (mới), thu số đo + manifest tái lập; (2) build tag `benchpprof` (pprof chỉ trên listener metrics loopback) + test; (3) microbench P14b đo được trong process. **Không** chạy benchmark lab, **không** bật `mc-bench`; chỉ chạy thử nhỏ trên `mc-ci` (PostgreSQL/PgBouncer tạm, xóa sau).

## 1. Đã đọc

| Nguồn | Nội dung liên quan |
|---|---|
| `AGENTS.md` | Rule 2 (không secret: pre-auth key chỉ trong file 0600, không in), 3/4 (không đụng lab ngoài phạm vi), 5 (inventory trước), 9 (debug/metrics lộ node/IP/key chỉ trên listener riêng hạn chế), 13 (isolation = BLOCK MERGE) |
| `tickets/P14.md` (P14a, P14a.0, P14b + 17 ứng viên, P14c–P14g, Q1–Q11 đã duyệt) | Danh sách việc harness; ứng viên #1 (dựng/mã hóa peer mỗi người nhận), #4 (3 round trip fence mỗi câu ghi), #6 (`ListPeers` mỗi patch), #7 (NodeStore chép mọi node mỗi batch); Q7 = pprof sau build tag, loopback, không bao giờ trong release |
| `evidence/P14-benchmark-plan.md` §1–§8 | Topology, policy shape (§3, `tag_heavy` mới), pha của một ô (§4), chỉ số (§5), manifest (§6), SLO ứng viên (§7), điều kiện dừng (§8) |
| `evidence/source-inventory-P14.md` §5–§9 | Khoảng trống harness (§8 mục 2, 3, 5), số hiện có |
| `evidence/P00b-capacity-baseline.md`, `evidence/source-inventory-P00c.md` | Cách đo P00b/P00c, định dạng bảng, nhãn đo/ngoại suy |
| `tools/capload/{main.go,server.sh,run-cell.sh,run-seeded.sh,summarize.py,sync.sh,README.md}` | `capload`: cờ `-server -keys -node-keys -nodes -register -per-user -concurrency -timeout -hold -deadline -decode-every -out`; frame chỉ đếm, decode first map mỗi 100 client; `server.sh` chỉ `headscale serve` + SQLite; sampler RSS/CPU 1 s |
| `tools/capseed/main.go` | Chỉ SQLite một tailnet, đăng ký trực tiếp qua `State.HandleNodeFromPreAuthKey` |
| `cmd/bench-db/*` | B2/B3 (tag `benchdb`), cách lấy `pg_stat_*`/PgBouncer `SHOW` |
| `cmd/headscale/cli/cluster.go` | `cluster serve`, `cluster register` (`--class`, `--max-devices`), `tailnet create/status/policy/authkey`; `createTailnetCLI` = `placement.CreateTailnetOn` (cluster phải đang giữ lease); `setTailnetPolicyCLI` = `policy.ValidatePolicy(…, WithEmptyPolicyDenies(true))` + `SetPolicy` + `BumpPolicyRevision`; `tailnet authkey` tạo user nếu chưa có rồi `CreatePreAuthKey` qua `TenantDB` |
| `hscontrol/cluster.go` | `NewCluster`/`validateClusterConfig` (policy mode database, base_domain bắt buộc, `server_url` phải là hostname — IP literal bị từ chối), `hostGuard` (421 nếu Host ≠ hostname của `server_url`), `load`/`reconcile` (runtime nạp user/policy lúc nạp; đổi `policy_revision` chỉ `ReloadPolicy`, **không** nạp lại user), `runtimeForRegister` → `TailnetForAuthKey` → `checkQuota`, `MetricsHandler` (chỉ `/metrics`, `operatorOnly`), `HTTPHandler` (listener public, không debug), `Serve` (listener metrics riêng) |
| `hscontrol/operator_access.go` | `operatorOnly`: loopback hoặc `metrics_operator_cidrs`, không bao giờ dải Tailscale |
| `hscontrol/db/platform.go`, `platform_admin.go` | `OpenPlatform`, `TenantDB` (ACTIVE + owner; CLI không có lease → ghi operator), `TailnetBySlug`, `SetTailnetStatus`, `BumpPolicyRevision`, `RegistryDB` |
| `hscontrol/db/preauth_keys.go` | `CreatePreAuthKey(uid, reusable, ephemeral, exp, tags)`: key có tag → node có tag (không theo user) |
| `hscontrol/state/state.go:3010-3020` | `RequestTags` chỉ xét khi **không** dùng pre-auth key → `tag_heavy` phải dùng key có tag |
| `hscontrol/poll.go:102-170`, `:480-505` | MapRequest `Stream=false, OmitPeers=true` = cập nhật endpoint/hostinfo "lite" (`UpdateNodeFromMapRequest` + `Change`); `encodeMapResponse` = `json.Marshal` + `zstdframe.AppendEncode(FastestCompression)` + tiền tố độ dài 4 byte LE |
| `hscontrol/mapper/mapper.go:258-340`, `builder.go` | `fullMapResponse` (ListPeers → builder đầy đủ), `policyChangeResponse`, `filterVisiblePeerPatches` |
| `hscontrol/mapper/batcher_test.go:133-260`, `batcher_scale_bench_test.go`, `batcher_bench_test.go` | `setupBatcherWithTestData` (SQLite + State thật, policy allow-all); `BenchmarkScale_MapResponse_FullUpdate` chỉ tới 500 node |
| `hscontrol/state/capacity_bench_test.go`, `node_store_test.go` (`BenchmarkNodeStoreWrite`, `BenchmarkSnapshotPayloadDense`) | Fixture `capacityNodes`, `snapshotFromNodes` |
| `hscontrol/db/tenantscope/tenantscope.go:478-622`, `fence_bench_test.go`, `tenantscope_test.go:100-191` | `checkFence`/`checkLease`/`boundTransaction` gọi thẳng `Statement.ConnPool` (không qua logger gorm); fixture `openDB`/`dialector` (SQLite hoặc schema riêng trên `MESH_TEST_POSTGRES_DSN`) |
| `hscontrol/p09_frame_gate_bench_test.go` | `benchMapResponse`, `BenchmarkS17EncodeFrame` (mã hóa frame tổng hợp) |
| `infra/lab/e2e/p05/{pg.sh,run.sh}`, `infra/lab/mc-cp/cluster/config.yaml` | Cách dựng PostgreSQL + PgBouncer tạm (transaction mode, `max_prepared_statements` 200, SCRAM, mật khẩu qua stdin/file 0600), trình tự register → serve → `/ready` → tạo tailnet → policy → key |
| `.github/workflows/ci.yml` | PostgreSQL tạm `initdb` + unix socket + `MESH_TEST_POSTGRES_DSN`; lint qua `nix shell nixpkgs#golangci-lint` |
| `.goreleaser.yml` | Build release `./cmd/headscale`, `CGO_ENABLED=0`, **không** có build tag → `benchpprof` không bao giờ vào release |
| `tailscale.com@v1.105.0-pre…/control/controlclient/direct.go`, `control/controlhttp/client.go`, `net/dnscache/dnscache.go`, `net/tsdial/tsdial.go` (module cache trên mc-ci) | Client phân giải hostname qua `dnscache.Resolver` với `Forward = dnscache.Get().Forward` (chép khi `NewDirect`) trước khi dial → cần hostname phân giải được |

## 2. Ràng buộc hiện có

- Cluster mode chỉ phục vụ hostname của `server_url` (P08 S11c). Harness phải dùng hostname (không IP); trên host tải không có DNS lab → `capload` cần cách ánh xạ hostname → IP mà **không** sửa `/etc/hosts` của host dùng chung.
- Tailnet chỉ tạo được khi process cluster đang giữ lease (quyết định 2026-11-25); runtime nạp user lúc nạp tailnet, `policy_revision` chỉ nạp lại policy → user tạo từ ngoài sau khi runtime đã nạp **không** vào policy manager (group `user@` không phân giải). Harness phải khởi động lại cluster sau khi cấp phát user/key/policy (hoặc khôi phục từ snapshot).
- Policy tenant rỗng = DENY (`PolicyEmptyDenies`); mọi tailnet benchmark có policy tường minh.
- Quota: tenant trên cluster `shared` bị giới hạn `DefaultSharedTenantLimit = 5000` (hằng số); cluster `dedicated` giới hạn bởi `max_devices`. Ô một tailnet > 5k phải dùng cluster `dedicated`.
- `tuning.max_peer_entries` mặc định 1.000.000 (≈ 1.000 thiết bị `allow_all`).
- Listener metrics cluster: chỉ `/metrics`, `operatorOnly` (loopback hoặc CIDR operator). pprof không có trong cluster mode.
- Fence/lease: 3 round trip phụ mỗi câu ghi tenant trên PostgreSQL, gọi thẳng `ConnPool` → đếm round trip phải ở tầng driver (pgx tracer), không qua logger gorm.

## 3. Thiết kế dự kiến (hợp đồng)

- `capseed -mode cluster`: mở platform bằng config cluster, tạo T tailnet qua registry (`CreateTailnetOn`, như CLI), ACTIVE, user + key (key có tag cho `tag_heavy`) qua `TenantDB` (như `tailnet authkey`), policy kiểm bằng `policy.ValidatePolicy(…, WithEmptyPolicyDenies(true))` rồi `SetPolicy` + `BumpPolicyRevision` (như `tailnet policy`). Xuất `plan.tsv` (mỗi dòng một thiết bị dự kiến: tailnet, user/tag, key) 0600 và `tailnets.json`/policy JSON (không secret).
- `capload -plan`: đăng ký qua Noise thật (pre-auth key → `TailnetForAuthKey` → quota → enroll gate); giữ stream; `-resolve host=ip` qua DNS giả trong process; decode frame của K stream quan sát (dấu thời gian mỗi frame), theo dõi tập peer, **kiểm cô lập** (peer phải cùng tailnet theo khóa node mà capload biết); probe endpoint/hostinfo (MapRequest lite) đo thay đổi → frame p50/p95/p99; churn endpoint theo tốc độ; probe policy qua lệnh ngoài; JSON kết quả + manifest (commit, sha256 binary, cờ, host).
- `server.sh` chế độ cluster: PostgreSQL qua PgBouncer, register/serve/`/ready`, sampler RSS/CPU, scrape metric định kỳ; `pgsample.sh`: `pg_stat_database`, `pg_stat_activity`, PgBouncer `SHOW POOLS/STATS` mỗi 1 s.
- `benchpprof`: file build-tag riêng gắn `/debug/pprof/*` vào mux metrics cluster, chỉ trả lời peer loopback (chặt hơn `operatorOnly`); không tag → không có route. Test cả hai phía.
- Microbench P14b: `#1` full map một người nhận (dựng + JSON + zstd) ở n lớn; `#4` round trip mỗi câu ghi có fence (pgx tracer) trên PostgreSQL thật; `#6` `ListPeers` cho lọc patch; `#7` NodeStore batch chỉ payload (endpoint) ở n lớn.

## 4. Giả định (chưa kiểm khi viết)

- `dnscache.Get().Forward` thay được trước khi tạo client và mọi `Direct` dùng nó (đọc mã: `Forward: dnscache.Get().Forward` trong `NewDirect`). Kiểm bằng chạy thử.
- Client `controlclient.Direct` chấp nhận `http://hostname:port` cho cluster không TLS (P00c dùng `http://IP:port` với `headscale serve`).
- Policy `tag_heavy` (120 tag, 300 grant, 50 group) qua được `ValidatePolicy` với user đã tạo; group tham chiếu `user@`.
- PgBouncer image `edoburu/pgbouncer:v1.24.1-p1` kéo được trên `mc-ci` (như `pg.sh` P05).
- Số microbench trên `mc-ci` (dùng chung với agent khác/E2E) chỉ mang tính chỉ dẫn.
