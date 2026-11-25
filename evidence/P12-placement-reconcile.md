# PR P12 Evidence — placement tự động, reconcile giữa cluster, trao tay tailnet, hai cluster HA

- **Repo commit before / after:**
  - Before: `master` `e86f91b` (P12 inventory merge).
  - After: `feat/P12-placement` `de8a562`.
  - Merge thử vào `master` `ac71728`: không xung đột.
- **Verified source paths & symbols:** `evidence/source-inventory-P12.md` (commit `b2b5d8d`), cộng các file sửa liệt kê dưới đây.
- **Design choice / ADR:**
  - Quyết định Q1–Q11 của chủ dự án: `tickets/P12.md` §Quyết định.
  - Q7 đổi ngày 2026-11-25: R11 phải loại bỏ trong P12.
  - **ADR 0010** (trao tay tailnet, đã duyệt): `design/adr/0010-tailnet-serving-claim.md`.
- **Trạng thái:** `Verified`. Chưa `Reviewed`.
  - Unit, race, servertest: PASS.
  - E2E lab: v1.104.7 đạt 12/12. v1.84.3 đạt 11/12; P08 fail, chủ dự án chấp nhận ngày 2026-11-25 (xem mục E2E).

## Tóm tắt thay đổi

| Bước | Thay đổi | Commit (RED → GREEN) |
|---|---|---|
| 1 | Placement chỉ chọn cluster có lease sống (Q2). Hòa tải thì xét số tailnet, rồi `id` (Q1). | `f6c136d` → `f342f5b` |
| 2 | Báo cáo reconcile R1–R9 (sau thêm R10), chỉ đọc; `GET /v1/reconcile` (Q4). | `a1c55c3` → `0224636` |
| 3 | Servertest hai cluster ID trên một DB; đo R11. | `d17deba` |
| R11 | Trao tay (ADR 0010): `MoveOwner` sang cluster khác chỉ ghi yêu cầu chuyển; nguồn retire rồi `HandOver`; nguồn chết thì `TakeOver`; thêm `CancelMove` và R10. | `d769cfd`, `298e5f0` → `c42c67a`, `9b4efd4` |
| 4 | Noise key của cluster: ghi một lần khi lấy lease; khác key thì không phục vụ (Q6). | `272a5e0` → `373bfd5` |
| 7 | Harness E2E `infra/lab/e2e/p12/run.sh`; `infra/lab/scripts/shard-test.sh`. | `fff6fcd` … `de8a562` |

- **Bước 5** (CLI đi qua Registry, Q5; bắt buộc lease) và **docs Q10** nằm trên nhánh riêng `feat/P12-cli`, theo yêu cầu của chủ dự án. **Không** thuộc PR này.
- **Bước 6** (lab: mc-c2b standby cho c2, HAProxy c2) đã merge riêng (`848f401`).

**DB migrations / downgrade:** không có migration. ADR 0010 dùng cột `tailnets.moving_to_cluster_id` có từ P11; bước 4 dùng cột `clusters.noise_public_key` có sẵn. Không có ràng buộc downgrade mới.

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

**Gộp với `feat/P12-cli` (2026-11-25, nhánh `feat/P12-cli-merge`):** chỉ xung đột ở doc comment của `MoveOwner`. Thân hàm gộp tự động: các kiểm tra sức chứa và lease của đích nằm ở bước yêu cầu. Test CLI và test placement của nhánh đó được sửa theo ngữ nghĩa yêu cầu chuyển. CLI `reassign` in thêm `moving_to`.

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

## E2E lab (bước 7) — `infra/lab/e2e/p12/run.sh`

**Môi trường:**
- c1: mc-cp + mc-c1b, sau HAProxy tạm.
- c2: mc-c2 + mc-c2b, sau `mesh-c2-ha`.
- DB `e2e_p12<run>` trên mc-db, qua PgBouncer.
- `mesh-placement` chạy loopback với mTLS và CA tạm; bootstrap công khai ở `:8444`.

**Binary** (build từ `373bfd5`; các commit sau đó chỉ sửa harness và evidence):

| Binary | sha256 (16 ký tự đầu) |
|---|---|
| headscale | `707313c76ffd3899` |
| mesh-placement | `90de4aa7a493e985` |
| mesh-bootstrap | `d229e64d41f4339b` |

**Lệnh:**
```
BIN=… PLBIN=… BOOT_BIN=… IMAGE=tailscale/tailscale:<v> infra/lab/e2e/p12/run.sh
```

