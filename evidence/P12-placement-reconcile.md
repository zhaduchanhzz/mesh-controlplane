# PR P12 Evidence (đang làm) — placement, reconcile giữa cluster, hai cluster trên một DB

- **Nhánh:** `feat/P12-placement`, tạo từ `master` `e86f91b`.
- **Trạng thái:**
  - Bước 1–3: `Implemented` + `Verified`, mới ở mức unit, race và servertest.
  - Chưa chạy E2E P12 (bước 7), chưa `Reviewed`.

## Bước 1 — placement (Q1, Q2)

**Commit:** `f6c136d` (test fail trước khi sửa) → `f342f5b` (sửa).

**Hành vi mới:**
- `place()` bỏ qua cluster không có dòng `cluster_leases` còn hạn. Hạn lease so theo đồng hồ của DB.
- Khi tỷ lệ tải thiết bị bằng nhau, cluster có ít tailnet sống hơn được chọn trước, rồi mới xét `id`.

**Kiểm chứng:**
- Test `TestPlacementSpreadsNewTailnets` và `TestPlacementSkipsClusterWithoutLiveLease` chạy trên SQLite và PostgreSQL.
- Trước khi sửa, cả hai test đều fail: 3 tailnet dồn hết về một cluster, và không có bước kiểm lease.
- Lệnh chạy trên mc-ci: `go vet` + `go test ./internal/placement/... ./internal/bootstrap/... ./cmd/...`. Kết quả exit 0.

## Bước 2 — báo cáo reconcile R1–R9 (Q4)

**Commit:** `a1c55c3` (test fail trước khi sửa) → `0224636` (sửa).

**Hành vi mới:**
- `Registry.Reconcile` và `GET /v1/reconcile` (operator, mTLS).
- Chỉ đọc: một transaction, trên PostgreSQL là READ ONLY + REPEATABLE READ.
- Báo cáo chỉ chứa ID và số đếm, xếp theo (code, cluster, tailnet).

**Kiểm chứng:**
- Mỗi ca R1–R9 cho đúng mục của nó; riêng R4 luôn đi kèm R5.
- Registry sạch trả về `[]`.
- API từ chối khi không có cert, khi dùng credential của tenant, và khi identity không nằm trong danh sách cho phép.
- `go test -race ./internal/placement/... ./internal/bootstrap/...`: exit 0.

## Bước 3 — servertest hai cluster ID trên một DB

**File:**
- Harness: `hscontrol/servertest/cluster_p12.go` (`AddCluster`, `Registry`, `PlaceTailnet`, `MoveOwner`).
- Test: `cluster_p12_test.go`.

**Lệnh:**
```
go test -tags p05cluster -race -count=1 -run 'TestTwoClusters|TestFailoverInOneCluster' ./hscontrol/servertest/
```
Kết quả: exit 0 (mc-ci, 2026-11-25).

| Test | Kiểm điều gì |
|---|---|
| `TestTwoClustersServeOnlyOwnedTailnets` | 3 tailnet đặt qua placement chia ra cả 2 cluster. Cluster owner nạp tailnet. Cluster kia không nạp trong ≥ 3 s, tức hơn một chu kỳ reconcile. Hai client mỗi tailnet chỉ thấy nhau. Mỗi tailnet có đúng 2 node. Key của tailnet gửi tới cluster không sở hữu bị từ chối, không tạo node (F07). |
| `TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11` | 6 lần `MoveOwner` qua lại. Sau mỗi lần, ghi qua runtime của nguồn (vẫn còn trong bộ nhớ) bị từ chối với `ErrFenced`, 6/6 lần. Epoch tăng 3 → 8. Đo R11. |
| `TestFailoverInOneClusterLeavesTheOtherAlone` | Failover trong c1: standby sẵn sàng sau 159 ms, epoch lease c1 tăng. c2 giữ nguyên runtime, holder và epoch lease. Map stream của client c2 không bị đóng, peer còn đủ. Client c1 kết nối lại, giữ nguyên node key (không đăng nhập lại). |

