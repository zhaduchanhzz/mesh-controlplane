# P14f — sửa các phát hiện của review bảo mật độc lập (R1–R8)

- **Trạng thái:** `Verified` trên mc-ci (§3: race 43 ok, lint 0, shard 239/240 với 1 flake không liên quan, M07 long + race PASS, bộ OFF PASS). Chưa `Reviewed`. Chưa merge.
- **Nhánh:** `feat/P14f-review` từ `feat/P14f-f4` `cc2ce81`.
- **Nguồn:** review độc lập `master...feat/P14f-f4` (2026-11-27): không có BLOCKER. Phát hiện F1–F8 của reviewer được đánh số lại thành R1–R8, để khỏi trùng tên pha F1–F6 của P14f.
- **ADR:** 0015 D2, phần bổ sung "review bảo mật độc lập".

## 1. Phát hiện → sửa

| ID | Mức | Phát hiện | Sửa | Test |
|---|---|---|---|---|
| R1 | MEDIUM | Memo hash của entry dùng chung là `sync.Map` toàn process, khóa theo `*tailcfg.Node`, không xóa khi runtime dừng: mỗi lần runtime dừng/khởi động lại để rò mọi entry | Memo chuyển vào cache `sharedPeers` của từng mapper (`hashes`), đi cùng runtime; `hashResponse(data, memo)` | `TestReviewR1SharedEntriesFreedWithMapper` (5 vòng đời mapper, `weak.Pointer` tới entry; sau GC không còn entry nào sống) |
| R2a | MEDIUM | Mốc delta-first theo kết nối O(n²), giá trị 24 byte mỗi peer | Gói vào một từ 8 byte: 48 bit hash và 16 bit hiện diện (Online 2 bit chính xác, LastSeen 14 bit) | `TestReviewR2BaselineBytesPerPeer`: 10k peer 65,6 → **29,6 B/peer**; 50k peer 52,5 → **23,6 B/peer**. Mục tiêu ≤ 16 B/peer không đạt được với map Go: riêng khóa và giá trị đã là 16 byte mỗi ô, phần còn lại là overhead của map |
| R2b | MEDIUM | `max_peer_entries` chỉ áp khi node join; tenant đăng ký nhiều node dưới policy cô lập rồi chuyển sang allow-all là làm phình bộ nhớ của process dùng chung | Áp cho mọi đường ghi policy: `State.ValidatePolicy` (setACL, validate, API v1 `UpdatePolicy`) và CLI `tailnet policy`. Policy làm tổng peer entry > giới hạn **và** > mức hiện tại bị từ chối (`ErrPeerEntriesBound`, 400). Đếm bằng bản dựng theo lớp chỉ đếm, dừng khi vượt giới hạn | `TestReviewR2PolicyOverPeerBoundRefused` (State), `TestP14fReviewTenantPolicyOverPeerBound` (API tenant, cluster), `TestValidatePolicyPeerEntriesBound`, `TestCountPeerEntriesMatchesBuild` (rapid: số đếm = bản dựng đầy đủ) |
| R2c | — | Không có giới hạn cho từng kết nối | Kết nối giữ > 100 000 peer thì không giữ mốc nữa (gửi đủ); metric `headscale_mapper_delta_baseline_dropped_total` | `TestReviewR2BaselineCap` |
| R3 | LOW–MEDIUM | Full map (lưới an toàn) vẫn lược filter và profile "đã giữ", nên lệch filter chỉ sửa được khi kết nối lại | Frame có `Peers` đầy đủ không bao giờ bị lược filter/profile; đặt lại mốc profile | `TestReviewR3FullMapCarriesFilterAndProfiles` |
| R4 | MEDIUM | Rào chắn `WaitMapsDelivered` làm một số kiểm tra trở nên vô nghĩa (kiểm điều đã đúng từ trước) | Rào chắn chờ thêm mọi channel của stream đã rỗng (`Batcher.Drained`). Đếm frame phía server (`Batcher.FramesDelivered`, `TestServer.FramesDelivered`). Thay đổi deny→deny và thay đổi group không liên quan nay **kiểm khẳng định thật**: 0 frame khi delta-first, ≥ 1 frame mỗi client khi tắt công tắc | `TestTenantDefaultDeny/*`, `TestPolicyChanges/*/unrelated_policy_change_keeps_client_ssh_rules` |
| R5 | MEDIUM | Đổi mặc định làm chế độ OFF gần như không được test; `ClusterOptions{IncrementalNetmap:false}` lại nghĩa là ON | (1) `servertest.WithIncrementalNetmap`, mặc định đọc env `SERVERTEST_INCREMENTAL_NETMAP=off` để chạy cả package với công tắc tắt. (2) `TestTenantDefaultDeny` và `TestPolicyChanges` chạy cả hai chế độ. (3) Danh sách bỏ qua compat phụ thuộc chế độ: 29 mục `POLICY_RESPONSE_TO_EVERY_NODE` chỉ áp khi OFF. (4) `ClusterOptions.Netmap` (`NetmapDefault`, `NetmapIncremental`, `NetmapFull`) thay hai bool | §3 (compat ON và OFF) |
| R6 | LOW | Thiếu test: hai kết nối của một node dưới backpressure | Không đổi code; viết test | `TestReviewR6TwoConnectionsUnderBackpressure` (PASS cả trước và sau) |
| R7 | LOW | Thiết kế hash | Không đổi: seed theo process, lỗi hash tính là "đã đổi". Phần hiện diện nay là dấu vân tay (xem R2a) | — |
| R8 | LOW | Ngưỡng sweep theo số peer của người xem, không theo kích thước tailnet | `put(..., live)` với `live` = số node của tailnet | `TestReviewR8SweepsBoundedByTailnetSize` |

