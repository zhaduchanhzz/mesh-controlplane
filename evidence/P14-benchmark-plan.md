# P14 — Kế hoạch benchmark (P14a, P14c, P14d, P14f)

- **Trạng thái:** `Planned` (2026-11-26). Chưa chạy phép đo nào theo kế hoạch này. Không có số đo mới trong file.
- **Ticket:** `tickets/P14.md`. Inventory: `evidence/source-inventory-P14.md` §5–§9.
- **Dựa trên:** cách đo của P00b (`evidence/P00b-capacity-baseline.md`), P00c/P00d (`tools/capload`, `tools/capseed`), B2/B3 (`cmd/bench-db`, `evidence/P14-db-benchmarks.md`), quy tắc `06-tests-benchmarks.md` §4–§5.

## 1. Nguyên tắc

1. **Đường production:** `headscale cluster serve` (nhiều tailnet, Noise key cluster), PostgreSQL **qua PgBouncer transaction mode**, active + standby giữ lease, fence thật. Chế độ `headscale serve` + SQLite chỉ để nối với số P00c/P00d.
2. **Server-only:** tải từ host khác (`capload`, client `controlclient.Direct` thật); CPU/RSS chỉ của server.
3. **Một biến mỗi lần:** before/after cùng ô chạy **xen kẽ** (A B A B …), cùng snapshot DB, cùng cấu hình.
4. **Lặp:** 3 lần cho mọi ô dùng để kết luận (`max_pass`, `first_fail`, before/after); 1 lần cho ô thăm dò. P00b/P00c chỉ có 1 mẫu/ô.
5. **Nhãn:** mọi con số là **đo**, **tỉ số của số đo** hoặc **ngoại suy**; không có số capacity ngoài ma trận.
6. **Không secret:** mật khẩu PG/PgBouncer và pre-auth key sinh trên VM, đọc bằng file `0600`, xóa sau ô; không in ra log/artifact (AGENTS 2).

## 2. Topology lab

| Vai trò | VM | Cỡ | Ghi chú |
|---|---|---|---|
| Server under test (active) | `mc-bench` (7703) | 8 vCPU / 24 GiB | Tắt mặc định; bật bằng plan đã lưu `tofu plan -var mc_bench_running=true -out=…` + `check-plan.py`, chỉ khi plan chỉ create/update trong pool `meshcontrol-lab` (AGENTS 3, như P12b B3). Tắt lại sau phiên |
| Standby | `mc-c1b` (7706) hoặc `mc-c2b` (7707), 4 vCPU / 6 GiB | | Cho storm failover cần standby cùng cỡ với active → chạy thêm ô "4/8 active + 4/6 standby" hoặc dùng `mc-bench` làm standby và VM khác làm active (Q2). Chỉ dùng khi E2E không chạy |
| PostgreSQL + PgBouncer | Q1: `mc-pgbench` mới (4 vCPU / 8 GiB) **hoặc** DB `bench_p14` trên `mc-db` (7704, PG 16, PgBouncer 1.24.1) | | Cùng phiên bản và tham số với `infra/lab/db/setup-db.sh` (transaction mode, `max_prepared_statements` 200, `query_wait_timeout` 5, `idle_transaction_timeout` 5); `pg_stat_statements` bật. Khác B2 2026-11-22 (PG 17.6 container trên chính `mc-bench`) |
| Tải | `mc-ci` (7701, 16 vCPU / 40 GiB) | | `capload`; > ~25k stream: thêm VM tải thứ hai hoặc nhiều IP nguồn (giới hạn cổng nguồn, **chưa kiểm**) |
| Đồng hồ | NTP trên mọi VM | | Storm dùng dấu thời gian của từng host; độ lệch đồng hồ ghi trong manifest (`chronyc tracking`) |

