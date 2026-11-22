# P14 / P12b — B2 (PostgreSQL dùng chung theo số cluster) và B3 (chuyển tenant): lần đo đầu

- **Trạng thái:** B2, B3 `Verified` cho đúng các phép đo dưới đây (đã chạy, có số, exit code, artifact). Chưa `Reviewed`. B1 `Planned` (chỉ thiết kế, `tickets/P05.md`).
- **Ngày đo:** 2026-11-22, 15:51–16:21 UTC.
- **Phạm vi:** phép đo, **không** phải capacity claim. Mỗi con số chỉ đúng cho workload, commit, cấu hình và phần cứng ở §1. Con số suy ra (không đo trực tiếp) được ghi nhãn **ngoại suy**.

## 1. Môi trường

| Mục | Giá trị |
|---|---|
| Commit harness | `0d5e3ee` (nhánh `agent/bench-db-pgbouncer`; schema v0.1 của `3a4cb6c`). Code non-test của `hscontrol/` **không đổi** |
| Chuyển mã nguồn | `git bundle create … HEAD` → `scp` → `git clone` trên VM; build `go build -tags benchdb -o bench-db ./cmd/bench-db` (Go 1.27.4 linux/amd64) |
| VM | `mc-bench` (192.168.1.242): Ubuntu 24.04.5 LTS, kernel 6.8.0-147, Xeon E5-2696 v3 @ 2.30 GHz, 8 vCPU, 24 GiB, đĩa ảo QEMU 80 GB |
| PostgreSQL | `postgres:17.6-bookworm` (digest `sha256:f3bd19c6…dd6e3`), PostgreSQL 17.6. Container ghim CPU 0–3 (`--cpuset-cpus 0-3`, như `mc-pg-01` 4 vCPU), `--memory 8g`. `shared_buffers=2GB`, `effective_cache_size=6GB`, `max_connections=400`, `synchronous_commit=on`, `wal_level=replica`, `max_wal_size=4GB`, `pg_stat_statements` |
| Đĩa | `pg_test_fsync` trong container: `fdatasync` ≈ 1.108 lần/s (0,90 ms/lần, ghi 8 kB) |
| PgBouncer | `edoburu/pgbouncer:v1.24.1-p1` (digest `sha256:3db3d722…320c9e`), PgBouncer 1.24.1, ghim CPU 4. Cấu hình: `artifacts/P14-db/pgbouncer.ini` (transaction, `default_pool_size` 20 trừ khi ghi khác, `max_prepared_statements=200`, `max_client_conn=2000`, `auth_type=scram-sha-256`) |
| Harness | Mọi process cluster ghim CPU 5–7 (`taskset -c 5-7`) |
| Mạng | PostgreSQL `127.0.0.1:55432`, PgBouncer `127.0.0.1:56432`, chỉ loopback. Mật khẩu sinh trên VM (`openssl rand -hex 24`), không in ra; harness đọc bằng `file:` |
| **Tải khác trên VM** | Một benchmark P00c của kỹ sư khác (`/home/mesh/p00c/bin/headscale … base-10000-…`) chạy **suốt** thời gian đo, không ghim CPU: 25 % CPU lúc 15:41, ~110 % lúc 15:57, 146–229 % cuối B2, 247–336 % trong B3 và lần đo lại fencing. Load average 2–37. Mỗi dòng `###` trong `b2.log`/`extra.log` ghi load và CPU của process đó |
| Dọn dẹp | Sau khi đo: `docker rm` container, xóa volume, network, image, thư mục `~/benchdb` (gồm file mật khẩu) và `/tmp/benchdb.bundle`. Kiểm tra: không còn container/volume `benchdb` |

**Hệ quả của môi trường lên số đo (đọc trước khi dùng số):**
- PostgreSQL, PgBouncer và harness chạy **cùng VM**, ghim CPU tách nhau nhưng chung host, cùng đĩa ảo, và tranh CPU với benchmark P00c không ghim.
- Ở các lần `sat`, harness dùng 2,6–2,9 trên 3 core được ghim: **thông lượng `sat` bị giới hạn bởi phía client**, nên chỉ là **cận dưới** năng lực PostgreSQL. PostgreSQL không lần nào dùng quá 2 trên 4 core.
- Lần đo lại N=8 `sat` (16:18, load 9–30) cho 1.568–1.810 thao tác/s, so với 3.536 lúc 16:07. Cùng cấu hình, thông lượng `sat` lệch ~2 lần theo tải nền. **Không dùng số `sat` tuyệt đối làm năng lực.** So sánh trong cùng một cặp chạy liền nhau thì dùng được.

