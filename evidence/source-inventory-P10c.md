# PR P10c Evidence — benchmark fence (review B3) và sửa starvation khi thay đổi ownership

- **Repo commit before / after:** `77ccd10` (master, sau merge P10b) → `1c21b42` (benchmark), `203859a` (sửa), `5dba471` (tiêu chí S18 của E2E), commit evidence (commit chứa file này). Nhánh `feat/P10c-fence-benchmark`.
- **Nguồn:** `evidence/REVIEW-2026-11-23-P10-P08.md` B3. Chủ dự án yêu cầu benchmark B3 (2026-11-23), rồi yêu cầu sửa bằng advisory lock sau khi benchmark lộ starvation.
- **ADR:** không có ADR mới. PLAN §5 (ADR 0006) cho phép `pg_advisory_xact_lock` và `SET LOCAL` qua PgBouncer transaction mode; thay đổi này chỉ dùng hai thứ đó.

## 1. Path / symbol đã đọc

- `hscontrol/db/tenantscope/tenantscope.go`: `Register`, `checkFence` (sau P10b đọc `owner_cluster_id, owner_epoch, status, deleted_at` `FOR SHARE`).
- Mọi chỗ `UPDATE` dòng `tailnets` ngoài test: `internal/placement/tailnets.go` `setStatus` (Activate/Suspend), `MoveOwner`; `hscontrol/db/platform_admin.go` `SetTailnetStatus`, `ReassignTailnet`, `BumpPolicyRevision`. `CreateTailnet` chỉ `INSERT` (không tranh khóa với fence).
- `internal/placement/audit.go` `mutate` (mọi thay đổi placement chạy trong một transaction), `internal/placement/quota.go` (advisory lock `placement/quota/…`, khác namespace).
- `cmd/bench-db/b3.go` là benchmark B3/L11 của P12b (CAS một dòng vs ghi lại mọi dòng), **không** liên quan B3 của review này.

## 2. Benchmark (mc-bench 7703, 8 vCPU Xeon E5-2696 v3, Go 1.27.4, PostgreSQL 16.16 trong Docker chỉ bind 127.0.0.1, `fsync=on`, `synchronous_commit=on`, `shared_buffers=128MB`)

`hscontrol/db/tenantscope/fence_bench_test.go`. `nofence` = cùng plugin nhưng gỡ 3 callback fence; `fence` = plugin đầy đủ. Mỗi worker ghi dòng riêng của mình, nên dòng dùng chung duy nhất giữa các worker là dòng `tailnets` mà fence đọc. Raw output: `artifacts/203859a/P10c/` (không commit, `.gitignore`).

### 2.1 Chi phí fence mỗi lần ghi (PostgreSQL, `benchstat`, n=6)

| Ca | nofence | fence P10 (`9f315ec`, owner+epoch) | fence P10b (`1c21b42`, + status, deleted_at) | fence P10c (`203859a`, + advisory shared) |
|---|---|---|---|---|
| update tuần tự | 1,56 ms | 1,81 ms | 1,83 ms | 1,84 ms (~ so với P10b, p=0,49) |
| create tuần tự | 1,60 ms | 1,84 ms | 1,90 ms | 1,83 ms |
| tx 10 update | 3,88 ms | 6,05 ms | 6,21 ms | 6,28 ms (~, p=0,24) |
| update song song, 8 luồng, cùng tailnet | 344 µs | 386 µs | 383 µs | 390 µs (~, p=0,82) |
| update song song, 32 luồng, cùng tailnet | 148 µs | — | 193 µs | 197 µs (~, p=0,18) |

- Fence tốn khoảng **+0,23–0,25 ms mỗi câu ghi** (một round trip đọc trong transaction), +12% thông lượng song song ở 8 luồng, +30% ở 32 luồng trên **cùng một** tailnet.
- P10b (đọc thêm 2 cột) và P10c (thêm advisory lock) **không** đo được khác biệt so với P10.
- SQLite: mọi ca ~9,2–9,9 ms (bị chặn bởi fsync), fence không đo được khác biệt.
- **Ghi chú về một lượt đo lỗi:** lượt chạy tuần tự đầu tiên (before toàn bộ rồi mới after) cho fence P10c +97% ở ca tuần tự. Tôi kiểm tra bằng pgbench (SQL thuần: `FOR SHARE` 1,31–1,34 ms, dạng mới 1,37–1,38 ms, WAL mỗi transaction đều 171 byte) và bằng một benchmark Go riêng cho 4 dạng câu (đều ~1,85 ms), rồi chạy lại xen kẽ before/after 3 vòng: không có khác biệt. Lượt đầu là nhiễu của lần chạy đó, không tái hiện được; số trong bảng lấy từ lượt xen kẽ (`artifacts/203859a/P10c/rerun/`).

### 2.2 Phát hiện chính: thay đổi ownership bị starvation