**Không có giai đoạn fail-trước-khi-sửa ở bước 3:**
- Hành vi (fence, chỉ owner nạp tailnet, failover tách biệt giữa cluster) đã có từ P10 và P11b. Bước 3 thêm harness hai cluster ID và test xác nhận; không đổi code sản phẩm.
- Lần chạy đầu fail ở test failover. Nguyên nhân là test đọc `selfKey` sau `Reconnect`, lúc netmap chưa về; đã sửa trong test.

### R11 — cửa sổ owner kép trong bộ nhớ sau `MoveOwner`

**Đo lần đầu (cơ chế cũ, `d17deba`):** `MoveOwner` lật owner ngay. Hai cluster cùng giữ runtime trong **0–213 ms**; đo 12 lần, trong đó 6 lần có chồng lấn.

**Quyết định Q7 của chủ dự án (2026-11-25):** không chấp nhận cửa sổ này; phải loại bỏ trong P12. Cơ chế được chọn là trao tay (ADR 0010, đã duyệt).

**Cơ chế trao tay:**
1. `MoveOwner` sang cluster khác chỉ ghi yêu cầu `moving_to`.
2. Nguồn retire runtime của tailnet.
3. Sau khi runtime đã ngừng, `HandOver` lật owner, `epoch + 1`, dưới lease của nguồn.
4. Nếu nguồn chết (không còn lease sống), đích tự lật (`TakeOver`).
5. Đích chỉ nạp khi đã là owner.

| Commit | Nội dung | Kết quả |
|---|---|---|
| `d769cfd` | RED: lấy mẫu 5 ms, cấm bất kỳ mẫu nào thấy cả hai cluster cùng giữ runtime | Fail: 3/6 lần move có 13–14 mẫu chồng lấn |
| `298e5f0` | RED cho ADR 0010 (DB, placement, API, bootstrap, servertest) | 11 test fail |
| `c42c67a` | GREEN | Xem bảng dưới |

**Số đo sau khi sửa** (`TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`, 6 lần move):

| Đại lượng | Giá trị |
|---|---|
| Cửa sổ chồng lấn | **0 ms** ở cả 6 lần; 0 mẫu chồng lấn |
| Nguồn retire | 1,89–1,99 s sau khi ghi yêu cầu |
| Đích nạp | 1,96–3,93 s sau khi ghi yêu cầu |
| Ghi qua runtime cũ của nguồn sau khi lật | `ErrFenced`, 6/6 |
| `owner_epoch` | Tăng đúng 1 mỗi lần, từ 3 lên 8 |
| `moving_to` sau khi lật | Rỗng |

Đích nạp chậm hơn trước (~2–4 s so với ~2 s): đó là chi phí đã ghi trong ADR. Trong khoảng này không cluster nào phục vụ tailnet, và bootstrap trả `MOVING`.

**Nguồn chết** (`TestTwoClustersMoveFromDeadClusterIsTakenOver`):
- Dừng process duy nhất của c1, rồi yêu cầu chuyển tailnet sang c2.
- c2 tự nhận tay (`TakeOver`), nạp tailnet và cho client mới đăng ký.

**Unit test (SQLite + PostgreSQL):**
- `HandOver`:
  - không có yêu cầu thì từ chối;
  - holder lease sai thì từ chối;
  - epoch sai thì từ chối;
  - chỉ thành công một lần.
- `TakeOver`:
  - lease của nguồn còn sống thì từ chối;
  - cluster thứ ba không nhận được;
  - 8 lần `TakeOver` đồng thời chỉ 1 lần thắng.
- `MoveOwner` / `CancelMove`:
  - mỗi lúc chỉ một yêu cầu chờ; yêu cầu thứ hai bị `ErrConflict`;
  - `CancelMove` không đổi owner hay epoch;
  - chuyển trong cùng cluster tăng epoch ngay.
