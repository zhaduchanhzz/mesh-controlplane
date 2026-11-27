# P13 integration Evidence — Gate N3 code (P13a–P13f) + P14a.0

- **Trạng thái:** tích hợp `Verified` trong process (build, vet, lint, race, toàn bộ suite `p05cluster` trên mc-ci). **Không** chạy E2E, **không** đổi lab/VPS (lab đang được agent khác dùng). Drill P13 (F05, kill PgBouncer, F08, canary, xoay Noise key, restart relay) `Not started`. Chưa `Reviewed`.
- **Nhánh:** `feat/P13-integration` từ `origin/master` `f868e22` (P12c đã merge). Chỉ push `mcci`; không push `origin`, không merge vào `master`.
- **Môi trường:** mc-ci (dùng chung), Go 1.27.4, `GOFLAGS=-buildvcs=false`, worktree `~/src/p13int` (detached ở tip của nhánh sau mỗi push). PostgreSQL 16 + PgBouncer 1.26.1 tạm bằng `tools/pgtest/throwaway.sh` (`/tmp/p13b-pg.*`, 127.0.0.1 + unix socket, trust, không mật khẩu; `MESH_TEST_POSTGRES_DSN`, `MESH_TEST_REQUIRE_POSTGRES=1` như CI, thêm `MESH_TEST_PG_*`/`MESH_TEST_PGBOUNCER*` nên các test P13b không bị skip), xóa sau khi chạy.

## 1. Source inventory (AGENTS rule 5)

- Commit trước: `f868e22` (`master`). Nhánh được merge, theo thứ tự:

| # | Nhánh | Tip | Nội dung |
|---|---|---|---|
| 1 | `feat/P13a-infra` | `247b186` | IaC `infra/lab/pg` (mc-pg-01/02, 7708/7709), Patroni + etcd, evidence P13a; mang theo `feat/P13-plan` `c4533aa` (ADR 0011–0014 Accepted, `tickets/P13.md`) |
| 2 | `feat/P13dc-migrate-fence` | `88a80d3` | bước migrate riêng, `schema_compat`, epoch fence; migration `202611271000-schema-compat`, `202611271100-epoch-fence`; harness gọi `headscale cluster migrate` |
| 3 | `feat/P13be-db-noise` | `d129aba` | danh sách host, pool lease (`lease_name`), self-check pooler, xoay Noise key (`--init-noise-key`, kiểm lại registry mỗi reconcile); khóa `database.postgres.*` |
| 4 | `feat/P13f-observability` | `732b3a8` | metric lease/fence/pool/reconcile, listener opsmetrics, PROXY v2 cho bootstrap, L3/L4/L5/L9/L10, artifact monitoring/PgBouncer/VPS HAProxy (không áp) |
| 5 | `feat/P14a-capload` | `f5b532b` | capload/capseed cluster mode, `benchpprof`, microbench P14b (mang theo `feat/P14-plan` `458697e`) |
| 6 | `feat/P14-plan` | `0e07dfd` | quyết định 2026-11-26: ước lượng từ đo nhỏ/vừa + mô hình chi phí, không chạy quy mô lớn |

- Đã đọc trước khi giải xung đột: `evidence/P13a-ha-postgres.md`, `P13dc-code.md`, `P13b-pgbouncer-ha.md`, `P13e-noise-rotation.md`, `P13f-monitoring-limits.md`, `P14a0-harness.md`; `tickets/P13.md` (D9/D9b, B7); ADR 0006 §3.5–3.6, 0011 §D, 0012, 0013, 0014.
- Path/symbol chạm khi tích hợp: `hscontrol/db/platform.go` (`OpenPlatform`, `openPlatformChain`, `writableDatabase`, `leaseDatabase`), `hscontrol/db/readonly.go` (`openReadOnly`), `hscontrol/db/migrate.go` (`migrateChain`), `hscontrol/db/db.go` (`openDB(cfg, path, readOnly)`), `internal/lease/manager.go` (`Manager.hold`, `holdEnd`, `observe`), `internal/lease/metrics.go` (`leaseState`), `hscontrol/cluster.go` (`reconcile`, `MetricsHandler`), `cmd/mesh-placement/main.go` (`run`, `runServe`, `runMigrate`, `runEpochFence`), `hscontrol/types/config.go` (`PostgresConfig.LeaseName`), harness `infra/lab/e2e/{n1/cases3.sh,p05,p12,p12b,twocluster}/run.sh`, `tools/capload/{cluster.sh,smoke-local.sh,README.md}`, `infra/lab/{pg,pgbouncer,vps-haproxy}/README.md`.
- Ràng buộc giữ nguyên: một Noise key mỗi cluster (ADR 0005, rule 12); `OpenPlatform` không bao giờ migrate (ADR 0013); writer mặc định `target_session_attrs=read-write` (ADR 0011 §D); metric chỉ trên listener operator (rule 9); file sinh (`config_docs_generated.go`, `docs/ref/*`, `nix/options.generated.nix`) chỉ sinh lại bằng `tools/configgen`.
- Giả định: không có thay đổi API Huma/OpenAPI trong năm nhánh (không chạy `make generate`); ADR không bị sửa.

