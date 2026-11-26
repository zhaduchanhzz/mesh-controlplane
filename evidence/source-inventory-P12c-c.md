# Source Inventory — P12c nhóm C (thứ tự chuyển tenant, sức chứa)

- **Commit gốc:** `1ffffc1` (`feat/P12c-review-hardening`, từ `master` `dece058`), ngày 2026-11-26.
- **Nhánh:** `feat/P12c-c`. Không migration (chỉ nhóm A thêm migration). Không đổi ADR 0003/0005/0010.
- **ID:** C1, C2, C3, L7 (`tickets/P12c.md`, `evidence/REVIEW-2026-11-26-tickets.md` §Nhóm C, L7).
- **Trạng thái:** `Planned`.

## 1. Đã đọc

| Nguồn | Path / symbol | Nội dung liên quan |
|---|---|---|
| Chính sách | `AGENTS.md` rule 5, 7, 11, 13 | inventory trước code; không migration; lỗi isolation = BLOCK MERGE |
| Ticket | `tickets/P12c.md` §Nhóm C; `tickets/P12b.md` (Q2: `tailnet_moves`, trạng thái, B3 chấp nhận thời gian chặn writer của `MoveOwner`) | RED bắt buộc C1–C3, L7 |
| Review | `evidence/REVIEW-2026-11-26-tickets.md` C1, C2, C3, L7 | kịch bản và vị trí |
| ADR | `design/adr/0010-tailnet-serving-claim.md` | yêu cầu → nguồn retire + drain → `HandOver`; bất biến 1: owner chỉ đổi sau khi runtime của owner cũ đã ngừng; hủy không tăng epoch |
| ADR | `design/adr/0003-*.md`, `0005-*.md` (Bổ sung 2) | shared: quota 5.000/tailnet + `max_devices` cluster (P10d); dedicated: một tenant |
| Cluster | `hscontrol/cluster.go`: `Cluster.{mu,runtimes,retiring,handingOver,leaseMu,stopping}`, `reconcile` (`:374`), `load` (`:276`), `retire` (`:583`), `handOverRequested` (`:628`), `handOver` (`:677`), `takeOverOrphans` (`:698`), `stopRuntime` (`:724`), `onLeaseAcquired`/`onLeaseLost`/`waitStopping` (`:999–1092`) | mọi lần nạp runtime chạy dưới `leaseMu` (reconcile, onLeaseAcquired); goroutine trao tay (`c.stopping.Go`) gọi `HandOver` **không** giữ `leaseMu`; `retire` ghi đè `c.retiring[id]` |
| Cluster | `hscontrol/inflight.go` `inflightGate` | drain chờ request đã nhận; cách giữ drain trong servertest |
| DB | `hscontrol/db/handover.go`: `PendingHandovers`, `OrphanedIncoming`, `HandOver`, `TakeOver`, `flip` | CAS dưới `LockOwnership`, lease sống; không kiểm lại dedicated |
| DB | `hscontrol/db/platform.go` `OwnedTailnets` (`moving_to_cluster_id IS NULL`), `hscontrol/db/tailnet_moves.go` `MoveNotices` | tailnet đang chuyển không được nạp |
| DB | `hscontrol/db/tenantscope/tenantscope.go` `LockOwnership`, `checkFence`, `ownershipLockPrefix` (chỉ đọc; file của nhóm A/B) | ghi có fence lấy advisory **shared** `tenantscope/ownership/<id>` rồi `FOR SHARE`; đổi owner lấy exclusive |
| DB | `hscontrol/db/schema_postgres.sql` `tailnets`, `tailnet_moves` (`idx_tailnet_moves_open` unique theo tailnet khi SCHEDULED/REQUESTED) | không cần cột mới |
| DB | `hscontrol/db/raw_write_guard_test.go` | guard hiện chỉ xét ghi; nhóm A mở rộng sang đọc (allowlist theo câu) |
| State | `hscontrol/state/state.go:3065–3075` | `enrollGate` là câu đầu tiên của transaction chèn node, trước câu ghi có fence |
| Routing | `hscontrol/session_route.go` `registerRefusal`/`isTenantRefusal`, `hscontrol/auth.go:497` (chỉ đọc) | lỗi không phải quota/tenant → 503, client thử lại |
| Placement | `internal/placement/tailnets.go`: `CreateTailnet`, `CreateTailnetOn`, `admits` (`:160`), `MoveOwner` (`:304`), `moveRefused`, `CancelMove` (`:389`), `deviceCount` | `admits` chỉ đếm `owner_cluster_id = c` |
| Placement | `internal/placement/clusters.go`: `place` (`:275`), `lockClusters`, `clusterUsage` (`:421`), `liveLeases`, `DeleteCluster` (đếm cả `moving_to`) | `place` dùng `clusterUsage` chỉ theo owner |
| Placement | `internal/placement/quota.go`: `EnrollmentAllowedTx` (khóa `placement/quota/<tailnet>`), `clusterBudget` (khóa `placement/cluster/<cluster>`) | ngân sách cluster chỉ đếm tailnet đang sở hữu |
| Placement | `internal/placement/moves.go`: `ScheduleMove`, `setMoveState`, `CancelScheduledMove` (`:218`), `RunDueMoves` (`:287`), `requestMove` (`:378`) | hai transaction tách rời; REQUESTED: hủy không kiểm epoch/`RowsAffected`; kết quả suy từ owner hiện tại, `tn.UpdatedAt` |
| Placement | `internal/placement/reconcile.go` R7/R8 (`clusterUsage` theo owner), R10 (`tailnets.updated_at`) | R7/R8 là trạng thái thật, giữ theo owner |
| Placement | `internal/placement/audit.go` `mutate`, `placement.go` `Registry.step`/`failAt` | audit trong cùng transaction; hook chèn lỗi cho test |
| Test | `internal/placement/helpers_test.go` (`backends`: SQLite + PostgreSQL `postgrestest`), `p12b_moves_test.go`, `cluster_budget_test.go`, `p12_placement_test.go` `setLease`; `hscontrol/db/handover_test.go`; `hscontrol/servertest/cluster_p12_test.go` (`TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`), `cluster_p12.go`, `cluster_p10.go`, `cluster_p05.go` | mẫu test hai DB, lấy mẫu R11 mỗi 5 ms |

