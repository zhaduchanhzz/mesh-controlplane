# ADR 0011 — HA PostgreSQL dùng chung: Patroni + etcd, sao chép đồng bộ; PgBouncer trên mỗi node PG, client dùng danh sách host

- **Trạng thái:** **Accepted** (2026-11-26). Chủ dự án duyệt toàn bộ khuyến nghị D1–D13 trong `tickets/P13.md` (gói quyết định ngày 2026-11-26).
- **Ticket:** P13a (HA PostgreSQL, F05), P13b (HA PgBouncer).
- **Liên quan:** ADR 0005 (PG dùng chung, lease trong DB, Bổ sung 2 §2), ADR 0006 §3.4–3.7, §5 (PgBouncer), ADR 0012 (epoch floor / restore fence), PLAN §7 N3, `06-tests-benchmarks.md` F05.
- **Inventory:** `evidence/source-inventory-P13.md` §2.1–2.2, §3.

## Bối cảnh

- Mọi cluster dùng một PostgreSQL (ADR 0005). Lease cluster (`cluster_leases.epoch`, `expires_at`) và quyền sở hữu tenant (`tailnets.owner_cluster_id`, `owner_epoch`) **nằm trong chính DB đó**. Fence của mọi ghi so (holder, epoch) và (owner, epoch) **bằng** giá trị trong DB, trong cùng transaction.
- Vì vậy an toàn (không owner kép, không holder kép) phụ thuộc vào việc **chỉ có một DB ghi được** và **DB đó không quên một transaction đã commit**.
- PG sập hoặc đang failover: không process nào gia hạn lease; mọi active tự fence sau 8 s (TTL 10 s − margin 2 s); control plane mọi cluster dừng, data plane chạy tiếp theo netmap cuối (ADR 0005 Bổ sung 2 §2).
- Hiện tại: một container PG 16.16 + một PgBouncer 1.24.1 trên `mc-db`; không replica, không WAL archive, `data_checksums = off`.

### Mất transaction khi failover thì epoch có thể lùi

Với sao chép **bất đồng bộ**, replica được promote có thể thiếu các commit cuối của primary cũ. Các ghi đó có thể là:
1. `Acquire` của standby: epoch lease `e → e+1`, holder mới H₂. Sau failover DB lại nói (H₁, e). H₂ tưởng mình giữ e+1: renew lỗi → H₂ tự fence (an toàn). Nhưng `Acquire` kế tiếp lại cấp **e+1 lần thứ hai**, có thể cho holder khác. Một epoch bị cấp hai lần cho hai holder: vi phạm "không hai holder cùng epoch" ở mức lịch sử (log, audit, mọi thứ ngoài DB đã ghi nhận e+1).
2. `HandOver`/`MoveOwner`: `owner_epoch E → E+1`, owner B. Sau failover DB nói (A, E). Cluster B đã nạp runtime ở E+1, client đã được báo chuyển; cluster A nạp lại tenant ở E. Fence ở DB vẫn chặn ghi của B (owner khác), nên không có ghi kép, nhưng epoch E+1 có thể bị cấp lại cho một lần move khác.

Fence ở DB vẫn chặn **ghi** kép trong cả hai trường hợp, vì nó so với dữ liệu của DB đang phục vụ. Cái bị phá là tính đơn điệu của epoch như một token (F05: "lease và `owner_epoch` không lùi; không hai holder"). Đây là lý do ADR này chọn đồng bộ, và ADR 0012 bổ sung epoch floor cho các trường hợp không tránh được mất dữ liệu (restore, promote cưỡng bức).

## Quyết định (đề xuất)

### A. Cơ chế failover: Patroni + etcd (D1)