## 2. Merge và xung đột

| Commit | Merge | Xung đột → cách giải |
|---|---|---|
| `0c7e490` | P13a-infra | `PLAN.md`: giữ dòng P12c (`Reviewed`) của master và dòng P13 của P13a, ghi ADR 0011–0014 **Accepted** (thay "Proposed") |
| `f9935d8` | P13dc | không có (`infra/lab/e2e/n1/run.sh` tự merge: S08b/S12 của master và bước migrate ở hunk khác) |
| `3b85222` | P13be | `hscontrol/db/platform.go`: `openPlatformChain` của P13dc + self-check/host list/pool lease của P13b, thứ tự self-check → open → (migrate tùy chọn) → version/schema compat → tenantscope → pool lease; `OpenPlatform` vẫn không migrate. `internal/lease/manager.go`: giữ `hwm`, `regressions` (P13c) và `down` (P13e) |
| `9d6c68f` | P13f | `cmd/mesh-placement/main.go`: giữ cả lệnh con migrate/epoch-fence và counter request + cờ metrics/revocation. `hscontrol/cluster.go` `reconcile`: đo thời gian (P13f) rồi kiểm Noise key (P13e). `platform.go`/`readonly.go`: `openDB(..., readOnly)` của P13f (L3: `query_only` trong DSN) trên luồng P13dc+P13b; PRAGMA một lần đã bỏ. `manager.go` `hold`: `holdEnd` (step down) + counter `self_fence`/`lost`. `placementapi/api_test.go`: `newEnvWith` của P13f trên `MigrateAndOpenPlatform` |
| `ec4ba59` | P14a-capload | `PLAN.md`: dòng P12c/P13 của nhánh này + dòng P14 của P14a. `cluster.go` tự merge (`withBenchPprof` quanh `operatorOnly`) |
| `6e2b03d` | P14-plan | không có |

## 3. Sửa sau merge (mỗi sửa một commit)