`BenchmarkFenceOwnerMoveUnderLoad`: N luồng ghi liên tục vào tailnet A; đo một lần `UPDATE tailnets SET owner_epoch = …` (giống `MoveOwner`/`SetTailnetStatus`).

| Luồng ghi | Trước (P10, `UPDATE` thuần) | Sau (P10c, `LockOwnership` + `UPDATE`) |
|---|---|---|
| 0 | 2,8 ms | 2,1 ms |
| 8 | **26,6 / 61,0 / 76,1 / 136,0 / 32,4 / 83,4 s** (6 lần) | **3,6 ms** ± 11% |
| 32 | 8,9 s (1 lần; lượt đo bị dừng sau hơn 1 giờ) | **6,7 ms** ± 4% |

Nguyên nhân: khóa dòng `FOR SHARE` không xếp hàng. Một `FOR SHARE` mới gia nhập nhóm người giữ hiện tại (MultiXact) kể cả khi đã có `UPDATE` đang chờ, nên `UPDATE` chỉ chạy được khi tình cờ không còn ai giữ. Dưới tải ghi đều thì lúc đó gần như không đến. Đây là đường failover/chuyển tenant (N2) và suspend/delete (A1 của P10b).

## 3. Thay đổi

| Path / symbol | Trước | Sau |
|---|---|---|
| `tenantscope.go` `checkFence` (PostgreSQL) | `SELECT … FROM tailnets WHERE id = $1 FOR SHARE` | Cùng câu, thêm `pg_advisory_xact_lock_shared(hashtextextended('tenantscope/ownership/' \|\| $1, 0))` trong `FROM`. `LockRows` nằm trên join nên advisory lock luôn lấy trước khóa dòng (đã xem `EXPLAIN`). Một round trip như cũ. `FOR SHARE` giữ nguyên làm lớp an toàn |
| `tenantscope.go` `LockOwnership`, `OwnershipLockTimeout` (mới) | — | Trên PostgreSQL: `SET LOCAL lock_timeout = '5000ms'`, rồi `pg_advisory_xact_lock` cùng key. Hàng đợi khóa nặng của PostgreSQL công bằng: khi thay đổi đang chờ, ghi mới xếp sau nó. Thay đổi chạy ngay khi các ghi đang dở commit; ghi sau đó đọc owner/epoch/status mới và bị `ErrFenced` nếu không còn hợp lệ. SQLite: no-op (SQLite tuần tự hóa ghi) |
| `internal/placement/tailnets.go` `setStatus`, `MoveOwner` | `UPDATE tailnets` trực tiếp | `LockOwnership` là câu **đầu tiên** của transaction `mutate` (trước `lockClusters`, nên không có thứ tự khóa ngược với ghi tenant) |
| `hscontrol/db/platform_admin.go` `SetTailnetStatus`, `ReassignTailnet`, `BumpPolicyRevision` | Một câu `Updates` | Transaction tường minh: `LockOwnership` rồi `Updates`; lỗi và giá trị trả về giữ nguyên |
| `tenantscope_test.go` helpers | `*testing.T` | `testing.TB` (dùng chung cho benchmark) |

**An toàn khi thiếu một chỗ gọi:** nếu sau này có code `UPDATE tailnets` mà quên `LockOwnership`, fence vẫn đúng (vẫn có `FOR SHARE`), chỉ có thể bị starvation như trước.

**Deadlock:** ghi tenant chỉ giữ advisory shared của đúng một tailnet (scope một tailnet mỗi transaction) và lấy lại trong cùng transaction là re-entrant. Bên thay đổi lấy advisory exclusive trước mọi khóa khác. Không có thứ tự khóa ngược.

## 4. Test — RED trước khi sửa, GREEN sau khi sửa (mc-bench, PostgreSQL 16.16)

| Test | RED | GREEN `203859a` |
|---|---|---|
| `TestOwnershipChangeNotStarvedByWriters` (16 luồng ghi liên tục, 20 lần thay đổi, ngưỡng 2 s mỗi lần) | Chạy trên `203859a` với `LockOwnership` bị tắt (`if true { return nil }`): **exit 1**, `slowest ownership change took 7.841423503s under write load`, test mất 43,8 s | PASS `-count=3`: chậm nhất 21 / 12 / 13 ms; 965–1046 ghi commit trong lúc thay đổi |
| `TestOwnershipChangeBoundedByLockTimeout` (một transaction ghi treo, `OwnershipLockTimeout` 300 ms) | — | PASS `-count=3`: thay đổi thất bại sau ~0,39 s, `owner_epoch` không đổi |
| Fence hiện có (`TestFenceStaleEpochRollsBack`, `TestFenceInactiveTailnetRollsBack`, …) | — | PASS trên SQLite và PostgreSQL |

