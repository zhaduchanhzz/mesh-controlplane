# PR P13a (infra) Evidence — LAB-P13 + P13a: HA PostgreSQL (Patroni + etcd, đồng bộ)

- **Ngày:** 2026-11-26 (UTC).
- **Duyệt:** chủ dự án duyệt gói P13 ngày 2026-11-26: ADR 0011 (D1, D2, D3) và D7 (VM 7708/7709, mỗi VM 4 vCPU / 4 GiB / 60 GB trên vmbr0, apply từ plan đã lưu, mc-bench giữ tắt). Chuyển `meshlab` **không** thuộc PR này.
- **Trạng thái:**
  - LAB-P13: `Verified`.
  - P13a, phần hạ tầng: `Verified` ở mức lab (cụm Patroni chạy, sync replica, một switchover có kiểm soát).
  - Chưa làm: F05 drill, runbook, chuyển `meshlab`. Vì vậy P13a **chưa** đạt nghiệm thu đầy đủ của ticket.
  - Chưa `Reviewed`.
- **Nhánh:** `feat/P13a-infra` (từ `feat/P13-plan`).
- **Artifact:** `artifacts/p13a/20261126T125823Z/` của checkout chính (gitignored): tofu plan/apply, check trước và sau, log ghi từng dòng của switchover, số đo commit.

## Repo commit before / after

- Trước: `c4533aa` (`feat/P13-plan`, ADR 0011–0014 Accepted).
- Sau: các commit của nhánh này (xem `git log c4533aa..feat/P13a-infra`).

## Verified source paths & symbols

Xem `evidence/source-inventory-P13a.md`.

## Design choice / ADR

ADR 0011 §A–C, E, đúng như đã duyệt, không đổi thiết kế:
- etcd 3 thành viên, mc-db là witness;
- softdog với `mode: required`;
- `synchronous_mode: true`, `strict: false`;
- `ttl 20 / loop_wait 5 / retry_timeout 5`;
- data checksums, `wal_log_hints`, `use_pg_rewind`.

Các lựa chọn triển khai mà ADR không quy định:

| Lựa chọn | Lý do |
|---|---|
| Template cloud-init mới `mc-pg.yaml.tftpl`, không cài Docker | `nftables.service` sở hữu toàn bộ ruleset trên VM PG; Docker không ghi đè iptables |
| etcd 3.6.17 lấy từ bản phát hành upstream (kiểm SHA-256 theo `SHA256SUMS`), unit `mc-etcd.service` | Ubuntu 24.04 chỉ có etcd 3.4 (EOL). Một phiên bản dùng cho cả 3 thành viên |
| Witness trên mc-db là unit systemd riêng (không dùng container); `MemoryMax=512M`; nft table riêng `inet mc_etcd`, chỉ lọc tcp/2379–2380 | Không đụng Docker, iptables-nft, `mc-pg`/`mc-pgb` |
| Gói PGDG được pin: `postgresql-16 16.16-1.pgdg24.04+1` (cùng minor với mc-db), `patroni 4.1.5-2.pgdg24.04+1`, `pgbackrest 2.60.1-1.pgdg24.04+1`, có `apt-mark hold`. Fingerprint key PGDG được pin (`B97B0AFC…ACCC4CF8`) | |
| Mật khẩu nằm trong `/etc/mc-pg/patroni.env`, nạp bằng systemd `EnvironmentFile` | YAML Patroni không chứa bí mật nên commit được |
| `archive_mode on` + `archive_command /bin/true` | Bật `archive_mode` cần restart, đổi `archive_command` chỉ cần reload: P13c chỉ việc đổi lệnh. **Hiện WAL không được lưu, chưa có backup** |
| Locale `C.UTF-8` | Thứ tự so sánh không phụ thuộc phiên bản glibc giữa hai node. Hệ quả cho việc chuyển `meshlab`: xem cuối file |

## LAB-P13: OpenTofu