## 2. Lệnh chính xác

Script nằm ở `evidence/artifacts/P14-db/` (`stack.sh`, `run-b2.sh`, `run-b3.sh`, `run-extra.sh`, `summarize.py`).

```sh
./stack.sh up                                  # PostgreSQL 17.6, DB bench và bench_b3
./stack.sh pgb transaction 20 200              # PgBouncer: mode, default_pool_size, max_prepared_statements
./run-b2.sh   > results/b2.log 2>&1            # ma trận B2 → results/b2.jsonl          (exit 0)
./run-b3.sh   > results/b3.log 2>&1            # B3 → results/b3.json                    (exit 0)
./run-extra.sh > results/extra.log 2>&1        # fencing on/off ×2, schema qua PgBouncer (exit 0)
python3 summarize.py results/b2.jsonl          # bảng §3
./stack.sh down
```

Mỗi ô B2 (trong `run-b2.sh`):

```sh
./bench-db b2 -label <L> -passfile <file> -admin-port 55432 -port <55432|56432> [-bouncer-admin-port 56432] \
  -cpus 5-7 -duration 30s -tenants 10 -devices 250 -users 10 -workers 8 -pool 8 -standby-pool 2 \
  -clusters <N> -rate <100|0> [-fence=false] -cgroups pg=<cpu.stat>,pgb=<cpu.stat> -out results/b2.jsonl
```

B3: `taskset -c 5-7 ./bench-db b3 -passfile <file> -port 55432 -db bench_b3 -others 9 -other-devices 5000 -sizes 1000,5000,10000 -users 20 -trials 7 -out results/b3.json`.

Lần chạy đầu của `run-b3.sh` thoát vì DB `bench_b3` chưa tồn tại (lệnh `CREATE DATABASE` trong `stack.sh up` chạy khi PostgreSQL chưa sẵn sàng và lỗi bị nuốt). Tạo DB bằng tay rồi chạy lại; số đo B3 là của lần thứ hai.

## 3. B2 — PostgreSQL dùng chung theo số cluster (L10)

### 3.1 Workload

- Mỗi cluster = 1 process active (pool 8, 8 goroutine) + 1 process standby (pool 2, đọc `tailnets` mỗi 1 s). Process là **OS process riêng**, mở DB bằng `db.NewHeadscaleDatabase` thật với `cfg.ClusterID = cNN` và tenant đầu tiên của nó; 9 tenant còn lại dùng chung pool qua scope riêng (`HSDatabase{DB: base.DB.WithContext(scope)}`).
- 10 tenant/cluster × 250 thiết bị (seed bằng đường đăng ký thật), 10 user/tenant. Dữ liệu **tăng dần**: chạy N=2 thêm tenant của `c01`; thao tác `register` (5 %) cũng làm bảng lớn lên (cột "rows nodes" ở §3.4).
- Mix /1000: `map_update` 450, `last_seen` 250, `node_by_key` 240, `register` 50, `list_nodes` 5, `create_user` 5 (`tickets/P14.md`). Mọi transaction ghi có `SELECT … FROM tailnets … FOR SHARE` (**mô phỏng** fencing P10) trừ khi `-fence=false`. Mix là **tổng hợp**.
- `paced` = 100 thao tác/s mỗi active (offered N×100). `sat` = không giới hạn, vòng kín.
- Độ trễ là phía client (bao gồm round-trip, chờ pool, chờ PgBouncer), histogram log ~2 %.

### 3.2 Kết quả chính (đo, mỗi ô một lần chạy 30 s)

`map_update` là thao tác ghi chính (transaction 3 câu: fence + UPDATE + COMMIT). Cột kết nối là số backend PostgreSQL (`pg_stat_activity`), không tính kết nối thống kê của coordinator.

