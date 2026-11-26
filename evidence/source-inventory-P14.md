# Source Inventory — P14 (lần đầu: B2/B3 trên PostgreSQL dùng chung, PgBouncer)

- **Trạng thái:** harness `Implemented`; B2/B3 lần đầu `Verified` trên `mc-bench` (`evidence/P14-db-benchmarks.md`); B1 `Planned` (`tickets/P05.md`). Chưa `Reviewed`. Kế hoạch P14 đầy đủ (2026-11-26): §5–§9 bên dưới.
- **Nhánh:** `agent/bench-db-pgbouncer`, tạo từ `feat/P03-tenant-schema` @ `3a4cb6c` (schema v0.1), rồi merge `docs/plan-v2-cluster` @ `4c1ac6e` (PLAN 2.0) để sửa tài liệu trên bản 2.0. Merge không xung đột; thay đổi `AGENTS.md` trong merge là của commit chủ dự án `4c1ac6e`, không phải của nhánh này.
- **Không đổi code non-test trong `hscontrol/`.** Chỉ thêm `cmd/bench-db/` (build tag `benchdb`, ngoài `go build ./...` và `go test ./...`).

## 1. Path/symbol đã đọc

| Path | Symbol | Dùng cho |
|---|---|---|
| `hscontrol/db/db.go` | `NewHeadscaleDatabase`, `openDB` (pool: `SetMaxOpenConns/SetMaxIdleConns/SetConnMaxIdleTime`, không `PrepareStmt`), `HSDatabase.Read/Write`, `Read[T]/Write[T]`, `Close` | Đường mở DB thật trong mỗi process; transaction ngắn |
| `hscontrol/db/tenant.go` | `createSchema`, `checkV01Database`, `bindRuntimeTailnet`, `resolveRuntimeTailnet`, `ScopedContext` | Mỗi process bind tenant đầu tiên + kiểm owner; tenant còn lại dùng cùng pool qua scope mới |
| `hscontrol/db/tenantscope/tenantscope.go` | `Register`, `scopeWhere`, `scopeMutation`, `stampCreate` | Mọi câu trên bảng tenant đi qua plugin; `Raw` không qua plugin |
| `hscontrol/db/tenant_test.go` | `backends`, `postgresConfig`, `addTailnet` | Cách hai runtime chia một DB; tạo tailnet qua handle nền tảng |
| `hscontrol/db/schema_postgres.sql` | toàn bộ | Bảng/index đo kích thước; `tailnets.status` CHECK **không có** `MOVING` |
| `hscontrol/db/node.go` | `ListNodes`, `GetNodeByNodeKey`, `SetLastSeen`, `preloadNode` | Thao tác đọc/ghi B2 |
| `hscontrol/db/preauth_keys.go` | `CreatePreAuthKey`, `GetPreAuthKey`, `UsePreAuthKey` | Đăng ký node |
| `hscontrol/db/users.go` | `CreateUser`, `ListUsers` | Seed + thao tác B2 |
| `hscontrol/state/state.go:102-123` | `nodeUpdateColumns` (unexported, chép lại trong harness) | `map_update` |
| `hscontrol/state/state.go:855-917` | `persistNode/persistNodeWith` | Hình dạng câu `Select(cols).Updates` |
| `hscontrol/state/state.go:~2942-2965` | đăng ký node qua pre-auth key | `register` |
| `hscontrol/types/config.go:531-620` | `PostgresConfig`, `DSN()` | DSN chỉ có `sslmode`: **không cấu hình được** `default_query_exec_mode` của pgx |
| `internal/tailnet/tailnet.go` | `ParseID`, `NewScope`, `WithScope`, `SourceInternalService` | Scope cho các tenant của process |
| `go.mod` | `gorm.io/driver/postgres v1.6.3`, `gorm.io/gorm v1.31.2`, `github.com/jackc/pgx/v5 v5.11.0` | Hành vi prepared statement |
| `PLAN.md` 2.0, ADR 0001/0005, `tickets/P12b.md`, `06-tests-benchmarks.md`, `04-proxmox-lab.md`, `evidence/P00b-capacity-baseline.md`, `evidence/source-inventory-P03b.md` | — | Bối cảnh, định dạng evidence |

