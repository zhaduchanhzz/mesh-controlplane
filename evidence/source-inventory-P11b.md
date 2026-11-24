# Source inventory P11b: lease cluster và failover active/standby

- **Nhánh:** `feat/P11b-cluster-failover`, từ `master` `20d178e`.
- **Ticket:** `tickets/P11b.md`. **ADR:** 0005 (Bổ sung 2 §2).
- **Bối cảnh (chủ dự án, 2026-11-24):** tạm gác DERP, hoàn thiện control plane trước. P11b không chờ câu hỏi nào; P12 và P12b phụ thuộc nó.

## Path và symbol đã đọc

| Path | Symbol | Liên quan P11b |
|---|---|---|
| `internal/tailnet/fence.go` | `Fence{cluster, epoch}`, `NewFence`, `WithFence`, `FenceFromContext` | Comment ghi sẵn "Placeholder for the cluster lease epoch: P11b". Thêm lease epoch và holder vào `Fence` |
| `hscontrol/db/tenantscope/tenantscope.go:306` | `checkFence` | Mọi ghi tenant kiểm `tailnets.owner_cluster_id/owner_epoch/status` trong transaction (`FOR SHARE` + advisory lock dùng chung). Thêm kiểm `cluster_leases` cùng chỗ |
| `hscontrol/db/tenantscope/tenantscope.go:286` | `LockOwnership` | Advisory lock theo tailnet (P10c); lease dùng lock riêng theo cluster |
| `hscontrol/db/tenant.go:211,309` | tạo `Fence` cho handle tenant | Fence lấy lease epoch hiện giữ |
| `hscontrol/db/platform.go:40` | `v01Migrations()` | Chuỗi: init, nodeKeyPair, registry, moving, nodeIP, tailnetSettings. Migration lease thêm vào **cuối** |
| `hscontrol/db/platform.go:251,276` | `OwnedTailnets`, `TenantDB` | Standby không gọi; active mới gọi sau khi giữ lease |
| `hscontrol/db/registry.go` | DDL `clusters`, trigger `owner_epoch` monotonic | Mẫu DDL SQLite/PostgreSQL song song; `cluster_leases` FK tới `clusters(id)` |
| `hscontrol/db/schema.sql`, `schema_postgres.sql` | schema v0.1 | Thêm `cluster_leases` vào cuối hai file (DB mới nhận qua InitSchema) |
| `hscontrol/cluster.go:137` | `NewCluster` | Hiện nạp runtime ngay khi khởi tạo. P11b: chỉ mở platform + key; nạp runtime khi giữ lease |
| `hscontrol/cluster.go:329,478` | `reconcile`, `retire` | Tái dùng `retire` cho mất lease (đóng phiên, hủy runtime) |
| `hscontrol/cluster.go:687,724,806` | `NoiseUpgradeHandler`, `HealthHandler`, `HTTPHandler` | Thêm `/ready`; standby trả 503 cho `/ts2021` |
| `hscontrol/cluster.go:835` | `Serve` | Thêm vòng lease (acquire/renew) và watchdog deadline monotonic |
| `cmd/headscale/cli/cluster.go` | `serve --cluster-id` | Thêm định danh holder (mặc định hostname + PID, cấu hình được) |

## Lựa chọn thiết kế (ghi ADR bổ sung khi code)

1. **Bảng** `cluster_leases(cluster_id PK → clusters.id, holder text, epoch bigint ≥ 1, expires_at timestamptz, updated_at)`; trigger: `epoch` không giảm, đổi `holder` thì `epoch` phải tăng (giống trigger `owner_epoch`). Thời gian chỉ dùng `now()` của PostgreSQL.
2. **Giá trị mặc định** (cấu hình được dưới `tuning.cluster_lease.*`): TTL 10 s; gia hạn mỗi TTL/3 ≈ 3,3 s; standby thử mỗi 1 s; biên an toàn tự fence 2 s (deadline = lúc **bắt đầu** lần gia hạn thành công cuối + TTL − 2 s, đồng hồ monotonic). Lý do: failover ≤ ~11 s, mất một lần gia hạn vẫn chưa fence.
3. **Fencing ở DB:** `Fence` mang thêm `leaseHolder`, `leaseEpoch`. `checkFence` đọc thêm `cluster_leases` của cluster (`FOR SHARE`): `holder` và `epoch` khớp, `expires_at > now()`. Không khớp → `ErrFenced`, rollback. Thứ tự khóa: advisory tailnet → hàng `tailnets` → hàng `cluster_leases`.
4. **Tự fence monotonic:** watchdog hẹn giờ tới deadline; quá deadline thì retire mọi runtime (đóng phiên Noise, long-poll, ngừng MapResponse) và về standby, kể cả khi không liên lạc được DB.
5. **Standby:** listener mở; `/health` 200 (process sống), `/ready` 503; `/ts2021` 503; không dựng runtime, không gọi `TenantDB`.
6. **Endpoint: L4 forwarder có health check `/ready`** (HAProxy chế độ TCP, TLS passthrough, check HTTPS `/ready` với SNI đúng), không dùng VIP/keepalived. Lý do: không cần VRRP/multicast trên LAN nhà và không đụng mạng host Proxmox (AGENTS rule 3–4); tương đương LB có health check ở production. Định tuyến chỉ để sẵn sàng; standby vẫn tự từ chối (fail closed).
7. **Một process đơn** (lab hiện tại, harness P05/hai cluster): tự giành lease sau ≤ 1 s nên hành vi cũ giữ nguyên.
8. **Noise key:** cả hai process đọc cùng file key của cluster (chép ngoài repo, 0600); không ghi DB, không log.

## Ràng buộc

- AGENTS rule 11: migration chỉ thêm vào cuối, chạy trong transaction, có DDL cho cả SQLite và PostgreSQL, không `AutoMigrate`.
- Rule 12: nếu client thật phải đăng nhập lại sau failover → dừng, ADR.
- Rule 13: bất kỳ lúc nào hai process cùng phát MapResponse hoặc cùng ghi → BLOCK MERGE.
- Ngoài phạm vi: HA PostgreSQL (P13), active-active, nhiều standby, move giữa cluster (P12b).

## Thứ tự làm

1. RED: unit `internal/lease` (DB thật PostgreSQL trên mc-ci + SQLite), test `checkFence` từ chối lease hết hạn/epoch cũ, servertest hai process (F02–F04, standby từ chối `/ts2021`).
2. Migration + schema; `internal/lease`; nối vào `Fence`, `checkFence`, `Cluster`.
3. Unit + race toàn repo.
4. Lab: hai process cho c1 (mc-cp + VM thứ hai trong dải 7700–7799, cần plan tạo VM: agent tự apply được), HAProxy trên mc-cp; E2E F01–F04 với client thật.
5. Evidence, runbook.

## Giả định còn lại

- `checkFence` thêm một lần đọc `cluster_leases` mỗi lần ghi: chi phí cần đo (benchmark B3 của P10c làm chuẩn).
- Các state trong bộ nhớ (`authCache`, pings, `registerLocks`) mất khi failover; chấp nhận theo ticket, ghi runbook.
