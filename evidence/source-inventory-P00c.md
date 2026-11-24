# PR P00c Evidence — tối ưu full rebuild peer map + đo server-only

- **Trạng thái:** `Implemented`. Unit/race tests trên cây đã port: xem §4 (exit code thật). Số đo capacity ở §6 là của **cây trước khi port** (chạy ngày 2026-11-22 trên mc-bench/mc-ci), **chưa** đo lại trên cây đã port lên `master`. Chưa `Reviewed`.
- **Nhánh:**
  - `feat/P00c-peermap-perf-base`: nguyên 15 commit P00c như đã làm trên mc-ci (`~/src/p00c/src`, chưa từng push), trên `82f9b36` (rescue, không sửa).
  - `feat/P00c-peermap-perf`: cùng các commit đó cherry-pick lên `master` `a316f78` (có P04+P05), cộng 2 commit sửa test thất bại (§3).

## 1. Repo commit before / after

| | Commit |
|---|---|
| Cây P00c gốc (mc-ci) dựng trên | `82f9b36b` (merge `agent/bench-db-pgbouncer`, đã có P03a/P03b) |
| Đầu P00c gốc | `b66cc73a` (= tip `feat/P00c-peermap-perf-base`) |
| Port: before | `master` `a316f78` |
| Port: after | tip `feat/P00c-peermap-perf` (commit chứa file này) |
| Binary "before" của số đo (`headscale-base`) | `a17872f0` (merge P00b), `vcs.modified=false` — **không** phải `82f9b36b`: before/after còn khác nhau cả P03a/P03b, không riêng P00c |
| Binary "after" của số đo (`headscale-new`) | build từ cây P00c tại thời điểm chạy (17:56–18:43 UTC). Code production không đổi sau `85ab4141` (13:52 UTC); các commit sau đó chỉ sửa `tools/` và test. File `bin/headscale-new` hiện có là build lại từ `b66cc73a` lúc 19:04 UTC, sau khi đo xong |

Thư mục `~/src/p00c/base` trên mc-ci là git worktree của `~/src/p00c/src`, detached ở `a17872f0`, sạch.

Port lên `master`: cherry-pick 15 commit **không có conflict**. Giữa `82f9b36b` và `a316f78`, `hscontrol/policy/**`, `hscontrol/mapper/**` và `hscontrol/state/node_store.go` không đổi; `hscontrol/state/state.go` chỉ tách `NewStateWithDB` (P05). Ngữ nghĩa P05 giữ nguyên: `BuildPeerMap` chạy trên `PolicyManager` của **một** runtime tailnet, input là node của NodeStore runtime đó; không có state mới dùng chung giữa các runtime.

## 2. Path & symbol đã đọc / sửa

| Path | Symbol | Thay đổi |
|---|---|---|
| `hscontrol/policy/v2/policy.go` | `BuildPeerMap`, `buildPeerMapPairwiseLocked` | `BuildPeerMap` gọi bản có index; vòng lặp từng cặp cũ giữ lại thành `buildPeerMapPairwiseLocked` (tham chiếu + fallback) |
| `hscontrol/policy/v2/peermap_index.go` (mới) | `buildPeerMapIndexedLocked`, `peerClass`, `peerIndex`, `peerNode` | Gom node theo lớp (cùng tập matcher global phía nguồn/đích), quyết định quan hệ một lần cho mỗi cặp lớp; matcher riêng node (autogroup:self, via) tra qua index địa chỉ; same-user và exit node theo danh sách. `pm.denyAll` → không peer nào. ID trùng hoặc view không hợp lệ → fallback pair scan |
| `hscontrol/policy/matcher/matcher.go` | `Match.SrcRanges`, `Match.DestRanges` | accessor chỉ đọc cho index |
| `hscontrol/policy/v2/peermap_index_test.go` (mới) | `TestBuildPeerMapIndexedMatchesPairwise` (rapid, so khớp đúng từng phần tử + thứ tự với pair scan, gồm tenant deny-all `WithEmptyPolicyDenies`), `TestBuildPeerMapIndexedFallback` | |
| `hscontrol/state/node_store.go` | `processWrite`, `applyBatch`, `RebuildPeerMaps` | (P00c) work `rebuildPeerMaps` flush batch ngay, không chờ `batch_timeout`; (port) nhả writer theo **thứ tự hàng đợi** (§3) |
| `hscontrol/state/node_store_rebuild_test.go` (mới) | `TestRebuildPeerMapsAppliesQueuedWritesFirst` | |
| `tools/capload/` (mới) | `main.go`, `server.sh`, `run-cell.sh`, `run-seeded.sh`, `summarize.py`, `sync.sh`, `README.md` | load generator server-only: client `controlclient.Direct` thật ở host khác, mỗi node một map stream |
| `tools/capseed/main.go` (mới) | `run` | tạo snapshot DB có sẵn N node đã đăng ký (đo steady-state connect) |