| Ô | N | Thao tác/s | `map_update` p50 / p95 / p99 ms | `register` p99 | commit/s | câu/s | tuple update/s | WAL MB/s | CPU PG (core) | CPU PgB | CPU harness | Kết nối PG max | `cl_waiting` max / TB | lỗi |
|---|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| direct-paced | 1 | 100 | 2,17 / 4,99 / 7,71 | 8,7 | 228 | 424 | 70 | 0,07 | 0,06 | – | 0,12 | 5 | – | 0 |
| pgbtx20-paced | 1 | 100 | 2,65 / 6,33 / 10,59 | 15,1 | 190 | 427 | 67 | 0,07 | 0,06 | 0,05 | 0,12 | 4 | 0 / 0 | 0 |
| direct-paced | 2 | 200 | 2,35 / 4,26 / 7,27 | 9,8 | 475 | 850 | 168 | 0,15 | 0,11 | – | 0,26 | 20 | – | 0 |
| pgbtx20-paced | 2 | 199 | 2,70 / 4,09 / 9,40 | 11,9 | 378 | 850 | 141 | 0,16 | 0,14 | 0,08 | 0,27 | 9 | 0 / 0 | 0 |
| direct-paced | 4 | 399 | 3,10 / 5,30 / 8,35 | 9,4 | 775 | 1.696 | 281 | 0,28 | 0,28 | – | 0,59 | 44 | – | 0 |
| pgbtx20-paced | 4 | 399 | 3,43 / 7,13 / 12,66 | 14,8 | 737 | 1.697 | 270 | 0,28 | 0,22 | 0,12 | 0,53 | 16 | 0 / 0 | 0 |
| direct-paced | 8 | 798 | 3,36 / 5,40 / 8,69 | 11,2 | 1.586 | 3.409 | 560 | 0,61 | 0,44 | – | 1,06 | 64 | – | 0 |
| pgbtx20-paced | 8 | 799 | 3,71 / 6,72 / 13,17 | 14,0 | 1.473 | 3.408 | 537 | 0,60 | 0,53 | 0,24 | 1,05 | 20 | 0 / 0 | 0 |
| direct-paced | 16 | 1.418 ⚠ | 10,38 / 58,14 / 107,42 | 162,8 | 2.771 | 6.043 | 994 | 1,09 | 1,01 | – | 1,86 | 164 | – | 0 |
| pgbtx20-paced | 16 | 1.515 ⚠ | 10,38 / 47,70 / 76,72 | 147,5 | 2.815 | 6.440 | 1.044 | 1,12 | 1,06 | 0,49 | 1,99 | 20 | 116 / 25 | 0 |
| direct-sat | 1 | 1.856 | 3,43 / 9,22 / 15,74 | 19,6 | 3.417 | 7.880 | 1.306 | 1,15 | 1,00 | – | 1,59 | 13 | – | 0 |
| pgbtx20-sat | 1 | 1.886 | 3,56 / 7,27 / 16,37 | 19,6 | 3.393 | 8.016 | 1.289 | 1,15 | 1,05 | 0,54 | 1,89 | 9 | 0 / 0 | 0 |
| direct-sat | 2 | 2.856 | 4,52 / 10,18 / 16,70 | 25,8 | 5.266 | 12.130 | 1.998 | 1,75 | 1,47 | – | 2,62 | 27 | – | 0 |
| pgbtx20-sat | 2 | 2.336 | 5,51 / 11,69 / 18,81 | 26,9 | 4.195 | 9.929 | 1.605 | 1,49 | 1,39 | 0,61 | 2,47 | 16 | 0 / 0 | 0 |
| direct-sat | 4 | 3.247 | 8,03 / 18,44 / 32,10 | 45,0 | 6.045 | 13.804 | 2.276 | 2,14 | 1,82 | – | 2,83 | 52 | – | 0 |
| pgbtx20-sat | 4 | 2.536 | 10,18 / 19,96 / 31,47 | 49,6 | 4.570 | 10.790 | 1.732 | 1,60 | 1,45 | 0,73 | 2,64 | 20 | 10 / 2 | 0 |
| direct-sat | 8 | 3.536 | 14,54 / 35,44 / 53,71 | 72,3 | 6.538 | 15.036 | 2.488 | 4,52 † | 1,96 | – | 2,90 | 92 | – | 0 |
| pgbtx20-sat | 8 | 2.634 | 16,37 / 28,50 / 42,35 | 70,9 | 4.794 | 11.215 | 1.786 | 1,75 | 1,45 | 0,77 | 2,63 | 20 | 48 / 34 | 0 |
| direct-sat | 16 | 1.911 ⚠ | 52,66 / 123,40 / 172,78 | 267,1 | 3.590 | 8.126 | 1.337 | 1,41 | 1,62 | – | 1,94 | 164 | – | 0 |
| pgbtx20-sat | 16 | 1.428 ⚠ | 53,71 / 81,41 / 111,76 | 232,6 | 2.649 | 6.090 | 985 | 0,97 | 0,98 | 0,53 | 1,82 | 20 | 124 / 106 | 0 |
| pgbtx10-sat | 16 | 1.168 ⚠ | 61,70 / 81,41 / 95,39 | 267,1 | 2.160 | 4.973 | 805 | 0,92 | 0,77 | 0,49 | 1,68 | 10 | 132 / 126 | 0 |
| pgbtx40-sat | 16 | 1.600 ⚠ | 52,66 / 99,24 / 147,47 | 246,8 | 2.944 | 6.817 | 1.108 | 1,26 | 1,20 | 0,56 | 1,86 | 40 | 86 / 62 | 0 |
| pgbsession20-sat | 4 | 193 | 10,18 / 20,36 / 33,40 | 46,8 | 363 | 822 | 135 | 0,15 | 0,17 | 0,08 | 0,29 | 20 | 16 / 13 | **12** (`query_wait_timeout`) |

