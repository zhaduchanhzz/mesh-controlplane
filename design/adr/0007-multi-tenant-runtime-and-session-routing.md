# ADR 0007 — Một process, nhiều tailnet: runtime theo tailnet và định tuyến phiên Noise (P04 + P05)

- **Trạng thái:** Accepted (chủ dự án yêu cầu thực hiện P04+P05, 2026-11-23). Hợp đồng dưới đây là nguồn chung cho code và cho các agent viết test song song.
- **Liên quan:** ADR 0001 (runtime theo tailnet), ADR 0005 (PostgreSQL dùng chung, một Noise key mỗi cluster, bootstrap), ADR 0006 (PgBouncer), P03b (`tenantscope`, `HSDatabase` gắn tailnet).

## Bối cảnh

- Sau P03b, mỗi `HSDatabase` được gắn với **một** tailnet: plugin `tenantscope` lọc và gán `tailnet_id` cho mọi câu lệnh.
- Theo ADR 0005, mọi tenant trên một cluster dùng **chung hostname và Noise key**. Lúc bắt tay Noise, server chỉ biết **machine key** (được Noise xác thực), không biết tenant.
- Một thiết bị có thể có profile ở nhiều tenant trên cùng cluster: cùng machine key, nhưng node key khác nhau theo từng profile.

## Quyết định

### 1. Cấu trúc process
```
Cluster process (1 binary, 1 Noise key cluster, 1 listener, 1 pool DB qua PgBouncer)
 ├─ Platform DB handle (không scope): bảng tailnets, phân giải tenant
 ├─ Runtime A: HSDatabase(scope A) + State (NodeStore, policy, IPAM, authCache) + Batcher + GC
 ├─ Runtime B: …
 └─ Noise router: mỗi phiên Noise được gắn vào đúng MỘT runtime
```
- **Runtime** là phần per-tailnet của `Headscale` hiện tại: state, batcher, ephemeral GC, scheduled tasks. Nó **không** tự mở listener, không giữ Noise key, không chạy DERP server.
- **Dùng chung một pool DB.** Mỗi runtime lấy một `HSDatabase` scope riêng từ cùng `*gorm.DB` (`WithContext` + scope), không mở pool mới.
- Runtime chỉ được nạp cho tailnet mà `owner_cluster_id = cluster này` và đang `ACTIVE`, theo đúng `owner_epoch`.

### 2. Phân giải tenant cho một phiên Noise (P04)
| Request | Nguồn tenant | Lookup (không scope, platform) |
|---|---|---|
| `POST /machine/register` có auth key | Credential của auth key | identifier của credential là duy nhất toàn hệ thống → `credentials.tailnet_id` |
| `POST /machine/register` không có auth key (node đã có) | Node đã có | `(machine_key, node_key)` → `nodes.tailnet_id` |
| `POST /machine/map` | Node | `(machine_key từ Noise, node_key trong MapRequest)` → `nodes.tailnet_id` |
| Đăng ký web/OIDC chưa có key | **Chưa có tenant** | Ngoài phạm vi MVP P04 (xem §4) |

- **Một cặp `(machine_key, node_key)` chỉ thuộc một tailnet** (sửa 2026-11-23, khi thực hiện). Ban đầu định dùng unique index toàn hệ thống, nhưng upstream cho một thiết bị đăng nhập lại thành user khác **trong cùng tailnet** với cùng cặp key và giữ node cũ (3 test upstream hỏng). Thay bằng: index lookup + trigger từ chối cặp key đã có ở tailnet khác (PostgreSQL dùng advisory lock), migration `202611230000-nodes-key-pair-one-tailnet`. `TailnetForNode` dùng `SELECT DISTINCT tailnet_id` và vẫn từ chối nếu ra hơn một tailnet. Machine key do Noise xác thực nên không giả mạo được; node key là riêng cho mỗi profile.
- **Phiên dính một tenant (I01).** Request đầu tiên được phân giải xong thì phiên Noise gắn với runtime đó. Request sau phân giải ra tenant khác thì bị **từ chối** và phiên bị đóng.
- **Không phân giải được thì từ chối (fail closed).** Không có tenant mặc định trong process nhiều tenant.
- Tenant không ACTIVE hoặc không thuộc cluster này: từ chối với lỗi rõ ràng, không lộ thông tin tenant khác.

### 3. Tương thích một tenant
- Process có đúng một tailnet (dev, test, `headscale serve`) giữ hành vi upstream: mọi phiên thuộc tailnet đó.
- `serve-tailnets` (POC P02) được thay bằng chế độ cluster ở P05. Front door còn lại làm allowlist hostname của cluster.

### 4. Ngoài phạm vi (ticket sau)
- Đăng ký web/OIDC gắn tenant khi admin duyệt: **P04b**. Trong lúc chưa có, chế độ nhiều tenant **tắt** đường web/OIDC (trả lỗi rõ ràng), chỉ dùng auth key.
- API admin theo tailnet (`/api/v2/tailnet/{id}`), metrics, debug, DERP verify theo tenant: P08.
- Thêm hoặc bớt tailnet khi process đang chạy: P10 (registry và lease).

## Hợp đồng code (để test viết song song)
- `db.Platform`: handle không scope, tạo bởi `db.OpenPlatform(cfg)`, có:
  - `TailnetForAuthKey(key string) (tailnet.ID, error)`
  - `TailnetForNode(mk key.MachinePublic, nk key.NodePublic) (tailnet.ID, error)`
  - `OwnedTailnets(clusterID string) ([]types.Tailnet, error)`
  - `TenantDB(id tailnet.ID) (*HSDatabase, error)` (dùng chung pool)
- `hscontrol.Cluster`: `NewCluster(cfg)`; `(*Cluster).Serve(ctx)`; `(*Cluster).Runtime(id) (*Headscale, bool)`.
- Lỗi: `db.ErrUnknownTenant`, `hscontrol.ErrSessionTenantSwitch`.

## Test bắt buộc (RED trước)
- servertest, 2 tailnet trong 1 cluster process, client thật trong cùng process:
  - client của A không thấy peer của B (S02, R01);
  - auth key của A đăng ký ở tailnet A dù mọi tenant cùng hostname; key không tồn tại bị từ chối;
  - cùng machine key với hai node key thì thành hai node ở A và B, mỗi map stream chỉ thấy tenant của nó (S05, I03);
  - trên một phiên Noise, request map với node key của tenant khác bị từ chối (I01);
  - policy, DNS và IP của A không ảnh hưởng B (S06, S07).
- Unit: lookup ở Platform; một cặp `(machine_key, node_key)` chỉ thuộc một tailnet; tenant không ACTIVE hoặc của cluster khác bị từ chối.