## 3. Test thất bại `TestRebuildPeerMapsAppliesQueuedWritesFirst` — nguyên nhân & sửa

**Triệu chứng** (`~/src/p00c/race2.log`, cây `8a4fc758`): `node_store_rebuild_test.go:67: a write queued before the rebuild was not applied with it` — kiểm tra **không chờ** rằng channel `result` của 2 `put` xếp trước rebuild đã đóng khi `RebuildPeerMaps` trả về.

**Nguyên nhân gốc:** `applyBatch` nhả completion **theo loại**, không theo thứ tự hàng đợi: (1) gửi `nodeResult` (duyệt map, thứ tự ngẫu nhiên), (2) đóng `rebuildResult` của mọi `RebuildPeerMaps`, (3) mới đóng `result` của các write còn lại. Write **không bị mất**: snapshot mới (đã `Store`) chứa đủ các write trong batch, và peer map được tính từ snapshot đó. Lỗi là thứ tự acknowledge: caller của `RebuildPeerMaps` (đổi policy/user) có thể chạy tiếp trên snapshot mới trong khi `PutNode` xếp trước nó vẫn đang bị chặn. Upstream cũng có thứ tự này, nhưng khi rebuild còn chờ `batch_timeout` thì hiếm gặp; commit P00c `85ab4141` flush batch ngay khi có rebuild nên kịch bản "put rồi rebuild trong cùng batch" thành thường xuyên. Thông điệp test cũ ("not applied") mô tả sai: write đã được áp dụng, chỉ là chưa được báo xong.

Về revocation/stale peer map: vì snapshot chứa write, không có peer map cũ bị publish do lỗi này. Ảnh hưởng là vi phạm bảo đảm thứ tự mà comment test/commit đã hứa ("writes queued before it are applied ... before it returns"), có thể làm đảo thứ tự các follow-up (change notification) giữa hai caller.

**Commit cuối trên VM (`b66cc73a`)** đã đổi test thành chờ có giới hạn 10 s — tức là làm yếu test, che lỗi. Trên nhánh port:
- `3c8f190` (RED): khôi phục kiểm tra không chờ, đặt **trước** các assert snapshot.
- `2ef3c84` (fix): `applyBatch` duyệt `batch` theo thứ tự; với mỗi work: gửi `nodeResult`/`errResult` (nếu có) rồi đóng `result`, hoặc đóng `rebuildResult`. Bỏ `nodeResultRequests`/`rebuildOps`. Các channel `nodeResult`/`errResult` đều buffered 1 nên không thêm điểm chặn.

## 4. Tests — lệnh chính xác và exit code (mc-ci, `go1.27.4 linux/amd64`, cây `~/src/p00c-rescue/repo` clone từ bundle)

