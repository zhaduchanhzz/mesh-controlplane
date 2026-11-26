# Source Inventory — P12c nhóm B (lease, tính sẵn sàng, tài nguyên)

- **Commit gốc:** `1ffffc1` (`feat/P12c-review-hardening`, từ `master` `dece058`), ngày 2026-11-26.
- **Nhánh:** `feat/P12c-b` (worktree riêng). Không push `origin`, không merge.
- **Trạng thái:** `Planned` khi viết file này (chưa đổi code).
- **Phạm vi:** B1–B6, L1, L2 của `tickets/P12c.md`. Không migration (chỉ nhóm A thêm). Trong `tenantscope.go` chỉ sửa `checkLease` và cách transaction có fence đặt timeout.

## 1. Đã đọc

| Nguồn | Nội dung liên quan |
|---|---|
| `AGENTS.md` | Rule 3 (không đụng lab ngoài phạm vi), 5 (inventory trước), 11 (migration), 12 (Noise), 13 (isolation = BLOCK MERGE) |
| `tickets/P12c.md` nhóm B, `evidence/REVIEW-2026-11-26-tickets.md` §Nhóm B, L1, L2 | B1–B6 và test RED bắt buộc |
| `design/adr/0005-…md` Bổ sung 2 §2 | Lease trong PostgreSQL theo đồng hồ DB; mọi mutation kiểm lease (holder + epoch + chưa hết hạn) **trong cùng transaction**; self-fence monotonic TTL − margin |
| `design/adr/0006-pgbouncer-connection-pooling.md` §3 ràng buộc 2, 4, 6 | Transaction mode: chỉ `SET LOCAL`, chỉ `pg_advisory_xact_lock*`; lease giữ thiết kế CAS trên `cluster_leases`; `query_wait_timeout` < hạn self-fence |
| `design/adr/0010-tailnet-serving-claim.md` | `HandOver`/`TakeOver` dưới `LockOwnership`, kiểm lease bằng `EXISTS` (không khóa dòng lease) |
| `hscontrol/db/tenantscope/tenantscope.go` | `checkFence` (advisory shared theo tailnet → `FOR SHARE OF t`), `LockOwnership` (lý do: `FOR SHARE` không xếp hàng, UPDATE bị bỏ đói), `checkLease:377-404` (`SELECT … FROM cluster_leases … FOR SHARE`) |
| `hscontrol/db/tenantscope/{lease_fence_test,fence_test,fence_bench_test,tenantscope_test}.go` | Fixture `openDB`/`leaseDB`/`leaseCtx`/`writeLoad`/`moveEpoch`; `TestOwnershipChangeNotStarvedByWriters` (mẫu đo bỏ đói), `TestOwnershipChangeBoundedByLockTimeout` (giữ tx 300 ms) |
| `internal/lease/lease.go` | `Acquire` = `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at <= now RETURNING`; `Renew` = `UPDATE` autocommit; `Release`; `clock_timestamp()` |
| `internal/lease/manager.go` | `Run`: `acquire` → `onAcquire` (đồng bộ) → `hold` (renew). Hạn self-fence tính từ lúc bắt đầu acquire, `hold` thấy quá hạn ngay nếu `onAcquire` > TTL − margin (B3) |
| `internal/lease/lease_test.go` | `openStore` (SQLite, hoặc schema riêng trên `MESH_TEST_POSTGRES_DSN`), `fast` config, `stuckStore` |
| `hscontrol/cluster.go` | `load:276-341` đưa runtime vào `c.runtimes` trước `start:344-352`; `reconcile`, `onLeaseAcquired:999-1045` (`waitStopping` dưới `leaseMu`), `onLeaseLost` (đã `waitStopping` trước khi `Run` acquire lại), `retire` (chỉ dừng runtime đang trong `c.runtimes`), `HealthHandler:949` (ping DB mỗi lần), `ReadyHandler:968` (trả `lease_epoch`), `NewCluster` (cấu hình lease) |
| `hscontrol/cluster_apiv2.go` | `APIv2Handler`: bucket toàn cục trừ trước khi xác thực, trước bucket key/tailnet |
| `hscontrol/db/platform_apikey.go:29-76` | `TailnetForAPIKey`: token sai dạng bị từ chối không chạm DB; token đúng dạng → 1 query + verify hash |
| `hscontrol/api/v2/api.go:202-290` | Runtime xác thực lại API key qua handle có scope (`AuthenticateAPIKey`) cho mọi request |
| `hscontrol/noise.go:150-235`, `hscontrol/session_route.go` | `Serve(netutil.NewOneConnListener(…))` rồi `ns.unbind()`; `bind` ghi `h.sessions`; `closeSessions` |
| `hscontrol/app.go:1668` | `servable()` = không retired và lease gate |
| `hscontrol/types/config.go:1048-1090`, `hscontrol/config/{default,validate}.go`, `tools/configgen` | `TenantAPIConfig`; tài liệu cấu hình sinh bằng `go generate` (`configgen table`, `configgen/files`) |
| `hscontrol/servertest/cluster_p05.go`, `cluster_p08b.go`, `cluster_p10.go`, `cluster_p11b.go`, `cluster_client_p05.go`, `cluster_p08b_api_test.go` | Harness cluster (SQLite), `RawNoiseSession`, `SetTailnetOwner`, `WaitRuntime`, `MintAPIKey`, test rate limit P08b |
| `infra/lab/db/setup-db.sh` | PgBouncer 1.24.1 transaction mode; chưa có `idle_transaction_timeout`, `query_wait_timeout`; PostgreSQL không đặt `idle_in_transaction_session_timeout` |
| `infra/lab/e2e/**/run.sh`, `cases*.sh` | Chỉ dùng **mã trạng thái** của `/ready` và `/health`; `lease_epoch` được đọc từ bảng `cluster_leases` hoặc log, không từ body `/ready` |
| `evidence/P11b-cluster-failover.md`, `source-inventory-P09a.md` | Cách chạy test PostgreSQL trên mc-ci: PostgreSQL tạm (`initdb`, unix socket) + `MESH_TEST_POSTGRES_DSN` |

## 2. Ràng buộc hiện có

- Lease: chỉ đồng hồ DB quyết định hết hạn; epoch chỉ tăng (trigger). Takeover chỉ qua `lease.Store.Acquire` và chỉ khi `expires_at <= clock_timestamp()`.
- Fence (I12): owner/epoch/status và lease kiểm trong transaction của câu ghi; lỗi → `ErrFenced`.
- PgBouncer transaction mode: chỉ `SET LOCAL`/`set_config(…, true)`, chỉ advisory lock mức transaction.
- Không đổi schema; không đổi ngữ nghĩa ADR 0005/0006/0010; không đổi mô hình Noise.
- `/ready`: harness và HAProxy chỉ dùng mã 200/503.

## 3. Giả định (kiểm lại trong PR)

- G1: `Serve(netutil.NewOneConnListener)` trả về ngay sau khi phục vụ kết nối đầu tiên trong goroutine (Accept lần hai `io.EOF`), như review đã đọc ở `tailscale.com/net/netutil`. Sẽ kiểm bằng test (B6).
- G2: Trong READ COMMITTED, câu `SELECT` dùng snapshot lúc bắt đầu câu; biểu thức `clock_timestamp()` ở target list của join được tính khi join trả dòng (sau khi function scan đã trả). Thiết kế B1 không dựa vào thứ tự trong một câu: khóa advisory được lấy ở câu riêng trước câu đọc lease.
- G3: `server_reset_query` của PgBouncer không cần cho `set_config(…, true)` (hết hiệu lực khi transaction kết thúc).
