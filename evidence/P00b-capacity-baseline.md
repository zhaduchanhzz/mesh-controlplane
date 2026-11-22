# P00b — Baseline năng lực một process (1k / 5k / 10k node)

- **Trạng thái:** `Verified` cho các phép đo liệt kê dưới đây (đã chạy, có số và exit code). Chưa `Reviewed`.
- **Ngày đo:** 2026-11-22, 10:40–13:23 UTC.
- **Phạm vi:** phép đo, **không** phải capacity claim. Mọi con số chỉ đúng cho workload, commit và phần cứng ghi dưới đây.

## 1. Môi trường

| Mục | Giá trị |
|---|---|
| Commit | `4f8af3fc9dcbc724910904fbb1007da9f153921f` (nhánh `agent/p00b-bench`) + 2 file test mới chưa commit (mục 2) |
| Chuyển mã nguồn | `git bundle create … HEAD` → `scp` → `git clone` trên VM (không copy cây Windows); 2 file test copy riêng (LF) |
| VM | `mc-bench` (192.168.1.242), Ubuntu 24.04.5 LTS, kernel 6.8.0-147, 8 vCPU, 24 GiB RAM, không swap |
| CPU | Intel Xeon E5-2696 v3 @ 2.30GHz (host 2 socket; VM thấy 8 vCPU) |
| Go | `go1.27.4 linux/amd64`, SHA-256 tarball `7226736c…dab24840` khớp `go.dev/dl/?mode=json&include=all` |
| `GOMAXPROCS` | mặc định = 8; `GOMEMLIMIT` không đặt |
| Giới hạn | mỗi lần chạy `TestScale` trong `systemd-run --user --scope -p MemoryMax=22G -p MemorySwapMax=0` |
| Tải khác | không có job nặng khác trên VM trong lúc đo, trừ 2 lần `go test -bench` ngắn (~10 s, profiling) chạy trong lúc `10000/isolated` đang ở pha tạo user (CPU gần như rảnh) |

## 2. File thêm (chỉ `_test.go`, không đổi code non-test)

| File | Nội dung |
|---|---|
| `hscontrol/state/capacity_bench_test.go` | `BenchmarkCapacity_BuildPeerMap` và `BenchmarkCapacity_SnapshotRebuild`: 4 policy giống `TestScale` (`allow_all`, `isolated`, `personal_devices`, `shared_service`), 10 node/user, n = 1000/5000/10000 (đổi bằng `HEADSCALE_CAPACITY_NODES`). Báo `ns/op`, `B/op`, `allocs/op`, `peer-entries` và `retained-MB` (heap sống của **một** kết quả sau `runtime.GC()` x2). Snapshot rebuild dùng `policyPeersFunc` thật (gồm `SetNodes`) và `snapshotFromNodes(…, reusePeers=false, reuseRoutes=false)`. |
| `hscontrol/servertest/capacity_main_test.go` | `TestMain` chỉ bật khi có `HEADSCALE_CAPACITY_OUT`: mẫu 1 s heap/Go-total/goroutine (`runtime/metrics`) → `samples.csv`; ghi `heap-peak.prof` khi heap tăng >15 %; dump mọi metric `headscale_*` lúc thoát → `metrics.txt`. Không có env thì là `m.Run()` thuần. |

`go vet ./hscontrol/state ./hscontrol/servertest` trên VM: exit 0.

## 3. Lệnh chính xác

Binary test build một lần: `go test -c -o ~/bin/{state,mapper,servertest}.test ./hscontrol/{state,mapper,servertest}`.

```sh
# (a) Benchmark peer map / snapshot — exit 0, wall 28:47, max RSS 2,010,232 KB
~/bin/state.test -test.run '^$' -test.bench 'BenchmarkCapacity_' -test.benchmem -test.count=3 -test.timeout=90m

# (b) Benchmark batcher tier 1–4 ở 1k/5k/10k — exit 0, wall 12:33, max RSS 3,310,992 KB
~/bin/mapper.test -test.run '^$' -test.bench 'BenchmarkScale_/^(1000|5000|10000)$' -test.benchmem -test.count=3 -test.timeout=60m

# (c) TestScale, mỗi ô một process mới (N, CASE, TMIN theo bảng 5)
HEADSCALE_STRESS_NODES=$N HEADSCALE_CAPACITY_OUT=$OUT \
systemd-run --user --scope -p MemoryMax=22G -p MemorySwapMax=0 \
  /usr/bin/time -v -o $OUT/time.txt \
  ~/bin/servertest.test -test.run "^TestScale/${N}_nodes/${CASE}\$" -test.count=1 -test.v \
  -test.timeout=${TMIN}m -test.cpuprofile=$OUT/cpu.prof
```

