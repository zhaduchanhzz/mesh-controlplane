# 05 — Database / API / Enrollment Contracts

> Chỉ là đặc tả thiết kế. Các SQL ví dụ chưa được kiểm tra trực tiếp trên schema thực của snapshot baseline v0.1 (fresh schema, xem ADR 0002); **không áp dụng production/staging bằng cách copy nguyên mẫu**. Cập nhật theo PLAN 2.0 và [ADR 0005](design/adr/0005-cluster-architecture-shared-postgres-bootstrap-incremental-netmap.md): PostgreSQL dùng chung, bootstrap, một Noise key cho mỗi cluster, lease active/standby, cô lập chỉ ở tầng ứng dụng.

## 1. Domain types

```go
// Pseudocode; không phải API upstream.
type TailnetID string
type ClusterID string
type OwnerEpoch uint64   // tailnets.owner_epoch: đổi khi tenant chuyển cluster
type LeaseEpoch uint64   // cluster_leases.epoch: đổi khi failover active/standby

type FencingToken struct {
    ClusterID  ClusterID
    Holder     string     // ID của process
    LeaseEpoch LeaseEpoch
}

type VerifiedScope struct {
    TailnetID     TailnetID
    ClusterID     ClusterID
    OwnerEpoch    OwnerEpoch
    AuthPrincipal string
    Source        SourceKind // RegistrationCredential | NodeRecord | InternalAdmin
}

type TailnetSettings struct {
    BaseDomain string
    MagicDNSEnabled bool
    NameServers []string
    ExtraDNSRecords []DNSRecord
    IPv4Prefix string
    IPv6Prefix string
    DERPProfile string
    NodeExpiryPolicy ExpiryPolicy
    MaxNodes int
    MaxPending int
    MaxMapStreams int
}
```

`VerifiedScope` không được tạo từ `X-Tenant-ID`, Host hay payload do Internet client gửi. Vì Noise key dùng chung cho cả cluster, scope của client được hình thành từ:
- **credential đăng ký** đã xác minh (register), hoặc
- **bản ghi node** khớp đúng một `(machine_key, node_key)` (map/poll), sau khi kiểm tenant thuộc cluster ở `OwnerEpoch` hiện tại.

Với API nội bộ, scope đến từ mTLS + signed service claims. Không lưu scope trong global singleton.

## 2. PostgreSQL dùng chung: bảng tenant

Một PostgreSQL cho mọi cluster (ADR 0005). SQLite chỉ cho dev/unit test, cùng schema.

| Table/concept | Scope bắt buộc | Constraint/Index bắt buộc |
|---|---|---|
| `tailnets` | Nền tảng (đọc bởi cluster sở hữu, bootstrap, placement) | Unique ID và slug, status enum, `owner_cluster_id` (không FK tới `clusters`, P12 Q10: owner chưa đăng ký = R1 trong báo cáo reconcile), `owner_epoch` ≥ 1 đơn điệu |
| `users`, memberships | Tailnet | `(tailnet_id, principal_id)` unique, composite FK |
| `nodes` | Tailnet | Index `(tailnet_id, machine_key)`; `(machine_key, node_key)` phân giải tối đa một node toàn DB (I01) |
| `routes`, `exit_node_approvals` | Tailnet | composite FK node belongs to same tailnet |
| `tailnet_policies` | Tailnet | `(tailnet_id, revision)` monotonic; immutable audit checksum |
| `credentials` (preauth, API key), `pending_registrations` | Tailnet | hash secrets, `nonce`, expiry, single-use, budget; `(kind, identifier)` unique toàn cục, lookup kiểm tailnet |
| `tailnet_dns`, `tailnet_ipam` | Tailnet | per-tailnet prefixes and stable allocations |
| `tailnet_events`, subscriptions | Tailnet | tenant-scoped delivery keys and retention |
| `audit_events` | Tailnet / nền tảng | actor, action, revision, owner_epoch, lease_epoch, timestamp |

**Cô lập chỉ ở tầng ứng dụng (ADR 0005 Bổ sung 2 §3):**
- Mọi truy cập bảng tenant qua plugin GORM fail-closed: thiếu scope → `ErrNoScope`.
- SQL thô (`Raw`, `Exec`, migration) chỉ nằm trong allowlist đã review tay; CI chặn câu mới.
- **Không** row-level security, **không** role DB theo cluster. Mọi cluster dùng một role ứng dụng; role migration tách riêng.
- Rủi ro còn lại: `03-isolation-threat-model.md` §8.

Query xuyên tenant chỉ qua API platform operator có quyền riêng; không có union dữ liệu nhiều tenant trên endpoint tenant.

## 3. Registry, lease và fencing (cùng PostgreSQL, minh họa)