Root `infra/lab/pg/`, chuyển từ `infra/lab/drafts/p13-pg/` bằng `git mv`.
- Chạy trong WSL `Ubuntu-24.04`: OpenTofu 1.13.9, `bpg/proxmox` 0.123.1, lock giữ nguyên.
- Thay đổi so với bản nháp:
  - template cloud-init: `mc-pg.yaml.tftpl` thay cho `mc-ci.yaml.tftpl`;
  - mô tả VM;
  - chú thích.

Kiểm trước khi apply (PVE API, cert đã pin):

| Kiểm | Kết quả |
|---|---|
| RAM host | 125,7 GiB tổng, 28,3 GiB available. Đã cấp cho VM đang chạy: 116,0 GiB → sau khi thêm 2 × 4 GiB là **124,0 / 125,7 GiB** (mc-bench 7703 tắt, 24 GiB không tính) |
| VMID | 7708 và 7709 trống (`/cluster/nextid vmid=…` trả về chính số đó) |
| IP | .247, .248, .249 không trả lời ping, ARP `INCOMPLETE` (kiểm từ mc-ci) |
| Snippet | `local:snippets` chưa có `mc-pg-0*` |
| E2E | Không có harness nào đang chạy trên mc-ci |

Lệnh (`bash infra/lab/scripts/tofu.sh -chdir=pg …`, chạy từ worktree, root mới với state rỗng):

| Lệnh | Exit | Kết quả |
|---|---|---|
| `init -input=false` | 0 | |
| `fmt -check -diff` | 0 | |
| `validate` | 0 | "Success! The configuration is valid." |
| `plan -input=false -out=pg.tfplan` | 0 | **Plan: 4 to add, 0 to change, 0 to destroy.** |
| `show -json pg.tfplan \| python3 scripts/check-plan.py` | 0 | "In lab scope: agent may apply this exact plan file." |
| `apply -input=false pg.tfplan` (chạy nền, không bọc `timeout`) | 0 | **Apply complete! Resources: 4 added, 0 changed, 0 destroyed.** 12:43:44Z → 12:44:56Z |

Plan gồm 4 tài nguyên, đều `create`, không có `delete` hay `replace`, không tài nguyên SDN:
- 2 file `local:snippets`: `mc-pg-01-user-data.yaml`, `mc-pg-02-user-data.yaml`.
- `proxmox_virtual_environment_vm.pg["mc-pg-01"]`: VMID 7708, pool `meshcontrol-lab`, 4 core, 4096 MiB, 60 GB `nvme01`, `192.168.1.247/24`, MAC `BC:24:11:77:02:47`, vmbr0, `on_boot=false`.
- `proxmox_virtual_environment_vm.pg["mc-pg-02"]`: VMID 7709, giống trên với `192.168.1.248/24`, MAC `BC:24:11:77:02:48`.

State: chép vào `mesh-controlplane/infra/lab/pg/terraform.tfstate` (checkout chính, gitignored) và `%USERPROFILE%\.mesh-lab\tofu-state\infra-lab-pg-20261126T1245Z.tfstate`.

Thông tin VM sau apply (PVE API `/nodes/hanh/qemu/<id>/config`):

| VM | VMID | Cấu hình | Mạng | Khác |
|---|---|---|---|---|
| `mc-pg-01` | 7708 | 4 core, 4096 MiB, `nvme01:vm-7708-disk-0` 60G (`iothread`, `discard`, `ssd`) | `virtio=BC:24:11:77:02:47,bridge=vmbr0`, `ip=192.168.1.247/24,gw=192.168.1.1` | `onboot 0`, tags `db;mesh-lab;pg-ha`, pool `meshcontrol-lab` |
| `mc-pg-02` | 7709 | giống trên, disk `vm-7709-disk-0` | `…:48`, `192.168.1.248/24` | giống trên |

- Kiểm boot ngay sau khi tạo, vì lần tạo 7705 từng hỏng đĩa:
  - cloud-init `status: done`, có `/var/lib/mesh-lab-bootstrap-done`;
  - `sda` 60G, `/` 58G;
  - `systemctl --failed` rỗng;
  - `dmesg` không có I/O error hay ext4 error.