⚠ N=16: 32 process trên 3 core ghim, load VM 24–37 (benchmark P00c tăng tải cùng lúc). CPU PostgreSQL ~1 core, `lockwait` max ≤ 1, thời gian thực thi trung bình phía server của mọi câu chính < 0,6 ms (§3.3). Độ trễ tăng ở N=16 **không** quy được cho PostgreSQL; nhiều khả năng do harness thiếu CPU. Cần đo lại với harness trên VM khác.
† WAL cao bất thường ở ô này (có thể trùng checkpoint, có full-page write). Không đo riêng.

### 3.3 Câu chính phía server (`pg_stat_statements`, N=16 sat)

| Câu | direct: lần gọi / TB ms / max ms | PgBouncer pool 20: lần gọi / TB ms / max ms |
|---|---|---|
| `SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id=$1 FOR SHARE` (fencing mô phỏng) | 43.437 / 0,090 / 17,1 | 32.367 / 0,035 / 3,7 |
| `UPDATE "nodes" SET machine_key…` (`map_update`) | 26.193 / 0,140 / 12,8 | 19.269 / 0,071 / 3,6 |
| `UPDATE "nodes" SET last_seen…` | 14.410 / 0,140 / 29,5 | 10.923 / 0,071 / 9,9 |
| `INSERT INTO "nodes" …` (`register`) | 2.834 / 0,588 / 24,6 | 2.175 / 0,288 / 3,8 |
| `SELECT * FROM nodes WHERE node_key=$1 AND tailnet_id=$2` | 13.980 / 0,101 / 14,7 | 10.372 / 0,057 / 3,4 |
| `SELECT * FROM nodes WHERE tailnet_id=$1 ORDER BY id` (`list_nodes`) | 310 / 11,256 / 115,0 | 215 / 5,471 / 33,5 |

Mọi câu có `AND "nodes"."tailnet_id" = $n` do plugin tenantscope thêm (kiểm bằng mắt trong `top_statements` của `b2.jsonl`). Qua PgBouncer, thời gian phía server ngắn hơn và đều hơn (ít backend tranh nhau hơn); thời gian chờ chuyển sang hàng đợi của PgBouncer (`cl_waiting`).

### 3.4 Kích thước bảng và index (đo, sau từng ô)

| Sau ô | dòng `nodes` | DB MB | heap `nodes` MB | `idx_nodes_tailnet_node_key` MB | `idx_nodes_tailnet_machine_key` MB | `nodes_pkey` MB |
|---|---:|---:|---:|---:|---:|---:|
| N1 direct-paced | 3.897 | 15,0 | 4,5 | 0,75 | 0,77 | 0,11 |
| N4 direct-paced | 25.985 | 51,1 | 30,3 | 4,99 | 4,99 | 0,70 |
| N8 direct-paced | 46.527 | 83,6 | 53,0 | 8,84 | 8,99 | 1,35 |
| N16 direct-sat | 84.321 | 147,2 | 98,7 | 16,11 | 16,15 | 2,43 |
| cuối (session) | 94.513 | 159,9 | 106,4 | 18,28 | 18,10 | 2,75 |