## 2. Ràng buộc hiện có

- Một process hiện tại (`NewHeadscaleDatabase`) = **một pool cho một tenant**. Process nhiều tenant (P05) chưa có; harness dùng `HSDatabase{DB: base.DB.WithContext(scope)}` để chia một pool, như ADR 0001 dự kiến.
- Code không dùng `SET`, advisory lock, `LISTEN/NOTIFY` (grep `hscontrol/ internal/ cmd/`).
- Fencing owner+epoch (P10), `cluster_leases` (P11b), câu CAS move (P12b) chưa có code → harness **mô phỏng** bằng SQL thô, có nhãn.

## 3. Giả định

- Mix thao tác B2 là tổng hợp (tỉ lệ ghi/đọc chọn tay), không phải mô hình client đã đo.
- Harness và PostgreSQL chạy cùng VM `mc-bench` (ghim CPU khác nhau); một benchmark P00c của người khác chạy song song (ghi trong evidence).
- Thiết kế "cluster_id trên mọi dòng" của bài blog được mô phỏng bằng bản sao bảng + cột + index; bài blog không có DDL.

## 4. SQL thô mới (chỉ trong harness, không vào code chạy thật)

`fenceCheck` (`SELECT … FROM tailnets … FOR SHARE`), standby poll, câu CAS owner/epoch, `UPDATE blogsim.*`, câu thống kê `pg_stat_*`, `CREATE SCHEMA blogsim …`. Không áp dụng allowlist SQL thô của P03b vì không nằm trong `hscontrol/`.

---

# Inventory kế hoạch P14 (2026-11-26, sau quyết định hạ mục tiêu capacity)

## 5. Commit và phạm vi

- **Commit đọc:** `c9ba662` (nhánh tích hợp `feat/P12c-review-hardening`: `master` `dece058` + P12c nhóm A/B/C + quyết định 2026-11-26 trong PLAN/ADR 0003). Nhánh kế hoạch: `feat/P14-plan`.
- **Chỉ tài liệu.** Không đổi code Go, không đổi harness, không chạy benchmark, không bật/tắt VM. Trên lab chỉ đọc: `ssh mc-ci` liệt kê thư mục (`~/src`, `nproc`, `free`, `uptime`); E2E đang chạy không bị đụng.
- **Cách đọc:** `git`/`grep`/`sed` trên worktree sạch. Ba lượt đọc hot path (mapper/poll; policy/NodeStore; DB/fence/lease/placement) do subagent chỉ-đọc làm; agent kế hoạch **tự kiểm lại** các file:line được trích trong `tickets/P14.md` (bảng ứng viên): `tenantscope.go:86-93, 478-494, 547-563, 604-622`; `quota.go:149-160`; `tailnets.go:480-483`; `policy.go:1330, 1374-1398`; `state.go:720, 825-828, 944, 964, 1325, 2959-2966, 4239, 4314-4324`; `node_store.go:705-709, 777-783, 1276-1294`; `poll.go:486-506`; `builder.go:266, 294, 318-326`; `mapper.go:428-436`; `node_conn.go:175-181, 627`; `noise.go:874-878`; `platform.go:275-300`; `cluster.go:209, 368-386, 1194-1206`; `schema_postgres.sql:130-134, 160`; `placement.go:137-142`; `config/default.go:50-55, 84, 90`; `types/common.go:345-350`; `setup-db.sh:60-64`; tên test/benchmark trích dẫn (grep `func …`). Các file:line khác trong ghi chú subagent **chưa** được kiểm lại từng dòng; ticket chỉ trích những dòng đã kiểm.

## 6. Path / symbol đã đọc (kế hoạch)

