# PR P10b Evidence — sửa A1, A3 (kèm A4), test và sửa B1 của review P10

- **Repo commit before / after:** `0350f8b` (master, sau merge P09a) → `e58db1d` (code + test), `aa439ae` (fixture servertest), commit evidence (commit chứa file này). Nhánh `feat/P10b-fence-status-quota-tx`.
- **Nguồn:** `evidence/REVIEW-2026-11-23-P10-P08.md` (A1, A3, A4, B1). Chủ dự án yêu cầu sửa A1, A3 và test B1 (2026-11-23).
- **ADR:** không có ADR mới; không đổi thiết kế Noise.

## 1. Thay đổi

| ID | Path / symbol | Trước | Sau |
|---|---|---|---|
| A1 | `hscontrol/db/tenantscope/tenantscope.go` `checkFence` | Chỉ so `owner_cluster_id`, `owner_epoch` | Đọc thêm `status`, `deleted_at IS NULL` trong cùng câu (PostgreSQL: `FOR SHARE`); không `ACTIVE` hoặc đã xóa mềm → `ErrFenced` |
| A3 | `hscontrol/state/state.go` `EnrollGate`, `SetEnrollGate`, `createAndSaveNewNode` | Quota kiểm trong transaction ngắn riêng, trước khi chèn node | Hook chạy **trong** transaction chèn node mới |
| A3 | `hscontrol/cluster.go` `load` | — | Mỗi runtime cài `registry.EnrollmentAllowedTx` vào hook: advisory lock (PostgreSQL) giữ tới commit, SQLite đã tuần tự bởi `BEGIN IMMEDIATE` |
| A3 | `hscontrol/auth.go` `handleRegisterWithAuthKey` | — | `placement.ErrQuotaExceeded` từ transaction → `RegisterResponse.Error` "this tailnet has reached its device limit" (cùng câu với kiểm sớm) |
| A4 | `cluster.go` `runtimeForRegister` | `checkQuota` trước `runtimeFor` | `runtimeFor` trước: tailnet không phục vụ ở đây nhận từ chối chung, không tốn truy vấn quota |
| B1 | `hscontrol/inflight.go` `inflightGate` (mới) | — | Đếm request đang phục vụ; `enter` sau khi đóng = từ chối; `drain(timeout)` |
| B1 | `hscontrol/session_route.go` `noiseServer.enter`; `hscontrol/noise.go` (register, map, set-dns, ssh action, c2n) | Handler chạy trên `h.state` mà runtime không biết | Mỗi handler, sau khi bind, vào cổng của runtime và ra khi kết thúc (map long-poll cũng vậy); runtime đã retire → `errRuntimeRetired` (503, client nối lại) |
| B1 | `cluster.go` `retire`, `stopRuntime`, `shutdownRuntimes`, `runtimeDrainTimeout` | `retire` đồng bộ trong goroutine reconcile duy nhất; `stopRuntime` chỉ chờ map stream (không timeout) rồi đóng State | `retire` gỡ khỏi routing, đánh dấu retired rồi dừng runtime **ở nền** (`stopping` WaitGroup; shutdown chờ hết). `stopRuntime` drain cổng (30 s) trước khi đóng State; quá hạn → log lỗi, **để State mở** (an toàn hơn handler trên State đã đóng; ghi vẫn bị fence chặn) |
| B1 | `state.go` `State.Closed` | — | Accessor cho test |

## 2. Test — RED trước khi sửa, GREEN sau khi sửa (mc-ci, Go 1.27.4)

RED chạy trên `0350f8b` cộng **chỉ** các file test và `state.go` (hook + accessor, chưa có code sửa).

| Test | Lỗi | RED (exit 1) | GREEN `e58db1d` |
|---|---|---|---|
| `TestFenceInactiveTailnetRollsBack` (SUSPENDED, DELETING, PENDING, soft-deleted) | A1 | Mọi loại ghi (`Create`, `Update`, `Save`, `Delete`, `Transaction`) thành công: `err=<nil>, want ErrFenced` | PASS, chạy với `MESH_TEST_POSTGRES_DSN` (PostgreSQL 16.15 tạm) |
| `TestClusterQuotaExactUnderConcurrentEnrollment` (limit 3, 10 đăng ký đồng thời) | A3 | **9 node** được tạo, 9 đăng ký được chấp nhận | PASS `-race -count=5`: đúng 3 node, 7 lần từ chối đều ghi "device limit" |
| `TestClusterQuotaNotBypassedByNodeOfOtherTailnet` | A2 (P09a) | (đã RED ở P09a) | PASS `-race -count=5` |
| `TestP10bRetireWaitsForInFlightRequest` | B1 | Không có RED tự động: xem ghi chú | PASS `-race -count=5`: `retire` trả về < 1 s; runtime ra khỏi routing ngay; `enter` mới → `errRuntimeRetired`; State **không** đóng khi request còn chạy; đóng sau khi request xong |
| `TestP10bRetireDrainTimeoutLeavesStateOpen` | B1 | — | PASS: request treo → stop bỏ cuộc sau timeout, State để mở |
| `TestP10bInflightGate` | B1 | — | PASS |