Tách server/client: `go tool pprof -focus=<regex>` trên `heap-peak.prof` (inuse_space) và `cpu.prof`, với client = `tailscale.com/control/controlclient|servertest.(*TestClient)`, server (heap) = `hscontrol.(*Headscale)|hscontrol/mapper|hscontrol/state|hscontrol/policy|hscontrol/db|hscontrol.(*noiseServer)`, server (CPU) = `NoiseUpgradeHandler|mapper.(*Batcher)|state.(*NodeStore)|state.(*State)`.

Artifacts thô (log, `cpu.prof`, `heap-peak.prof`, `metrics.txt`, `samples.csv`, `time.txt`, `meta.txt`) nằm trên VM tại `mesh@192.168.1.242:~/results/`; **chưa** copy vào repo (log chứa key rút gọn đã redact của headscale nhưng chưa được rà soát).

## 4. Microbenchmark peer map — **đo** (median của 3 mẫu)

### 4.1 `BuildPeerMap` (chỉ policy manager)

| Policy | n | thời gian/op | B/op | allocs/op | peer-entries | retained |
|---|---:|---:|---:|---:|---:|---:|
| allow_all | 1 000 | 57.4 ms | 25.5 MB | 16 014 | 999 000 | 9.84 MB |
| allow_all | 5 000 | 1.88 s | 642.7 MB | 100 038 | 24 995 000 | 195.6 MB |
| allow_all | 10 000 | 8.17 s | 3.58 GB | 230 070 | 99 990 000 | **938.1 MB** |
| isolated | 1 000 | 24.8 ms | 0.56 MB | 7 014 | 9 000 | 0.20 MB |
| isolated | 5 000 | 767 ms | 2.61 MB | 35 038 | 45 000 | 0.92 MB |
| isolated | 10 000 | 3.36 s | 5.23 MB | 70 070 | 90 000 | 1.85 MB |
| personal_devices | 1 000 | 624 ms | 192.5 MB | 4.00 M | 9 000 | 0.20 MB |
| personal_devices | 5 000 | 19.8 s | 4.80 GB | 100.0 M | 45 000 | 0.93 MB |
| personal_devices | 10 000 | 78.6 s | **19.2 GB** | 400.0 M | 90 000 | 1.85 MB |
| shared_service | 1 000 | 30.1 ms | 0.59 MB | 8 021 | 10 980 | 0.21 MB |
| shared_service | 5 000 | 1.02 s | 2.78 MB | 40 049 | 54 980 | 0.96 MB |
| shared_service | 10 000 | 4.05 s | 5.66 MB | 80 084 | 109 980 | 1.94 MB |

Dải 3 mẫu ở 10k: allow_all 7.74–8.55 s; isolated 3.34–3.36 s; personal_devices 64.9–87.4 s; shared_service 3.99–4.31 s.

### 4.2 `snapshotFromNodes` với `policyPeersFunc` thật (chi phí mỗi batch đổi topology)

| Policy | n | thời gian/op | B/op | allocs/op | retained (cả snapshot) |
|---|---:|---:|---:|---:|---:|
| allow_all | 1 000 / 5 000 / 10 000 | 66.0 ms / 1.97 s / 8.75 s | 27.5 MB / 652 MB / 3.60 GB | 24.6 k / 143 k / 315 k | 10.7 / 199.8 / **946.5 MB** |
| isolated | 1 000 / 5 000 / 10 000 | 32.6 ms / 912 ms / 3.71 s | 2.5 / 12.0 / 24.1 MB | 15.6 k / 77.8 k / 155 k | 1.1 / 5.1 / 10.2 MB |
| personal_devices | 1 000 / 5 000 / 10 000 | 822 ms / 24.6 s / **101 s** | 195 MB / 4.81 GB / 19.2 GB | 4.0 M / 100 M / 400 M | 1.1 / 5.1 / 10.2 MB |
| shared_service | 1 000 / 5 000 / 10 000 | 30.7 ms / 957 ms / 4.04 s | 2.6 / 12.2 / 24.6 MB | 16.6 k / 82.8 k / 165 k | 1.1 / 5.2 / 10.3 MB |