≈ 1,1–1,2 KB heap/thiết bị (hostinfo + endpoints thực tế), ≈ 190–200 B cho mỗi index `(tailnet_id, key)`. Các index khác < 1 MB. Bảng `users` 1,0 MB với 6.200 dòng.

**Ngoại suy (tuyến tính, không tính bloat):** 1 triệu thiết bị ≈ 1,2 GB heap `nodes` + ~0,4 GB cho hai index key. Vừa `shared_buffers`/RAM của VM 8 GB ở quy mô đó; chưa đo.

### 3.5 Có và không có PgBouncer (đo)

- **Kết nối:** N=16 direct dùng **164** backend (16 × 8 + 16 × 2 + 4); qua PgBouncer dùng **đúng `default_pool_size`** (10 / 20 / 40). Ở `paced` N≤8, PgBouncer chỉ mở 4–20 backend.
- **Độ trễ khi chưa bão hòa (`paced`, N≤8):** PgBouncer thêm khoảng **+0,3–0,6 ms p50** và **+2–5 ms p99** cho `map_update` (thêm một hop, PgBouncer một luồng trên 1 CPU). N=16 `paced`: p99 qua PgBouncer thấp hơn direct (77 so với 107 ms), nhưng ô này bị nhiễu (⚠).
- **Thông lượng `sat`:** direct cao hơn ở N=2–8 (ví dụ N=8: 3.536 so với 2.634), vì 8N goroutine vòng kín phải xếp hàng trên 20 kết nối server (`cl_waiting` TB 34). Đây là hành vi mong muốn của pooler (giới hạn đồng thời ở DB), không phải lỗi; nhưng pool quá nhỏ làm giảm thông lượng: ở N=16, pool 10 / 20 / 40 cho 1.168 / 1.428 / 1.600 thao tác/s.
- **CPU PgBouncer:** 0,49–0,77 core ở 2.600–4.800 transaction/s. **Ngoại suy:** một process PgBouncer (đơn luồng) chạm 1 core ở khoảng 6.000 transaction/s với mix này.
- **Standby qua pool chung:** p99 của standby poll là 3–33 ms khi direct, nhưng **141–651 ms** qua PgBouncer khi pool bão hòa (N≥4 `sat`), vì lease poll xếp hàng sau traffic dữ liệu. Với lease TTL vài giây, việc gia hạn lease qua pool bão hòa có thể trễ tới mức gây failover giả. → khuyến nghị pool riêng cho lease (§6).
- **Session mode** (N=4, pool 20, 40 client): 20 client giữ kết nối server suốt phiên; tới 16 client khác phải chờ (`cl_waiting` max 16, TB 12,6), có client chờ tới `query_wait_timeout` (120 s, `maxwait` 119,9 s); 12 thao tác lỗi `FATAL: query_wait_timeout`; thông lượng 193 thao tác/s. **Session mode không dùng được** với pool Go giữ kết nối rảnh.
- **`max_prepared_statements = 0` (transaction mode):** PostgreSQL log `ERROR: prepared statement "stmtcache_…" already exists` (3 lần); process không mở được DB và báo lỗi gây hiểu nhầm: `version check: database was not created by mesh-controlplane v0.1 … tables exist but no migration record`. Fail closed, nhưng cần self-check (ADR 0006 §3.5).
- **Tạo schema mới qua PgBouncer transaction mode:** `NewHeadscaleDatabase` trên DB rỗng `bench_mig` qua PgBouncer: thành công (`migrations` = `SCHEMA_INIT`, trigger `trg_credentials_user_tailnet` có mặt), sau đó tải 5 s không lỗi (`b2-schema-pgb.jsonl`). ADR 0006 vẫn khuyến nghị migration đi đường trực tiếp để dễ chẩn đoán.

### 3.6 Chi phí fencing mô phỏng (đo, 4 lần chạy liền nhau, N=8 direct `sat`, load 9–30)

