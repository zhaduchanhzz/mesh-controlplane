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

### R11 — cửa sổ owner kép trong bộ nhớ sau `MoveOwner` (Q7: đo, không đổi cơ chế)

**Cách đo:** sau khi CAS commit, cứ 5 ms kiểm `Runtime(tailnet)` ở cả hai cluster.

| Đại lượng | Giá trị |
|---|---|
| Cluster đích nạp xong | 1,77–2,01 s sau CAS |
| Cluster nguồn retire xong | 1,72–1,99 s sau CAS |
| Cửa sổ cả hai cùng giữ runtime | lần 1: 0 / 67 / 68 ms (min / median / max, n = 6); lần 2 (race): 0 / 207 / 213 ms (n = 6) |

**Diễn giải:**
- Hai cluster reconcile bằng ticker riêng, mỗi 2 s, lệch pha nhau.
- Cửa sổ tối đa theo thiết kế ≈ một chu kỳ (2 s) cộng thời gian nạp tailnet. Test chặn ở mức ≤ 3 s.
- Trong cửa sổ đó, ghi của nguồn đã bị fence ở DB, nên dữ liệu không có owner kép.
- Thứ còn lộ là: nguồn có thể vẫn gửi MapResponse từ bộ nhớ, cho tới khi retire đóng các stream.
- Chủ dự án quyết định có chấp nhận mức này hay không (Q7). Phương án rút ngắn, chẳng hạn đích chỉ nạp sau khi nguồn đã retire, thuộc P12b.

## Còn lại

- Bước 4: cluster tự ghi và đối chiếu Noise key khi lấy lease (Q6).
- Bước 5: CLI đi qua Registry (Q5) và sửa docs Q10. Đang làm song song trên nhánh `feat/P12-cli`.
- Bước 7: E2E lab và evidence cuối.
