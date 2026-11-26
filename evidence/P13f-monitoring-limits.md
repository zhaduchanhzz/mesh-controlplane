# PR P13f Evidence — giám sát, giới hạn, B7, L3/L4/L5/L9/L10

- **Repo commit before / after:** `c4533aa` (`feat/P13-plan`) → nhánh `feat/P13f-observability` (commit RED `b504d4e`, commit GREEN và artifact: xem `git log c4533aa..`). Chỉ push lên `mcci`.
- **Trạng thái:** code `Implemented` + unit/race/in-process `Verified` trên mc-ci; artifact cấu hình (monitoring, PgBouncer, VPS HAProxy) `Planned` — **không áp dụng**, không đụng lab/VPS, không chạy E2E. Chưa `Reviewed`.
- **Inventory:** `evidence/source-inventory-P13f.md`.

## 1. Verified source paths & symbols

Xem inventory §1–§2. Symbol mới: `internal/lease/metrics.go` (`managerCollector`, `renewDuration`, `leaseEvents`), `tenantscope.refuse`/`fenceRefusals`, `internal/dbstats` (`Register`), `hscontrol/cluster_metrics.go` (`reconcileDuration`, `tailnetReloadFailures`, `clusterCollector`), `Cluster.reloadFailed`, `tenantlabel.{Acquire,Release,Register}`, `sqliteconfig.QueryOnly`, `openDB(..., readOnly)`, `internal/proxyproto` (`Listener`, `Conn`, `ParseTrusted`), `internal/opsmetrics` (`Handler`, `Serve`, `ParseOperatorCIDRs`, `IsOperatorIP`), `placementapi.{Revocations,LoadRevocations,ErrRevoked,ErrRevocationUnavailable}`, `Authorizer.WithRevocations`.

## 2. Design choice

| Mục | Lựa chọn |
|---|---|
| Metric lease | collector đọc `Manager` đang chạy lúc scrape (held/epoch/deadline theo `{cluster,holder}`), process dừng thì series biến mất; counter sự kiện + histogram renew theo `{cluster}` |
| Fence | counter `{reason}` cố định (`no_fence`, `owner`, `not_active`, `lease`, `read_error`, `bound_error`), không nhãn tenant |
| Pool | `sql.DB.Stats()` (không query DB), cộng theo tên pool |
| L4 | nhãn cấp khi `State` được dựng (`NewStateWithDB`: chỉ cho tailnet được nạp dưới lease), trả trong `State.Close`, đếm tham chiếu; `Label` chỉ tra; trả lần cuối xóa series của tailnet |
| L9 | revision chỉ ghi khi reload thành công; lỗi → counter + `reload_pending`, log Error một lần mỗi revision, thử lại mỗi vòng reconcile (áp cho policy và DNS) |
| L3 | `_pragma=query_only=1` trong DSN (mọi kết nối), không pragma ghi file ở handle chỉ đọc |
| L5 | file thu hồi (serial/sha256), nạp lại theo chu kỳ; lỗi → fail closed (503 mọi request) |
| L10 `.After` | khai báo chuỗi `begin_transaction < scope < fence < op` |
| B7 | parser PROXY v2 tự viết (không đổi `go.mod`/vendor hash); nguồn tin cậy bắt buộc header, nguồn khác gửi header bị đóng; mặc định 10/s burst 20 theo địa chỉ (IPv6 /64) khi bật |
| Listener metric 2 binary | `internal/opsmetrics`, cùng quy tắc operator với cluster (`hscontrol.isOperatorIP` giờ gọi chung hàm) |

## 3. Tests + exact commands + exit codes (mc-ci, `~/src/p13f`, PG tạm `initdb` unix socket port 5442 như CI, `MESH_TEST_REQUIRE_POSTGRES=1`)

### RED (commit `b504d4e`, trước code)

| Lệnh | Exit | Kết quả |
|---|---|---|
| `go test -count=1 -p 4 -run TestLeaseMetrics ./internal/lease/` | 1 | `headscale_cluster_lease_held{cluster=metrics-c1,holder=proc-a} = 0 (present false)`; `self_fence events = 0` |
| `go test -count=1 -p 4 -run 'TestFenceRefusalsCountedByReason\|TestFenceCallbacksDeclareTheirOrder' ./hscontrol/db/tenantscope/` | 1 | 4 lý do: `grew by 0`; `tenantscope:fence_create declares After("gorm:begin_transaction"), want After("tenantscope:create")` (×3 op); `tenantscope:create declares After("")` (thứ tự hiệu lực đúng nhờ thứ tự đăng ký) |
| `go test -count=1 -p 4 -run 'TestOpenReadOnlySQLiteEveryConnectionQueryOnly\|…' ./hscontrol/db/` | 1 | L3: `write 0 through the read-only handle succeeded` (kết nối mới không có `query_only`) |
| `go test -count=1 -p 4 -run TestB7 ./internal/bootstrap/` | 1 | `expected: 200, actual: 429` — tenant ở địa chỉ khác bị 429 sau flood 1000 req từ một địa chỉ |
| `go test -tags p05cluster -count=1 -p 4 -run TestP13f ./hscontrol/servertest/` | 1 | metric cluster: `Condition never satisfied … []`; L4: `Max difference between 3 and 4 … requests for a tailnet this process does not serve took a label` và series `headscale_tailnet_nodes{tailnet=<A>}` còn sau khi retire; L9: counter không có |
| như trên, biến thể hành vi L9 (thay chờ metric bằng `time.Sleep(6s)`, không commit) | 1 | `WaitForCondition(l9-a1, "deny-all from the retried reload"): timeout after 20s` — revision lỗi đã bị ghi là đã áp, không thử lại |