| Lần | fence | thao tác/s | `map_update` p50 / p99 ms | commit/s | CPU PG |
|---|---|---:|---|---:|---:|
| r1 | ON | 1.682 | 29,07 / 103,25 | 3.139 | 1,25 |
| r1 | OFF | 1.810 | 22,92 / 97,30 | 3.372 | 1,15 |
| r2 | ON | 1.568 | 29,07 / 109,57 | 2.932 | 1,31 |
| r2 | OFF | 1.751 | 22,92 / 93,52 | 3.266 | 1,14 |

Fencing (`FOR SHARE` trong mọi transaction ghi) làm giảm thông lượng **7–10 %**, tăng CPU PostgreSQL ~10–15 %, và là câu được gọi nhiều nhất. Không thấy lock wait. Lần `N8-direct-sat-nofence` trong ma trận chính (1.818 thao tác/s) chạy lúc tải nền cao nhất, nên không so được với `N8-direct-sat` (3.536); dùng bảng này thay thế.

## 4. B3 — Chuyển tenant (L11)

### 4.1 Thiết lập

DB `bench_b3`: 9 tenant × 5.000 thiết bị + tenant được chuyển 1.000 / 5.000 / 10.000 (tổng 61.000 node, 20 user/tenant), seed qua handle có scope (28,7 s). Bản sao `blogsim` có `cluster_id NOT NULL` + index `(cluster_id, tailnet_id)` trên `users`, `credentials`, `nodes`, `policies`. `VACUUM ANALYZE` trước khi đo. Mỗi lần đo: `CHECKPOINT`, rồi một transaction move trên kết nối riêng; `pg_locks` của backend đó được đọc trước `COMMIT`. Thứ tự hai thiết kế đổi luân phiên mỗi lần.

### 4.2 Pha cô lập (đo, 7 lần mỗi ô; median, [min–max])

| Tenant | Thiết kế | Tổng ms | Câu lệnh ms | COMMIT ms | Dòng đổi | WAL |
|---|---|---|---|---|---:|---|
| 1.000 | CAS `tailnets` | 3,02 [2,63–5,80] | 1,19 | 1,73 | 1 | 4–5 KB |
| 1.000 | ghi lại `cluster_id` | 70,1 [60,9–74,0] | 63,9 | 6,4 | 1.021 | 3,22 MB |
| **5.000** | **CAS `tailnets`** | **2,34 [2,15–5,01]** | 0,96 | 1,38 | **1** | **6 KB** |
| **5.000** | **ghi lại `cluster_id`** | **292,0 [244,9–312,3]** | 272,6 | 16,7 | **5.021** | **15,8 MB** |
| 10.000 | CAS `tailnets` | 4,84 [2,45–5,20] | 1,10 | 3,82 | 1 | 8 KB |
| 10.000 | ghi lại `cluster_id` | 618,5 [538,1–683,7] | 595,0 | 21,6 | 10.021 | 31,5 MB |

- WAL đo ngay sau `CHECKPOINT` nên gồm full-page image: đây là **trường hợp xấu**. CAS chỉ ghi một trang (`tailnets`); ghi lại mọi dòng chạm ~1 trang mỗi vài dòng của `nodes` và mọi index của nó (không HOT được vì `cluster_id` có index).
- **Lock giữ trước COMMIT:**
  - CAS: `RowExclusiveLock` trên `tailnets` (và index của nó ở lần mở đầu), `transactionid`/`virtualxid` ExclusiveLock, **1 row lock**;
  - ghi lại mọi dòng: `RowExclusiveLock` trên 4 bảng `blogsim` và **mọi index** của chúng, **5.021 row lock** (row lock nằm trong tuple, không hiện trong `pg_locks`).
  - Không thiết kế nào lấy lock chặn tenant khác ở mức bảng.
- Dead tuple sau 21 lần move (7 × 3 kích thước): `blogsim.nodes` **112.000**, 0 HOT; `public.tailnets` 21.

### 4.3 Pha có writer đồng thời (đo, 5 lần mỗi ô)

Hai writer chạy liên tục (mỗi 2 ms một transaction ghi một thiết bị ngẫu nhiên): một trên **tenant đang chuyển**, một trên tenant khác. Với CAS, writer làm như cluster thật: `SELECT … tailnets FOR SHARE` + `UPDATE nodes` trong một transaction.