- Mạng: LAN lab `192.168.1.0/24` như P00c (server lắng nghe chỉ trên LAN lab, metrics `127.0.0.1`). Không mở gì ra Internet.
- **Khóa độc quyền:** file khóa `/var/lock/mesh-bench` trên `mc-ci` và VM server; script từ chối chạy nếu E2E (`infra/lab/e2e/*`) hoặc CI đang giữ VM. Ghi `uptime`, top 5 process CPU của mọi VM tham gia trước/sau mỗi ô.

## 3. Policy shape

| Tên | Policy | Ghi chú |
|---|---|---|
| `allow_all` | `{"grants":[{"src":["*"],"dst":["*"],"ip":["tcp:443"]}]}` | Như P00b/P00c; n(n−1) peer entry; cần `tuning.max_peer_entries: 0` để vượt ~1.000 thiết bị |
| `isolated` | `{"acls":[]}` | Đối chứng (chỉ cùng user thấy nhau) |
| `personal_devices` | `{"grants":[{"src":["autogroup:member"],"dst":["autogroup:self"],"ip":["tcp:443"]}]}` | Policy thực tế của tenant doanh nghiệp; đường per-node |
| `tag_heavy` (mới) | 100 tag vai trò (`tag:r000`…`tag:r099`, mỗi node đúng một tag) + 20 tag môi trường (mỗi node một); 300 grant `tag:rI → tag:rJ` (mỗi vai trò tới 3 vai trò khác, `tcp:22,443`), 50 group (mỗi group 2 user) → 5 tag; `tagOwners` cho group admin | 70 % node có tag, 30 % node của user (10 node/user); bộ sinh có seed, policy JSON lưu trong artifact. Gần `BenchmarkCompileWideACL` (`hscontrol/policy/v2/policy_test.go:4605`) |
| trộn (nhiều tailnet) | mỗi tailnet một shape: 70 % `personal_devices`, 20 % `tag_heavy`, 10 % `allow_all` | Thứ tự gán theo seed |

Policy tenant rỗng = DENY (cluster ép `PolicyEmptyDenies`); mọi tailnet benchmark phải có policy tường minh như trên.

## 4. Ma trận và thứ tự chạy

**Giai đoạn 0 — nối với số cũ (1 lần/ô):** `headscale serve` + SQLite, `run-seeded.sh` 10k `personal_devices`, 10k `isolated`, 5k `allow_all` trên binary tree hiện tại. Mục đích: xác nhận harness và thấy thay đổi từ P00d → hiện tại (P05–P12c) trước khi đổi sang PG.

**Giai đoạn 1 — cluster mode, một tailnet lớn (3 lần/ô kết luận):**

| Policy | N |
|---|---|
| `personal_devices`, `tag_heavy` | 1k, 2,5k, 5k, 7,5k, 10k (15k nếu 10k đạt) |
| `allow_all` (`max_peer_entries` 0) | 1k, 2,5k, 5k (7,5k nếu 5k đạt) |
| `isolated` | 5k, 10k (đối chứng) |

Tìm biên: khi ô N đạt và ô kế tiếp không đạt, chạy thêm điểm giữa (chia đôi tới bước 500).

**Giai đoạn 2 — cluster mode, nhiều tailnet:**

| T tailnet | thiết bị/tailnet | tổng |
|---|---|---|
| 10 | 200 | 2k |
| 100 | 10 / 50 / 200 | 1k / 5k / 20k |
| 500 | 10 / 50 | 5k / 25k |
| 1.000 | 10 | 10k |
| 2.000 | 10 | 20k |
| hỗn hợp | 1 × 2,5k (hoặc 5k) `personal_devices` + 100 × 20 trộn | 4,5k / 7k |

So với B1/L9 (`tickets/P05.md`): giai đoạn 2 là số đo trên runtime P05 thật; biến thể (b)/(b′) của B1 không chạy ở đây.

**Giai đoạn 3 — before/after cho từng PR P14b** (chỉ các ô liên quan, xen kẽ, 3 lần).