### 4.3 Scaling quan sát (tỉ số từ bảng trên; lý thuyết n²: ×25 cho 1k→5k, ×4 cho 5k→10k)

| Chỉ số | 1k→5k | 5k→10k | Kết luận |
|---|---:|---:|---|
| allow_all retained | ×19.9 | ×4.80 | ≈ n² (≈ 9.4–10.3 byte/entry, entries đúng n(n−1)) |
| allow_all thời gian | ×32.8 | ×4.35 | ≈ n² |
| isolated thời gian | ×30.9 | ×4.38 | ≈ n² **dù không có peer nào ngoài cùng user**; bộ nhớ ≈ n |
| personal_devices thời gian | ×31.7 | ×3.97 | ≈ n²; alloc ×25 / ×4 (≈ 8 alloc, 384 B mỗi cặp node) |
| shared_service thời gian | ×33.8 | ×3.99 | ≈ n²; bộ nhớ ≈ n |

Ước lượng trước đây "~200 MB ở 5k, ~800 MB ở 10k cho riêng peer map": **đo được** 195.6 MB ở 5k (khớp) và 938 MB ở 10k (cao hơn ước lượng ~17 %). Ngoài phần giữ lại, mỗi lần rebuild ở 10k allow_all cấp phát thêm 3.58 GB tạm (append tăng dần slice).

### 4.4 Hot spot trong `BuildPeerMap` (CPU profile microbench, đo)

- `isolated` n=5000: ~47 % CPU ở `runtime.mapaccess*` trong vòng lặp cặp (tra map `routeInfo[ids[j]]` cho `addCapSourceOnExit`, cộng `sameUser`/`IsTagged`) — vòng O(n²) vẫn chạy toàn bộ dù không matcher nào khớp (`hscontrol/policy/v2/policy.go` ~917–933).
- `personal_devices` n=1000 (đường per-node, autogroup:self): 53 % ở `NodeView.CanAccessWithRoutes`→`Node.canAccess`; `Node.IPs()` cấp phát slice mỗi lần gọi (29 % cum), `growslice`/`mallocgc` ~25 % (`policy.go` ~963–987, 4 lần `CanAccessWithRoutes` mỗi cặp).

## 5. Microbenchmark batcher — **đo** (median của 3 mẫu, `BenchmarkScale_*`)

| Benchmark | 1 000 | 5 000 | 10 000 | Ghi chú |
|---|---:|---:|---:|---|
| IsConnected | 53 ns | 63 ns | 69 ns | ~phẳng |
| AddToBatch_Targeted | 1.03 µs | 3.31 µs | 6.77 µs | tier "O(1)" nhưng tăng ~tuyến tính |
| ConnectionChurn | 0.94 µs | 1.29 µs | 1.38 µs | |
| AddToBatch_Broadcast | 0.27 ms, 160 KB | 1.64 ms, 800 KB | 3.77 ms, 1.6 MB | O(n) |
| AddToBatch_FullUpdate | 0.40 ms | 2.87 ms | 6.18 ms | O(n) |
| ProcessBatchedChanges | 29.6 µs | 168 µs | 367 µs | O(n) |
| BroadcastToN | 0.60 ms | 6.1 ms | 8.5 ms | nhiễu (4.2–13.5 ms) |
| SendToAll | 1.14 ms | 6.59 ms | 13.8 ms | O(n) |
| ConnectedMap | 0.24 ms | 1.25 ms | 2.77 ms | |
| ComputePeerDiff | 77 µs | 399 µs | 900 µs | |
| UpdateSentPeers_Full | 0.20 ms | 0.80 ms | 1.79 ms | |
| UpdateSentPeers_Incremental | 8.0 µs | 44.6 µs | 94.7 µs | |
| MultiChannelBroadcast | 1.68 ms | 9.60 ms | 20.6 ms | |
| ConcurrentAddToBatch | 0.22 ms | 1.28 ms | — | bench chỉ tới 5k |
| ConcurrentSendAndChurn | 1.68 ms | 8.43 ms | — | bench chỉ tới 5k |
| MixedWorkload | 0.15 ms | 1.0 ms | — | bench chỉ tới 5k |
| SingleAddNode (full pipeline) | 28.7 ms, 54 MB/op | — | — | bench chỉ tới 1k |

