# P09 / S17 — frame gửi sau SIGCONT: nguyên nhân, cách sửa, cách đo trong harness N1

- **Bối cảnh:** E2E P12, lần v1.84.3, ca P08 (F02 trên c1). Process được resume sau khi bị SIGSTOP quá TTL ghi nhận thêm **1** MapResponse (bộ đếm 4 → 5).
- **Quyết định Q5 của chủ dự án (2026-11-25):** đo chính xác trong P09.
- **Nhánh:** `feat/P09-s17`.
- **Commit:** RED `503c768`, GREEN `d95cc29`.

## Nguyên nhân tìm được trong code

`writeMap` (`hscontrol/poll.go`) kiểm `servable()` **trước** khi marshal JSON và nén zstd frame. Nếu process bị dừng ở bất kỳ điểm nào giữa lần kiểm đó và lúc ghi socket, thì sau SIGCONT frame vẫn được ghi ra, dù lease đã hết hạn. Đây là giả thuyết 2 (kẽ hở check-then-act). Khoảng hở không nhỏ: nó gồm toàn bộ thời gian marshal và nén, lớn dần theo kích thước netmap.

Bộ đếm cũ không phân biệt được giả thuyết 2 với giả thuyết 1: frame được gửi trước SIGSTOP, nhưng harness đã đọc bộ đếm từ sớm hơn.

## Đã sửa (GREEN)

- Kiểm `servable()` **lần thứ hai ngay trước khi ghi socket**, sau khi đã encode xong. Không qua thì frame bị bỏ và đếm vào `headscale_mapresponse_not_servable_total{stage="before_write"}`.
- **Khoảng hở còn lại:** chỉ còn quãng giữa lần kiểm thứ hai và syscall `write`, cỡ micro giây. Muốn bỏ hẳn thì cần fence ở tầng truyền tải (ví dụ: đóng mọi stream trước hạn lease một khoảng an toàn). Đó là thay đổi thiết kế lease/Noise, không làm ở đây (AGENTS 12); ghi thành đề xuất ở cuối file.
- **Không đổi:** lease, Noise, fence DB. `leaseGate` vẫn là `Manager.Current()`, so deadline bằng `time.Now()`.

**Test:** `TestP09NoFrameWrittenWhenLeaseEndsBeforeWrite`. Lease mất trong hook đặt giữa bước encode và bước ghi. Kết quả mong đợi: không ghi, không đếm vào `sent`, `before_write` +1, mốc thời gian frame cuối không đổi. Trước khi sửa: **FAIL** (frame vẫn được ghi). Sau khi sửa: **PASS**.

## Metric mới

Các metric này chỉ có trên listener metrics (loopback, `metrics_listen_addr`). Chúng là giá trị chung của cả process, không gắn nhãn node hay tailnet.

| Metric | Ý nghĩa |
|---|---|
| `headscale_mapresponse_last_gate_unix_nanos` | Thời điểm (Unix ns) lần kiểm `servable()` **cuối cùng** của frame mới nhất **đã ghi** đi qua. Sau khi sửa, đây là lần kiểm ngay trước khi ghi. |
| `headscale_mapresponse_last_write_unix_nanos` | Thời điểm (Unix ns) syscall ghi của **chính frame đó** trả về. |
| `headscale_mapresponse_not_servable_total{stage}` | Số frame bị gate chặn: `before_encode` (lần kiểm đầu), `before_write` (lần kiểm thứ hai). |

- Hai gauge thời gian luôn mô tả **cùng một frame**: được cập nhật cùng lúc, dưới một mutex.
- Giá trị là float64 của số nanosecond, chính xác tới khoảng 256 ns. Harness đọc giá trị dạng float rồi đổi sang số nguyên.

## Cách đo trong harness N1 (vòng F02)

Chạy **trong cùng một lệnh ssh trên host của process active**, để mọi mốc thời gian dùng chung một đồng hồ với process:

