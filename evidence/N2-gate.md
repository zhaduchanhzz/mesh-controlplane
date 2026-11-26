# Gate N2 Evidence — Placement, Fencing & Failover (P10–P12b)

- **Trạng thái:** `Reviewed`. **Gate N2 đạt**: chủ dự án duyệt ngày 2026-11-26 và cho merge `feat/P12b-inventory` vào `master`.
- **Repo commit before / after:**
  - Before: `master` `2d80f21` (sau Gate N1).
  - After: `feat/P12b-inventory` (commit chứa file này). Merge `master` `2d80f21` vào nhánh: `cec4c27` (không xung đột). Code đã test ở E2E và race dưới đây là code của `cec4c27`; các commit sau đó chỉ sửa harness và evidence.
- **Phạm vi:** gom bằng chứng của P10, P10b–P10d, P11, P11b, P12 (đã merge) và P12b (nhánh này) theo từng điều kiện N2 của `PLAN.md` (dòng 47 và §7 "N2 — Placement, Fencing & Failover"). File này không thay evidence của từng ticket. Nó chỉ trỏ tới chúng và ghi các lần chạy mới của P12b.

## 1. Điều kiện N2 và bằng chứng

| # | Điều kiện N2 (`PLAN.md`) | Bằng chứng | Kết quả |
|---|---|---|---|
| 1 | 2 cluster, mỗi cluster active + standby, trên một PostgreSQL dùng chung | E2E P12 (`evidence/P12-placement-reconcile.md` §E2E): c1 = mc-cp + mc-c1b, c2 = mc-c2 + mc-c2b, một DB trên mc-db qua PgBouncer. E2E P12b dùng cùng bố trí | PASS |
| 2 | 3+ tailnet được tạo và bootstrap trả đúng cluster | P12 P01: x → c2, y → c1, z → c2, bootstrap trả URL của owner. P12b M00/M01: tailnet o, m1, m2; client đăng ký tại URL do bootstrap trả. Servertest `TestTwoClustersServeOnlyOwnedTailnets` (3 tailnet / 2 cluster) | PASS |
| 3 | owner/epoch duy nhất; không ghi nhận owner kép; request stale bị chặn | P10: test owner uniqueness, stale epoch, quota (RED 13/13 FAIL → GREEN, SQLite + PG16; E2E 13/13). P10b–P10c: fence từ chối tailnet không ACTIVE, advisory lock theo tailnet. P12 P11: mỗi cluster đúng một holder lease sống, epoch tăng ngặt, reconcile `[]`. ADR 0010: R11 (chồng lấn runtime trong bộ nhớ sau trao tay) = 0 (`TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`). P12b M03 (nguồn trao tay **trước** khi đích nạp, epoch 2) và M06 (một owner mỗi tailnet, không move mở, reconcile `[]`) | PASS |
| 4 | Bootstrap trả đúng cluster và từ chối `MOVING` / `SUSPENDED` | P11: `TestBootstrapStoreFollowsOwner` (sau `MoveOwner` trả URL đích, không bao giờ nguồn; `moving_to` → MOVING; SUSPENDED → từ chối), `TestBootstrapRefusalsIndistinguishable` (16 lý do từ chối, phản hồi giống từng byte), E2E 25/25 ×2 gồm đường public. P12 P10: bootstrap lấy mẫu `200 → 503 MOVING → 200`. P12b M03: bootstrap → c2 sau trao tay | PASS |
| 5 | Cluster từ chối tenant không thuộc mình, kể cả khi client tới nhầm | Servertest `TestTwoClustersServeOnlyOwnedTailnets` (credential gửi tới cluster không sở hữu → từ chối, không tạo node), `TestClusterOwnershipMoveRetiresRuntime`. P12 P10: sau trao tay c1 từ chối key cũ. P12b: client còn trỏ nguồn sau trao tay nhận 401 (`PollNetMap: initial fetch failed 401` trong log tailscaled) | PASS |
| 6 | Failover active → standby tự động trong TTL, client không đăng nhập lại, active cũ sống lại không ghi được (F01–F04) | P11b: lab E2E 9/9 ×2 (v1.104.7, v1.84.3); tỉ lệ đăng nhập lại 0; F02 frame sau SIGCONT 0. P12: F01/F02 trên **cả hai** cluster, cluster kia không bị đụng | PASS, một ngoại lệ đã được chấp nhận (§3) |
| 7 | Tenant move P12b không có owner kép | P12b E2E `0a17355` `20261126T045654Z` **13/13** (§2): M03 nguồn trao tay trước khi đích nạp (0,77 s và 0,89 s), epoch 2; M06 một owner mỗi tailnet, không move mở, reconcile `[]`; M04 NodeID và IP giữ nguyên; M05 tailnet đối chứng không bị đụng. Servertest `TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11`, `TestScheduledMoveNotifiesOnlyItsClients`. Ở cả 9 lần chạy E2E của P12b (8 lần không đạt vì lý do khác, §2): mọi lần tới M03 đều thấy nguồn trao tay trước khi đích nạp, mọi lần tới M06 đều PASS; không lần nào ghi nhận owner kép | PASS |