- Không xóa được cluster đang là đích của một yêu cầu chuyển.
- Báo cáo reconcile có R10.
- Bootstrap trả `MOVING`, sau khi trao tay thì trả URL của đích.

**Lưu ý khi gộp với `feat/P12-cli`:** nhánh đó thêm vào `MoveOwner` các kiểm tra sức chứa và lease của đích. Các kiểm tra này vẫn đúng ở bước yêu cầu, nhưng phải gộp tay vì cùng sửa thân hàm `MoveOwner`. Ngoài ra, `ReassignTailnet` của platform admin vẫn lật thẳng owner; sau khi gộp, CLI `reassign` sẽ đi qua `Registry.MoveOwner`.

## Bước 4 — Noise key của cluster trong registry (Q6)

**Commit:** `272a5e0` (RED) → commit GREEN ngay sau đó.

**Hành vi:**
- Khi lấy lease, process gọi `Platform.RecordNoiseKey`. Hàm này đọc key đã đăng ký của cluster trong registry:
  - Key rỗng hoặc NULL: ghi key của process vào. Chỉ ghi một lần, và chỉ khi process đang giữ lease sống.
  - Key trùng: không làm gì.
  - Key khác: trả `ErrNoiseKeyMismatch`, **không ghi đè**.
  - Cluster chưa đăng ký: không kiểm. Trường hợp này đã lộ ra qua R1/R5 trong báo cáo reconcile.
- Khi key khác, `onLeaseAcquired` trả lỗi. Lease được nhả, process ở lại standby và thử lại theo chu kỳ (fail closed).

**Kiểm chứng:**

| Test | Kết quả |
|---|---|
| `TestRecordNoiseKey` (SQLite + PostgreSQL, `-race`) | PASS |
| `TestClusterRecordsNoiseKeyAndRefusesMismatch` (servertest) | PASS |

Servertest kiểm các điểm sau:
- Mỗi cluster tự ghi key của mình, và key của hai cluster khác nhau.
- Process có key lệch với registry không lên `/ready` trong 3 s, và key trong registry không bị đổi.
- Operator xóa key sai: process ghi key đúng và phục vụ.

Lần chạy đầu fail do quét NULL vào `[]*string`. Đã sửa bằng `COALESCE`. Ngay cả trong lần fail đó, process vẫn ở lại standby, đúng hành vi fail closed.

## Lượt race giữa P12 (sau ADR 0010)

- **Lệnh:** `go test -race ./hscontrol/ ./hscontrol/db/... ./internal/... ./cmd/...`, cùng toàn bộ cluster suite `-tags p05cluster`.
- **Thời gian:** ~50 phút; riêng cluster suite 2520 s.

**Các test fail và cách xử lý:**

| Test | Nguyên nhân | Xử lý | Kết quả chạy lại |
|---|---|---|---|
| `TestP10bRetireWaitsForInFlightRequest` | Lỗi thật: map `retiring` là nil khi `Cluster` được dựng không qua `NewCluster` | Commit `9b4efd4` | PASS (race) |
| `TestDNS01SolverWait` | `bind: address already in use`, cùng loại flake trùng cổng ACME đã ghi ở P08b | Không sửa code | PASS 3/3 |
| `TestAPIv2`, `TestAPIv2OAuthScopes` | Chạy ngoài `nix shell`, thiếu `tscli`/`tofu` | Chạy lại trong `nix shell` | PASS |

**Sau khi sửa:** `TestCluster*`, `TestTwoClusters*` và `TestFailoverInOneCluster*` (p05cluster) đều PASS, trên `cluster.go` mới.

## Còn lại

- Bước 5: CLI đi qua Registry (Q5) và sửa docs Q10. Đã xong trên nhánh `feat/P12-cli`; chủ dự án yêu cầu giữ nhánh riêng.
- Bước 7: E2E lab và evidence cuối.