| Tenant | Thiết kế | Move ms | Writer cùng tenant: nền p50 / max trong lúc move | Writer tenant khác: max trong lúc move |
|---|---|---|---|---|
| 1.000 | CAS | 4,5–10,4 | 2,7 / 4,7–7,6 ms | 4,0–8,7 ms |
| 1.000 | ghi lại | 73–92 | 1,6 / 41,5–65,7 ms | 4,1–8,1 ms |
| 5.000 | CAS | 3,9–6,5 | 2,6 / 3,1–12,0 ms | 3,9–16,1 ms |
| 5.000 | ghi lại | 334–448 | 1,5 / **264–401 ms** | 5,4–46,0 ms |
| 10.000 | CAS | 3,5–8,8 | 2,4 / 5,7–10,9 ms | 6,2–9,2 ms |
| 10.000 | ghi lại | 656–801 | 1,5 / **633–791 ms** | 6,1–8,4 ms |

Với ghi lại mọi dòng, writer của tenant đang chuyển bị chặn gần bằng thời gian transaction move. Với CAS, writer chỉ chờ một UPDATE một dòng. Nền p50 của writer CAS cao hơn (~2,5 so với ~1,5 ms) vì có thêm câu fencing.

### 4.4 Phát hiện phụ

`hscontrol/db/schema_postgres.sql`: CHECK của `tailnets.status` chỉ có `PENDING, ACTIVE, SUSPENDED, DELETING`, **không có `MOVING`**, trong khi P12b dùng `ACTIVE → MOVING → ACTIVE`. Cần migration mới (cuối chuỗi v0.1) trong P10/P12b. B3 vì vậy chỉ đo một bước CAS đổi owner + epoch.

## 5. Đo và suy luận

| Khẳng định | Loại |
|---|---|
| 1–16 cluster (mỗi cluster 1 active + 1 standby, 10 tenant × 250 thiết bị) chạy trên một PostgreSQL 4 vCPU; ở 100 thao tác/s mỗi cluster, N≤8 đạt đủ tải với `map_update` p99 ≤ 8,7 ms (direct) / ≤ 13,2 ms (PgBouncer) | **đo** |
| CPU PostgreSQL ≈ **0,54 ms mỗi thao tác** của mix này (1.784–1.856 thao tác/s mỗi core ở N=1, 4, 8 `sat`) | **đo** (tỉ số của số đo) |
| PostgreSQL không bão hòa ở bất kỳ ô nào (≤ 1,96 / 4 core, lock wait ≈ 0) | **đo** |
| Thông lượng `sat` là năng lực PostgreSQL | **sai**: bị giới hạn bởi harness và tải nền (§1) |
| ~5.000 thao tác/s của mix này trên 4 vCPU với 30 % dư (≈ 0,7 × 4 core / 0,54 ms) | **ngoại suy** (giả định CPU tuyến tính, không có nút thắt fsync/lock/IO; chưa đo) |
| PgBouncer một process chạm 1 core ở ~6.000 transaction/s | **ngoại suy** |
| 1 triệu thiết bị ≈ 1,6 GB heap + index `nodes` | **ngoại suy** |
| Chuyển tenant 5.000 thiết bị: CAS 2,3 ms / 1 dòng / 6 KB WAL; ghi lại mọi dòng 292 ms / 5.021 dòng / 15,8 MB WAL | **đo** |

## 6. Kết luận (chỉ trong phạm vi số đo)

**Bao nhiêu cluster trên một primary?**
- Đã đo: **16 cluster** + 16 standby trên một PostgreSQL 4 vCPU, ~85–95 nghìn dòng `nodes`. Với 100 thao tác/s mỗi cluster (1.600 thao tác/s tổng), PostgreSQL dùng ~1 core. Độ trễ tăng ở N=16 xuất phát từ phía harness, không phải PostgreSQL (§3.2 ⚠, §3.3).
- Ngoại suy theo CPU: ~5.000 thao tác/s của mix này. Tức khoảng **40–50 cluster ở 100 thao tác/s mỗi cluster**, hoặc ~250 cluster ở 20 thao tác/s. **Chưa biết** một cluster 2.500 thiết bị thật sinh bao nhiêu thao tác/s ở trạng thái ổn định và khi reconnect storm. Phải đo bằng P00c/E2E trước khi đổi thao tác/s thành số thiết bị. Ví dụ để định hướng, không phải số đo: một reconnect storm 2.500 thiết bị trong 60 s, mỗi thiết bị ~3 thao tác, là ~125 thao tác/s cho cluster đó.
- Nút thắt kế tiếp có thể là: PgBouncer đơn luồng (~6k transaction/s, ngoại suy); độ trễ commit của đĩa (fdatasync 0,9 ms); replica đồng bộ (chưa bật).