| Lệnh | Commit | Exit | Ghi chú |
|---|---|---:|---|
| `go test -race -count=300 -run TestRebuildPeerMapsAppliesQueuedWritesFirst ./hscontrol/state/` | `3c8f190` (RED) | **1** | 1/300 FAIL, `node_store_rebuild_test.go:72: RebuildPeerMaps returned before a write queued ahead of it was acknowledged` (lỗi phụ thuộc lịch, không tất định) |
| cùng lệnh, `-count=300` | `2ef3c84` | 0 | |
| cùng lệnh, `-count=3000` | `2ef3c84` | 0 | 28.4 s |
| `go build ./...` | `2ef3c84` | 0 | |
| `go vet ./...` | `2ef3c84` | 0 | |
| `go test -race -count=1 -p 8 ./hscontrol/state/ ./hscontrol/mapper/ ./hscontrol/db/... ./hscontrol/policy/...` | `2ef3c84` | 0 | 9 gói ok; `policy/v2` 501 s, `state` 256 s, `db` 180 s, `mapper` 116 s |
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | `2ef3c84` | 0 | `servertest` ok, 65 s |

Log trên mc-ci: `~/src/p00c-rescue/{red,green,green3000,race-main,race-cluster}.log` (không commit).

## 5. Negative tenant isolation

- Không đổi code định tuyến tenant, DB scope hay runtime. Peer map vẫn tính riêng từng runtime tailnet (P05).
- Tenant deny-all (`WithEmptyPolicyDenies`, P03a): `buildPeerMapIndexedLocked` trả map rỗng khi `pm.denyAll`, giống pair scan; property test so khớp hai bản có cả trường hợp này.
- Suite cluster P05 (`-tags p05cluster -run Cluster`: `TestClusterTwoTailnetsPeersIsolated`, `TestClusterSessionCannotSwitchTenant`, `TestClusterOverlapAndConfigIndependence`, ...) chạy lại ở §4.

## 6. Số đo server-only (cây trước khi port; một mẫu mỗi ô)

Server: `mc-bench` (8 vCPU / 24 GiB), load: `mc-ci` (`capload`), client `controlclient.Direct` thật qua LAN, 10 node/user, không retry, timeout 120 s/node. RSS/CPU server từ `summarize.py` trên `samples.csv` (cả lần chạy). Raw results **không** commit: `mc-ci:~/src/p00c/results/<cell>/`, `mc-bench:~/p00c/results/<cell>/`.

**Lưu ý so sánh:** có hai kiểu workload, không được trộn.
- *enroll* (base-*, P00c lần đầu): đăng ký mới N node, 32 song song, hạn 20 phút.
- *seeded* (base-seeded-*, new-*): DB đã có N node (capseed); pha `connect` = N node có sẵn kết nối lại (256 song song), pha `register` = đăng ký thêm 500 node mới trên nền N node.

Con số "10k personal_devices 3 757/10 000 trong 20 phút → 10 000/10 000 trong 72 s" **so sánh hai workload khác nhau** (enroll vs connect lại). Với cùng workload seeded, pha connect của binary cũ cũng đạt 10 000/10 000 trong 72 s; cải thiện thật nằm ở pha đăng ký mới (bảng B).

### A. Enroll (chỉ binary cũ `a17872f0`)

| Ô | Enrolled | Wall | register p95 | first map p95 | Server peak RSS | CPU s |
|---|---:|---:|---:|---:|---:|---:|
| 1k allow_all | 1 000/1 000 | 82 s | 0.93 s | 3.87 s | — | — |
| 1k isolated | 1 000/1 000 | 65 s | 0.56 s | 1.53 s | — | — |
| 1k personal_devices | 1 000/1 000 | 74 s | 1.33 s | 1.52 s | — | — |
| 5k isolated | 5 000/5 000 | 367 s | 1.37 s | 1.64 s | 1.17 GB | 508 |
| 5k personal_devices | 4 480/5 000 (hết 20 phút) | 1 200 s | 18.4 s | 1.63 s | — | — |
| 5k allow_all | 3 656/5 000 | 1 200 s | 5.7 s | 15.3 s | — | — |
| 10k isolated | 7 640/10 000 | 1 200 s | 3.1 s | 8.9 s | 1.84 GB | 2 914 |
| 10k personal_devices | 3 757/10 000 | 1 200 s | 13.8 s | 14.8 s | 0.92 GB | 1 952 |
| 10k allow_all | 3 349/10 000 | 1 200 s | 4.8 s | 15.3 s | 5.93 GB | 7 978 |