| Path | Symbol | Dùng cho |
|---|---|---|
| `PLAN.md` §0.6, §1, §6, §7 (N3, N4), §9, §11 | quyết định 2026-11-26, phụ thuộc P13→P14, lab | Phạm vi, gate |
| `tickets/P00b.md`, `P00d.md`, `P05.md` (B1/L9), `P08b.md`, `P09.md`, `P12b.md` | — | Ma trận cũ, S13/I10 → P14, B3 → P14 |
| `evidence/P00b-capacity-baseline.md`, `source-inventory-P00c.md` §6, §9, `source-inventory-P00d.md` §3, §9–§10, `P14-db-benchmarks.md`, `P12b-tenant-move.md` §Benchmark, `P12c-b.md` §2, §4, `P11b-cluster-failover.md`, `REVIEW-2026-11-26-tickets.md`, `source-inventory-P08b.md` §3.1, `LAB-resize-2026-11-23.md` | — | Số hiện có (§7), việc còn nợ |
| `06-tests-benchmarks.md` §1c, §4, §5; `04-proxmox-lab.md` §4; ADR 0006, 0009 | — | Hợp đồng benchmark, artifact, lab |
| `tools/capload/{README.md,main.go,server.sh,run-seeded.sh}`, `tools/capseed/main.go` | cờ `-nodes/-register/-concurrency/-hold/-deadline/-decode-every`; `server.sh` chỉ `headscale serve` + SQLite; capseed chỉ SQLite | Khoảng trống harness (P14a.0) |
| `cmd/bench-db/{b2.go,b3.go,main.go}` | B2/B3 | P14d |
| `cmd/headscale/cli/cluster.go:86-130` | `cluster serve`, `cluster tailnet …` | Cách chạy cluster mode |
| `hscontrol/mapper/batcher.go`, `builder.go`, `mapper.go`, `node_conn.go`, `incremental.go`; `hscontrol/poll.go`; `hscontrol/map_frame_timing.go` | `addToBatch`, `processBatchedChanges`, `buildTailPeers`, `WithPeers`, `policyChangeResponse`, `filterVisiblePeerPatches`, `lastSentPeers`, `connectionEntry.send`, `encodeMapResponse`, `recordFrameWritten` | Ứng viên #1, #3, #6, #9, #15, #16 |
| `hscontrol/policy/v2/policy.go`, `peermap_index.go`, `compiled.go`, `incremental.go` | `SetNodes`, `SetUsers`, `updateLocked`, `invalidateNodeCache`, `buildPeerMapIndexedLocked`, `bitsIntersect`, `nodeOnlyMatchers`, `localSafeLocked` | #2, #8, #14 |
| `hscontrol/state/state.go`, `node_store.go`, `node_store_reconcile.go` | `nodeUpdateColumns`, `persistNodeWith`, `persistMu`, `CreateUser`, `updatePolicyManagerUsers`, `updatePolicyManagerNodes`, `ListPeers`, `applyBatch`, `snapshotFromNodesWith`, kiểm `MaxPeerEntries` | #2, #4, #6, #7, #10, #12 |
| `hscontrol/db/tenantscope/tenantscope.go`; `hscontrol/db/db.go`, `platform.go`; `internal/lease/lease.go`, `manager.go` | `checkFence`, `checkLease`, `boundTransaction`, `boundAndLockLease`, `HSDatabase.Write`, `openDB`, `TailnetForNode`, `Acquire/Renew` | #4, #11, #17 |
| `internal/placement/quota.go`, `tailnets.go`, `clusters.go`, `placement.go` | `clusterBudget`, `deviceCount`, `admits`, `DefaultSharedTenantLimit`, `MaxDevices` | #5, P14e |
| `hscontrol/cluster.go` | `placement.New(…, placement.Config{})`, `SetEnrollGate`, vòng nạp tailnet, listener metrics (không pprof) | #13, P14e, Q7 |
| `hscontrol/config/default.go`, `hscontrol/types/config.go`, `hscontrol/types/common.go` | pool PG 10/10, `BatchChangeDelay` 800 ms, `MaxPeerEntries` 1.000.000, worker = 3/4 core | Tham số mặc định |
| `hscontrol/state/node_store.go:48-111`, `hscontrol/metrics.go`, `hscontrol/mapper/batcher.go:37-45` | metric hiện có | Không có histogram thay đổi → frame, gia hạn lease, transaction fence |
| `infra/lab/{bench.tf,c2.tf,c1b.tf,c2b.tf,cp.tf,db/main.tf,db/setup-db.sh}` | cỡ VM, PG 16, PgBouncer 1.24.1, pool 20, `max_db_connections` 80 | Topology §2 kế hoạch benchmark |
| `internal/tenantlabel/tenantlabel.go:23-24` | trần 2.000 tailnet/cluster cho label metric | P14e |