```
m() { curl -s -m 3 http://127.0.0.1:9095/metrics; }
S0=$(m | awk '/^headscale_mapresponse_sent_total\{status="ok"/ {s+=$2} END {printf "%d", s}')
TS=$(date +%s%N); systemctl kill --kill-whom=main -s STOP <unit>
#   ... chờ process kia ready (lệnh khác cũng được: không cần đồng hồ ở đây) ...
TC=$(date +%s%N); systemctl kill --kill-whom=main -s CONT <unit>
sleep 12
m > after.txt   # đọc sent (S1), last_gate (G1), last_write (W1), not_servable{before_write}
```

**Kết luận:**

| Điều kiện | Kết luận | Kết quả |
|---|---|---|
| `S1 == S0` | Không có frame nào sau khi resume | PASS |
| `S1 > S0` và `W1 < TS` | Frame được ghi **trước** SIGSTOP; harness đọc bộ đếm sớm (giả thuyết 1) | PASS, ghi chú "artefact đo" |
| `S1 > S0`, `W1 > TC` và `G1 < TS` | Gate qua trước khi pause, ghi sau khi resume (giả thuyết 2, khoảng hở µs còn lại) | **FAIL**, ghi lại cả ba mốc |
| `G1 > TC` | Gate qua **sau** khi resume, nghĩa là `leaseGate` sai | **FAIL, BLOCK MERGE** (AGENTS 13) |
| `not_servable_total{stage="before_write"}` tăng sau khi resume | Bản sửa đã chặn một frame mà code cũ sẽ gửi đi | Ghi chú (bằng chứng cho giả thuyết 2) |

Harness ghi `TS`, `TC`, `S0`, `S1`, `G1`, `W1` vào `timings.txt` cho mỗi lần F02, trên cả c1 và c2, rồi chạy F02 lặp lại nhiều lần (ví dụ 10 lần cho mỗi cluster).

## Giới hạn còn lại và hướng xử lý (chưa làm, cần duyệt)

- **Không bỏ hẳn được trong userspace.** Mọi cách "kiểm rồi ghi" đều còn một quãng giữa lần kiểm và syscall; một SIGSTOP rơi đúng quãng đó vẫn để frame ra sau deadline.
- Đóng stream sớm, khi còn `SafetyMargin` trước deadline, chỉ thu hẹp xác suất. Nó **không** chặn được trường hợp bị dừng đúng giữa lần kiểm và lúc ghi.
- **Bảo đảm thật hiện có:** fence ở DB theo owner/epoch và lease. Process cũ không **ghi** được gì; frame lọt ra chỉ mang trạng thái đã có khi process còn giữ lease.
- **Muốn client tự loại frame cũ:** phải gắn epoch vào luồng map để client kiểm. Đó là thay đổi giao thức và thiết kế (ADR 0005, AGENTS 12), cần chủ dự án duyệt.

## Benchmark hot path (2026-11-26)

Bản sửa S17 thêm vào mỗi frame của `writeMap`:
- một lần kiểm `servable()` thứ hai;
- hai lần `time.Now()`;
- `recordFrameWritten`, chạy dưới một mutex chung cả process.

Benchmark nằm trong `hscontrol/p09_frame_gate_bench_test.go`.

**Lệnh** (chạy trên mc-ci: Intel Xeon E5-2696 v3, 16 vCPU, Go 1.27.4; exit 0):
`go test -run XXX -bench BenchmarkS17 -benchmem -count 5 -cpu 1,4 ./hscontrol/`

| Đo | -cpu 1 | -cpu 4 (song song) |
|---|---|---|
| Phần S17 thêm vào mỗi frame (`BenchmarkS17FrameGate`) | 155 ns/op, 0 alloc | 197–206 ns/op, 0 alloc |
| Mã hóa frame 10 peer, zstd (`BenchmarkS17EncodeFrame`) | 77 µs/op | 26 µs/op |
| Mã hóa frame 100 peer, zstd | 549–557 µs/op | 156–162 µs/op |

Nhận xét:
- Phần thêm bằng 0,1–0,8 % chi phí mã hóa frame mà nó chặn.
- Mutex chung làm phần này chạy tuần tự, nên trần là khoảng 5 triệu frame/s cho cả process. Mức đó cao hơn nhiều so với số frame mã hóa được, nên không phải nút cổ chai.
- Không có số "trước": phần đo chỉ gồm đúng code mới thêm, vì đường cũ không có các bước này.