Không có ô enroll nào chạy với binary mới.

### B. Seeded: connect N + register 500 (before = `a17872f0`, after = P00c)

| Ô | connect ok / wall | connect first-map p95 | register 500: ok / wall | register p95 | Server peak RSS | CPU s |
|---|---|---:|---|---:|---:|---:|
| 5k personal_devices — before | 5 000 / 18.5 s | 1.83 s | **75/500** / 240 s (hết hạn phase) | 118 s | 1.55 GB | 713 |
| 5k personal_devices — after | 5 000 / 19.2 s | 1.79 s | **500/500** / 12.6 s | 4.9 s | 1.64 GB | 230 |
| 10k personal_devices — before | 10 000 / 72.0 s | 3.49 s | **0/500** / 240 s | — | 2.86 GB | 1 197 |
| 10k personal_devices — after | 10 000 / 72.0 s | 3.49 s | **500/500** / 33.4 s | 15.8 s (323/500 > 10 s) | 3.14 GB | 1 003 |
| 10k isolated — after | 10 000 / 70.6 s | 3.68 s | 500/500 / 31.6 s | 14.8 s (318/500 > 10 s) | 2.97 GB | 998 |
| 10k allow_all — after | **6 180/10 000** / 1 200 s | 43.7 s | 0/500 / 240 s | — | **23.5 GB** | 8 351 |

(`before` 10k isolated/allow_all seeded: không chạy.)

## 7. Known limitations / blockers

- Số đo §6 là của cây trước khi port, binary before là `a17872f0` (trước P03a/P03b), một mẫu mỗi ô. **Chưa** đo lại trên `feat/P00c-peermap-perf` sau port.
- 10k `allow_all` vẫn không đạt: 6 180/10 000 node kết nối trong 20 phút, server chạm ~23.5 GB RSS trên VM 24 GiB (gần OOM). Giới hạn còn lại là map đầy đủ O(n) mỗi node (≈10k peer/map) và fan-out, không còn là `BuildPeerMap`. Đó là phạm vi P00d (netmap tăng dần).
- 10k personal_devices/isolated: đăng ký mới trên nền 10k vẫn có register p95 ≈ 15–16 s (> 10 s của TestScale).
- Không có số microbenchmark `BenchmarkCapacity_*` (P00b) cho bản index trong `results/`; chưa chạy lại.
- Raw logs và `nodes.csv` chưa được rà soát để commit (log headscale có key rút gọn).
- Unit/race test **không** chứng minh an ninh với client thật; E2E cluster thật của P05 chưa chạy lại với thay đổi này.
- 15 commit gốc trên VM giữ nguyên tác giả/nội dung khi cherry-pick, kể cả commit `b66cc73a` (làm yếu test); commit `3c8f190` đảo lại phần đó.

## 8. Reviewer approval

Chưa có.

## 8. Merge vào `master` (2026-11-23, theo lệnh chủ dự án)

Merge `37f8f2c` = `master` `df7a4b8` (đã có P08, P09a, P10, P10b, P10c) + nhánh này, không có conflict. Kiểm chứng trên cây đã gộp (code giống hệt commit thử `0ae602e`, chỉ khác evidence):

| Lệnh | Máy | Exit | Ghi chú |
|---|---|---:|---|
| `go build ./... && go vet ./hscontrol/state/ ./hscontrol/policy/... ./hscontrol/db/...` | mc-bench | 0 | |
| `go test -race -count=1 -p 6 -timeout 90m ./hscontrol/... ./internal/... ./cmd/...` | mc-bench | 1 | 31 package ok (gồm `hscontrol/state` 306 s, `policy/v2` 547 s, `mapper` 132 s). `servertest` FAIL chỉ ở `TestAPIv2` và `TestAPIv2OAuthScopes`: `exec: "tscli"` / `"tofu": executable file not found` (mc-bench không có nix dev shell) |
| `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -race -count=1 -run '^(TestAPIv2\|TestAPIv2OAuthScopes)$' ./hscontrol/servertest/` | mc-ci | 0 | 50 s |
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | mc-ci | 0 | 114 s |
| E2E `infra/lab/e2e/p05/run.sh`, binary `37f8f2c` (CGO_ENABLED=0, sha256 `2b1ec3ced400c4d3…`) | mc-cp | 0, 0 | **19/19 PASS ×2**: `artifacts/37f8f2c/P05/e2e-20261123T122016Z/`, `…T122630Z/`; `cleanup_ok=1`, 0 key chưa redact |