## 2. P12b E2E (lần này)

Chi tiết: `evidence/P12b-tenant-move.md` §E2E. Tóm tắt lần chạy sạch:

- **Lần chạy:** `0a17355`, `20261126T045654Z`, **13/13 PASS**, exit 0, `cleanup_ok=1`, 544 s. Binary build trên mc-ci từ cây `cec4c27` (668/668 file đối chiếu với blob git): cluster `56352dc01879ffee`, mesh-placement `93e176150edf525c`, mesh-bootstrap `a53c77fb0132cb69`, mesh-rebootstrap `6a63b80973851a90`.
- **Bố trí:** c1 (mc-cp) và c2 (mc-c2), một PostgreSQL dùng chung (mc-db qua PgBouncer), mesh-placement mTLS, bootstrap công khai. Tailnet đối chứng o trên c2; tailnet m1 (v1.104.7: 24 container + 2 VM) và m2 (v1.84.3: 24 container) trên c1, chuyển sang c2.

| Đo | v1.104.7 | v1.84.3 |
|---|---|---|
| Trao tay → đích nạp | 0,77 s | 0,89 s |
| Thông báo tới đủ 24 client | 16,3 s | 8,7 s |
| Đăng nhập lại 24 container, PAR=8: t50 / t90 / t100 (từ lúc chạy) | 4,1 / 6,1 / 6,1 s | 0,25 / 0,38 / 0,39 s |
| Target phải chạy lại (retry) | 0/24 container; **2/2 VM** (lần 1 `invalid pre auth key`, lần 2 thành công) | 0/24 |
| Ping gap của cặp client đo | 5,2 s (4/361 mẫu lỗi) | 5,1 s (4/283) |
| Can thiệp tay (`debug restun`) | 0 | 0 |

- **Ngưỡng Q7 chưa đặt.** Số đo trên là cơ sở để chủ dự án chốt. t50/t90/t100 là thời gian lệnh `tailscale up` trả về: v1.104.7 chờ tới khi đăng nhập xong, v1.84.3 trả về sớm hơn; với cả hai, harness chỉ cho PASS sau khi mọi client `Running` ở c2.
- **Lần chạy không đạt (8 lần):** đều do harness hoặc môi trường lab (đường laptop → lab bị reset, `/run` noexec trên VM, logind xóa `/dev/shm`, parser `snap`), trừ race cửa sổ nạp ở lần 3–4, đã xử lý bằng retry (§3). Bảng đầy đủ ở evidence P12b.

## 3. Ngoại lệ và quyết định đã có của chủ dự án

| Mục | Nội dung | Quyết định |
|---|---|---|
| P12 P08 (v1.84.3) | F02 trên c1: process được resume ghi thêm 1 MapResponse (4 → 5). 16 lần F02 khác của P12 và mọi lần của P11b: 0 frame. Chưa phân biệt được "gửi khi lease còn hạn" với kẽ hở check-then-act | Chấp nhận, cho merge (2026-11-25). Đề xuất đo chính xác ghi lại cho P13/P14 |
| P12b B3 | `MoveOwner` đếm thiết bị của cluster đích trong lúc giữ `LockOwnership`: writer của tenant đang chuyển chờ 94–153 ms khi đích giữ 51.000–60.000 thiết bị. Tenant khác không bị chặn | Chấp nhận, ghi giới hạn; tối ưu ở P14 (2026-11-26) |
| ADR 0009, client 1.84.3 | Sau khi đổi sang cluster có DERP map khác, client 1.84.3 có thể giữ home DERP cũ | Phương án (a): runbook `tailscale debug restun` / khởi động lại `tailscaled` (2026-11-26) |
| P12b, cửa sổ nạp ở cluster đích | Bootstrap trả URL đích ngay khi nguồn trao tay; đích nạp tailnet chậm tới một chu kỳ reconcile (đo được 0,77–1,34 s). Đăng nhập trong khoảng đó nhận `invalid pre auth key` (fail closed, không owner kép). Client 1.104.7 dừng; 1.84.3 tự thử lại | `mesh-rebootstrap` chạy lại target lỗi (`-attempts 3 -retry-delay 2s`) (2026-11-26, `d889da7`). Đã chạy thật ở lần sạch: 2 VM thử lại và thành công (§2). Đóng hẳn cửa sổ là thay đổi thiết kế, chưa làm |

## 4. Tests + exact commands + exit codes

**Lần chạy cho gate này** (2026-11-26, Go 1.27.4 trên mc-ci; cây `~/src/p12bm` đối chiếu với blob git của `cec4c27`: 2.290/2.290 file khớp):

