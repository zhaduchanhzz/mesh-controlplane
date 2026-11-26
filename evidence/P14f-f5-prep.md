# P14f F5: chuẩn bị đo (driver, VM sinh tải, dung lượng host)

- **Trạng thái:**
  - Driver `Implemented`; chạy thử (dry-run) ở cỡ nhỏ `Verified`.
  - **Chưa đo thật**: chờ F4. Chưa `Reviewed`.
- **Nhánh:** `feat/P14f-f5`, tách từ `feat/P14f-netmap-delta` `7f56cc0`, đã gộp `feat/P14a-capload` `f5b532b`. Xung đột duy nhất ở `PLAN.md`: giữ dòng P14 của P14a và thêm dòng P14f.
- **Ticket:** `tickets/P14f.md` §F5 và mục "Quyết định của chủ dự án (2026-11-27)".

## 1. Dung lượng host Proxmox

Đọc qua API, chỉ đọc, 2026-11-27 khoảng 01:30.

| Mục | Giá trị |
|---|---|
| Node | `hanh`: 2 × Xeon E5-2696 v3, **36 CPU logic**, **125,7 GiB RAM**, swap 8 GiB, KSM chia sẻ khoảng 13,7 GiB |
| RAM host đang dùng | 100,7 GiB khi mc-bench tắt; 99,1 GiB khi mc-bench và mc-load chạy không tải (nhờ KSM và cache) |
| VM ngoài lab (không đụng) | 100 OPNsense 8 GiB, 101 pandownloader 32 GiB, 102 VPN 8 GiB; thực dùng khoảng 33 GiB |
| VM lab (pool `meshcontrol-lab`) | xem danh sách dưới |

VM lab theo vCPU / RAM cấp:
- mc-ci 16c / 24G; mc-cp 4c / 8G; mc-bench 8c / 24G (tắt mặc định); mc-db 4c / 8G;
- mc-c2, mc-c1b, mc-c2b mỗi VM 4c / 6G; mc-pg-01, mc-pg-02 mỗi VM 4c / 4G;
- mc-relay 2c / 2G; 4 VM client mỗi VM 1c / 2G;
- **mc-load 12c / 8G**: mới, tắt mặc định.

### Ngân sách RAM khi đo

Giả định chia ca: tắt các VM chỉ dùng cho E2E (mc-c2, mc-c1b, mc-c2b, mc-relay và 4 client). Tính trường hợp xấu nhất là mỗi VM dùng hết RAM được cấp:

| VM | GiB |
|---|---:|
| VM ngoài lab (thực dùng) | ~33 |
| mc-ci | 24 |
| mc-bench | 24 |
| mc-load | 8 |
| mc-db | 8 |
| mc-cp | 2 |
| mc-pg-01/02 | 4,5 |
| **Tổng** | **≈ 103,5** |

Cộng phần host tự dùng, còn dư khoảng **17 GiB** trên 125,7 GiB.

### Nâng RAM mc-bench (chỉ khi RAM là yếu tố chặn)

- **Tới 32 GiB:** an toàn. Còn dư khoảng 13 GiB nếu tắt thêm mc-pg-01/02.
- **Tới 40 GiB:** phải tắt mc-ci trong lúc đo; khi đó mc-ci không làm máy sinh tải, chỉ dùng mc-load.
- **Không vượt 40 GiB:** OPNsense là router của mạng; host thiếu RAM có thể làm nó bị OOM.

**Plan đã chuẩn bị nhưng chưa apply.** Chạy từ thư mục `infra/lab` của main checkout:

```sh
scripts/tofu.sh plan -refresh=false -var mc_bench_running=true -var mc_load_running=true \
  -var 'mc_bench={vm_id=7703,ipv4="192.168.1.242",mac="BC:24:11:77:02:42",cores=8,memory_mb=32768,disk_gb=80}' \
  -out=bench-32g.tfplan
```

- Kết quả plan thử ngày 2026-11-27: **update in-place** `memory.dedicated 24576 → 32768`, 0 add / 1 change / 0 destroy.
- `check-plan.py` báo "In lab scope".
- File plan đã bị xóa vì sẽ lỗi thời trước khi dùng; khi cần thì tạo lại.
- Đổi RAM cần khởi động lại VM.

## 2. VM sinh tải `mc-load`