- Hai node dữ liệu `mc-pg-01` (7708, .247) và `mc-pg-02` (7709, .248): PostgreSQL 16 (PGDG), Patroni, `initdb --data-checksums`, `wal_log_hints = on` (cho `pg_rewind`), `use_pg_rewind: true`.
- DCS: **etcd 3 thành viên**: `mc-pg-01`, `mc-pg-02`, và `mc-db` (7704) làm thành viên thứ ba (witness; mc-db không chạy PG của Patroni). Mất một thành viên vẫn còn quorum.
- Watchdog: `softdog` + `watchdog.mode: required` trên hai node dữ liệu, để primary mất quyền leader trong DCS bị dừng cứng thay vì tiếp tục nhận ghi.
- Thời gian (lab, để F05 đo được): `ttl: 20`, `loop_wait: 5`, `retry_timeout: 5`. Production xem lại ở P14 (mặc định Patroni 30/10/10).
- `maximum_lag_on_failover`: 0 có hiệu lực khi đồng bộ (chỉ node trong `/sync` được promote).
- Không đọc từ replica (code không có đường đọc replica; bootstrap đọc qua cùng PgBouncer).

### B. Sao chép đồng bộ, không strict (D2)

- `synchronous_mode: true`, `synchronous_mode_strict: false`, `synchronous_commit: on` (remote flush). Patroni chỉ promote node nằm trong khóa `/sync` của DCS, tức node đã xác nhận mọi commit.
- Khi replica chết: Patroni bỏ đồng bộ, primary ghi tiếp một mình (tính sẵn sàng giữ nguyên). Nếu primary chết **trong lúc đó**, Patroni **không** tự promote replica (không nằm trong `/sync`). Operator phải promote cưỡng bức; trường hợp đó bị coi là **failover có thể mất dữ liệu** và bắt buộc chạy epoch fence của ADR 0012 trước khi mở lại PgBouncer.
- Chi phí: mỗi COMMIT chờ replica flush WAL (lab: cùng host). Dự kiến ban đầu < 0,5 ms; **đo trong P13a (2026-11-26): +~0,95 ms mỗi commit** (`pgbench` 1 client: 2,16 ms với `on` so với 1,21 ms với `local`, `evidence/P13a-ha-postgres.md`), phần lớn là fsync trên replica, không phải mạng. Đo dưới tải ở P14.
- `strict: true` (từ chối ghi khi không có replica đồng bộ) được ghi là phương án, không chọn: nó biến lỗi replica thành dừng control plane mọi cluster.

### C. Đồng hồ

- Lease dùng `now()`/`clock_timestamp()` của **primary hiện tại**. Sau failover, `expires_at` do máy cũ ghi được so với đồng hồ của máy mới. Nếu máy mới chạy **nhanh** hơn quá safety margin (2 s), standby có thể lấy lease trước khi active cũ tự fence theo đồng hồ monotonic: ghi vẫn bị fence ở DB, nhưng hai process có thể cùng phát netmap từ bộ nhớ trong khe đó.
- Bắt buộc: chrony trên mọi node PG; cảnh báo khi lệch giữa hai node PG > 100 ms (P13f); drill F05 ghi độ lệch đo được. Liên quan L6.

### D. PgBouncer HA (D3)

- Một PgBouncer 1.24.1 trên **mỗi** node PG (cổng 6432), cấu hình như `infra/lab/db/setup-db.sh` (transaction, `max_prepared_statements = 200`, `query_wait_timeout = 5`, `idle_transaction_timeout = 5`).
- Mỗi PgBouncer nối tới **HAProxy cục bộ** `127.0.0.1:5000`, HAProxy chọn node PG trả 200 ở `GET /primary` (REST Patroni :8008), `on-marked-down shutdown-sessions`. Vì vậy cả hai PgBouncer chỉ gửi tới primary hiện tại; sau F05 tự trỏ sang primary mới; không bao giờ gửi ghi tới replica.
- **Client (process cluster, bootstrap) nối bằng danh sách host**: `host=mc-pg-01:6432,mc-pg-02:6432`, `target_session_attrs=read-write`, `connect_timeout=2`, keepalive TCP. pgx thử lần lượt; host không trả lời hoặc trả read-only bị bỏ qua. Cần sửa `PostgresConfig`/`DSN()` (P13b).
- Pool lease riêng (ADR 0006 §3.6): alias `mesh_lease` trong `[databases]` của cả hai PgBouncer, `pool_size = 4`, `reserve_pool_size = 2`; process dùng DSN thứ hai cho `LeaseStore` (P13b).
- Self-check khi khởi động (ADR 0006 §3.5): ngay sau khi mở pool, chạy cùng một câu có tham số hai lần trong hai transaction và `SHOW transaction_read_only`; lỗi "prepared statement … already exists/does not exist" → thông báo rõ "pooler cấu hình sai (`max_prepared_statements = 0`?)"; `read_only = on` → "đang nối tới replica". Fail closed.
- Phương án VIP (keepalived, VRRP, địa chỉ .249) không cần sửa code nhưng thêm VRRP trên LAN nhà và một điểm phải tự kiểm sức khỏe; ghi là phương án dự phòng nếu chủ dự án không muốn sửa DSN.