| Lệnh | Commit | Exit |
|---|---|---|
| `go build ./... && go vet ./hscontrol/db/... ./internal/placement/` | `203859a` | 0 |
| `gofmt -l hscontrol/db internal/placement` | `203859a` | sạch |
| `go test -race -count=1 ./hscontrol/db/... ./internal/...` (SQLite) | `203859a` | 0 |
| như trên với `MESH_TEST_POSTGRES_DSN` (PostgreSQL 16.16) | `203859a` | 0: `hscontrol/db` 88 s, `tenantscope`, `placement`, `placementapi`, `tailnet` ok |
| `go test -run '^$' -bench … -benchtime 2s -count 6` (bảng §2) | `1c21b42` / `203859a` | 0 |

| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` (mc-ci) | `1bfd04e` | 0, 131 s |
| `nix shell … -c go test -race -count=1 -p 8 -timeout 60m ./hscontrol/... ./internal/... ./cmd/...` (mc-ci) | `1bfd04e` | 31 package ok; `servertest` bị **tôi** dừng bằng SIGQUIT ở phút 21 vì tưởng treo (thực ra package này chạy ~34 phút, như ở P10b: 2051 s) |
| `nix shell … -c go test -race -count=1 -timeout 90m ./hscontrol/servertest/` (chạy lại riêng) | `1bfd04e` | 0, 2014 s. Cộng lại: 32 package ok |

## 4b. E2E client thật

`infra/lab/e2e/p05/run.sh`, binary build từ `876b60f` trên mc-ci (sha256 `1cf1e125a75e44cb…`), `tailscale/tailscale:v1.104.7`.

| Lần | Harness | Artifact | Exit | Kết quả |
|---|---|---|---|---|
| 1 | `876b60f` | `artifacts/876b60f/P05/e2e-20261123T114557Z/` | 0 | 19/19 PASS, 365 s |
| 2 | `876b60f` | `artifacts/876b60f/P05/e2e-20261123T115202Z/` | **1** | 18/19: **S18** `deleted tenant's node e1 is Running again` |
| 3 | `5dba471` | `artifacts/5dba471/P05/e2e-20261123T115919Z/` | 0 | **19/19 PASS**, 411 s |
| 4 | `5dba471` | `artifacts/5dba471/P05/e2e-20261123T120610Z/` | 0 | **19/19 PASS**, 366 s |

Tất cả: `cleanup_ok=1`, 0 key chưa redact.

**Phân tích S18 ở lần 2 (không phải lỗi cô lập):** log của e1 (`logs/p05-e1.tailscaled.log`) cho thấy:
- Sau khi tenant e bị DELETING, mọi request của e1 bị từ chối: `PollNetMap: initial fetch failed 401: unauthorized`, rồi sau restart `this server only registers nodes with an auth key`.
- Sau `docker restart`, tailscaled 1.104 nạp netmap đã lưu trên đĩa (`Start: loaded netmap from disk cache; 0 peers`, `control-netmap … cached=true`) và tự chuyển `Starting -> Running` mà không có câu trả lời nào từ control.
- e2 chỉ có node của mình và không thấy e1.

Tiêu chí cũ (`BackendState != Running`) phụ thuộc việc client kịp ghi cache trước khi bị từ chối. Commit `5dba471` đổi tiêu chí sang phía server: sau restart, e1 không được nhận netmap nào từ control (`control-netmap … cached=false`), phải thấy dòng từ chối, và không có peer. State của client chỉ còn được ghi lại. Một node được phục vụ thật sẽ sinh dòng `cached=false` như ở lần đăng ký đầu của e1, nên tiêu chí mới vẫn bắt được lỗi thật.

Các lần chạy chậm hơn P10b (365–411 s so với 192 s) ở **mọi** bước, kể cả bước chép binary sang mc-cp (74 s so với 5 s), nên nguyên nhân là mạng hoặc máy lúc chạy, không phải code.

## 5. Known limitations / còn mở

- Fence đọc dòng `tailnets` **mỗi câu ghi**: tx 10 câu tốn 10 lần đọc (+2,3 ms). Có thể nhớ kết quả fence trong transaction (đã giữ khóa shared tới commit nên vẫn đúng); chưa làm.
- `OwnershipLockTimeout` 5 s là giá trị chọn, chưa đo trên tải thật. Trong lúc thay đổi chờ, ghi mới của tailnet đó cũng chờ tối đa bấy nhiêu.
- Benchmark chạy thẳng vào PostgreSQL, không qua PgBouncer; advisory xact lock và `SET LOCAL` hợp lệ ở transaction mode (ADR 0006), nhưng chưa đo qua PgBouncer (P14).
- Tải benchmark là ghi liên tục không nghỉ (xấu nhất); tải thật thưa hơn.
- Review A3 (limit shared tenant chung config), A5–A10, B2, B4 vẫn mở.
- **Reviewer approval:** chưa.