| Commit, giờ | Client | Kết quả | Artifact |
|---|---|---|---|
| `de8a562` 11:00Z | v1.104.7 | **12/12 PASS**, exit 0, cleanup_ok=1, 477 s | `artifacts/de8a562/P12/e2e-20261125T110054Z` |
| `de8a562` 14:18Z | v1.84.3 | **11/12**: P08 FAIL (xem dưới), các ca còn lại PASS; exit 1, cleanup_ok=1, 1347 s | `artifacts/de8a562/P12/e2e-20261125T141832Z` |

**P08 ở lần v1.84.3 — chủ dự án chấp nhận và cho merge (2026-11-25):**
- **Hiện tượng:** ở F02 của c1, process c1b sau khi SIGCONT ghi nhận thêm **1** MapResponse (bộ đếm 4 → 5).
- **Những gì đã xác nhận đúng:**
  - Ngay khi resume, process báo `self-fence deadline passed`, retire runtime và đóng stream (14:36:39.62).
  - Lệnh ghi last-seen bị fence chặn (`tailnet ownership fence failed`).
  - `/ready` trả 503.
  - Ở cùng lần chạy, F02 trên c2 có bộ đếm 8 → 8.
- **Vì sao chưa kết luận được:**
  - `writeMap` gọi `servable()`, hàm kiểm deadline của lease theo `time.Now()` cho từng frame. Bộ đếm chỉ tăng sau khi ghi xong frame.
  - Frame thứ 5 có hai cách giải thích:
    1. được gửi khi lease còn hạn, trong khoảng vài trăm ms giữa lúc harness đọc bộ đếm và lúc gửi SIGSTOP; hoặc
    2. SIGSTOP rơi vào khoảng vài µs giữa lúc kiểm `servable()` và lúc ghi socket. Khi đó frame đã dựng lúc còn lease được ghi ra sau khi resume (kẽ hở check-then-act).
  - Bộ đếm hiện có không phân biệt được hai trường hợp.
- **Ngoài P08:** các lần F02 khác của P12 (16 lần trên c1 và c2) và của P11b đều 0 frame.
- **Đề xuất đo chính xác** (chủ dự án chưa chọn, ghi lại cho P13/P14): thêm metric thời điểm ghi frame cuối; harness đọc bộ đếm và gửi SIGSTOP trong cùng một lệnh trên host; chạy F02 lặp lại.

**Số đo (lần v1.104.7):**

| Ca | Kết quả |
|---|---|
| P01 | x → c2, y → c1, z → c2 (dàn đều). Chỉ owner nạp tailnet. Bootstrap trả URL của owner. Reconcile `[]`. |
| P04 / P05 | Restart cả cluster: c1 12 s, c2 11 s. Cluster kia vẫn phục vụ, epoch lease của nó không đổi. Client không đăng nhập lại. |
| P06 / P07 (F01, kill -9) | c1: standby ready 11,4 s, client chạy lại 12,5 s. c2: 10,2 s và 11,3 s. Cluster kia không bị đụng. |
| P08 / P09 (F02, SIGSTOP quá TTL) | Process kia ready sau 10,8 s (c1) và 10,4 s (c2). Process được resume không gửi MapResponse nào (3 → 3, 5 → 5), `/ready` 503. |
| P10 (trao tay y: c1 → c2) | Bootstrap lấy mẫu trên mc-cp: `200 → 503 MOVING → 200`. c1 trao tay 11:08:07.693, c2 nạp 11:08:08.600. Epoch 1 → 2. c1 từ chối key cũ. Client đăng nhập lại ở c2. Các tailnet khác không bị ảnh hưởng. |
| P11 | Owner `x=mc-c2@1 y=mc-c2@2 z=mc-c2@1`. Lease epoch tăng ngặt ở cả hai cluster; mỗi cluster đúng một holder lease sống. Reconcile `[]`. |
| P12 | Artifact không chứa auth key, private key hay machine/node key đầy đủ. |

**Các lần chạy không đạt** (đều do harness hoặc môi trường; không có lỗi sản phẩm):