| Lệnh | Exit | Kết quả |
|---|---|---|
| `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c infra/lab/scripts/shard-test.sh -n 8 -race -tags p05cluster -o ~/p12bm-race2 ./hscontrol/servertest/` (cây `cec4c27`) | 0 | **212/212 PASS** (fail 0, skip 0, missing 0); wall 1.402 s, tổng tuần tự 3.321 s. Gồm mọi servertest cluster của P10–P12b và N1 (S04, S11, S15/`/verify`, S17), `TestTwoClusters*`, `TestScheduledMoveNotifiesOnlyItsClients`, `TestAPIv2*` (tscli, tofu) |
| `go test -race -count=5 ./internal/rebootstrap/ ./cmd/mesh-rebootstrap/` (`d889da7`) | 0 | Gồm 4 test retry mới |
| Mutation: ép `attempts := 1` trong `rebootstrap.Run` | 1 (mong đợi) | 3 test retry FAIL; khôi phục → PASS |
| `golangci-lint run ./internal/rebootstrap/... ./cmd/mesh-rebootstrap/...` (2.15.0 từ flake.lock, `b5e61e7`) | 0 | 0 issues |
| GitHub CI `b5e61e7` (run 45755076537) | — | 6/6 job xanh (lint, race không gồm servertest, servertest ×3, build) |
| GitHub CI `cec4c27` (merge `master`, run 45756318156) | — | 6/6 job xanh |
| E2E P12b `0a17355` `20261126T045654Z` | 0 | 13/13 (§2) |

**Lần chạy race đầu trên cây đã merge (không tính):** `~/p12bm-race`, RC 1: 180/212 PASS, 32 FAIL vì môi trường, không phải mã. 30 test `*Compat` không mở được `testdata/*.hujson`: cây trên mc-ci lúc đó chỉ đối chiếu file `.go`, thiếu 1.356 file testdata. `TestAPIv2`, `TestAPIv2OAuthScopes`: thiếu `tscli`/`tofu` vì không chạy trong `nix shell`. Đã bổ sung testdata (1.319 file lấy từ cây N1 trên mc-ci có blob khớp, 38 file chép từ máy làm việc), đối chiếu lại toàn bộ cây và chạy lại trong `nix shell` (dòng đầu bảng). Binary của E2E build từ cây cũ: mọi file chênh lệch của cây đó đều là file **thiếu** (không file nào khác nội dung), và một file `go:embed` thiếu thì build đã lỗi, nên binary đúng code của `cec4c27`.

**Bằng chứng đã có của từng ticket** (không chạy lại ở đây): P10 `evidence/source-inventory-P10.md`, `…-P10-placement.md`; P10b–P10d `evidence/source-inventory-P10b.md`, `…-P10c.md`, `…-P10d.md`; P11 `evidence/source-inventory-P11.md` §10; P11b `evidence/P11b-cluster-failover.md`; P12 `evidence/P12-placement-reconcile.md`; P12b `evidence/P12b-tenant-move.md`.

## 5. Negative tenant isolation tests

- N1 (đã review): S01–S18, M06 với client thật, 4 tổ hợp (`evidence/P09-N1.md`).
- N2: điều kiện 3 và 5 ở §1; P11b I01 (tailnet b không thấy node của a qua F01–F04); P12b M05 (tailnet đối chứng o không nhận thông báo, không bị chuyển, chỉ thấy peer của mình), `TestScheduledMoveNotifiesOnlyItsClients`, `TestMoveNoticesOnlyOwnOpenMoves`, `TestAPIv2ScheduledMove`.
- P12b M07: artifact không chứa auth key, API key, private key hay machine/node key đầy đủ.

## 6. DB migrations and downgrade constraints

- P11 `202611232000-tailnets-moving-to`; P12b `202611261000-tailnet-moves`. Cả hai nối cuối chuỗi v0.1, chạy trong transaction, không `AutoMigrate`, có rollback.
- Cluster, mesh-placement và mesh-bootstrap phải nâng cấp cùng nhau (bootstrap mở DB read-only, đòi migration mới nhất).

## 7. Known limitations / security blockers

- **Không có security blocker** trong phạm vi N2. Không lần chạy nào ghi nhận owner kép hay hai process cùng phục vụ một tailnet.
- Gate N2 được review ở mức gate (file này). Evidence riêng của P10–P12 vẫn ghi trạng thái của chính chúng.
- P11 còn mở: test timing phản hồi từ chối qua mạng, spec OpenAPI cho bootstrap, review bảo mật (`evidence/source-inventory-P11.md` §10).
- P12: CLI qua Registry ở nhánh `feat/P12-cli`, chưa merge.
- Chuyển tenant cần mọi client đăng nhập lại (Q1: không đổi protocol). iOS/Android cần người dùng thao tác.
- Ngoài N2 (N3): HA PostgreSQL, HA PgBouncer, backup/restore không lùi epoch, xoay Noise key.

## 8. Reviewer approval

- **Chủ dự án, 2026-11-26:** duyệt Gate N2; merge `feat/P12b-inventory` vào `master` và push.
- **Ngưỡng Q7 của P12b (chủ dự án chốt cùng ngày, theo số đo của lần chạy sạch):** M04 FAIL khi t100 của 24 client (8 song song) > 15 s, ping gap > 10 s, hoặc cần can thiệp tay. Lần chạy sạch đạt cả ba (6,1 s / 0,39 s; 5,2 s / 5,1 s; 0). Harness kiểm các ngưỡng này từ `5ef2069`.