```sql
CREATE TABLE clusters (
  id TEXT PRIMARY KEY,
  endpoint_url TEXT NOT NULL UNIQUE,     -- https://cluster-2.cp.gta5web.online:8443
  class TEXT NOT NULL CHECK (class IN ('shared','dedicated')),
  state TEXT NOT NULL DEFAULT 'DISABLED' CHECK (state IN ('ACTIVE','DRAINING','DISABLED')),
  noise_public_key TEXT,                 -- chỉ public key, để đối chiếu (P12 Q6: cluster ghi lần đầu khi lấy lease)
  software_version TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE cluster_leases (
  cluster_id TEXT PRIMARY KEY,           -- không FK (P12 Q10)
  holder TEXT NOT NULL,
  epoch BIGINT NOT NULL CHECK (epoch >= 1),
  expires_at TIMESTAMPTZ NOT NULL
);
-- tailnets.owner_cluster_id / owner_epoch: xem PLAN §4.
-- state (P10, P12 Q10): ACTIVE = placement có thể chọn; DRAINING = giữ tailnet đang có, không nhận tailnet mới;
-- DISABLED = mặc định khi đăng ký, không nhận tailnet mới, bootstrap trả 503. Process vẫn phục vụ tailnet
-- mình sở hữu ở mọi state (Q3). Schema thật: hscontrol/db/schema_postgres.sql.
-- Không có FK tailnets.owner_cluster_id → clusters(id) và cluster_leases.cluster_id → clusters(id)
-- (P12 Q10): owner chưa đăng ký được báo cáo là R1 qua GET /v1/reconcile của mesh-placement;
-- CLI và API placement từ chối tạo/chuyển tailnet tới cluster chưa đăng ký hoặc không ACTIVE (Q5).

-- Mọi mutation tenant (helper fencing, P10 + P11b), trong CÙNG transaction:
--   SELECT 1 FROM cluster_leases
--    WHERE cluster_id=$c AND holder=$me AND epoch=$lease_epoch AND expires_at > now()
--    FOR SHARE;
--   SELECT 1 FROM tailnets
--    WHERE id=$t AND owner_cluster_id=$c AND owner_epoch=$owner_epoch AND status='ACTIVE'
--    FOR SHARE;
--   … mutation …
-- Không có dòng nào → ROLLBACK, runtime/process tự fence.
```

- `owner_epoch` và `cluster_leases.epoch` đơn điệu, không được giảm sau restore (giữ high-water mark ngoài backup, P13).
- Takeover lease chỉ khi `expires_at < now()` theo đồng hồ của PostgreSQL. Không dùng đồng hồ process để quyết định lease ở DB.
- Process tự fence theo deadline monotonic (PLAN §1 "Runtime HA", `tickets/P11b.md`).

## 4. Required APIs

API nội bộ dùng `/internal/v1` dưới mTLS; API bootstrap là public HTTPS. Tên và schema sẽ được mã hóa thành OpenAPI qua Huma (AGENTS rule 10).

| Operation | Mức quyền | Điều kiện an toàn |
|---|---|---|
| `CreateTailnet`, `ActivateTailnet`, `SuspendTailnet`, `DeleteTailnet` | platform operator | owner_epoch + idempotency + audit; default-deny atomic |
| `GetTailnet`, `UpdateTailnetSettings` | tenant operator | CAS config revision, whitelist fields |
| `BeginEnrollment`, `AuthRegister`, `ApproveEnrollment`, `DenyEnrollment` | user/admin scoped | nonce TTL, binding machine key, single-use; trang phê duyệt hiển thị tenant |
| `CreatePreauthKey`, `ListKeys`, `RevokeKey` | tenant operator | secret hashed, scope, expires |
| `ListNodes`, `GetNode`, `RenameNode`, `ExpireNode`, `DeleteNode` | tenant operator | node ownership validation |
| `SetTags`, `ApproveRoute`, `DisableRoute`, `EnableExitNode` | tenant operator | policy validation + identity rules |
| `ListUsers`, `AddMembership`, `RevokeMembership` | tenant operator | do not trust email alone |
| `GetPolicy`, `PutPolicy`, `CheckPolicy` | tenant operator | dry-run no changes, scope, revision |
| `GetDNS`, `SetDNS`, `GetRoutes` | tenant operator | prevent cross-tailnet names/routes |
| `SubscribeEvents` | scoped consumer | resume token, bounded queues, no cross-tenant topics |
| `GetTailnetHealth`, `GetClusterHealth` | tenant/operator | redaction per role |
| `ListClusters`, `UpdateClusterState` | platform operator | mTLS, audit; không giảm epoch |
| `BeginMove`, `AbortMove`, `GetMoveStatus` | platform operator | owner_epoch CAS, thông báo trước (P12b) |
| `Bootstrap` (public) | caller có credential gắn tenant (mặc định; xem §8) | chỉ `ACTIVE`; phản hồi từ chối đồng nhất; rate limit |

**Identity and enrollment MVP:** tối thiểu dùng pre-auth token hoặc web-auth approval scoped. OIDC upstream global không bật tùy tiện trong cluster; xác minh `oidc.go` và chọn mô hình theo ADR sau khi N1 đã hoạt động.