**Cấu hình:**
- IaC `infra/lab/load.tf`, theo mẫu `bench.tf`.
- VMID **7710**, IP 192.168.1.249 (địa chỉ cuối của dải .240–.249 dành cho lab), MAC `BC:24:11:77:02:49`.
- **12 vCPU / 8 GiB RAM / 40 GB đĩa**, cloud-init như mc-ci.
- `started = var.mc_load_running`, mặc định **false**.

**Tạo VM:**
- `tofu fmt -check` OK, `validate` OK.
- `plan -out`: **2 to add, 0 change, 0 destroy** (`proxmox_virtual_environment_file.mc_load_user_data`, `proxmox_virtual_environment_vm.mc_load`).
- `check-plan.py`: "In lab scope: agent may apply this exact plan file".
- `apply`: 2 added; tạo VM mất 1 phút 23 giây.

**Kiểm sau khi tạo:**
- SSH bằng khóa lab OK.
- Ubuntu 24.04.5, kernel 6.8.0-147, `ulimit -Hn` 1048576.
- `ip_local_port_range` mặc định 32768–60999.

**Ghi chú:**
- Không cài Go trên mc-load: driver chép binary build sẵn trên mc-ci qua HTTP trong LAN, có kiểm sha256.
- `load.tf` cũng được đặt (chưa commit) trong `infra/lab/` của main checkout, vì state tofu nằm ở đó. Cách làm giống `c2b.tf` trước đây; giữ cho tới khi nhánh này merge.
- Sau dry-run, mc-bench và mc-load **đã tắt lại**: plan 0 add / 2 change / 0 destroy, chỉ đổi `started`.

## 3. Driver `tools/capload/ramp.sh` (kèm `ramp-verdict.py`)

### Các lệnh

| Lệnh | Việc |
|---|---|
| `ramp.sh limit SHAPE…` | Tăng dần 5k, 10k, 15k, … (bước `STEP`) tới khi một bước **trượt**, rồi chia đôi giữa bước đạt cuối và bước trượt đầu, tới độ phân giải `RES` (1k). Không có trần. Ghi kết quả vào `limits.txt` |
| `ramp.sh step SHAPE N` | Chạy một bước |
| `ramp.sh matrix` | 1k / 2k / 5k × 4 shape × ON/OFF, làm điểm cho mô hình chi phí |
| `ramp.sh keys-up` / `keys-down` | Tạo / gỡ khóa SSH riêng cho lượt chạy, khi chạy driver trên mc-load thay vì laptop |

### Một bước gồm

1. Tạo DB mới `capf5_*` trên mc-db; xóa sau bước.
2. `cluster.sh` config, register và start một process active:
   - `CLASS=dedicated`, `MAX_DEVICES = 2N + 10000`;
   - với `allow_all`: `MAX_PEER_ENTRIES=0`.
3. `capseed -mode cluster -tailnets 1 -devices N -policy SHAPE -extra 500`, rồi restart active.
4. Chia `plan.tsv` cho các máy sinh tải theo trọng số, thành các khúc liền để thiết bị của một user nằm cùng một máy.
5. Chạy `capload` đồng thời trên mọi máy sinh tải:
   - mở `ip_local_port_range` thành 1024–65535 trong lúc chạy rồi trả lại;
   - `ulimit -n 1048576`;
   - lấy mẫu mỗi giây: RSS và CPU của capload, CPU host, số kết nối TCP.

### Điều kiện "đạt"

Đúng theo ticket; phải đủ tất cả:
- Enroll N/N và register 500/500.
- RSS đỉnh của server dưới 90 % RAM của VM.
- Server còn sống, không restart, và `dmesg` không có OOM kể từ lúc bắt đầu bước.
- `netmap_divergence_total` = 0, **và khi ON phải có ít nhất 1 lần đối chiếu đã chạy**. Counter này có label nên không có series khi giá trị bằng 0; nếu chưa có lần đối chiếu nào thì kiểm tra vô nghĩa, và bước bị tính là trượt.
- Không có vi phạm cô lập. Nếu capload thoát với mã 3, verdict là `ISOLATION`, driver dừng, và đây là lỗi BLOCK.

### Phân loại yếu tố chặn của bước trượt

Xét theo thứ tự:
1. **Server RAM:** OOM, process chết, hoặc RSS ≥ 90 %.
2. **Divergence.**
3. **Máy sinh tải:** CPU ≥ 90 %, hoặc lỗi port/fd. Khi đó giới hạn đo được chỉ là **cận dưới**.
4. **Server CPU:** ≥ 90 % số core.
5. **Timeout / độ trễ.**