| Commit | Lỗi tích hợp | Sửa |
|---|---|---|
| `b109181` | P13f thêm tham số `readOnly` cho `openDB`; bước migrate của P13dc và helper `rawConn` gọi dạng cũ → không build | thêm `false` |
| `3a67f46` | Trùng tên method: `Manager.observe(epoch)` (P13c HWM) và `Manager.observe()` (P13f collector) → không build | đổi tên accessor metric thành `leaseState()` |
| `b097770` | Test P13f mở DB trống bằng `OpenPlatform` (trước P13d nó tạo schema). RED ở `3a67f46`: `TestOpenReadOnlySQLiteEveryConnectionQueryOnly`, `TestDefaultTailnetOnlyFromSingleTenantOpener` → `database schema is behind this binary; run mesh-placement migrate` | test chạy bước migrate trước (`MigrateAndOpenPlatform`; `Migrate` rồi `OpenPlatform` ở test về việc `OpenPlatform` không tạo tailnet) |
| `88c8ffc` | Sau khi áp HAProxy VPS, bootstrap cần `-proxy-protocol-from 10.99.77.1/32` (P13f §8.3) | harness (n1/cases3, p05, p12, p12b, twocluster) đọc `BOOT_PROXY_PROTOCOL_FROM` (mặc định rỗng = dòng lệnh y như cũ; chỉ nhận ký tự CIDR); README VPS HAProxy trỏ tới biến. Không áp, không chạy |
| `c3b7bc6` | `tools/capload/cluster.sh` (P14a.0) viết trước P13d/P13e: trên DB mới `cluster register`/capseed/serve dừng vì chưa migrate; `cluster serve` không khởi động khi thiếu file Noise key | thêm `cluster.sh migrate` (`headscale cluster migrate`); `start` truyền `--init-noise-key` cho active khi thiếu file key (standby thiếu key → từ chối); `smoke-local.sh` migrate trước register; README |
| `3a1afb7` | Alias lease: P13a/P13b ghi một alias chung `mesh_lease`, artifact P13f (D9b) dùng `mesh_c1_lease`/`mesh_c2_lease` | thống nhất: mỗi cluster `name: mesh_cN`, `lease_name: mesh_cN_lease` (README pgbouncer + ini, README pg, ghi chú trong evidence P13b); ví dụ `lease_name` trong config → `mesh_c1_lease`, tài liệu sinh lại bằng `go generate ./hscontrol/types/` trên mc-ci. ADR 0011 §D và `tickets/P13.md` vẫn ghi `mesh_lease` (không sửa ADR, xem §7) |
| `f007995` | Test P13b (self-check, host list, pool lease) mở DB trống bằng `OpenPlatform` → `ErrSchemaNeedsMigration` ở 7 test, gồm `TestLeaseStoreHasOwnPool` (CI chạy test này) | test chạy bước migrate trước: cùng danh sách host khi test về danh sách host, trực tiếp tới server ghi được ở chỗ khác (ADR 0006 §4) |
| `56ea3ad` | Bước migrate (P13d) mở kết nối với cấu hình nguyên dạng (pgx `any`), không qua `writableDatabase` (P13b): danh sách host có replica đứng đầu → migrate thất bại. RED ở `f007995`: `TestOpenPlatformMultiHostSkipsReadOnly/{direct,pgbouncer}`, `TestOpenPlatformHostsKey` → `cannot execute CREATE TABLE in a read-only transaction (SQLSTATE 25006)` | `migrateChain` dùng `writableDatabase` như `OpenPlatform` (giữ `target_session_attrs` nếu đặt rõ). GREEN: mọi test P13b PASS |
| `00138ae` | golangci-lint: `run()` của mesh-placement gocyclo 31 (> 30) sau khi gộp lệnh con P13dc + cờ P13f | tách `runServe()`; hành vi không đổi |

## 4. Raw SQL guard, endpoint inventory, file sinh

- `MESH_RAW_SQL_GUARD_PRINT=1 go test -count=1 -run TestRawSQLGuard -v ./hscontrol/db/` ở `6e2b03d` → **exit 0**, `TestRawSQLGuard` + `TestRawSQLGuardCatches` PASS. Danh sách in ra khớp đúng allowlist sau merge: **không thêm/bớt mục nào** (mục `internal/lease/lease.go` của P13c — `AcquireAbove` `SET LOCAL lock_timeout`, `get` — đã có từ nhánh P13dc; P13b `pooler.go` và `dbstats` không nêu bảng tenant).
- `evidence/P08a-endpoint-inventory.md` + testdata: chỉ P13f đổi; tự merge, `TestEndpointInventory`/`TestP08*` nằm trong lượt race `./hscontrol` (§5).
- `go generate ./hscontrol/types/` ở `6e2b03d`: không lệch (`git status --porcelain` rỗng). Sau `3a1afb7` (đổi doc/ví dụ `lease_name`): sinh lại, áp nguyên diff của generator.
- Không có thay đổi API Huma → không chạy `make openapi`/`make generate`; `gen/` không đổi.

## 5. Lệnh và exit code (mc-ci)