**Giai đoạn 4 — storm (P14c) và DB (P14d)** trên các ô `max_pass` của giai đoạn 1–2.

**Giai đoạn 5 — lần cuối** trên tree có mọi tối ưu đã merge, ma trận giai đoạn 1–2 rút gọn quanh biên → số cho P14e.

### Pha của một ô (cluster mode)

1. Khôi phục DB từ template seed (`CREATE DATABASE bench_cell TEMPLATE seed_<cấu hình>`), `VACUUM ANALYZE`, reset `pg_stat_statements`, `SHOW STATS` PgBouncer làm mốc.
2. Khởi động standby rồi active; chờ `/ready` 200 trên active, 503 trên standby.
3. `connect`: N node có sẵn kết nối lại (256 song song, như P00c).
4. `steady` 5 phút: churn endpoint (mặc định 1 % node đổi endpoint mỗi phút; tốc độ ghi trong manifest), keepalive tự nhiên.
5. `probe`: 20 thay đổi cách nhau 10 s (10 đổi endpoint của một node, 10 đổi hostinfo); mỗi lần đo thời gian tới khi K = min(200, N) stream quan sát nhận frame chứa thay đổi.
6. `register` 500 node mới (theo user, như P00c).
7. `policy` 3 lần đổi policy (thêm/bớt một grant) cách 30 s; đo tới frame ở K stream.
8. Dừng: thu metric, log (đã redact), `pg_stat_*`, PgBouncer, CPU cgroup; dừng active rồi standby.

## 5. Chỉ số thu

| Nhóm | Chỉ số | Nguồn |
|---|---|---|
| Server | CPU core TB/đỉnh theo pha, RSS đỉnh/ổn định, Go heap live, GC pause p99, goroutine | sampler 1 s của `server.sh` (`/proc`, `runtime/metrics` qua metric) |
| Map | connect wall, register p50/p95/p99, first map p50/p95/p99, thay đổi → frame p50/p95/p99, byte gửi/thay đổi | `capload` (thêm dấu thời gian frame) |
| NodeStore/policy | `nodestore_peers_calculation_duration_seconds`, `nodestore_snapshot_build_duration_seconds`, `nodestore_snapshot_builds_total{peers}`, `nodestore_queue_depth`, `mapresponse_generated_total`, `mapper_changes_dropped_total` | `/metrics` loopback |
| Lease/fence | độ trễ gia hạn lease, thời gian transaction có fence | metric mới (P14a.0, cần duyệt) hoặc log |
| PostgreSQL | commit/s, câu/s theo loại (`pg_stat_statements`: calls, mean/max), WAL byte/s, backend đang dùng, lock wait, kích thước bảng/index | truy vấn mỗi 1 s (như `cmd/bench-db`) |
| PgBouncer | `cl_active`, `cl_waiting`, `sv_active`, `maxwait`, `total_xact_count` | `SHOW POOLS`, `SHOW STATS` mỗi 1 s |
| Profile (P14b) | CPU 30 s, alloc, mutex, block ở mỗi pha | build tag `benchpprof` (Q7) |

## 6. Tái lập: manifest mỗi ô

`artifacts/<gitsha>/P14/<cell>/manifest.json` (thư mục `artifacts/` bị ignore; bản tóm tắt + manifest được chép vào `evidence/` khi viết báo cáo):