Trạng thái vẫn là `Implemented` (chưa `Reviewed`). Số đo capacity §6 vẫn là của cây trước khi port, chưa đo lại trên `master`; đó là điều kiện để đóng N0 và là chuẩn so sánh cho P00d.

## 9. Đo lại capacity trên `master` (2026-11-23)

Mục đích: xác nhận số đo §6 (cây trước khi port) còn đúng sau khi P00c được gộp vào `master` có P04–P10c. Đây cũng là số "before" của P00d.

- **Binary:** before = `df7a4b8` (`master` ngay trước merge P00c, sha256 `7235af135011a578…`); after = `3269688` (`master` sau merge, sha256 `95aa54ff96bf3082…`). Cả hai build `CGO_ENABLED=0` trên mc-bench, Go 1.27.4.
- **Snapshot:** seed lại bằng `tools/capseed` của `master` (sha256 `f4c3e375d0972b44…`), vì schema đã đổi từ P05/P10: 5 000 node (500 user, 32,5 s), 10 000 node (1 000 user, 119 s). Snapshot cũ giữ ở `mc-bench:~/p00c/snap-82f9b36/`.
- **Harness:** `tools/capload/run-seeded.sh LABEL N POLICY 500` của `master`; `capload` build từ `3269688` (sha256 `732fb7eef61fde82…`). Server mc-bench (8 vCPU / 24 GiB), tải mc-ci, `HOLD=60s`, `DEADLINE=20m`, kết nối 256 song song. Một mẫu mỗi ô; before/after chạy xen kẽ theo từng ô.
- **Raw:** `mc-ci:~/src/p00c/results/m-*/`, `mc-bench:~/p00c/results/m-*/` (không commit).

| Ô | connect: ok / wall | register 500: ok / wall | Server peak RSS | CPU s (cả lần chạy) |
|---|---|---|---:|---:|
| 5k personal_devices, before | 5 000 / 19,3 s | **257/500** / 204 s | 1,59 GB | 650 |
| 5k personal_devices, after | 5 000 / 18,8 s | **500/500** / 14,1 s | 1,60 GB | 329 |
| 10k personal_devices, before | 10 000 / 75,7 s | **0/500** / 240 s (hết hạn pha) | 2,97 GB | 1 227 |
| 10k personal_devices, after | 10 000 / 74,4 s | **500/500** / 37,9 s | 2,95 GB | 1 063 |
| 10k isolated, before | 10 000 / 74,8 s | 500/500 / 86,6 s | 2,93 GB | 1 043 |
| 10k isolated, after | 10 000 / 72,9 s | 500/500 / 29,2 s | 2,88 GB | 992 |
| 10k allow_all, after | **6 161/10 000** / 1 200 s | 0/500 / 240 s | **23,3 GB** (VM 24 GiB) | 9 081 |

Kết luận (số đo, không phải tuyên bố capacity):
- Cải thiện của P00c giữ nguyên trên `master`: pha đăng ký thêm trên nền 5k/10k từ 0–257/500 lên 500/500. Pha kết nối lại không đổi, như §6.
- 10k allow_all vẫn không đạt (6 161/10 000 trong 20 phút, gần hết RAM), giống §6 (6 180, 23,5 GB). Đây là phạm vi P00d (map đầy đủ O(n) mỗi node và fan-out).
- Trạng thái P00c vẫn `Implemented`, chưa `Reviewed`; một mẫu mỗi ô.