**Ghi chú B1 (RED):** bản servertest đầu tiên (giữ một đăng ký trong transaction chèn node rồi đổi owner) không chạy được trên SQLite: transaction `BEGIN IMMEDIATE` đang bị giữ chặn mọi ghi khác (`database is locked (SQLITE_BUSY)` khi đổi owner). Test được chuyển xuống mức unit trong package `hscontrol`, dùng đúng đường vào cổng của handler (`noiseServer.enter`). Code cũ không có cơ chế nào để quan sát request đang chạy, nên không có RED tự động; lỗi B1 được xác nhận bằng đọc code: `stopRuntime` cũ chỉ `clientStreamsOpen.Wait()` rồi `state.Close()` (dừng `NodeStore`, đóng auth cache) trong khi register/SSH action/set-dns có thể đang chạy.

| Lệnh khác | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go build ./...`; `go vet ./hscontrol/ ./hscontrol/state/ ./hscontrol/db/tenantscope/`; `go vet -tags p05cluster ./hscontrol/servertest/` | `e58db1d` | 0 | |
| `go test -race -count=1 ./hscontrol/db/... ./internal/...` (SQLite + PostgreSQL 16.15) | `e58db1d` | 0 | 6 package ok |
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | `e58db1d` | **1** | `TestClusterRefusesForeignOrInactiveTailnet`: harness seed user vào tailnet `SUSPENDED` qua handle có fence, đúng loại ghi A1 giờ chặn. Lỗi của fixture, không phải của code: sửa harness (`aa439ae`) seed khi tailnet còn `ACTIVE` rồi mới đặt trạng thái, như operator làm |
| như trên | `aa439ae` | 0 | 132 s |
| `nix shell … -c go test -race -count=1 -p 8 -timeout 60m ./hscontrol/... ./internal/... ./cmd/...` | `aa439ae` | **0** | 32 package ok, gồm toàn bộ `servertest` và API v2. Lần chạy đầu ở `e58db1d` bị dừng giữa chừng sau khi suite cluster lộ lỗi fixture |

## 3. E2E client thật

`infra/lab/e2e/p05/run.sh`, binary build từ `aa439ae` trên mc-ci (sha256 `1e082210549d864f…`), `tailscale/tailscale:v1.104.7`, chạy sau khi race suite xong (không chạy song song).

| Lần | Artifact | Exit | Kết quả |
|---|---|---|---|
| 1 | `artifacts/aa439ae/P05/e2e-20261123T071951Z/` | 0 | **19/19 PASS**, 192 s, `cleanup_ok=1`, 0 key chưa redact |
| 2 | `artifacts/aa439ae/P05/e2e-20261123T072303Z/` | 0 | **19/19 PASS**, 191 s, `cleanup_ok=1`, 0 key chưa redact |

Các ca liên quan trực tiếp: F02 (suspend B từ chối thiết bị mới, resume b1 tự nối lại) và S18 (DELETING) vẫn PASS với fence mới; S16 (restart cluster) PASS với retire bất đồng bộ; R05/R06 (từ chối key) không đổi thông báo.

## 4. Known limitations / còn mở

- A3: limit chung cho shared tenant vẫn là mặc định 5000 trong `NewCluster` (`placement.Config{}`), có thể khác `mesh-placement -shared-tenant-limit`. Nên đưa vào config chung (chưa làm).
- A3 trên PostgreSQL ở đường cluster: servertest chạy SQLite; `EnrollmentAllowedTx` + advisory lock trên PostgreSQL đã có test trong `internal/placement`, chưa có test đăng ký đồng thời end-to-end trên PostgreSQL.
- A1 làm mọi ghi của tailnet không `ACTIVE` thất bại, kể cả GC nền của runtime chưa kịp retire (mong muốn). Ghi của operator lúc tạo tailnet (`CreateTailnet`, SQL thô đã allowlist) không đi qua fence nên không bị ảnh hưởng.
- B1: runtime bị bỏ lại khi quá `runtimeDrainTimeout` là rò tài nguyên có chủ đích (log `retired tailnet runtime still has requests in flight`); cần alert vận hành.
- Review A5–A10, B2–B4 vẫn mở (`evidence/REVIEW-2026-11-23-P10-P08.md`), gồm benchmark chi phí fence (B3), nay câu fence đọc thêm 2 cột.
- **Reviewer approval:** chưa.
