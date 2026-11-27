# Source Inventory — LAB-P13 + P13a (hạ tầng HA PostgreSQL)

- **Commit đọc:** `c4533aa` (`feat/P13-plan`, ADR 0011–0014 Accepted, D1–D13 đã duyệt 2026-11-26).
- **Nhánh:** `feat/P13a-infra`.
- **Phạm vi:**
  - VM 7708/7709 (apply từ plan đã lưu);
  - Patroni + etcd (3 thành viên, witness trên mc-db), chrony, softdog;
  - một switchover có kiểm soát.
- **Không thuộc phạm vi:** code Go, chuyển `meshlab`, F05 drill, PgBouncer/HAProxy (P13b), pgBackRest (P13c).

## 1. Đã đọc

| Nguồn | Nội dung liên quan |
|---|---|
| `AGENTS.md` rule 2, 3, 4, 5 | Bí mật ngoài repo; tự apply chỉ khi plan create/update trong pool `meshcontrol-lab` và VMID 7700–7799; `root@pam` chỉ trong pool lab; inventory đầu PR |
| `tickets/P13.md` LAB-P13, P13a, D1–D13, Rủi ro | VM .247/.248, 4/4/60; nftables trong VM (5432 / 6432 / 2379-2380 / 8008); `setup-pg.sh`; `ttl 20 / loop_wait 5 / retry_timeout 5`; RAM 124/125,7 GiB với mc-bench tắt; điều kiện dừng khi RAM trống < 8 GiB; kiểm boot sau apply |
| `design/adr/0011-postgresql-ha-patroni-sync-pgbouncer.md` §A–E | Patroni + etcd 3 thành viên; softdog `mode: required`; `synchronous_mode` không strict; chrony; DSN nhiều host + `target_session_attrs=read-write`; VIP .249 dự phòng; HAProxy `/primary` |
| `design/adr/0006-pgbouncer-connection-pooling.md` §3.4–3.7, §5 | Timeout PgBouncer < hạn self-fence; pool `mesh_lease` (cho P13b, chỉ ghi vào README) |
| `evidence/source-inventory-P13.md` §3.3–3.4, giả định 3, 4, 7 | mc-db: PG 16.16, `data_checksums=off`; bản nháp root `drafts/p13-pg` với plan 4 to add; IP .247–.249 trống |
| `infra/lab/drafts/p13-pg/main.tf` (giờ là `infra/lab/pg/main.tf`) | Biến `pg_vms` có validation (VMID, IP, tổng RAM ≤ 16 GiB), `prevent_destroy`, `ignore_changes` |
| `infra/lab/db/main.tf`, `infra/lab/db/setup-db.sh` | Kiểu root riêng có state riêng; role `headscale` với `idle_in_transaction_session_timeout=5s`; quy ước bí mật (0700, stdin, không argv) |
| `infra/lab/cloud-init/mc-ci.yaml.tftpl` | Có cài Docker và build tools → không dùng cho VM PG; viết `mc-pg.yaml.tftpl` riêng |
| `infra/lab/scripts/{tofu.sh,check-plan.py,pve_api.py}`, `infra/lab/README.md` | Chạy tofu trong WSL; cổng tự apply; PVE API dùng cert đã pin |
| Memory của agent: lab access, stage 2, quyền thường trực | State ở checkout chính; không bọc `apply` trong `timeout`; `.wsl-hosts.sh` trước mỗi lần chạy tofu |

## 2. Hiện trạng đo trước khi làm (2026-11-26 ~12:38Z)

| Mục | Đo được |
|---|---|
| Host | RAM 125,7 GiB, available 28,3 GiB. VM đang chạy đã được cấp 116,0 GiB, 70 vCPU. mc-bench (7703, 24 GiB) tắt |
| VMID | Đang dùng: 7701–7707, 7714–7718. Trống: 7708, 7709 |
| IP | .247, .248, .249: không trả lời ping, ARP `INCOMPLETE` |
| mc-db | 4 vCPU; available 7,2 GiB; đĩa trống 54 GB; loadavg 0,00 |
| mc-db: dịch vụ | `mc-pg`, `mc-pgb` Up 2 days; `nftables.service` disabled; chỉ có bảng iptables-nft của Docker; giờ do `systemd-timesyncd` đồng bộ |
| mc-db: kernel | `6.8.0-147-generic`, có `softdog.ko` |
| Bản phát hành có sẵn | PGDG noble: `postgresql-16 16.16`, `patroni 4.1.5-2`, `pgbackrest 2.60.1`, `pgbouncer 1.26.1`. etcd: v3.7.4, v3.6.17, v3.5.36 |

## 3. Ràng buộc

- Không đụng tới:
  - mc-cp, mc-c2, mc-c1b, mc-c2b, VM client, relay, VPS, HAProxy;
  - container `mc-pg`/`mc-pgb` trên mc-db.
- mc-db chỉ được thêm witness etcd, chạy như một dịch vụ riêng.
- Không restart hay tạo IO nặng trên mc-db, vì E2E có thể đang dùng PgBouncer của nó.
- Cụm mới là cụm fresh, **không** chuyển `meshlab`.
- Chỉ một switchover có kiểm soát; chưa chạy F05.

## 4. Giả định (chưa kiểm hoặc chỉ kiểm một phần)

1. pgx đi qua PgBouncer với DSN nhiều host hoạt động đúng như libpq. Hôm nay mới kiểm bằng `psql` (libpq 16.15) tới thẳng 5432. Kiểm thật ở P13b.
2. `.247`/`.248`/`.249` không nằm trong dải DHCP của router: chỉ kiểm ping/ARP, chưa xem cấu hình router.
3. Cửa sổ ~14–16 s không có sync standby sau khi đổi leader (đo một lần) chấp nhận được với D2. F05 đo lại.
4. Chi phí commit đồng bộ ~1 ms mỗi commit (1 client, đo trên leader) không ảnh hưởng tới đường nóng. P14 đo dưới tải.