**Pool đề xuất (cho P11b/P13/P14 xác nhận):**

| Thành phần | Giá trị | Cơ sở |
|---|---|---|
| Active: `max_open_conns` | 8 | Đã đo: `paced` N≤8 ổn định |
| Active: `max_idle_conns` | 2–4 | Giảm kết nối client rảnh; chưa đo riêng |
| Standby: `max_open_conns` / `max_idle_conns` | 2 / 1 | Đã đo: poll 1 s dùng ≤ 1 kết nối |
| PgBouncer `pool_mode` | transaction | §3.5: session hỏng, statement không dùng được |
| PgBouncer `max_prepared_statements` | 200 (> 0 bắt buộc) | §3.5 |
| PgBouncer `default_pool_size` (PG 4 vCPU) | **32–40** | N=16 `sat`: 40 > 20 > 10. Direct 164 backend nhanh hơn chút nhưng tốn 4 lần kết nối. Đo lại trên `mc-pg-01` riêng |
| PgBouncer `max_client_conn` | ≥ 1,5 × Σ pool client (≈ 15 × số cluster) | |
| **Pool riêng cho lease/fencing control** | alias DB riêng (ví dụ `mesh_lease`), `pool_size` 2–4 + `reserve_pool_size` | §3.5: lease poll p99 tới 651 ms qua pool dữ liệu bão hòa |
| `query_wait_timeout` | < deadline self-fence của lease (P11b) | Session test cho thấy client chờ tới timeout |

**Chuyển tenant:** chi phí DB của thiết kế owner/epoch là hằng số (~2–5 ms, 1 dòng), không phụ thuộc kích thước tenant. Ghi lại `cluster_id` tăng tuyến tính (~58 ms / 1.000 thiết bị), chặn writer của tenant trong suốt transaction, và tạo dead tuple bằng số dòng. Số đo ủng hộ quyết định ADR 0005 Bổ sung §4 (không đặt cluster ID trên từng dòng).

## 7. Giới hạn và việc còn lại

- Một VM cho cả PostgreSQL, PgBouncer và harness, cộng benchmark P00c chạy song song: số `sat` không ổn định (lệch ~2 lần); số N=16 bị nhiễu.
- Mỗi ô một lần chạy 30 s (B2); không có khoảng tin cậy. B3: 7 lần (cô lập) và 5 lần (đồng thời) mỗi ô.
- Mix B2 là tổng hợp; fencing, lease poll và câu CAS move là **mô phỏng** (P10/P11b/P12b chưa có code).
- Không có replica streaming, không TLS tới PostgreSQL, user bench là superuser (để dùng `pg_stat_statements_reset`).
- Process nhiều tenant của harness chia pool bằng scope riêng, như P05 dự kiến. Code hiện tại mở một pool cho mỗi `NewHeadscaleDatabase`; nếu P05 giữ cách đó thì số kết nối nhân với số tenant.
- Dữ liệu B2 có ~500 node thừa ở 2 tenant đầu của `c00` (từ lần smoke test trước ma trận).
- Đo lại khi lab giai đoạn 2 có `mc-pg-01` riêng; thêm reconnect/failover storm (L6, L8).

## 8. Artifact

`evidence/artifacts/P14-db/`:
- `b2.jsonl`, `b2.log`: 24 ô B2;
- `b2-ablation.jsonl`, `extra.log`: fencing on/off, schema qua PgBouncer;
- `b2-schema-pgb.jsonl`;
- `b3.json`, `b3.log`;
- `pgbouncer.ini` (cấu hình cuối, không có mật khẩu);
- script `stack.sh`, `run-*.sh`, `summarize.py`.

Đã grep mật khẩu trong mọi file kết quả trước khi copy: không có. JSON không chứa đường dẫn file mật khẩu (`-passfile` bị loại khỏi `params`). UUID tailnet và hostname tổng hợp có trong log; đây là dữ liệu test.