Batcher tự thân (không sinh MapResponse) là O(n) mỗi broadcast và vài ms ở 10k — không phải nút cổ chai chính. `MapResponse_FullUpdate`/`AddAllNodes` chỉ định nghĩa tới 500 node nên không chạy ở tier này.

## 6. `TestScale` end-to-end — **đo** (một mẫu mỗi ô; server + client cùng process)

| N | Case | Exit | Wall | Hoàn thành | Max RSS cả process | Peak Go heap | CPU user+sys (avg %) |
|---:|---|---:|---:|---|---:|---:|---:|
| 1 000 | allow_all | 0 | 4:11 | PASS mọi pha | **8.35 GB** | 7.41 GB | 1 419 s (565 %) |
| 1 000 | isolated | 0 | 2:08 | PASS | 0.55 GB | 0.34 GB | 134 s (104 %) |
| 1 000 | personal_devices | 0 | 2:24 | PASS | 0.58 GB | 0.39 GB | 182 s (126 %) |
| 5 000 | isolated | 0 | 17:05 | PASS | 2.76 GB | 1.87 GB | 3 286 s (320 %) |
| 5 000 | personal_devices | 1 | 11:37 | FAIL ở enroll, 2 459/5 000 client | 1.31 GB | 0.95 GB | 1 388 s (199 %) |
| 5 000 | allow_all | 1 | 10:54 | FAIL ở enroll, 1 567/5 000 client | **15.9 GB** | 13.8 GB | 3 059 s (467 %) |
| 10 000 | isolated | 1 | 43:32 | FAIL ở enroll, 9 238/10 000 client | 4.53 GB | 3.45 GB | 9 630 s (368 %) |
| 10 000 | personal_devices | 1 | 14:42 | FAIL ở enroll, 2 045/10 000 client | 1.11 GB | 0.79 GB | 1 353 s (153 %) |
| 10 000 | allow_all | 1 | 14:44 | FAIL ở enroll, 1 471/10 000 client | 13.9 GB | 12.3 GB | 2 899 s (328 %) |

- Không có OOM: không ô nào chạm `MemoryMax=22G`. Mọi FAIL đều là **deadline 10 s của helper đăng ký** (`register request: context deadline exceeded` hoặc `no map after 10s`, `scale_test.go:262`), tức server không trả lời đăng ký/map đầu tiên trong 10 s dưới tải. Theo README đây là kết quả, không retry. Client thật sẽ retry; phép đo này **không** cho biết server có hội tụ nếu được chờ lâu hơn.
- `shared_service` và `personal_ssh` không chạy end-to-end (hết time-box).
- Pha tạo user (trước khi đăng ký node) tốn ~0.5 s/user: 51 s cho 100 user, 251 s cho 500, 502 s cho 1 000 (từ `samples.csv`, lúc goroutine bắt đầu tăng). `CreateUser`→`updatePolicyManagerUsers`→`RebuildPeerMaps` chờ batch NodeStore (`node_store_batch_timeout` mặc định 1 s). Thời gian `enroll` dưới đây đã gồm phần này.

### 6.1 Thời gian pha (ô PASS)

| Ô | enroll | burst 0/1/2 | revoke | restore | reconnect |
|---|---:|---:|---:|---:|---:|
| 1k allow_all | 226.7 s | 6.09 / 1.08 / 1.53 s | 0.81 s | 5.70 s | 7.89 s |
| 1k isolated | 123.5 s | 0.41 / 0.45 / 0.43 s | 0.53 s | 0.57 s | 1.17 s |
| 1k personal_devices | 137.6 s | 0.38 / 0.37 / 0.39 s | 0.54 s | 1.43 s | 2.12 s |
| 5k isolated | 981.8 s | 5.34 / 5.14 / 5.12 s | 3.39 s | 3.16 s | 7.65 s |

