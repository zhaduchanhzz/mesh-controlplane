# 05 — Database / API / Enrollment Contracts

> Chỉ là đặc tả thiết kế. Các SQL ví dụ chưa được kiểm tra trực tiếp trên schema thực của snapshot baseline v0.1 (fresh schema, xem ADR 0002); **không áp dụng production/staging bằng cách copy nguyên mẫu**.

## 1. Domain types

```go
// Pseudocode; không phải API upstream.
type TailnetID string
type ShardID string
type OwnershipEpoch uint64

type VerifiedScope struct {
    TailnetID TailnetID
    ShardID ShardID
    Epoch OwnershipEpoch
    AuthPrincipal string
    Source SourceKind // authenticated client session vs internal admin
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

`VerifiedScope` không được tạo từ `X-Tenant-ID` do Internet client gửi. Hình thành từ edge route binding đã xác thực + Noise session, hoặc mTLS + signed service claims nội bộ. Không lưu scope trong global singleton.

## 2. Shard SQLite ownership

| Table/concept | Scope bắt buộc | Constraint/Index bắt buộc |
|---|---|---|
| `tailnets` | Shard | Unique tailnet ID, status enum |
| `users`, memberships | Tailnet | `(tailnet_id, principal_id)` unique, composite FK |
| `nodes` | Tailnet | `(tailnet_id, machine_key)` unique; node key binding scoped |
| `routes`, `exit_node_approvals` | Tailnet | composite FK node belongs to same tailnet |
| `tailnet_policies` | Tailnet | `(tailnet_id, revision)` monotonic; immutable audit checksum |
| `preauth`, `auth_requests`, `pending` | Tailnet | hash secrets, `nonce`, expiry, single-use, budget |
| `tailnet_dns`, `tailnet_ipam` | Tailnet | per-tailnet prefixes and stable allocations |
| `events`, `subscriptions` | Tailnet | tenant-scoped delivery keys and retention |
| `audit_events` | Tailnet / shard | actor, action, revision, epoch, timestamp |

Use SQLite within a shard; registry PostgreSQL holds placements, not headscale node records. Multi-shard query requires privileged operator API; no union-of-all-shard datasets on tenant endpoint.

## 3. Placement Registry PostgreSQL design (illustrative)

```sql
CREATE TABLE shards (
  id UUID PRIMARY KEY,
  endpoint TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL CHECK (state IN ('READY','DRAINING','MAINTENANCE','UNAVAILABLE')),
  software_version TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE placements (
  tailnet_id UUID PRIMARY KEY,
  shard_id UUID NOT NULL REFERENCES shards(id),
  epoch BIGINT NOT NULL CHECK(epoch >= 1),
  status TEXT NOT NULL CHECK(status IN ('RESERVED','ACTIVE','MOVING','SUSPENDED','DELETING')),
  lease_until TIMESTAMPTZ,
  revision BIGINT NOT NULL DEFAULT 1,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX placements_by_shard ON placements(shard_id, status);
-- Real migration: use transactional CAS UPDATE WHERE epoch/revision matches.
```

`epoch` monotonic, không được giảm sau restore. Tạo ownership lease/fence qua CAS transaction. Cấp shard-bound service certificates và signed route manifest có revision + expiry, chống giả mạo.

## 4. Required internal APIs

API dùng `/internal/v1` dưới mTLS; tên và schema sẽ được mã hóa thành OpenAPI/proto sau khi POC được duyệt.

| Operation | Mức quyền | Điều kiện an toàn |
|---|---|---|
| `CreateTailnet`, `ActivateTailnet`, `SuspendTailnet`, `DeleteTailnet` | platform operator | epoch+idempotency+audit; default-deny atomic |
| `GetTailnet`, `UpdateTailnetSettings` | tenant operator | CAS config revision, whitelist fields |
| `BeginEnrollment`, `AuthRegister`, `ApproveEnrollment`, `DenyEnrollment` | user/admin scoped | nonce TTL, binding machine key, single-use |
| `CreatePreauthKey`, `ListKeys`, `RevokeKey` | tenant operator | secret hashed, scope, expires |
| `ListNodes`, `GetNode`, `RenameNode`, `ExpireNode`, `DeleteNode` | tenant operator | node ownership validation |
| `SetTags`, `ApproveRoute`, `DisableRoute`, `EnableExitNode` | tenant operator | policy validation + identity rules |
| `ListUsers`, `AddMembership`, `RevokeMembership` | tenant operator | do not trust email alone |
| `GetPolicy`, `PutPolicy`, `CheckPolicy` | tenant operator | dry-run no changes, scope, revision |
| `GetDNS`, `SetDNS`, `GetRoutes` | tenant operator | prevent cross-tailnet names/routes |
| `SubscribeEvents` | scoped consumer | resume token, bounded queues, no cross-tenant topics |
| `GetTailnetHealth`, `GetShardHealth` | tenant/operator | redaction per role |
| `GetShardStatus`, `UpdateShardState`, `ReservePlacement` | platform operator | mTLS, epoch CAS, audit |

**Identity and enrollment MVP:** tối thiểu dùng pre-auth token hoặc web-auth approval scoped. OIDC upstream global không bật tùy tiện trong shard; xác minh `oidc.go` và chọn mô hình theo ADR sau khi N1 đã hoạt động.

## 5. Credential format / verification

Không cho client tự set tenant bằng header. API credentials là short-lived signed claims hoặc opaque IDs server lookup; ví dụ claim khái niệm (không phải JWT chuẩn có thể dán dùng ngay):

```json
{
  "iss": "mesh-placement",
  "aud": "shard:shard-01",
  "sub": "service:control-api",
  "tailnet_id": "TAILNET_UUID",
  "epoch": 12,
  "actions": ["node.list", "policy.update"],
  "iat": 1234567890,
  "exp": 1234567950,
  "jti": "random-unique-id"
}
```

- Ký bởi key được quản lý/rotate; shard verify `iss/aud/exp/jti/actions/epoch`, channel mTLS đúng workload identity.
- Mutation có idempotency key và revision/ETag; replay `jti` bị chặn cho operation nhạy cảm, bằng bounded replay cache hoặc one-time tokens.
- Không đưa secret vào JSON logs; audit chỉ fingerprint/hash.
- Token tenant A không thể chuyển sang tailnet B khi thay path/query/payload.

## 6. Session và policy revision

- Map stream mang bound immutable `tailnet_id`, `node_id`, `policy_revision`, `epoch`; session refresh không được tự nhảy tenant.
- Event và stream cursor có tenant, revision và shard identity; unsubscribe/cancel khi delete/suspend.
- `policy` update invalid phải reject atomic, không reset policy thành allow-all.
- `CheckPolicy` dry-run chỉ dùng data node tenant hiện tại; `wildcard` giới hạn trong tailnet.

## 7. Configuration split

**Shard-global config:** listener, TLS/proxy trust, DB path, host resource limits, own shard ID, internal auth, metrics listen, DERP default profile. File YAML chỉ cho infra.

**Tailnet-dynamic config persisted:** domain/MagicDNS, custom nameservers, extra DNS records, IP pools, device expiry, ACL/grants, route approvals, DERP profile, max node/registration/streams, member login settings. Update qua transactional API, validate rồi publish to per-tailnet state/event. Không bắt restart shard khi đổi DNS A hoặc policy A.

## 8. Internal service discovery and edge

- Registry emits versioned signed route manifest (`hostname`, `tailnet_id`, `shard_id`, `epoch`, `expiry`, `revision`). Agent dùng Caddy config API hoặc file reload **được xác minh bằng POC**, không trông chờ Caddy tự tra SQL.
- Long-lived connection upgrade phải giữ shard pin. Registry đổi placement không cho proxy silently rebind stream cũ.
- Nếu manifest expired/unrecognized hostname/SNI mismatch: return error, không route về shard mặc định.

## 9. Nghiệm thu source-level

Mỗi API cần integration test cho success + token wrong tenant + stale epoch + missing scope + forbidden role + replay, lưu audit consistent. Mỗi schema change có FK negative test, unique collision test, fresh DB test và migration/upstream regression.