- `git rev-parse HEAD`, `git status --porcelain` rỗng; `go version -m` của `headscale`, `capload`, `capseed` (`vcs.revision`, `vcs.modified=false` bắt buộc), sha256 mỗi binary; Go 1.27.4, `CGO_ENABLED=0`, build trên `mc-ci`, chuyển qua LAN có kiểm sha256 (như `CI_BINDIR` của N1).
- `config.yaml` (đã bỏ đường dẫn secret), policy JSON, `tuning.*` (gồm `incremental_netmap`, `max_peer_entries`, `cluster_lease`), số tailnet/thiết bị, seed của bộ sinh policy/dữ liệu, sha256 snapshot seed.
- PostgreSQL: `SELECT version()`, các tham số khác mặc định (`pg_settings WHERE source <> 'default'`); PgBouncer: phiên bản, `SHOW CONFIG` (lọc auth).
- VM: `nproc`, `free -b`, `lscpu` (model), kernel, `ulimit -n`, `ip_local_port_range`; `uptime` + top process trước/sau; độ lệch NTP.
- Lệnh chính xác, exit code, thời điểm bắt đầu/kết thúc (UTC).
- **Raw giữ ở đâu:** lần P00c/P00d raw chỉ nằm trên VM và đã mất (`mc-ci:~/src/p00c/results` không còn ngày 2026-11-26). Lần này raw (đã kiểm redact: `grep` key prefix `nodekey:`, `mkey:`, `privkey:`, mật khẩu) được chép về laptop `artifacts/` sau mỗi phiên và liệt kê sha256 trong evidence.

`summarize.py` mở rộng cho JSONL nhiều ô, sinh bảng markdown dùng trực tiếp trong evidence.

## 7. SLO ứng viên (dùng làm điều kiện "đạt" của ô; chủ dự án chốt ở P14e)

| Chỉ số | Ứng viên | Cơ sở |
|---|---|---|
| Connect N (ổn định → mọi stream có map) | 100 % trong ≤ max(60 s, N/100 s) | P00c: 10k trong 72–84 s |
| Register (pre-auth) p99 | ≤ 10 s; lỗi 0 | deadline `TestScale` 10 s |
| First map p99 | ≤ 10 s | như trên |
| Thay đổi → frame p99 (endpoint/hostinfo) | ≤ 5 s | tick batch 800 ms (`hscontrol/config/default.go:84`) |
| Thu hồi (xóa node / policy chặn) p100 | ≤ 5 s | M06: max 1,44 s |
| Failover: takeover lease | ≤ TTL + acquire (≈ 11 s mặc định) | P11b |
| Failover: t100 client có map lại, không đăng nhập lại | ≤ 60 s ở `max_pass` | đề xuất; P12b Q7 cho move là ≤ 15 s với 24 client |
| Reconnect storm t100 | ≤ 2 × connect wall ổn định | đề xuất |
| RSS ổn định | ≤ 70 % RAM VM | biên cho storm/GC |
| CPU ổn định | ≤ 50 % số core | biên cho storm |
| Gia hạn lease p99 dưới tải | ≤ 1 s (≪ biên self-fence 8 s) | P12c-b B1 |
| PG CPU ở tải đỉnh của ô | ≤ 60 % | ngưỡng "cell" (P14d) |
| Cô lập | 0 peer khác tailnet trong mọi frame decode | AGENTS 13 |

## 8. Điều kiện dừng

- Bất kỳ frame decode nào chứa peer/khóa của tailnet khác, hoặc tenant B nhận thay đổi của A: **dừng toàn bộ, BLOCK MERGE**, giữ artifact.
- RSS server > 90 % RAM VM: dừng ô (tránh OOM làm treo VM lab), ghi `first_fail`.
- PostgreSQL hết đĩa hoặc CPU 100 % > 60 s liên tục; PgBouncer `maxwait` > 5 s kéo dài: dừng ô, ghi.
- Lease mất/failover ngoài ý muốn trong pha ổn định: ô **không đạt**, không chạy lại để "làm đẹp" số; ghi nguyên nhân.
- Plan `tofu` bật/tắt `mc-bench` có `destroy`/`replace` hoặc chạm ngoài pool/VMID lab: dừng, hỏi người (AGENTS 3).
- E2E/CI/P13 cần VM đang dùng: dừng ở ranh giới ô, nhả khóa.
- Secret xuất hiện trong log/artifact: dừng, xóa, báo.
- Một phase vượt deadline 20 phút: ô không đạt.