### GREEN

| Lệnh | Exit | Kết quả |
|---|---|---|
| `CGO_ENABLED=1 go test -race -count=1 -p 4 ./internal/lease/ ./internal/tenantlabel/ ./internal/dbstats/ ./internal/opsmetrics/ ./internal/proxyproto/ ./internal/bootstrap/ ./internal/placement/placementapi/ ./hscontrol/db/tenantscope/ ./hscontrol/db/sqliteconfig/` | 0 | 9 gói ok |
| `CGO_ENABLED=1 go test -race -count=1 -p 4 -timeout 30m ./hscontrol/db/` | 0 | ok 166.7 s (SQLite + PostgreSQL) |
| `go test -tags p05cluster -count=1 -p 4 -v -run TestP13f ./hscontrol/servertest/` | 0 | `TestP13fClusterMetrics` 3.35 s, `TestP13fTailnetLabelsOnlyForServedTailnets` 2.17 s, `TestP13fPolicyReloadFailureRetried` 8.68 s |
| `go test -tags p05cluster -count=1 -p 4 -run 'TestP08b\|TestClusterEmptyPolicyReloadDenies\|TestP06\|TestP11b\|TestP12c\|TestClusterTwoTailnetsPeersIsolated' ./hscontrol/servertest/` | 0 | ok 213.5 s (tập con liên quan; **không** chạy cả bộ p05cluster theo chỉ dẫn) |
| `CGO_ENABLED=1 go test -race -count=1 -p 4 -timeout 45m ./hscontrol/state/ ./hscontrol/ ./cmd/... ./internal/... ./hscontrol/db/...` (tại cây của `bfd2f5b`) | 0 | 22 gói ok (state 600 s, hscontrol 514 s, db 370 s, placement 181 s, …) |
| Sau sửa lint (`09371ff`): race lại 8 gói đã đổi, `-tags p05cluster -run TestP13f`, `go test -run "TestP12c|TestP08|TestEndpointInventory|TestClusterNever" ./hscontrol/` | 0 | ok |
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --timeout 20m --max-issues-per-linter 0 --max-same-issues 0 ./internal/... ./cmd/mesh-bootstrap/... ./cmd/mesh-placement/... ./hscontrol/ ./hscontrol/db/... ./hscontrol/state/...` | xem §3.1 | lần 1: 35 issue (wsl, zerologlint, …) → sửa ở `09371ff`; lần 2: 1 (wsl) → sửa ở commit cuối |
| `infra/lab/monitoring/check.sh` | 0 | `promtool check config` SUCCESS; `check rules` 34 rules; `promtool test rules` SUCCESS (22 ca bắn + ca không bắn); `amtool check-config` SUCCESS; compose: plugin không có trên mc-ci → chỉ kiểm YAML; dashboard JSON hợp lệ |

### 3.1 Lint

Kết quả lần chạy cuối ghi ở đây (commit cuối của nhánh): LINT_RESULT

Thử nghiệm L10 (bỏ): đổi `createDefaultTailnet` sang PENDING + `{"grants":[]}` → `go test -race ./hscontrol/state/ ./hscontrol/` exit 1: `TestReloadPolicyEmpty`, `TestReloadPolicyRebuildsPeerMapOnChangeOnly`, `TestAPIV1PolicyGet`, `TestAPIv2ACLDefaultWhenUnset`, `TestAPIv2ACLContentNegotiation`, `TestAPIv2ACLSetBodyLimited`, `TestAPIv2ACLIfMatchTsDefault` (và `TestP08bNodeStoreGaugeIsNotLastWriter` do API nhãn, đã sửa: `NodeStore.SetMetricsTailnet` nhận nhãn đã cấp).

## 4. Negative tenant isolation tests

- L4: request `/api/v2` với key của tailnet thuộc cluster khác → 404 và **không** cấp nhãn, không series mang ID đó (`TestP13fTailnetLabelsOnlyForServedTailnets`); `TestLabelNeverAdmits`.
- Metric chỉ trên listener operator: `TestOperatorsOnly` (opsmetrics: dải Tailscale, `X-Forwarded-For`, RFC1918 ngoài CIDR → 403, không lộ `# HELP`), `TestP08ClusterMetricsOperatorsOnly` (không đổi, vẫn xanh).
- B7 chống giả mạo: `TestUntrustedPeerCannotSendHeader`, `TestB7ProxyV2Spoofing` (peer không tin cậy gửi header → đóng, địa chỉ giả không lọt), `TestTrustedPeerWithoutValidHeaderRefused` (8 dạng header sai/thiếu).
- L5: chứng chỉ bị thu hồi → 403, file hỏng/mất → 503 cho mọi operator (`TestOperatorCertificateRevocation`).
- L10: `TestClusterNeverCreatesDefaultTailnet` (AST: mã cluster không gọi `NewHeadscaleDatabase`/`NewState`/`createDefaultTailnet`), `TestDefaultTailnetOnlyFromSingleTenantOpener` (OpenPlatform không tạo tailnet; `TenantDB` tailnet lạ lỗi).