### Bí mật và thay đổi phụ

- `plan.tsv` chứa pre-auth key: sinh trên server, cắt và chuyển từ host sang host qua pipe, xóa sau mỗi bước.
- Mật khẩu PostgreSQL lấy từ `mc-db.env` vào một file 0600 trên server; file này đã xóa sau dry-run.
- Đã `grep`: thư mục kết quả không chứa key.
- `capseed` có thêm shape **`shared_service`**: mỗi user thấy thiết bị của mình cộng "dịch vụ chung" là thiết bị của `u0000`. `TestShapesValidateAsTenantPolicies` đã gồm shape mới; `go test ./tools/capseed/` → ok.
- `cluster.sh`: vì F1 đổi mặc định sang ON, `INCREMENTAL=0` nay ghi `enabled: false` tường minh.

## 4. Dry-run

Cấu hình: binary `master` `483afb0`, server mc-bench, tải mc-load, DB mc-db.

**Lượt 1:** `CI_BINDIR=/home/mesh/binp14f5 LOAD_HOSTS="mesh@192.168.1.249:12" REGISTER=20 HOLD=10s DEADLINE=5m CONCURRENCY=32 INC=1 ramp.sh step isolated 200`
- Chạy hết chuỗi: binary (sha256 khớp), DB, seed, enroll **200/200** trong 14 s, register 20/20, RSS 0,1 GiB.
- Dọn sạch: DB đã xóa, thư mục chạy trống, dải port đã trả lại.
- Verdict ban đầu là PASS. Sau khi thêm điều kiện "phải có đối chiếu", bước này bị tính trượt đúng như mong đợi: 14 s là quá ngắn để có lần đối chiếu nào (rec=0).

**Lượt 2:** `… REGISTER=50 HOLD=10s DEADLINE=10m CONCURRENCY=128 INC=1 ramp.sh step isolated 2000` → **PASS**
- Enroll 2000/2000 trong 54 s, register 50/50.
- RSS server 0,54 GiB / 23,5 GiB; CPU server trung bình 1,37 core khi enroll.
- Divergence 0, với 1 lần đối chiếu.
- Byte gửi 7,3 MB.
- capload RSS đỉnh **212,6 MB** cho khoảng 2050 client; CPU mc-load 2,9 %.

### Chi phí máy sinh tải (ước lượng)

- **RAM:** khoảng **0,1 MB mỗi client** với shape không giải mã map, nên 8 GiB của mc-load đủ cho khoảng 60k client.
- **Cổng nguồn** là giới hạn cứng thực tế: khoảng **64k kết nối mỗi máy sinh tải** tới một IP:port của server (driver đã mở dải 1024–65535). Hai máy mc-load và mc-ci cho khoảng **128k** kết nối.
- **CPU:** gần như rảnh ở 2k client.
- **`allow_all` ở N lớn:** mỗi client vẫn phải đọc full map O(N) byte, dù không giải mã. Đây là chỗ máy sinh tải có thể chặn trước server; driver sẽ ghi lại nếu xảy ra.

## 5. Rủi ro cho kế hoạch đo không trần

- **Hơn ~120k thiết bị cho một shape:** cổng của máy sinh tải sẽ chặn trước server. Khi đó cần thêm IP nguồn hoặc thêm máy sinh tải, và con số đo được chỉ là **cận dưới**.
- **`allow_all`:** khi enroll, lưu lượng mạng tăng theo O(N²). Băng thông LAN hoặc CPU đọc của máy sinh tải có thể chặn trước; driver ghi CPU host và số kết nối mỗi giây để nhận ra.
- **mc-db** (4 vCPU / 8 GiB, PgBouncer) có thể thành yếu tố chặn ở N rất lớn, và driver chưa tự phân loại trường hợp này. Dấu hiệu: thời gian `register` tăng và log server. Nếu nghi, chạy thêm `pgsample.sh`.
- **Đĩa mc-ci từng đầy:** cache go-build lên 92 GiB lúc 01:3x ngày 2026-11-27. Đã tỉa các mục không dùng quá 6 giờ, còn 27 GiB, đĩa ở mức 43 %. Có thể lặp lại khi nhiều agent build song song.
