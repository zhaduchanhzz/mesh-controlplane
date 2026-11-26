# PR P12c nhóm C Evidence — thứ tự chuyển tenant, sức chứa (C1, C2, C3, L7)

- **Repo commit before / after:** `1ffffc1` (`feat/P12c-review-hardening`) → `feat/P12c-c` (đầu nhánh: xem `git log 1ffffc1..feat/P12c-c`). Đã push lên `mcci`, chưa lên `origin`, chưa merge.
- **Verified source paths & symbols:** `evidence/source-inventory-P12c-c.md` (commit `f4aefa1`, trước mọi thay đổi code).
- **Design choice / ADR:** không đổi ADR 0003/0005/0010; không migration; không đổi OpenAPI (`gen/` không đụng). Chi tiết từng ID dưới đây.
- **Trạng thái:** `Verified` (unit/race SQLite + PostgreSQL, servertest `p05cluster`). Chưa `Reviewed`.

## Thay đổi

| ID | File | Thay đổi |
|---|---|---|
| C1 | `hscontrol/cluster.go` (chỉ `retire`, `handOverRequested`, `awaitStopped`, `tryHandOver`, `handOver`, `isClosed`) | Lật owner chỉ chạy khi giữ `leaseMu` (mọi lần nạp runtime cũng giữ khóa này) **và** tailnet không có runtime trong routing hay đang dừng. `handOverRequested` retire mọi runtime đang phục vụ của tailnet có yêu cầu chuyển, kể cả runtime nạp lại sau hủy + yêu cầu lại (bỏ nhánh `handingOver` bỏ qua). Không còn gì để drain → lật ngay trong reconcile; còn drain → một goroutine chờ, xong thì `TryLock` `leaseMu` (thăm dò tối đa một `reconcileInterval`, bỏ ngay khi process không còn phục vụ: không bao giờ chặn đổi lease/shutdown), không được thì lượt reconcile kế lật. `retire` nối kênh `retiring`: kênh đóng khi **mọi** runtime đã retire của tailnet đã dừng. Hủy vẫn không tăng epoch (ADR 0010 giữ nguyên). |
| C1 | `hscontrol/inflight_hook.go` (mới) | `Headscale.HoldRequestForTest`: giữ một request trong runtime để servertest giữ drain. |
| C2 | `internal/placement/moves.go`, `tailnets.go` | `MoveOwner` tách thành `moveOwnerTx`. `RunDueMoves` làm yêu cầu (kiểm + CAS) và `SCHEDULED→REQUESTED` trong **một** transaction dưới `LockOwnership` (`CancelScheduledMove` cũng lấy khóa này): hủy trước thì không có yêu cầu nào; hủy sau thì thấy REQUESTED và rút yêu cầu. Yêu cầu bị từ chối ghi FAILED ở transaction thứ hai, cũng chỉ khi còn SCHEDULED. Lỗi DB/khóa không đổi gì, thử lại lượt sau. Dòng audit `MoveOwner` giữ nguyên, trong cùng transaction. Hủy SCHEDULED **không** xóa `moving_to` (không cần nữa; một yêu cầu operator tự đặt bằng tay vẫn của operator). |
| C3 | `internal/placement/tailnets.go` `admits`; `clusters.go` `place`/`clusterUsage(db, incoming)`; `quota.go` | Sức chứa và luật dedicated đếm cả tailnet có `moving_to_cluster_id = C` (move, `CreateTailnetOn`, placement, ngân sách enrollment shared). `GetCluster`/`ListClusters`/R7/R8 vẫn theo owner. `MoveOwner` sang cluster khác lấy khóa ngân sách `placement/cluster/<đích>` (cùng khóa enrollment). Enrollment lấy khóa sở hữu tailnet **shared** trước (`tenantscope/ownership/<id>`, khóa fence vẫn lấy sau đó) → mọi transaction khóa sở hữu trước khóa cluster, không có chu trình chờ. Tailnet đang có yêu cầu chuyển không enroll thiết bị mới: `ErrTailnetMoving` (tạm thời, không phải quota → 503/thử lại; API quota trả `allowed=false` + `reason`). |
| C3 | `hscontrol/db/handover.go` | `HandOver`/`TakeOver` khóa dòng `clusters` của đích (`FOR UPDATE`, PostgreSQL) và từ chối lật vào cluster dedicated đang sở hữu tailnet sống khác (`ErrHandoverStale`). Sức chứa **không** kiểm lại khi lật: giới hạn mềm, từ chối ở đó làm tailnet không ai phục vụ; nó được giữ chính xác từ lúc yêu cầu (đếm tailnet đang tới + enrollment bị chặn khi đang chuyển). |
| L7 | `hscontrol/db/handover.go` `recordHandedOver`; `tailnets.go` `CancelMove`; `moves.go` `moveOutcome`/`recordOutcome`/`CancelScheduledMove` | Kết quả ghi nơi nó xảy ra: lật owner đóng move REQUESTED khớp (from, epoch, to) thành HANDED_OVER trong cùng transaction, giờ lật; `CancelMove` đóng move đã tạo yêu cầu thành CANCELLED. `RunDueMoves` chỉ còn dự phòng, quyết định theo epoch (epoch E + yêu cầu còn: chờ; E không yêu cầu: CANCELLED; E+1 owner = đích: HANDED_OVER; khác: FAILED kèm owner/epoch, không đoán), ghi dưới `LockOwnership` sau khi đọc lại, giờ ghi = giờ scheduler (không dùng `tailnets.updated_at`); move đã đóng giữa chừng thì bỏ qua, không lỗi. `CancelScheduledMove` REQUESTED đòi owner **và** epoch = của move, chỉ rút yêu cầu của chính nó, kiểm `RowsAffected`. |