## 7. Số hiện có (nguồn và môi trường)

Không có số nào dưới đây đo trên cluster mode nhiều tenant với PostgreSQL thật qua PgBouncer. "1 mẫu" = một lần chạy mỗi ô.

| Số | Giá trị | Nguồn | Môi trường | Ghi chú |
|---|---|---|---|---|
| 10k `personal_devices`, connect lại | 10.000/10.000 trong 74,4 s; đăng ký thêm 500: 500/500 trong 37,9 s; RSS đỉnh 2,95 GB | P00c §9 (sau merge, `3269688`) | `headscale serve`, SQLite, `mc-bench` 8/24, tải `mc-ci`, 1 mẫu | register p95 ≈ 15–16 s (P00c §6) > 10 s |
| 10k `isolated` | 10.000 trong 72,9 s; 500/500 trong 29,2 s; 2,88 GB | P00c §9 | như trên | |
| 10k `allow_all` | **không đạt**: 6.161/10.000 trong 20 phút; 23,3 GB RSS | P00c §9 | như trên | gần OOM |
| 5k `allow_all` (`max_peer_entries` 0) | 5.000 trong 365,6 s (ON) / 367,3 s (OFF); 500 thêm trong 60–62 s; RSS 12,9–13,2 GB | P00d §9.2 (`2fde828`) | như trên | đạt nhưng chậm, RAM lớn |
| 10k `isolated`, netmap ON / OFF | đăng ký 500: 18,5 s, 28,6 MB gửi / 28,8 s, 289,5 MB | P00d §9.2 | như trên | |
| 10k `personal_devices`, ON / OFF | 500 trong 38,8 / 41,9 s | P00d §9.2 | như trên | P00d không áp cho autogroup:self |
| Giới hạn mặc định allow_all | `max_peer_entries` 1.000.000 ⇒ ≈ 1.000 thiết bị allow-all/tailnet | **tỉ số** từ `config/default.go:90` | — | P00d §9.2: 5k allow-all từ chối node mới khi để mặc định |
| P00b (client trong cùng process) | ≈ 1,5k–2,5k thiết bị với policy thực tế | P00b §7 | `TestScale`, upstream | Thay bởi số P00c; chỉ để lịch sử |
| `BuildPeerMap` 10k | allow_all 8,2 s, isolated 3,4 s, personal_devices 78,6 s (pair scan) | P00b §4.1 | microbench | Trước P00c; **chưa có** số microbench cho bản index |
| B2 PostgreSQL | 16 cluster × (1 active + 1 standby), ~85–95k dòng `nodes`, 1.600 thao tác/s tổng, PG ~1 core; ~0,54 ms CPU PG/thao tác; **ngoại suy** ~5.000 thao tác/s trên 4 vCPU | P14-db §3, §5 | PG 17.6 container trên `mc-bench`, harness cùng VM, fence **mô phỏng**, tải nền P00c | Số `sat` lệch ~2× theo tải nền |
| PgBouncer | +0,3–0,6 ms p50, +2–5 ms p99 khi chưa bão hòa; lease poll p99 tới 651 ms qua pool bão hòa; **ngoại suy** ~6.000 tx/s/core | P14-db §3.5 | như trên | |
| Chi phí fence (thật) | fence+lease/update serial 0,679 ms so với nofence 0,300 ms; +~0,10 ms mỗi câu ghi có fence do round trip khóa lease | P12c-b §4 | PG 16 socket cục bộ, `synchronous_commit=off`, mc-ci | Qua PgBouncer/mạng sẽ lớn hơn (chưa đo) |
| Đọc lease mỗi ghi | +110 µs (+25 %) create serial | P11b §benchmark | PG cục bộ | Trước P12c-b |
| Gia hạn lease dưới 16 writer | tệ nhất 5–15 ms (sau P12c-b), trước đó tới 8 s | P12c-b §1 B1 | PG 16 tạm, mc-ci | |
| MoveOwner (thật) | 6–67 ms (đích rỗng) / 107–152 ms (đích 51–60k thiết bị); writer tenant đang chuyển chờ 94–153 ms | P12b §Benchmark | PG 17.6, `mc-bench`, không tải nền | Chủ dự án chuyển tối ưu sang P14 |
| Failover F01/F02 lab | standby ready ~10–12,5 s (F02 SIGSTOP); servertest F01 có release 104 ms | P11b | lab E2E, vài client | Chưa có số với N lớn |
| Move t100 | 24 client: 6,07 s (từ lúc chạy); ngưỡng Q7 ≤ 15 s | P12b M04 | lab E2E | "số lớn hơn thuộc P14" |
| M06 thu hồi | p50 0,38–1,00 s, max 1,44 s (ON và OFF) | P00d §9.1 | `headscale serve` một tailnet, mc-cp | Chưa có ở cluster mode |
| Nhiều tenant/process (B1/L9) | **chưa đo** | `tickets/P05.md` | — | |

