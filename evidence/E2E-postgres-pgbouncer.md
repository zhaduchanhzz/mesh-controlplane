# E2E cluster trên PostgreSQL + PgBouncer (2026-11-24)

- **Mục đích:** chạy bộ E2E client thật của cluster (harness `infra/lab/e2e/p05/run.sh`) trên đúng tầng dữ liệu kiến trúc chốt (ADR 0005: PostgreSQL dùng chung; ADR 0006: PgBouncer transaction mode), thay vì SQLite.
- **Nhánh:** `feat/E2E-postgres` (từ `master` `d56a931`). Trạng thái: `Verified`, chưa `Reviewed`.

## Cách chạy

`DB=postgres BIN=… BOOT_BIN=… infra/lab/e2e/p05/run.sh`

| Thành phần | Chi tiết |
|---|---|
| PostgreSQL | `postgres:16` (16.16), container mới mỗi lần chạy trên mc-bench, `--auth=scram-sha-256` |
| PgBouncer | `edoburu/pgbouncer:v1.24.1-p1` (1.24.1): `pool_mode = transaction`, `max_prepared_statements = 200`, `auth_type = scram-sha-256`, `default_pool_size = 20`, nghe `192.168.1.242:6432` (LAN lab, không qua VPS) |
| Cluster | mc-cp, config cluster với `database.type: postgres`, `host 192.168.1.242:6432`, `pass: file:/etc/headscale/cluster/pg.pass`, `ssl: disable` |
| Mật khẩu | 32 ký tự ngẫu nhiên mỗi lần; chỉ nằm trong file 0600 (máy chạy harness), thư mục 0700 (mc-bench), file 0640 `root:headscale` (mc-cp); không trên dòng lệnh, không trong log; bị xóa khi cleanup |
| Dọn dẹp | `pg.sh down` xóa container, network, file; xóa `pg.pass` trên mc-cp; khôi phục POC |

Binary: cluster sha256 `cde8c6904e5d89c1…`, `mesh-bootstrap` `3b1821814b5699bd…` (build từ `fbf0e53`, mã server giống `master` `d56a931`). Client `tailscale/tailscale:v1.104.7`.

## Kết quả

| Lần | Kết quả | Artifact (không commit) |
|---|---|---|
| 1 | **29/29 PASS**, cleanup_ok=1, 0 key chưa redact | `artifacts/f20f872/P05/e2e-20261124T114151Z` |
| 2 | **29/29 PASS**, cleanup_ok=1, 0 key chưa redact | `artifacts/f20f872/P05/e2e-20261124T115009Z` |

29 ca gồm: HTTP/Host guard, cô lập peer A/B, IPAM theo tailnet, data plane, profile A↔B cùng máy, từ chối, S01/S03, restart cluster không đăng nhập lại (S16), P10 (owner/epoch, suspend/resume, chuyển cluster, retire), vòng đời (S18), bootstrap B01–B06 qua đường public, DNS theo tailnet D01–D03, S12. Log cluster xác nhận `database=postgres` (không có `sqlite`); quét artifact không có chuỗi giống mật khẩu.

## Sự cố trong quá trình (không tính, đã sửa)

1. **Tiến trình harness mồ côi:** lần khởi chạy đầu dùng `&` trong một tác vụ nền; tiến trình con tiếp tục chạy sau khi tác vụ cha kết thúc, và bước cleanup của nó (`pg.sh down`) xóa container của các lần chạy sau (sự kiện Docker 11:36:12). Không phải lỗi sản phẩm; từ đó chỉ chạy một harness mỗi lúc, không dùng `&`.
2. **Race sẵn sàng của PostgreSQL** (`f20f872`): lần khởi động đầu, image chạy một server tạm chỉ trên unix socket rồi khởi động lại; `pg_isready` qua socket có thể đạt với server tạm rồi lệnh kiểm tra kế tiếp thất bại. Sửa: kiểm qua TCP, hai lần liên tiếp.

## Hạn chế

- `ssl: disable` giữa mc-cp và PgBouncer (LAN lab). Production cần TLS (`verify-full`) tới PgBouncer và từ PgBouncer tới PostgreSQL.
- Một PostgreSQL, một PgBouncer, không HA (P13): chưa kiểm failover, kill PgBouncer giữa tải, self-check cấu hình pooler khi khởi động (ADR 0006 §5).
- Không đo hiệu năng trong lần này (E2E chức năng); số đo PgBouncer có ở `evidence/P14-db-benchmarks.md`.
- Unit test tầng DB đã chạy trên PostgreSQL từ trước (P03b, P06, P10c); đây là lần đầu cả cluster + client thật chạy trên PostgreSQL qua PgBouncer.

## Cập nhật: PostgreSQL cố định trên mc-db (2026-11-24, chủ dự án duyệt)

- **VM `mc-db`** (VMID 7704, 192.168.1.243, 4 vCPU / 8 GiB / 60 GB, pool `meshcontrol-lab`): root OpenTofu riêng `infra/lab/db` (state riêng, vì state của `infra/lab` không có trên máy chạy). `tofu fmt`/`validate` exit 0; plan: `2 to add, 0 to change, 0 to destroy` (snippet `mc-db-user-data.yaml`, VM 7704); `check-plan.py`: "In lab scope"; apply đúng file plan, exit 0.
- **`infra/lab/db/setup-db.sh`** (chạy lại được): PostgreSQL 16.16 (volume `mc-pg-data`, SCRAM cho kết nối mạng, socket nội bộ container trust) + PgBouncer 1.24.1 (transaction, `max_prepared_statements = 200`, database wildcard) trên `192.168.1.243:6432`; database lâu dài `meshlab`. Mật khẩu qua stdin, lưu trong `/opt/mc-db` (0700) và `%USERPROFILE%\.mesh-lab\mc-db.env` (ACL chỉ người dùng hiện tại). Lần cài đầu dùng `--auth=scram-sha-256` cho cả socket nội bộ nên bước tạo role thất bại; volume khi đó mới tạo, chưa có dữ liệu, được xóa và tạo lại với `--auth-local=trust`.
- **Harness:** `DB=postgres` tạo database `e2e_<run>` (owner `headscale`) trên mc-db và xóa khi cleanup (chỉ tên dạng `e2e_[0-9a-z]+`, nên không bao giờ đụng `meshlab`); `DB=pgcontainer` giữ cách container tạm (`pg.sh`).

| Lần | Kết quả | Database | Artifact (không commit) |
|---|---|---|---|
| 1 | **29/29 PASS**, cleanup_ok=1, 0 key chưa redact | `e2e_20261124t125103z`, đã xóa | `artifacts/605b30c/P05/e2e-20261124T125103Z` |
| 2 | **29/29 PASS**, cleanup_ok=1, 0 key chưa redact | `e2e_20261124t125949z`, đã xóa | `artifacts/605b30c/P05/e2e-20261124T125949Z` |

Sau hai lần chạy, mc-db chỉ còn database `meshlab`.

**Phân loại DB theo loại test:** DB cố định `meshlab` cho lab giai đoạn 2, thử tay và test dài (dữ liệu tích lũy, migration chạy dần); E2E P05 dùng database riêng mỗi lần (harness có slug cố định, chuyển cluster, DELETING terminal, nên chạy lại trên cùng DB sẽ hỏng); unit test và servertest tự tạo DB tạm; benchmark dùng snapshot.