## 2. RED (code trước khi sửa, `45c8e71`)

`go test -count=1 -v -run TestReview ./hscontrol/mapper/`: exit 1.
- R1 FAIL: entry còn sống.
- R2: 65,6 / 52,5 B/peer.
- R2 cap FAIL, R3 FAIL.
- R6 PASS (test bổ sung độ phủ).

`go test -run TestReviewR2PolicyOverPeerBoundRefused ./hscontrol/servertest/`: exit 1, policy allow-all được chấp nhận.

R4, R5 và R8 cần hook hoặc API mới (đếm frame, `NetmapMode`, bộ đếm sweep), nên không có RED chạy trên code cũ. Bằng chứng của chúng là phân tích của reviewer cộng test mới.

## 3. GREEN và kiểm tra cuối (mc-ci, Go 1.27.4)

Code `1d90f3e`. `08da94d` chỉ sửa lint trong test: thêm `t.Helper`, `require`, khoảng trắng, và bỏ một `time.Sleep` thừa trong R6.

| Kiểm tra | Lệnh | Kết quả |
|---|---|---|
| Test review, mapper | `go test -count=1 -v -run "TestReview\|TestSentState\|TestSharedPeer\|TestDeltaFirst" ./hscontrol/mapper/` | PASS. Baseline 29,6 B/peer (10k), 23,6 B/peer (50k) |
| R1/R2 cap/R3/R6 lặp lại | `go test -race -count=20 -run "TestReviewR6\|TestReviewR3\|TestReviewR2BaselineCap\|TestReviewR1" ./hscontrol/mapper/` (`08da94d`) | ok |
| Policy v2 | `go test -run "TestCountPeerEntries\|TestValidatePolicyPeerEntriesBound\|TestBuildPeerMapIndexed" ./hscontrol/policy/v2/` | ok |
| CLI | `go test ./cmd/headscale/cli/` | ok |
| Servertest, mặc định (ON) | `go test -run "TestTenantDefaultDeny\|TestPolicyChanges\|TestReviewR2\|TestGrantPolicies\|TestStressStateMutations" ./hscontrol/servertest/` | ok |
| Servertest + compat, công tắc **OFF** | `SERVERTEST_INCREMENTAL_NETMAP=off go test -run "Compat\|TestTenantDefaultDeny\|TestPolicyChanges" ./hscontrol/servertest/` | ok, 165 s (danh sách bỏ qua của chế độ OFF) |
| Compat, ON | `go test -run Compat ./hscontrol/servertest/` | ok, 146 s |
| Cluster | `go test -tags p05cluster -run "TestP14fReview\|TestClusterP07\|TestP12dTenantPolicyWrite\|TestClusterP14fSelfTagProfile\|TestClusterP14fTaggedRenameProfile" ./hscontrol/servertest/` | ok, 271 s |
| Race toàn repo (PostgreSQL + PgBouncer tạm) | `~/p14frev-verify.sh` (như F4) | `race_exit=0`, **43 package ok** |
| Lint | `golangci-lint run` (`08da94d`) | **0 issues** (`1d90f3e` có 7 lỗi lint trong test, đã sửa ở `08da94d`) |
| `p05cluster` shard | `shard-test.sh -n 8 -race -tags p05cluster` | **239/240**. 1 fail là `TestP08bAdminStandbyRefuses`: dial unix socket admin dưới `/tmp` lúc shard tải cao, không liên quan thay đổi; chạy riêng `-race -count=3`: ok |
| M07/M08 long (40 seed × 40 bước mỗi chế độ) | `P14F_M07_CHECKS=40 P14F_M07_STEPS=40 go test -tags p05cluster -run ^TestClusterP14fReplayEqualsFullMap$` | **PASS** cả hai chế độ (1382 s và 1351 s) |
| M07/M08 race | `P14F_M07_CHECKS=6 P14F_M07_STEPS=30 go test -race -tags p05cluster -run ^TestClusterP14fReplayEqualsFullMap$` | `m07_race_exit=0` |
| Bộ test với công tắc OFF, race | `SERVERTEST_INCREMENTAL_NETMAP=off go test -race -tags p05cluster -run "Compat\|TestTenantDefaultDeny\|TestPolicyChanges\|TestGrantPolicies"` | `off_suite_exit=0`, 0 fail |