## 5. Credential format / verification

Không cho client tự set tenant bằng header. API credentials là short-lived signed claims hoặc opaque IDs server lookup; ví dụ claim khái niệm (không phải JWT chuẩn có thể dán dùng ngay):

```json
{
  "iss": "mesh-placement",
  "aud": "cluster:cluster-2",
  "sub": "service:control-api",
  "tailnet_id": "TAILNET_UUID",
  "owner_epoch": 12,
  "actions": ["node.list", "policy.update"],
  "iat": 1234567890,
  "exp": 1234567950,
  "jti": "random-unique-id"
}
```

- Ký bởi key được quản lý/rotate; cluster verify `iss/aud/exp/jti/actions/owner_epoch`, channel mTLS đúng workload identity, rồi **vẫn** kiểm owner + lease trong transaction.
- Mutation có idempotency key và revision/ETag; replay `jti` bị chặn cho operation nhạy cảm, bằng bounded replay cache hoặc one-time tokens.
- Không đưa secret vào JSON logs; audit chỉ fingerprint/hash.
- Token tenant A không thể chuyển sang tailnet B khi thay path/query/payload.
- Credential đăng ký (pre-auth key, enrollment token) là nguồn scope của register (I01); credential của tenant không thuộc cluster → từ chối.

## 6. Session và policy revision

- Map stream mang bound immutable `tailnet_id`, `node_id`, `policy_revision`, `owner_epoch`, `lease_epoch`; session refresh không được tự nhảy tenant.
- Một phiên Noise gắn đúng một tailnet sau request đầu tiên đã xác thực; request sau khác tailnet → từ chối.
- Event và stream cursor có tenant, revision và cluster identity; unsubscribe/cancel khi delete/suspend/move, và khi process mất lease.
- `policy` update invalid phải reject atomic, không reset policy thành allow-all. Đổi policy luôn full rebuild netmap (P00d).
- `CheckPolicy` dry-run chỉ dùng data node tenant hiện tại; `wildcard` giới hạn trong tailnet.

## 7. Configuration split

**Cluster-global config:** listener, TLS cert của hostname cluster, allowlist hostname, DSN PostgreSQL (credential qua env/secret store), giới hạn kết nối, `cluster_id`, `instance_id` (holder), đường dẫn Noise key của cluster (file/secret store, giống nhau trên active và standby), TTL/chu kỳ lease và biên tự fence, metrics listen, DERP default profile, công tắc netmap tăng dần (mặc định OFF). File YAML chỉ cho infra.

**Tailnet-dynamic config persisted:** domain/MagicDNS, custom nameservers, extra DNS records, IP pools, device expiry, ACL/grants, route approvals, DERP profile, max node/registration/streams, member login settings. Update qua transactional API, validate rồi publish to per-tailnet state/event. Không bắt restart cluster khi đổi DNS A hoặc policy A.

## 8. Bootstrap (thay "edge route manifest" của bản 1.1)

Không còn edge định tuyến theo Host, không còn route manifest ký. Client hỏi bootstrap một lần (và lại khi tenant chuyển cluster), rồi kết nối thẳng cluster.

**Phác thảo API (chốt trong P11):**

```http
POST /v1/bootstrap
Content-Type: application/json

{"tenant": "acme", "credential": "<enrollment token hoặc preauth key>"}
```

```json
200 OK
{"control_url": "https://cluster-2.cp.gta5web.online:8443", "issued_at": "2026-11-22T10:00:00Z"}
```

| Trạng thái tenant | Có credential hợp lệ của tenant | Không có / sai credential |
|---|---|---|
| `ACTIVE` | 200 + `control_url` | 404 đồng nhất |
| `MOVING` | 503 + `Retry-After` | 404 đồng nhất |
| `SUSPENDED`, `PENDING`, `DELETING`, không tồn tại | 404 đồng nhất | 404 đồng nhất |

- Phản hồi 404 giống nhau về status, body và thời gian (trong ngưỡng đo) để không lộ tenant tồn tại (I15). Có cho phép tra ẩn danh theo slug hay không là câu hỏi mở của P11.
- Bootstrap chỉ đọc PostgreSQL; không ghi; không giữ state phiên.
- Bootstrap **không** là nguồn tin cậy: cluster tự kiểm owner/epoch/lease ở mọi request (F07).
- Long-lived connection: client nối thẳng cluster nên không có proxy nào có thể "rebind" stream sang cluster khác. Endpoint của cluster (VIP/L4) chỉ chọn giữa active và standby của **cùng** cluster; process không giữ lease từ chối.

## 9. Nghiệm thu source-level

Mỗi API cần integration test cho success + token wrong tenant + stale `owner_epoch` + stale `lease_epoch` + missing scope + forbidden role + replay, lưu audit consistent. Mỗi schema change có FK negative test, unique collision test, fresh DB test trên PostgreSQL và SQLite, test plugin scope và kiểm allowlist SQL thô.