| Lệnh | Commit | Exit | Kết quả |
|---|---|---|---|
| `go build ./...`; `go vet ./...` | `3b85222` | 0; 0 | sau merge P13be |
| `go test -count=1 -run 'TestOpenReadOnlySQLiteEveryConnectionQueryOnly\|TestDefaultTailnetOnlyFromSingleTenantOpener' ./hscontrol/db/` | `3a67f46` | **1** | RED: `database schema is behind this binary` |
| `MESH_RAW_SQL_GUARD_PRINT=1 go test -count=1 -run TestRawSQLGuard -v ./hscontrol/db/` | `6e2b03d` | 0 | allowlist không đổi |
| `go generate ./hscontrol/types/` + `git status --porcelain` | `6e2b03d` | 0 | rỗng |
| `go test -count=1 -run 'TestOpenPlatform\|TestOpenReadOnly\|TestLeaseStoreHasOwnPool\|TestLeaseDatabase' ./hscontrol/db/` (throwaway PG + PgBouncer) | `3a1afb7` | **1** | 7 test P13b `ErrSchemaNeedsMigration` |
| như trên | `f007995` | **1** | RED: migrate trên host list `ro,rw` → `cannot execute CREATE TABLE in a read-only transaction` (3 ca) |
| như trên, `-v` (+ `TestDefaultTailnetOnlyFromSingleTenantOpener`) | `56ea3ad` | **0** | mọi test P13b + P13f PASS |
| `go build ./...`; `go vet ./...`; `go vet -tags p05cluster ./...`; `go vet -tags benchpprof ./...`; `gofmt -l` | `56ea3ad` | 0; 0; 0; 0; rỗng | |
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --concurrency 4 ./...` | `56ea3ad` | **1** | 1 issue: gocyclo `cmd/mesh-placement/main.go run` 31 |
| như trên; và `--build-tags benchpprof ./hscontrol/` | `00138ae` | **0**; **0** | `0 issues.` |
| `CGO_ENABLED=1 go test -race -count=1 -p 4 -timeout 60m $(go list ./... \| grep -v -e /integration -e /hscontrol/servertest)` (throwaway PG 16 + PgBouncer 1.26.1, `MESH_TEST_REQUIRE_POSTGRES=1`) | `00138ae` | **0** | 61 package: 48 ok, 13 không có test, 0 FAIL (policy/v2 593 s, hscontrol 539 s, state 427 s, db 189 s) |
| `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c bash infra/lab/scripts/shard-test.sh -n 8 -race -tags p05cluster -o ~/p13int-shard ./hscontrol/servertest/` | `00138ae` | **1** | `tests=226 pass=225 fail=1 skip=0 missing=0 wall=1459s`; FAIL `TestP13dClusterDoesNotMigrateAtStart`: P13e kiểm file Noise key trước khi mở DB (`ErrNoiseKeyMissing`), helper `NewClusterOn` trỏ tới đường key trống → sửa ở `e1a3519` (chép key của cluster) |
| `go test -race -tags p05cluster -count=1 -run 'TestP13d\|TestP13c\|TestP13e\|TestP13f' ./hscontrol/servertest/` | `e1a3519` | **0** | ok 46,9 s. Chưa chạy lại toàn bộ suite sau `e1a3519` (chỉ đổi helper test) |

Commit sau `00138ae` (`0b3cdb3`, evidence này) chỉ đổi tài liệu.


## 6. Negative tenant isolation

Không đổi đường dữ liệu tenant khi tích hợp. Các test cô lập của từng nhánh nằm trong lượt race và suite `p05cluster` ở trên (tenantscope, raw SQL guard, L4 nhãn tailnet chỉ cho tailnet được phục vụ, B7 chống giả mạo PROXY v2, `TestP08ClusterMetricsOperatorsOnly`, `TestBenchPprof*`). Không có API unscoped mới.

## 7. Còn mở / cần chủ dự án quyết

1. **ADR 0011 §D** và `tickets/P13.md` (dòng 115, 119) vẫn ghi một alias lease chung `mesh_lease` (`pool_size = 4`, `reserve_pool_size = 2`); artifact P13f (D9b) và tài liệu lab giờ dùng một alias mỗi cluster (`mesh_cN_lease`, `pool_size=2 max_db_connections=3`). Không sửa ADR: cần chủ dự án xác nhận (hoặc sửa ADR) trước khi áp PgBouncer.
2. Metric cho hai sự kiện mới của nhánh song song chưa có: lease bị từ chối vì epoch lùi (P13c `Manager.Regressions()`, hiện chỉ log `CRITICAL`) và step down do Noise key lệch (P13e). Counter `headscale_cluster_lease_events_total` của P13f chưa có `epoch_regression`/`stepped_down`; pool lease (P13b) chưa đăng ký với `dbstats` (chỉ pool `platform`). Đề xuất ticket nhỏ + rule cảnh báo; không làm ở đây (thêm hành vi).
3. P13c HWM vẫn chỉ trong bộ nhớ (`cluster_lease.epoch_hwm_path` chưa làm — trước đây chờ P13b/P13f vì chung config; nay đã gộp, có thể làm).
4. Giới hạn đã ghi của từng nhánh giữ nguyên: versioncheck chặn canary khác minor (ADR 0013 §6, P13dc §1); renew treo khi mạng "mất không RST" (P13b §5); L10 `createDefaultTailnet` giữ hành vi single-tenant (P13f §8.1); PROXY v2 chỉ cho bootstrap (P13f §8.2).
5. `tools/capload` đổi ở `c3b7bc6` chưa chạy lại smoke (`smoke-local.sh`).
6. Suite `p05cluster` đầy đủ: 225/226 ở `00138ae`; test hỏng đã sửa ở `e1a3519` và chạy lại nhóm P13 xanh; chưa chạy lại cả suite.

## 8. Drill lab cần gì tiếp (chưa làm)

Mọi drill chạy binary từ **một** commit của nhánh này (build trên mc-ci, sha256 trong manifest), sau khi lab rảnh (agent khác đang dùng). Trước drill đầu tiên:

- **Triển khai lần đầu P13d là stop-the-world** (P13dc quy trình bước 1–3): deployment `meshlab` (c1, c2 systemd) đang ở schema P12c; dừng mọi process → backup → `mesh-placement migrate` trực tiếp tới primary → khởi động bootstrap → placement → cluster (standby trước). Unit systemd **không** cần `--init-noise-key` (key đã có); chỉ cluster dựng mới mới cần.
- **PgBouncer trên mc-pg-01/02** (P13a/P13b): `max_prepared_statements = 200`, `query_wait_timeout` < 8 s, `%include pgbouncer-clusters.ini` (alias `mesh_c1`/`mesh_c2` + `mesh_c1_lease`/`mesh_c2_lease` + `mesh_ops`; chờ chủ dự án xác nhận alias lease theo cluster, §7.1); config cluster: `hosts: ["192.168.1.247:6432","192.168.1.248:6432"]`, `name: mesh_cN`, `lease_name: mesh_cN_lease`. Self-check pooler sẽ từ chối khởi động nếu thiếu prepared statements hoặc gặp replica.
- **Monitoring** (`infra/lab/monitoring`, chưa áp) để có số cho mọi drill; `-metrics-listen` cho bootstrap/placement.

| Drill | Cần gì / đo gì |
|---|---|
| **F05** (failover PG, P13a) | Patroni switchover + failover cưỡng bức khi cluster c1/c2 đang phục vụ qua PgBouncer host list; đo: thời gian không ghi được, lease có self-fence không, `headscale_cluster_lease_events_total`, lag; không mất commit ở chế độ sync |
| **Kill PgBouncer khi tải** (P13b) | capload nhỏ trên c2 → `kill -9` PgBouncer đứng đầu danh sách host; đo thời gian client chuyển sang host thứ hai (`connect_timeout_secs` 2 s), self-fence có xảy ra không; thêm ca chặn 6432 không RST (giới hạn renew treo, P13b §5): nếu RED → đề xuất giới hạn mỗi renew ≤ `renew_interval` |
| **F08** (restore + epoch fence, P13c) | pgBackRest restore về điểm trước → PAUSE PgBouncer → `mesh-placement epoch-fence -reason … -operator …` → kiểm `max(epoch)` > HWM trước restore → RESUME; process cũ phải `ErrLost`/`ErrFenced`, reconcile log `CRITICAL epoch regression` khi chưa fence; move chờ bị hủy và lên lịch lại |
| **Canary** (P13d) | build N+1 **cùng minor** (quy ước PATCH, ADR 0013 §6) có một migration expand thử → `migrate` → c2 làm canary (standby → switchover → active), c1 giữ N và restart được; sau đó contract chỉ ở bản sau. Hai `migrate` đồng thời (L7) |
| **Xoay Noise key** (P13e, `docs/ops/noise-key-rotation.md`) | c2 với ≥ 2 tenant, client 1.104.7 + 1.84.3, c1 đối chứng; xoay có kế hoạch (`noise-key install/rotate`, switchover) và khẩn cấp ×2; đo thời gian tới `/ready`, tới ping/status phục hồi, tỉ lệ tự phục hồi không restart, đăng nhập lại?, NodeID/IP giữ nguyên; log protocol đã redact nếu client không hồi phục (rule 12) |
| **Restart relay** | restart derper/relay trong lúc tải nhẹ; client qua relay phục hồi, metric/alert tương ứng bắn rồi tắt |
| **VPS HAProxy (B7)** | chỉ sau khi nhánh này xanh (quyết định 2026-11-26): áp trong cửa sổ ngắn, chạy harness với `BOOT_PROXY_PROTOCOL_FROM=10.99.77.1/32`, rồi N1 S13b qua đường mới; rollback = `rollback.sh` + bỏ biến |
| **P14a (capload)** | `cluster.sh migrate` trước `register` trên DB mới (sửa `c3b7bc6`); active đầu tiên tự `--init-noise-key` |


## Reviewer approval

Chưa.