Đăng ký node thuần (enroll trừ tạo user): 1k isolated ≈ 72 s, 5k isolated ≈ 731 s → ×10.1 cho ×5 node (≈ n^1.44).

### 6.2 Metric server (`metrics.txt`, cộng dồn cả lần chạy)

| Ô | peers calc: số lần / tổng / TB | snapshot build tổng | MapResponse `peers` + `policy` | allocated trong enroll |
|---|---:|---:|---:|---:|
| 1k allow_all | 155 / 3.28 s / 21 ms | 4.72 s | 72 228 + 44 081 | 175.5 GB |
| 1k isolated | 137 / 0.48 s / 3.5 ms | 0.72 s | 39 440 + 85 954 | 7.5 GB |
| 1k personal_devices | 137 / 12.2 s / 89 ms | 12.5 s | 51 374 + 112 349 | 12.3 GB |
| 5k isolated | 662 / 51.4 s / 78 ms | 90.6 s | 2 153 464 + 2 190 113 | 184.4 GB |
| 5k personal_devices (tới 2 459) | 579 / 154.7 s / 267 ms | 160.3 s | 557 396 + 825 600 | 103.2 GB |
| 10k isolated (tới 9 238) | 1 292 / 342.0 s / 265 ms | 552.1 s | 7 442 064 + 6 520 660 | 572.1 GB |

(TB là trung bình trên các kích thước tăng dần trong lúc enroll, không phải chi phí tại N cuối.) MapResponse `peers`+`policy` từ 1k→5k isolated tăng ×34.7: mỗi lần node join sinh thay đổi broadcast tới mọi node đã kết nối → enrollment O(n²) thông điệp, kể cả khi không node nào thấy nhau.

### 6.3 Tách server / client (**xấp xỉ**, không phải RSS riêng server)

Không tách được RSS riêng server: client chạy cùng process. Chỉ phân bổ theo stack trong profile:

| Ô | Heap (inuse tại `heap-peak.prof`): client / server / còn lại | CPU: client / server / GC |
|---|---|---|
| 1k allow_all (3.31 GB) | 61 % / 27 % / 12 % | 38 % / 49 % / 11 % |
| 5k allow_all (6.92 GB) | 61 % / 25 % / 14 % | 37 % / 49 % / 11 % |
| 10k allow_all (5.62 GB) | 61 % / 24 % / 15 % | 37 % / 49 % / 11 % |
| 1k isolated (167 MB) | 24 % / 32 % / 44 % | 25 % / 53 % / 11 % |
| 5k isolated (870 MB) | 28 % / 29 % / 43 % | 26 % / 56 % / 9 % |
| 10k isolated (1.46 GB) | 26 % / 29 % / 45 % | 25 % / 57 % / 9 % |
| 5k personal_devices (465 MB) | 26 % / 33 % / 41 % | 23 % / 55 % / 9 % |

"Còn lại" là stack dùng chung (http2, zstd, net, runtime) không gán được. Profile heap phản ánh trạng thái sau GC gần nhất, nhỏ hơn RSS. Ở 1k allow_all, CPU lớn nhất là JSON encode/decode MapResponse (server `writeMapResponse` ~31 %, client `decodeMsg` ~27 %) và zstd.

## 7. Kết luận (chỉ trong phạm vi số đo)

**Trả lời câu hỏi: một process 8 vCPU / 24 GiB có phục vụ được 5k và 10k node không?**

- **allow_all — 5k: không đạt; 10k: không đạt** (đo được ở harness này). Cả hai FAIL khi mới ~1.5k client, process đã 13.9–15.9 GB RSS (≈ 60 % heap là client in-process). Phía server đo riêng (microbench): mỗi lần đổi topology ở 10k mất 8.2–8.8 s CPU một luồng, giữ 938–947 MB adjacency và cấp phát 3.6 GB tạm; mỗi thay đổi còn phải gửi map chứa ~10k peer tới ~10k node. Việc server-only có chịu được 10k allow_all với client thật hay không là **chưa đo** (cần server tách process + client thật).
- **isolated — 5k: đạt** (PASS mọi pha, 17 phút, ≤ 2.76 GB RSS cả process, burst hội tụ ~5 s). **10k: không đạt trong harness** — FAIL ở 9 238 node do deadline đăng ký 10 s; mỗi rebuild peer ở 10k mất 3.4–3.7 s dù không có peer chéo user.
- **personal_devices (autogroup:self) — 5k và 10k: không đạt.** FAIL khi mới ~2.0–2.5k node bất kể đích. Microbench: một rebuild 19.8 s ở 5k và 65–107 s ở 10k, cấp phát 4.8 GB / 19.2 GB mỗi lần. Đây là policy thực tế cho tenant doanh nghiệp, nên với code hiện tại một process **không** phục vụ tailnet 5k–10k node kiểu này.
- **shared_service:** chỉ có microbench (4.0 s/rebuild ở 10k, bộ nhớ nhỏ); chưa chạy end-to-end.