## Tests + exact commands + exit codes (mc-ci, Go 1.27.4, PostgreSQL 16 qua `postgrestest`)

Chung: `cd ~/src/p12c-c[-b] && export PATH=/usr/local/go/bin:/usr/lib/postgresql/16/bin:$PATH GOFLAGS=-buildvcs=false CGO_ENABLED=1`.

| ID | Test RED | Commit RED | Lệnh RED → exit | Commit GREEN | Lệnh GREEN → exit |
|---|---|---|---|---|---|
| C1 | `hscontrol/servertest/cluster_p12c_move_test.go` (tên file đổi khi tích hợp) `TestTwoClustersCancelReloadRerequestKeepsR11` (lấy mẫu 5 ms: owner = c2 khi c1 còn runtime; cả hai cùng giữ runtime) | `e4593fd` (bản đầu **không** RED: nhả drain sau 3 s, lúc đó reconcile của nguồn đã retire runtime nạp lại vì không còn trong `OwnedTailnets`; giữ lại để minh bạch) → `a3ce13d` (nhả drain ngay sau yêu cầu lại) | `go test -race -count=1 -tags p05cluster -run '^TestTwoClustersCancelReloadRerequestKeepsR11$' -v ./hscontrol/servertest/` ×3 → **exit 1** cả 3 lần: `both loaded 256; owner c2 while c1 served 259` / `0; 266` / `0; 246` | `3bbee31`, `84ddf6f` | cùng lệnh ×3 → **exit 0** cả 3: `both loaded 0; owner c2 while c1 served 0` |
| C2 | `internal/placement/p12c_moves_test.go` `TestRunDueMovesCancelBetweenRequestAndRecord` (hook `move-due`: hủy giữa yêu cầu và ghi) | `6c2427e` | `go test -race -count=1 -run '^TestRunDueMovesCancelBetweenRequestAndRecord$' -v ./internal/placement/` → **exit 1** (sqlite + postgres: `RunDueMoves` trả lỗi, `moving_to` vẫn `c2` sau khi move CANCELLED) | `b474b04` | `go test -race -count=1 ./internal/placement/...` → **exit 0** |
| C3 | `internal/placement/p12c_capacity_test.go`: `TestMoveIntoDedicatedCountsIncoming`, `TestMoveCapacityCountsIncoming`, `TestEnrollmentRefusedWhileMoving`, `TestMoveSerialisesWithEnrollmentIntoTarget` (PostgreSQL); `hscontrol/db/handover_p12c_test.go` `TestHandOverRefusesSecondTenantOnDedicated` | `4b2d3cf` | `go test -race -count=1 -run '^(TestMoveIntoDedicatedCountsIncoming\|TestMoveCapacityCountsIncoming\|TestEnrollmentRefusedWhileMoving\|TestMoveSerialisesWithEnrollmentIntoTarget)$' -v ./internal/placement/` → **exit 1** (cả sqlite + postgres; move thứ hai vào dedicated được nhận; 4+6+6 > 10 được nhận; tailnet đang chuyển vẫn enroll; move không chờ enrollment đang bay); `go test -race -count=1 -run '^TestHandOverRefusesSecondTenantOnDedicated$' -v ./hscontrol/db/` → **exit 1** | `693ac51`, `ce47db3` (API quota), `92d67ad` (SQL literal) | placement + placementapi → exit 0 (sau `ce47db3`); `go test -race -count=1 -p 4 ./hscontrol/db/...` → **exit 0**. Thêm `TestEnrollmentTakesOwnershipLockFirst` (PostgreSQL: khóa shared của enrollment trùng khóa `LockOwnership`) |
| L7 | `p12c_moves_test.go`: `TestRunDueMovesOutcomeByEpoch`, `TestCancelScheduledMoveRequestedChecksEpoch`, `TestCancelMoveClosesPlannedMove`; `handover_p12c_test.go` `TestHandOverRecordsPlannedMove` | `1ab7e7e` | `go test -race -count=1 -run '^(TestRunDueMovesOutcomeByEpoch\|TestCancelScheduledMoveRequestedChecksEpoch\|TestCancelMoveClosesPlannedMove)$' -v ./internal/placement/` → **exit 1**; `go test -race -count=1 -run '^TestHandOverRecordsPlannedMove$' -v ./hscontrol/db/` → **exit 1** (cả hai backend) | `71a115d` | `go test -race -count=1 ./internal/placement/...` → **exit 0**; `go test -race -count=1 -run 'Handover\|HandOver\|TakeOver\|Move\|Tailnet' ./hscontrol/db/` → **exit 0** |