- Kernel `6.8.0-147-generic`.
- Sau khi cụm chạy:
  - RAM host: available 27,5 GiB (điều kiện dừng là < 8 GiB);
  - cấp phát 124,0 / 125,7 GiB, 78 vCPU;
  - mỗi VM PG dùng ~530 MiB trong guest, ~1,9 GiB phía host.

## P13a: cài đặt

Lệnh (từ laptop, Git Bash):

| Lệnh | Exit | Ghi chú |
|---|---|---|
| `bash infra/lab/pg/deploy.sh etcd` | 0 | 23 s. etcd 3.6.17 trên mc-pg-01, mc-pg-02, mc-db; `endpoint health` 3/3 |
| `bash infra/lab/pg/deploy.sh pg` (lần 1) | 141 | Lỗi script: `tr </dev/urandom \| head` chết vì SIGPIPE dưới `pipefail`. Đã xóa file bí mật dở dang, sửa `gen()` (đọc 4096 byte có giới hạn) |
| `bash infra/lab/pg/deploy.sh pg` (lần 2) | 0 | 2 phút. mc-pg-01 initdb, lên primary; bí mật chép sang mc-pg-02 qua pipe; mc-pg-02 clone, lên replica |
| `bash infra/lab/pg/deploy.sh pg` (chạy lại) | 1 | `patroni --validate-config` báo cổng 8008/5432 đang dùng (do chính Patroni đang chạy). Đã sửa: chỉ bỏ qua đúng thông báo đó khi `mc-patroni` đang active |
| `bash infra/lab/pg/deploy.sh pg` (chạy lại, ×2) | 0, 0 | Idempotent: PID của `mc-patroni`, `mc-etcd` trên hai node không đổi. Lần cuối nftables cũng không nạp lại (sửa: chỉ nạp khi file đổi); sha256 của `mc-pg.env` không đổi |

### Sự cố: etcd của mc-pg-02 bị kẹt ở term cũ

**Triệu chứng.** `endpoint status` của mc-pg-02 báo `etcdserver: no leader`, raft term 3 / index 30, trong khi hai thành viên kia ở term 4 / index 41.

**Nguyên nhân.**
- Lần nạp ruleset đầu tiên có `ct state` nên kernel mới bắt đầu dùng conntrack.
- Các luồng TCP peer của etcd đã mở từ trước đó bị conntrack nhận vào giữa chừng.
- Gói của các luồng này bị xếp `invalid` và bị luật `ct state invalid drop` bỏ đi.
- Kết quả: etcd giữ một stream chết mà không báo lỗi.
- mc-pg-01 cũng mất quorum một lúc ("stepped down … quorum is not active"), rồi đồng bộ lại.

**Xử lý.**
- `systemctl restart mc-etcd` lần lượt trên mc-pg-02, rồi mc-pg-01 (luôn còn 2/3 quorum).
- Sau đó cả ba thành viên ở term 4, cùng index. Patroni leader không bị ảnh hưởng.
- Đã sửa `setup-pg.sh`: nếu conntrack chưa nạp trước lần nạp firewall đầu tiên thì restart etcd cục bộ.
- mc-db không gặp vì Docker đã nạp conntrack từ trước.

## Topology và phiên bản

| | mc-pg-01 (.247) | mc-pg-02 (.248) | mc-db (.243) |
|---|---|---|---|
| PostgreSQL | 16.16 (PGDG) dưới Patroni 4.1.5 | như mc-pg-01 | không (Docker `mc-pg` 16.16 cũ vẫn chạy, không đụng) |
| etcd | 3.6.17 (`mc-etcd`) | 3.6.17 | 3.6.17 (witness, RSS ~24 MB) |
| Khác | chrony 4.5, softdog, nftables `inet mc_pg`, pgbackrest 2.60.1 (chưa có stanza) | như mc-pg-01 | nft `inet mc_etcd` (chỉ 2379/2380) |

- Patroni scope `mc-pg`, system id `7712461298187464035`.
- Khởi đầu: leader mc-pg-01, sync standby mc-pg-02.
- **Sau switchover (trạng thái hiện tại): leader mc-pg-02, sync standby mc-pg-01, timeline 2.**

## Kiểm chứng