Hệ quả cho lớp shard *dedicated* (một tenant 5k–10k node / process): **chưa khả thi với code upstream hiện tại** ở mọi policy đã chạy end-to-end trừ isolated 5k. Theo điều kiện dừng của ticket, cần báo chủ dự án trước P10 và xem lại ADR 0003. Bộ nhớ của peer map riêng phần allow_all (~0.94 GB ở 10k) không phải giới hạn chính; giới hạn đo được là CPU/độ trễ của rebuild O(n²) và fan-out broadcast.

**Hot spot cần tối ưu (xếp theo tác động đo được):**

1. `BuildPeerMap` đường per-node (`policy.go` ~944–990): 8 alloc / 384 B mỗi cặp, `Node.IPs()` cấp phát mỗi lần; 78–101 s ở 10k. Ứng viên: precompute IP/matcher per node, nhóm theo user cho autogroup:self thay vì quét mọi cặp.
2. Vòng cặp đường global (`policy.go` ~917–933): tra map `routeInfo` và `sameUser` cho mọi cặp kể cả khi không matcher nào khớp (isolated 3.4 s ở 10k). Ứng viên: chỉ quét cặp khi có bitset nguồn/đích; gom theo user; lưu `isExit` theo chỉ số slice.
3. Rebuild toàn bộ adjacency mỗi batch đổi topology (`node_store.go` `snapshotFromNodes`): nên cập nhật gia tăng khi một node join/leave thay vì O(n²) mỗi lần.
4. Fan-out khi join: mỗi đăng ký sinh MapResponse `peers`/`policy` tới mọi node (14 M MapResponse cho 9.2k join ở isolated) → enrollment O(n²) thông điệp; chỉ nên gửi tới node bị ảnh hưởng.
5. Mã hóa MapResponse (JSON + zstd) chiếm phần lớn CPU server ở allow_all; map đầy đủ O(n) mỗi node → O(n²) byte mỗi thay đổi.
6. Ghi admin tuần tự (`CreateUser`) chờ batch NodeStore ~0.5 s/lần (1 000 user = 502 s).
7. `AddToBatch_Targeted` tăng 1.0 → 6.8 µs (1k→10k) dù thuộc tier "O(1)" — nhỏ nhưng trái kỳ vọng.

## 8. Đo vs suy luận, giới hạn

- **Đo:** mọi số ở mục 4–6.
- **Ngoại suy / suy luận (chưa đo):** chi phí server-only ở 5k/10k allow_all và personal_devices; việc server có hội tụ nếu deadline đăng ký dài hơn; ảnh hưởng của client thật/Noise thật qua mạng.
- Một mẫu cho mỗi ô `TestScale`; benchmark 3 mẫu. Biến thiên lớn ở personal_devices 10k (65–107 s).
- `GOMAXPROCS=8` dùng chung cho server và client; mapper workers = 4.
- Tách server/client dựa trên regex stack trong pprof — xấp xỉ, có phần "còn lại" 12–45 % không gán được.
- SQLite file trong `/tmp` của VM (đĩa `sda`), không đo SQLite busy riêng.
- Chưa có reconnect storm cho toàn bộ client (chỉ pha reconnect của `TestScale` ở các ô PASS).
- Chưa chạy `shared_service`, `personal_ssh` end-to-end; chưa có `heap.prof`/`environment.json` theo mẫu `artifacts/<gitsha>/P00b/`.
- Không đổi production code. Hai file test mới chưa commit.