### Kiểm tra cuối trên đầu nhánh

Chạy ở `92d67ad` (`~/p12c-c-final.sh`, log `~/p12c-c-final.log` trên mc-ci); `f932e90` sau đó chỉ sửa định dạng lint (một dòng trống, nhóm import).

| Lệnh | Exit | Kết quả |
|---|---|---|
| `go vet ./internal/placement/... ./hscontrol/db/... ./hscontrol/` | 0 | |
| `go vet -tags p05cluster ./hscontrol/servertest/` | 0 | |
| `go test -race -count=1 -timeout 30m ./internal/placement/...` | 0 | placement, placementapi ok |
| `go test -race -count=1 -p 4 -timeout 40m ./hscontrol/db/...` | 0 | db, sqliteconfig, tenantscope ok |
| `go test -race -count=1 -timeout 30m ./hscontrol/` | 0 | gồm `TestP10b*` (retire/drain) |
| `go test -race -count=1 -p 4 -timeout 40m -tags p05cluster -v -run '^(TestTwoClusters.*\|TestScheduledMove.*\|TestFailoverInOneClusterLeavesTheOtherAlone\|TestClusterRecordsNoiseKeyAndRefusesMismatch)$' ./hscontrol/servertest/` | 0 | 7/7 PASS; `TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`: R11 overlap max 0 s, target_loaded max 3,77 s (n=6); `TestTwoClustersCancelReloadRerequestKeepsR11`: both 0, owner c2 khi c1 phục vụ 0 |
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run ./internal/placement/... ./hscontrol/db/...` (ở `f932e90`) | 0 | 0 issues |
| `… golangci-lint run --build-tags p05cluster ./hscontrol/ ./hscontrol/servertest/` (ở `f932e90`) | 1 | 20 issue có sẵn (wsl_v5 17, usetesting 2, whitespace 1), **0** ở file nhóm C (`cluster.go`, `inflight_hook.go`, `cluster_p12c_test.go`) |

Không chạy toàn bộ suite `p05cluster` (coordinator chạy khi tích hợp).

## Negative tenant isolation tests

Nhóm C không đổi đường dữ liệu tenant. R11 (bộ nhớ) và bất biến 1 của ADR 0010 được kiểm bằng `TestTwoClustersCancelReloadRerequestKeepsR11` và chạy lại `TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11` (ghi của nguồn sau trao tay vẫn `ErrFenced`). Không phát hiện owner kép dữ liệu hay ghi bởi process đã bị fence trong code hiện có.

## DB migrations and downgrade constraints

Không migration, không đổi schema. Câu SQL thô đổi/thêm (để nhóm A cập nhật allowlist theo câu của guard A4 khi tích hợp):

| File | Câu | Bảng tenant |
|---|---|---|
| `internal/placement/tailnets.go` `admits` | đếm tailnet + node, nay `(owner_cluster_id = ? OR moving_to_cluster_id = ?)` | đọc `nodes` |
| `internal/placement/clusters.go` `clusterUsage` | thêm biến thể `UNION ALL` theo `moving_to_cluster_id` (hai câu literal) | đọc `nodes` |
| `internal/placement/quota.go` `clusterBudget` | đếm node, nay gồm tailnet đang tới | đọc `nodes` |
| `internal/placement/quota.go` | `pg_advisory_xact_lock_shared(hashtextextended(? \|\| ?, 0))`; `lockClusterBudget` (tách từ `clusterBudget`) | không |
| `internal/placement/tailnets.go` `CancelMove`, `moves.go` `CancelScheduledMove` | `UPDATE tailnet_moves …`, `UPDATE tailnets SET moving_to_cluster_id = NULL …` (thêm điều kiện epoch) | không |
| `hscontrol/db/handover.go` | `HandOver`/`TakeOver` + `dedicatedFree` (hằng); `lockTargetCluster` (`SELECT id FROM clusters … FOR UPDATE`); `recordHandedOver` (`UPDATE tailnet_moves`) | không |

## Benchmark

Không phải hot path. `MoveOwner` thêm một advisory lock (chờ enrollment đang bay vào cùng cluster đích); `HandOver` thêm một khóa dòng `clusters` và một câu `UPDATE tailnet_moves` (theo `tailnet_id`, có index `idx_tailnet_moves_open`). B3 (P12b) chưa đo lại.

## Known limitations / security blockers

- Không có blocker.
- **R10** vẫn tính tuổi từ `tailnets.updated_at` (phần "reset tuổi R10" của L7): sửa đúng cho mọi move cần cột thời điểm yêu cầu (schema) → báo coordinator; không làm trong nhóm C.
- "Hai process cùng chạy migration mới" (L7) thuộc migration (nhóm A); không làm.
- C1 dựa trên giả định: mọi lần nạp runtime giữ `leaseMu` (`reconcile`, `onLeaseAcquired`). Nếu nhóm B (B5) đổi `load`/`start` mà bỏ điều này, C1 phải xem lại.
- Hằng `ownershipLockPrefix` trong `placement/quota.go` lặp lại hằng không export của `tenantscope`; `TestEnrollmentTakesOwnershipLockFirst` (PostgreSQL) bắt lệch.
- Enrollment vào tailnet đang chuyển trả lỗi không phải quota: `checkQuota` (cluster.go, ngoài vùng của nhóm C) ghi log mức error "quota cannot be checked" cho trường hợp này; client nhận 503 và thử lại.
- `RunDueMoves` dự phòng ghi FAILED (không đoán) khi epoch đã đi quá E+1 mà lần lật không ghi move (chỉ xảy ra với lật bằng tay/`ReassignTailnet` chỉ dùng trong test).

## Reviewer approval

Chưa có.