### `bash infra/lab/pg/check-pg.sh`: exit 0, trước và sau switchover

Toàn văn: `check-pg-before.txt`, `check-pg-after.txt`. "ALL CHECKS PASSED" cả hai lần.

| Kiểm | Kết quả (sau switchover) |
|---|---|
| etcd quorum | `endpoint health` 3/3; 3 member `started`, không có learner; mọi member cùng raft term, cùng một leader |
| Patroni | đúng 1 leader (mc-pg-02); sync standby mc-pg-01 `streaming`; DCS `synchronous_mode=true`, `strict=false`, `ttl=20`, `loop_wait=5`, `retry_timeout=5` |
| PostgreSQL | `data_checksums=on`, `wal_log_hints=on`, `synchronous_commit=on`; `synchronous_standby_names="mc-pg-01"`; `pg_stat_replication`: `mc-pg-01 streaming sync`; `max_connections=200`, `shared_buffers=1GB` |
| Archive | `archive_mode=on`, `archive_command=/bin/true` (chưa có pgBackRest, P13c) |
| Role | `headscale` LOGIN, `idle_in_transaction_session_timeout=5s` |
| Watchdog | Leader: `/dev/watchdog` do process `patroni` giữ, "Software Watchdog active", timeout 15 s. Replica: softdog đã nạp, `/dev/watchdog` thuộc `postgres`, chưa arm (Patroni chỉ arm khi là leader) |
| Thời gian | chrony `Leap status Normal` trên cả hai node; độ lệch so với NTP 0,1–1,2 ms. Chênh nhau giữa hai node: < 2 ms (cả hai lệch dưới 1,3 ms so với NTP) |
| Unit | `mc-etcd`, `mc-patroni`, `nftables`, `chrony`: active và enabled trên cả hai node |
| mc-db | `mc-etcd` và `mc-etcd-nft` active; container `mc-pg`, `mc-pgb` vẫn "Up 2 days" (không restart) |
| Firewall (âm tính) | Từ mc-ci: 2379 trên mc-pg-01 **đóng**, 2379 trên mc-db **đóng**, 8008 trên mc-pg-02 **đóng**. Từ mc-cp: 5432 **đóng** |
| Firewall (dương tính) | Từ mc-ci: 5432 mở. PgBouncer cũ `192.168.1.243:6432` vẫn tới được |

Log Patroni khi đổi leader:
- mc-pg-02: "Software Watchdog activated with 15 second timeout, timing slack 10 seconds", rồi "promoted self to leader by acquiring session lock".
- mc-pg-01: "Demoting self (graceful)", rồi "a secondary, and following a leader (mc-pg-02)".

### Một switchover có kiểm soát + database thử: `bash infra/lab/pg/switchover-test.sh <artifact>`, exit 0

- **Chuẩn bị.**
  - `CREATE DATABASE p13a_rt OWNER headscale`, bảng `rt(seq PK, server inet, ts)`.
  - mc-ci ghi một dòng mỗi ~200 ms trong 60 s, mỗi lần một kết nối mới.
  - DSN: `host=192.168.1.247,192.168.1.248 port=5432 dbname=p13a_rt user=headscale target_session_attrs=read-write connect_timeout=2`.
  - Mật khẩu nằm trong pgpass tạm trên `/dev/shm` (0600), chuyển qua stdin, đã xóa (kiểm: 0 file còn lại).
- **Switchover.** Sau 10 s chạy `sudo mc-patronictl switchover --leader mc-pg-01 --candidate mc-pg-02 --force`: exit 0, 3760 ms, "Successfully switched over to "mc-pg-02"".

Kết quả:

| Chỉ số | Giá trị |
|---|---|
| Leader | mc-pg-01 → mc-pg-02; node cũ `pg_is_in_recovery() = t`; timeline 1 → 2 (không cần `pg_rewind`: demote sạch) |
| Thời gian tới khi cụm khỏe lại (leader mới running **và** node cũ là sync standby streaming) | 16 664 ms tính từ lúc bắt đầu switchover |
| Ghi thành công | 207: 40 vào .247 trước khi đổi, 167 vào .248 sau khi đổi. Mọi lần ghi đều vào đúng primary lúc đó |
| Ghi lỗi | 5 (seq 41–45, từ 12:58:41.340 tới 12:58:42.455): 1 "the database system is shutting down", 4 "Connection refused" (.247 đang demote, .248 chưa promote) |
| Khoảng trống ghi lớn nhất | **1670 ms** (lần ghi thành công cuối vào .247 lúc 12:58:41.065 → lần đầu vào .248 lúc 12:58:42.735). Khớp với log Patroni: demote 41.229 → promote 42.723 |
| Mất commit | **0**. 207/207 dòng đã được xác nhận có trên primary mới và trên replica mới; 0 dòng có trong bảng mà client không nhận được xác nhận |
| Dọn | `DROP DATABASE p13a_rt` |

Sau lượt chạy này, script được thêm `psql -q`. Lý do: lượt chạy ghi thêm các dòng `INSERT 0 1` vào `writes.txt`. Phần phân tích (lọc theo cột `ok`/`fail`) không bị ảnh hưởng, nên kết quả trên vẫn đúng.

Hai điểm:
- **Cửa sổ không có sync standby.** Từ lúc promote (12:58:42.7) tới lúc node cũ được thêm lại làm sync standby (~12:58:58), primary mới commit **cục bộ**: chế độ non-strict, D2 đã chấp nhận. Nếu mc-pg-02 chết trong cửa sổ đó, Patroni không tự promote mc-pg-01; muốn promote phải cưỡng bức và chạy fence ADR 0012. F05 cần đo lại cửa sổ này.
- **Thông báo lỗi khi cả hai host đều không nhận.** Lỗi kết nối dạng "Connection refused" có nghĩa cả hai host đều từ chối trong ~1,1 s đó. Libpq chỉ in lỗi của host đầu. Code client (P13b) phải retry trong khoảng này (≤ hạn self-fence 8 s).

### Chi phí commit đồng bộ (ADR 0011 §B ghi "chưa đo")

- Cách đo: `pgbench -n -c1 -T10`, mỗi giao dịch một `INSERT`, chạy trên leader mc-pg-02 qua socket cục bộ, `PGOPTIONS=-c synchronous_commit=…`, mỗi chế độ ×2. Exit 0.
- DB `p13a_commit` đã xóa. Kết quả: `commit-latency.txt`.

| `synchronous_commit` | latency TB | tps (1 client) |
|---|---|---|
| `on` (chờ replica flush WAL xuống đĩa) | 2,184 / 2,141 ms | 458 / 467 |
| `local` | 1,185 / 1,243 ms | 844 / 805 |

- Mỗi commit đắt thêm **~0,95 ms**, cao hơn ước lượng "< 0,5 ms" của ADR. Lý do: `on` chờ fsync WAL trên đĩa của replica, không chỉ một vòng mạng.
- Đây không phải benchmark đường nóng. P14 đo dưới tải thật, có thể so thêm `remote_write`.

## Điểm kết nối cho client (ADR 0011 §D, D3)

Chi tiết: `infra/lab/pg/README.md` §Client endpoint.
- **Hiện tại (đã kiểm qua switchover):** PostgreSQL trực tiếp với DSN nhiều host.

  ```
  host=192.168.1.247,192.168.1.248 port=5432 dbname=<db> user=headscale target_session_attrs=read-write connect_timeout=2
  ```

  Chỉ mc-ci và mc-db tới được 5432 (operator, test, dump/restore).
- **Đích cho process cluster (P13b):** cùng danh sách host với cổng 6432. Trên mỗi node: PgBouncer → HAProxy cục bộ `127.0.0.1:5000`, chọn node theo `GET :8008/primary`. Phía Go cần thêm hỗ trợ nhiều host.
- **Dự phòng:** VIP keepalived `192.168.1.249` (chưa cài, IP để trống), nếu pgx nhiều host qua PgBouncer không chạy như giả định.

## Negative tenant isolation tests

Không áp dụng. PR này chỉ có hạ tầng: không có API, không có dữ liệu node, không đổi code Go. Cách ly tenant không phụ thuộc vào cụm PG (một DB dùng chung, ADR 0005).