| Commit | Client | Fail | Nguyên nhân | Xử lý |
|---|---|---|---|---|
| `fff6fcd` | v1.104.7 | P11 | Harness so sánh với `t`, nhưng PostgreSQL in `true` | `ced356d` |
| `ced356d` | v1.104.7 | — | **12/12 PASS** | — |
| `ced356d` | v1.84.3 | P10 | Trao tay chỉ mất 0,8 s, mỗi lần gọi bootstrap từ laptop mất ~1 s, nên không bắt được `MOVING` | `596ce92` (sampler chạy trên mc-cp) |
| `596ce92` | v1.104.7 | P10 | Sampler chạy bằng job nền qua ssh không khởi động | `352fac7` (`systemd-run`) |
| `596ce92` | v1.84.3 | dừng sau P08 | Phiên agent bị ngắt nên không có cleanup; đã dọn tay theo đúng các bước của `cleanup()`, cleanup_ok=1 | — |
| `352fac7` | cả hai | P10 | Đã thấy `MOVING`, nhưng harness đọc file sau 9 s trong khi sampler cần hơn 10 s | `fc737c7`, `de8a562` |
| `de8a562` | v1.84.3 | P02–P10 | ssh tới mc-ci bị reset và lượt chạy treo 2 giờ 48 phút. Key của y (hạn 120 phút) hết hạn, nên bootstrap trả 404, đúng thiết kế | Chạy lại |

Ở mọi lần chạy có P10, log đều cho thấy nguồn trao tay **trước** khi đích nạp.

## Tests + exact commands + exit codes (mc-ci, go 1.27.4)

**Unit/race cuối** (trên code sau bước 4):
```
nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go vet ./...
go test -race -count=1 -timeout 45m ./hscontrol/ ./hscontrol/db/... ./internal/... ./cmd/...
```
Kết quả: `RACE_RC=0`, 14 package OK, 487 s.

**Cluster suite** (chạy song song 6 phần):
```
infra/lab/scripts/shard-test.sh -n 6 -tags p05cluster ./hscontrol/servertest/
```
- 202/203 PASS. Wall 1586 s (tuần tự 2762 s).
- `TestPolicyChanges` fail một lần lúc máy tải nặng (6 phần song song cùng E2E). Test này single-tenant, không đi qua code P12. Chạy riêng `-count=5`: PASS.
- Các test P12 (`TestTwoClusters*`, `TestFailoverInOneCluster*`, `TestClusterRecordsNoiseKey*`): PASS.

## Negative tenant isolation tests

- **Servertest:**
  - Cluster không sở hữu không bao giờ nạp tailnet.
  - Key gửi tới cluster không sở hữu bị từ chối, không tạo node.
  - Sau khi trao tay, mọi lần ghi qua runtime cũ của nguồn đều `ErrFenced` (6/6).
  - Không mẫu nào thấy hai cluster cùng giữ runtime.
- **E2E:**
  - P02: client chỉ thấy peer trong tailnet của mình.
  - P03: key gửi tới cluster không sở hữu bị từ chối.
  - P10: sau khi trao tay, c1 từ chối key cũ; không node nào của y xuất hiện ở tailnet khác.
- **API reconcile:**
  - Từ chối khi không có cert, khi dùng credential của tenant, và khi identity không được phép.
  - Chỉ trả ID và số đếm.

## Known limitations / security blockers

- **Move gây gián đoạn:** khoảng 2–4 s không cluster nào phục vụ tailnet. Client phải đăng nhập lại ở đích (ADR 0005); P12b sẽ xử lý.
- **Move có thể kẹt:** nếu nguồn treo nhưng vẫn giữ lease, move bị kẹt. R10 làm lộ trường hợp này; operator dùng `CancelMove`.
- **CLI `reassign`** đi qua `Registry.MoveOwner` từ khi gộp `feat/P12-cli`, nên cũng theo cơ chế trao tay. `Platform.ReassignTailnet` (lật thẳng) chỉ còn test dùng.
- **Không có FK** `tailnets.owner_cluster_id → clusters` (Q10); trường hợp lệch được báo bằng R1.
- **F02 / P08 (v1.84.3):** có thể một frame dựng khi còn lease được ghi ra muộn sau SIGCONT, do kẽ hở check-then-act ở cấp µs (xem mục E2E). Dữ liệu vẫn bị fence ở DB. Chủ dự án chấp nhận cho P12 (2026-11-25); cần đo chính xác ở P13/P14.
- **Isolation:** không có trường hợp tenant thấy dữ liệu, peer hay node của tenant khác, và không có owner kép ở dữ liệu. Ngoại lệ duy nhất cần ghi là P08 ở trên.

## Benchmark

Không có đường nóng mới:
- Trao tay thêm 2 truy vấn mỗi lượt reconcile (2 s) và một lần ghi mỗi move.
- `RecordNoiseKey` chạy một lần mỗi khi lấy lease.

## Reviewer approval

Chưa có. Cần review bảo mật, đặc biệt:
- điều kiện CAS của `HandOver` / `TakeOver` (ADR 0010);
- fail-closed của Noise key (Q6).