## 2. Ràng buộc hiện có

- R11 (ADR 0010, bất biến 1): tại mọi thời điểm nhiều nhất một cluster giữ runtime; owner chỉ đổi sau khi runtime của owner cũ ngừng (hoặc owner cũ không còn lease).
- Hủy move không tăng epoch (ADR 0010 §1). Không đổi.
- `LockOwnership` là câu đầu của mọi transaction đổi dòng `tailnets`. Thứ tự khóa hiện có trong quota: tailnet rồi cluster.
- Không migration; không sửa `gen/`; không đổi API OpenAPI (kiểu `ClusterUsage`, `Move` giữ nguyên).
- File của nhóm khác: chỉ đọc (`tenantscope`, `session_route.go`, `noise.go`, `app.go`, phần `load/start/onLease*` của `cluster.go`).

## 3. Thiết kế dự kiến (chưa code)

- **C1:** việc lật owner chỉ chạy khi giữ `leaseMu` (mọi lần nạp runtime cũng giữ khóa này) và không có runtime nào của tailnet trong `c.runtimes` hay đang drain: reconcile lật ngay khi không có gì để drain; goroutine chờ drain chỉ thử `leaseMu.TryLock` (không chặn failover/shutdown), nếu không được thì lượt reconcile kế làm. Runtime nạp lại sau hủy + yêu cầu lại bị retire như lần đầu. `retire` nối kênh `retiring` để kênh chỉ đóng khi **mọi** runtime đã retire của tailnet đã dừng. Không đổi epoch, không đổi ADR.
- **C2:** yêu cầu (`MoveOwner`) và `SCHEDULED→REQUESTED` trong **một** transaction (`LockOwnership` trước, đổi trạng thái có kiểm `RowsAffected` → hủy thắng); thất bại ghi FAILED ở transaction riêng cũng chỉ khi còn SCHEDULED.
- **C3:** sức chứa và dedicated đếm cả tailnet có `moving_to_cluster_id = C` (`admits`, `place`, `clusterBudget`); `MoveOwner` lấy khóa `placement/cluster/<đích>` như enrollment; enrollment lấy khóa shared sở hữu tailnet trước (giữ thứ tự sở hữu → cluster, tránh chu trình); enrollment vào tailnet đang chuyển bị từ chối (tạm thời, client thử lại); `HandOver`/`TakeOver` khóa dòng cluster đích và kiểm lại dedicated. `clusterUsage` cho báo cáo (R7/R8, `GetCluster`) giữ theo owner.
- **L7:** `HandOver`/`TakeOver` và `CancelMove` đóng move REQUESTED khớp (from, epoch, to) trong cùng transaction (thời điểm chính xác); `RunDueMoves` chỉ còn là đường dự phòng và quyết định theo epoch (`E` + `moving_to`, `E+1` + owner đích), không dùng `tn.UpdatedAt`; `CancelScheduledMove` REQUESTED kiểm owner + epoch và `RowsAffected`.

## 4. Giả định / chưa xác minh

- Nhóm B (B5) có thể đổi `load`/`start`; giả định mọi lần nạp runtime vẫn giữ `leaseMu`. Nếu đổi, C1 phải xem lại.
- Câu SQL thô đổi/thêm (đọc `nodes` để đếm) cần mục allowlist theo dạng mới của nhóm A (A4) khi tích hợp; danh sách ghi ở `evidence/P12c-c.md`.
- Khóa shared sở hữu trong enrollment dùng lại chuỗi `tenantscope/ownership/` (hằng không export trong file nhóm A); có test hành vi (PostgreSQL) bảo đảm khớp.
- R10 dựa trên `tailnets.updated_at` (L7 phần "reset tuổi R10") cần cột mới hoặc đổi nguồn tuổi: ngoài phạm vi nhóm C nếu cần schema; báo coordinator.
- "Hai process cùng chạy migration mới" (L7) thuộc migration (nhóm A); không làm.