### E. Chuyển dữ liệu lab

- `meshlab` (8,4 MB) chuyển từ container trên mc-db sang cụm Patroni bằng `pg_dump`/`pg_restore` trong một cửa sổ có báo trước (c2 lâu dài dừng). Sau khi cụm mới chạy ổn và E2E qua nó: dừng `mc-pg`/`mc-pgb` trên mc-db (không xóa volume cho tới khi chủ dự án duyệt).
- E2E dùng DB `e2e_*` tạo/xóa mỗi lượt trên cụm mới.

## Hệ quả

- Failover PG tự động, không mất commit trong trường hợp thường (một node chết), nên lease epoch và `owner_epoch` không lùi (F05) mà không cần fence.
- Thêm thành phần vận hành: Patroni, etcd (3), HAProxy (2), PgBouncer (2), watchdog. Cần giám sát riêng (P13f).
- Lỗi kép hoặc promote cưỡng bức vẫn có thể mất commit → ADR 0012 bắt buộc.
- Mọi VM trên một host Proxmox: lab không kiểm lỗi phần cứng; chỉ kiểm kill process, dừng VM, cắt mạng trong VM.
- RTO dự kiến (chưa đo): Patroni phát hiện ≤ `ttl` 20 s + promote vài giây + process cluster lấy lại lease (≤ TTL lease 10 s + 1 s). Ngưỡng nghiệm thu đề xuất ở `tickets/P13.md` (D12).

## Phương án đã cân nhắc

| Phương án | Ưu | Nhược | Kết luận |
|---|---|---|---|
| **Patroni + etcd** | Chuẩn de facto, đang phát triển; `synchronous_mode` chỉ promote node đồng bộ; REST `/primary` cho HAProxy; watchdog; `pg_rewind`; tích hợp pgBackRest | Nhiều thành phần; cần 3 thành viên DCS | **Đề xuất** |
| pg_auto_failover (monitor + 2 node) | Ít thành phần hơn; mặc định đồng bộ | Monitor là điểm đơn (mất monitor = không failover); cộng đồng nhỏ hơn; tình trạng bảo trì chưa kiểm | Dự phòng |
| repmgr + witness | Quen thuộc | Không có DCS đồng thuận; chống split brain chủ yếu bằng script; không có chế độ "chỉ promote node đồng bộ" tương đương | Loại |
| Promote thủ công có script + fence | Đơn giản nhất; luôn chạy fence (ADR 0012) | Không tự động; RTO phụ thuộc người; không đúng tinh thần "HA" của N3 | Dự phòng nếu chủ dự án muốn tối giản lab |
| Bất đồng bộ + fence sau mỗi failover | Không thêm độ trễ commit | Mỗi failover PG buộc nạp lại mọi tailnet của mọi cluster (fence tăng mọi epoch) và cấp lại epoch có thể đã thấy | Loại |

## Kiểm chứng

- F05 (P13a): `patronictl` failover/switchover và kill -9 primary, dừng VM primary, cắt mạng primary ↔ etcd, mỗi loại ≥ 2 lần, có client thật và tải ghi; ghi chuỗi `cluster_leases.epoch` và `owner_epoch` trước/sau, độ lệch đồng hồ, thời gian dừng control plane.
- Kill PgBouncer khi đang tải (P13b): xem `tickets/P13.md`.