## 5. DB migrations and downgrade constraints

Không migration. Không đổi schema. L3 chỉ đổi DSN của handle chỉ đọc.

## 6. Config keys

Không thêm khóa YAML (không đụng `types/config.go`, `tools/configgen`). Cờ mới: `mesh-bootstrap -metrics-listen -metrics-operator-cidrs -proxy-protocol-from -proxy-header-timeout` (`-ip-rate` mặc định đổi thành `-1` = tự: 10/s khi bật PROXY v2, 0 khi tắt); `mesh-placement -metrics-listen -metrics-operator-cidrs -revoked-certs -revoked-certs-reload`.

## 7. Artifact (không áp dụng)

- `infra/lab/monitoring/` (compose, Prometheus, rules + tests, Alertmanager, Grafana, exporter, script pgBackRest, `check.sh`, README).
- `infra/lab/pgbouncer/` (alias `mesh_c1`, `mesh_c2`, `mesh_cN_lease`, `mesh_ops`; công thức `max_connections`).
- `infra/lab/vps-haproxy/` (`haproxy.cfg`, `mesh-lab-haproxy.service`, `apply.sh`, `rollback.sh`, README với diff chính xác trên VPS).

## 8. Known limitations / security blockers

1. **Lệch so với chỉ dẫn (L10 `createDefaultTailnet`):** không đổi sang PENDING + `{"grants":[]}`. Thử nghiệm (đã làm rồi bỏ) làm hỏng 3 test state và 5 test API tương thích upstream (`TestReloadPolicyEmpty`, `TestReloadPolicyRebuildsPeerMapOnChangeOnly`, `TestAPIV1PolicyGet`, `TestAPIv2ACLDefaultWhenUnset`, `TestAPIv2ACLContentNegotiation`, `TestAPIv2ACLSetBodyLimited`, `TestAPIv2ACLIfMatchTsDefault`, …): ở single-tenant `{"grants":[]}` (grants rỗng, khác nil) là **deny-all**, đổi hành vi server Headscale mới tạo. Thay vào đó theo kế hoạch trong `REVIEW-2026-11-26-tickets.md` L10: chứng minh không có đường gọi ở cluster mode (test AST + test platform) và ghi chú trong code. Cần chủ dự án quyết nếu muốn đổi hành vi single-tenant.
2. PROXY v2 chỉ cho bootstrap; cluster listener (8443/8445) chưa nhận → per-IP cho `/ts2021`, `/key`, `/verify` để ticket sau. HAProxy VPS gửi PROXY v2 chỉ trên 8444.
3. Sau khi áp HAProxy VPS, mọi harness khởi động `mesh-bootstrap` trên `10.99.77.2:8444` phải thêm `-proxy-protocol-from 10.99.77.1/32` (chưa sửa harness; không chạy E2E).
4. Flood từ **nhiều** địa chỉ vẫn có thể cạn bucket toàn cục (bản chất của bucket toàn cục); per-IP chặn nguồn đơn lẻ.
5. Runtime bị rò (drain quá hạn) giữ nhãn tailnet tới khi process dừng (có giới hạn).
6. Chuỗi kết nối pgbouncer_exporter nằm trong argv container (exporter v0.10.2 không nhận env) — user `stats_users` chỉ đọc thống kê, trên VM lab.
7. Dashboard/alert chưa chạy với dữ liệu thật; ngưỡng (renew p99 1 s, fence 0.2/s, lag 16 MiB) cần chỉnh sau drill P13a–e.
8. Go race unit tests không chứng minh hành vi qua HAProxy/VPS thật: cần E2E sau khi áp.

## 9. Reviewer approval

Chưa.