## 8. Khoảng trống (gaps)

1. Không có số capacity nào cho cluster mode: nhiều tailnet, PostgreSQL qua PgBouncer, fence + lease thật, standby. Mọi số server-only là `headscale serve` + SQLite.
2. Harness: `capload`/`capseed` chỉ SQLite một tailnet; không đo độ trễ thay đổi → frame; không có churn MapRequest; không có policy `tag_heavy`; decode mẫu không kiểm tenant.
3. Không có pprof trong cluster mode (listener metrics chỉ `/metrics`, `hscontrol/cluster.go:1325`).
4. Không có metric độ trễ gia hạn lease, thời gian transaction có fence, thay đổi → frame phía server.
5. Không có microbench cho: mã hóa JSON+zstd map đầy đủ ở n lớn; `policyChangeResponse` mọi node; join dưới autogroup:self qua `policyPeersFunc` (`BenchmarkCapacity_SnapshotRebuild` lặp trên tập node không đổi). `BenchmarkScale_AddToBatch_Targeted` có lỗi đo.
6. Tốc độ ghi DB/thiết bị/giờ thật (ổn định, storm) chưa đo → B2 chưa đổi được "thao tác/s" thành "thiết bị".
7. Raw P00c/P00d không còn trên `mc-ci` (`~/src/p00c/results` không tồn tại, kiểm 2026-11-26). `mc-bench` đang tắt nên không kiểm được `~/p00c/results` trên đó.
8. Quota tenant shared là hằng số trong code (`placement.go:137`), không cấu hình được; không có giới hạn số tailnet/cluster ngoài trần label metric.
9. Không có storm nào ở quy mô > 24 client; relay (DERP) chưa có harness tải.

## 9. Giả định (chưa kiểm chứng)

- Lợi ích dự kiến của các ứng viên P14b là ước lượng từ đọc code và tỉ lệ CPU của P00b (client trong cùng process, upstream), **không** phải số đo trên tree hiện tại.
- "Khoảng 10 đăng ký/s/cluster shared do đếm thiết bị cluster" là **ngoại suy** từ số count của P12b B3 (cùng loại truy vấn, 61k dòng); chưa đo trên đường enrollment.
- Bộ nhớ `lastSentPeers` ở allow-all 10k "vài GB" là ước lượng (10⁸ mục xsync), chưa đo.
- Cổng nguồn của một host tải (~28k kết nối tới một đích) có thể là giới hạn của `capload` > 25k stream; chưa kiểm cấu hình `ip_local_port_range` trên `mc-ci`.
- RAM host Proxmox còn trống cho VM `mc-pgbench` (Q1) chưa kiểm; `LAB-resize-2026-11-23` ghi 120 GiB đã cấp cho guest nhưng không ghi tổng RAM host.
- Client `derphttp` tối giản dùng được với verify scoped (ADR 0009) cho bài relay saturation: chưa kiểm.
- `zstdframe.AppendEncode` có pool encoder bên trong (theo hiểu biết về upstream Tailscale); module cache không có trên máy đọc nên chưa xác nhận.