## 4. Ảnh hưởng cho vận hành và cho F5

- `tuning.max_peer_entries` (mặc định 1 000 000) nay chặn cả ghi policy. Ví dụ: tailnet `allow_all` trên khoảng 1 000 node, hoặc nhỏ hơn nếu còn các đường quan hệ khác, bị từ chối với 400 và lý do.
  - Phép đo giới hạn F5 phải đặt `MAX_PEER_ENTRIES=0` (`tools/capload/cluster.sh`, `server.sh`), như vốn đã phải làm cho join.
  - `tools/capseed` tự ghi policy (`hs.SetPolicy`), không đi qua đường ghi có kiểm tra, nên không bị ảnh hưởng.
- Metric mới: `headscale_mapper_delta_baseline_dropped_total`.
- Không thêm khóa cấu hình nào; không phải sinh lại tài liệu cấu hình.

## 5. Còn lại

- R2a đạt 23,6–29,6 B/peer, chưa đạt mục tiêu ≤ 16 B/peer. Muốn thấp hơn phải bỏ map Go và dùng bảng riêng (mảng sắp xếp cộng lớp phủ). Chưa làm, vì đây là đường liên quan bảo mật và giới hạn ở R2b đã chặn tổng bộ nhớ.
- Ghi policy bị từ chối vì vượt giới hạn **không** được ghi audit, chỉ log ở mức thông thường, theo hợp đồng audit của P12d: audit chỉ ghi lần ghi đã lưu.
- Đường CLI `tailnet policy` áp giới hạn chặt (`now` = 0), vì CLI không có số peer entry hiện tại của runtime.
- Reviewer approval: chưa.