## DB migrations and downgrade constraints

- Không có migration. Cụm mới chưa có DB ứng dụng nào: chỉ `postgres`, `template0`, `template1`.
- Downgrade: không áp dụng.

## Benchmark before/after

Không có đường nóng nào bị đổi. Số đo chi phí commit đồng bộ ở trên chỉ để tham khảo.

## Known limitations / security blockers

1. **Chưa có backup, chưa lưu WAL.** `archive_command=/bin/true` cho tới P13c. Không chuyển dữ liệu thật trước khi quyết định có bật pgBackRest trước hay không (xem dưới).
2. **etcd chạy HTTP, không TLS, không auth.** Chỉ nftables giới hạn: 3 thành viên + localhost. Ai có root trên một VM thành viên có thể ghi vào DCS. Nên thêm TLS cho peer/client của etcd ở P13f/N4.
3. **REST API của Patroni chạy HTTP.** Các endpoint không an toàn cần user/password (`PATRONI_RESTAPI_*`). Cổng 8008 chỉ mở cho hai node PG + mc-db.
4. **PostgreSQL không TLS** trên LAN lab, giống mc-db hiện tại. `scram-sha-256`, `pg_hba` theo từng /32.
5. **Cửa sổ không có sync standby** ~14–16 s sau mỗi lần đổi leader (non-strict, xem trên).
6. **Mọi VM trên một host Proxmox.** HA chỉ kiểm được lỗi process, VM và mạng trong VM; không kiểm được lỗi phần cứng.
7. **`on_boot=false`** (giống các VM lab khác): host khởi động lại thì phải bật mc-pg-01/02 bằng tay. Khi đó watchdog và etcd tự lên theo unit.
8. **RAM host sát:** 124 / 125,7 GiB đã cấp; mc-bench phải tắt khi cụm chạy (D7). Cửa sổ P14: theo phương án chia ca đã duyệt.
9. **Chưa làm (trong phạm vi P13a nhưng để sau theo brief):**
   - F05 drill (ngoài một switchover có kiểm soát): sẽ làm cùng process cluster;
   - runbook `infra/lab/pg/RUNBOOK.md`;
   - chuyển `meshlab`;
   - HAProxy/PgBouncer trên node PG (P13b);
   - cảnh báo lệch đồng hồ (P13f).

## Cần gì cho cửa sổ chuyển `meshlab` (làm sau, có báo trước)

1. Điểm kết nối cho c2/c1:
   - hoặc PgBouncer + HAProxy của P13b trên mc-pg-01/02, cổng 6432 đã mở cho .241, .244–.246;
   - hoặc quyết định tạm trỏ thẳng 5432: khi đó phải mở 5432 cho .241, .244–.246 trong `mc-pg.nft`. `pg_hba` đã cho phép `headscale` từ các địa chỉ đó.
2. Dừng c2 lâu dài (mc-c2/mc-c2b) và POC c1 (quyền thường trực).
3. Dump và restore:
   - `docker exec mc-pg pg_dump -U postgres -Fc meshlab` trên mc-db;
   - trên leader: `CREATE DATABASE meshlab OWNER headscale`. Locale `C.UTF-8` khác `en_US.utf8` của mc-db, nên **không** dùng `pg_restore -C`;
   - `pg_restore --no-owner --role=headscale -d meshlab`;
   - so `migrations` và số dòng từng bảng.
4. Đổi DSN trong config c2/c1 rồi khởi động lại. Giữ container cũ ở trạng thái dừng, không xóa volume.
5. Nên bật pgBackRest (P13c), hoặc ít nhất `pg_dump` định kỳ sang mc-db, **trước khi** dữ liệu thật chỉ còn nằm trên cụm mới.
6. Mật khẩu của role `headscale` trên cụm mới **khác** mc-db. Lấy từ `%USERPROFILE%\.mesh-lab\mc-pg.env` (`MC_PG_HEADSCALE_PASSWORD`) và chép vào `pg.pass` của các process bằng pipe.

## Reviewer approval

Chưa có. Chờ chủ dự án duyệt.
