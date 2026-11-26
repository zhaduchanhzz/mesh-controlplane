# Source Inventory — P13f (giám sát, giới hạn, B7, L3/L4/L5/L9/L10)

- **Commit gốc:** `c4533aa` (`feat/P13-plan`, chứa P12c), nhánh `feat/P13f-observability`, ngày 2026-11-26.
- **ID:** P13f code (metric lease/fence/pool/reconcile/runtime, listener metric cho `mesh-bootstrap`/`mesh-placement`), B7 (PROXY v2 cho bootstrap), L3, L4, L5, L9, L10 (phần còn lại); artifact cấu hình (Prometheus/Alertmanager/Grafana trên mc-db, alias PgBouncer theo cluster, HAProxy riêng trên VPS cho 8443–8445) — **không áp dụng**.
- **Quyết định chủ dự án (2026-11-26):** D9a (PROXY v2, **chỉ** thay forwarder cổng lab 8443–8445 trên VPS bằng một HAProxy riêng; HAProxy 80/443, stund 3478, postfix/opendkim, WireGuard 51820 không đổi; diff trình trước khi áp), D9b (alias PgBouncer theo cluster), D10 (monitoring là container trên mc-db).
- **Trạng thái:** `Planned` (inventory trước code, AGENTS rule 5).
- **Migration:** không cần (không cột/bảng mới). **Config key YAML:** không thêm; tham số mới là cờ dòng lệnh của `mesh-bootstrap`/`mesh-placement` (§4).

## 1. Đã đọc

| Nguồn | Path / symbol | Nội dung liên quan |
|---|---|---|
| Chính sách | `AGENTS.md` rule 2, 3, 5, 9, 11, 13 | không secret; không đụng lab/VPS (rule 3: firewall thật/Internet cần người duyệt); listener metric/debug không bao giờ public; lỗi isolation = BLOCK |
| Ticket | `tickets/P13.md` §P13f, §Quyết định D9/D9b/D10, §Rủi ro 8 | phạm vi code/lab, cảnh báo tối thiểu, RED bắt buộc (lease/fence/pool, L4, L9, B7) |
| Review | `evidence/REVIEW-2026-11-26-tickets.md` B7, L3, L4, L5, L9, L10 | vị trí và kịch bản |
| Inventory | `evidence/source-inventory-P13.md` §2.1, §2.2, §2.5, §2.6 | lease/fence; một pool cho cả lease và dữ liệu; rate limit bootstrap; chưa có metric lease/fence/pool |
| Endpoint | `evidence/P08a-endpoint-inventory.md` §8, `hscontrol/endpoint_inventory_test.go`, `hscontrol/testdata/p08a_endpoint_allowlist.txt` | listener metric cluster chỉ `GET /metrics` sau `operatorOnly`; binary riêng (`mesh-bootstrap`, `mesh-placement`) không nằm trong allowlist chi → ghi thêm dòng §8 |
| Cluster | `hscontrol/cluster.go`: `NewCluster`, `serveTailnet`, `load`, `reconcile` (`:428`, vòng policy `:499-516`, DNS `:479-497`), `retire` (`:623`), `stopRuntime` (`:848`), `MetricsHandler` (`:1328`), `Serve` (`:1377`), `onLeaseAcquired`/`onLeaseLost` | L9: `rt.policyRev = rev` ghi **trước** `ReloadPolicy`, lỗi chỉ log; DNS cùng mẫu. Metric: `promhttp.Handler()` (registry mặc định) |
| Metric | `hscontrol/metrics.go` (`tailnetMapSent`, `tailnetMapSessions`), `hscontrol/cluster_apiv2.go` (`clusterAPIRefused`, `tailnetAPIRequests`, `APIv2Handler` `:191-270`), `hscontrol/poll.go:186,464`, `hscontrol/state/node_store.go` (`tailnetNodes`, `SetMetricsTailnet` `:573`), `hscontrol/state/node_store_reconcile.go:31` (`tailnetNetmapDivergence`) | L4: `tenantlabel.Label` cấp nhãn ở lần dùng đầu, kể cả trong `APIv2Handler` **trước** khi kiểm runtime (`:239`), không bao giờ trả |
| Nhãn | `internal/tenantlabel/tenantlabel.go` (`Label`, `SetLimit`, `admitted`, gauge `tailnet_metric_labels`, counter overflow) | không có Release; không xóa series |
| Operator | `hscontrol/operator_access.go` (`operatorOnly`, `peerIP`, `isOperatorIP`), `hscontrol/config/validate.go:808` (từ chối CIDR chồng dải Tailscale) | quy tắc dùng lại cho listener metric của hai binary |
| Lease | `internal/lease/manager.go` (`Manager.Run/acquire/hold/release`, `Current`, `deadline`), `internal/lease/lease.go` (`Store`, `ErrHeld`, `ErrLost`), `internal/lease/lease_test.go` (fake store `stuckStore`, `newManager`) | không có metric; deadline đơn điệu; lý do mất lease là chuỗi trong `hold` |
| Fence | `hscontrol/db/tenantscope/tenantscope.go`: `Register` (`:79-97`), `checkFence` (`:458`), `checkLease` (`:535`), `boundTransaction` (`:614`) | mọi lỗi fence là `ErrFenced` bọc chuỗi; `ErrNoFence` riêng; L10: `.After(name+":create").After("gorm:begin_transaction")` — GORM `Callback.After` chỉ giữ một tên (cái cuối) |
| GORM | `gorm.io/gorm@v1.31.2/callbacks.go` (`callback.after string`, `sortCallbacks`) | ràng buộc "sau stamp" bị mất; thứ tự đúng hiện nay chỉ nhờ thứ tự đăng ký |
| DB | `hscontrol/db/db.go` `openDB` (`:91`), `NewHeadscaleDatabase`; `hscontrol/db/platform.go` (`Platform`, `OpenPlatform`, `Ping`, `Close`); `hscontrol/db/readonly.go` `OpenReadOnly` | L3: `PRAGMA query_only = ON` chạy một lần trên một kết nối (`:61`); `openDB` SQLite `MaxOpen=1`, `ConnMaxIdleTime=1h` → kết nối thay thế mất pragma |
| SQLite DSN | `hscontrol/db/sqliteconfig/config.go` (`Config.ReadOnly` → `_pragma=query_only=1` + `mode=ro`, `ToURL`) | `_pragma` của driver modernc chạy trên **mỗi** kết nối mới |
| Default tailnet | `hscontrol/db/tenant.go` `resolveRuntimeTailnet`/`createDefaultTailnet` (`:233-300`); `hscontrol/db/platform_admin.go` `CreateTailnet` (PENDING + `{"grants":[]}` cùng transaction); `hscontrol/servertest/cluster_p05.go:308` (seed dùng tailnet mặc định) | L10: `createDefaultTailnet` chèn ACTIVE, không policy, không transaction; gọi qua `NewHeadscaleDatabase` (đường single-tenant). Cluster mode dùng `Platform.TenantDB`, không gọi `NewHeadscaleDatabase` (servertest có gọi để seed) |
| Policy | `hscontrol/state/state.go` `ReloadPolicy` (`:650`), `reloadPolicyLocked` | `{"grants":[]}` ở single-tenant (không `PolicyEmptyDenies`) cho filter như "không policy" của upstream → đổi `createDefaultTailnet` không đổi hành vi single-tenant [I, kiểm bằng test hiện có] |
| Bootstrap | `internal/bootstrap/http.go` (`HandlerConfig`, `DefaultHandlerConfig` IPRate 0, `bootstrap` (IP bucket trước global), `clientIP` = `RemoteAddr`, `buckets` max 100k key), `cmd/mesh-bootstrap/main.go` (`ListenAndServeTLS`, `-ip-rate` mặc định 0) | B7: flood > 200 req/s làm mọi tenant 429; IP thật bị forwarder che |
| PROXY | `go.mod:213` `github.com/pires/go-proxyproto v0.15.0 // indirect` | dùng thư viện này đổi `go.mod` (indirect → direct) và `flakehashes.json` goModSum → tự viết parser v2 tối thiểu trong `internal/proxyproto` (spec HAProxy PROXY protocol v2 §2.2) |
| Placement | `cmd/mesh-placement/main.go`, `internal/placement/placementapi/auth.go` (`Authorizer.Wrap`, `TLSConfig` `VerifyClientCertIfGiven`), `placementapi/api_test.go` (`newCA`, `issue`, `newEnv`) | L5: không thu hồi; không metric, không `/health` |
| Lab | `infra/lab/README.md` §Public test endpoint, `infra/lab/stage2a-public-path.md`, `infra/lab/e2e/{p12,p12b,twocluster}/run.sh` (BOOT_LISTEN `10.99.77.2:8444`) | VPS: `mesh-cp-proxy` (8443 → 10.99.77.2:8443), `mesh-boot-proxy` (8444), `mesh-c2-proxy` (8445), `systemd-socket-proxyd --connections-max=512`; nft `inet mg_vps` đã mở 8443–8445 |
| Lab | `infra/lab/db/setup-db.sh`, `evidence/source-inventory-P13.md` §3.3 | PG 16 + PgBouncer 1.24.1 container trên mc-db; `pool_mode=transaction`, `default_pool_size=20`, `max_connections=200` |

## 2. Symbol sẽ thêm/đổi (dự kiến)

| Mục | Chỗ | Thay đổi |
|---|---|---|
| Lease metric | `internal/lease/metrics.go` (mới) | collector toàn cục trên các `Manager` đang chạy: `headscale_cluster_lease_held{cluster,holder}`, `_epoch`, `_deadline_seconds`; `headscale_cluster_lease_renew_duration_seconds{cluster}` (histogram), `headscale_cluster_lease_events_total{cluster,event}` |
| Fence metric | `tenantscope` | `headscale_tenant_fence_refusals_total{reason}`: `no_fence`, `owner`, `not_active`, `lease`, `read_error`, `bound_error` |
| Pool | `internal/dbstats` (mới) + `Platform` | `headscale_db_pool_*{pool}` từ `sql.DBStats` (open, in_use, idle, max_open, wait_count, wait_duration); cộng theo `pool` nếu nhiều handle cùng tên (test) |
| Reconcile/runtime | `hscontrol/cluster.go`, `hscontrol/cluster_metrics.go` (mới) | `headscale_cluster_reconcile_duration_seconds`, `headscale_cluster_tailnet_runtimes{cluster,holder,state}` (`serving`, `stopping`, `reload_pending`) |
| L9 | `reconcile` | revision chỉ ghi khi reload thành công; lỗi → thử lại mỗi vòng, log Error một lần mỗi revision; `headscale_tailnet_reload_failures_total{tailnet,kind}` (policy, dns) |
| L4 | `tenantlabel` | `Acquire`/`Release` đếm tham chiếu; `Label` chỉ tra (không cấp); `Release` về 0 xóa series của tailnet trong các vec đã đăng ký. Cấp trong `NewStateWithDB` (chỉ khi runtime được dựng: tailnet sở hữu, ACTIVE, dưới lease), trả trong `State.Close` |
| L3 | `openDB(..., readOnly)`, `sqliteconfig.Config.QueryOnly` | DSN `_pragma=busy_timeout`, `_pragma=query_only=1` cho mọi kết nối; không pragma ghi (journal_mode, auto_vacuum) ở handle chỉ đọc |
| L10 | `tenantscope.Register` | stamp/scope `After("gorm:begin_transaction")`, fence `After(name+":create")` → begin < scope < fence < gorm op, bằng ràng buộc bắc cầu |
| L10 | `createDefaultTailnet` | một transaction: PENDING + `{"grants":[]}` rồi ACTIVE |
| L5 | `placementapi/revocation.go` (mới) | file thu hồi (`serial:<hex>`, `sha256:<hex>`), nạp lại theo mtime; lỗi đọc/parse → **fail closed** (503 cho mọi request) tới khi file hợp lệ |
| B7 | `internal/proxyproto` (mới), `cmd/mesh-bootstrap` | listener PROXY v2: nguồn tin cậy **phải** gửi header v2 (thiếu/sai → đóng); nguồn không tin cậy gửi header → đóng; `DefaultHandlerConfig` IPRate 10/s burst 20; IPv6 gom theo /64 |
| Listener metric | `internal/opsmetrics` (mới) | `GET /metrics`, `GET /health` (chỉ placement có DB ping; bootstrap cũng ping DB chỉ đọc), chỉ loopback hoặc CIDR operator; dải Tailscale bị từ chối cả khi cấu hình (giống `config/validate.go:808`) |

## 3. Ràng buộc hiện có

- AGENTS 9: listener public không có `/metrics`/debug; listener metric riêng, loopback hoặc CIDR operator; `X-Forwarded-For` không bao giờ được đọc. Listener metric **không** nhận PROXY protocol.
- AGENTS 13: tailnet ID là định danh dữ liệu tenant → chỉ trên listener operator (đã có). Không slug, không node key, không IP client trong nhãn (bootstrap không ghi IP vào nhãn hay log).
- PgBouncer transaction mode: không `SET` phiên, không `LISTEN` (ADR 0006). Metric pool đọc `sql.DB.Stats()` phía Go, không query DB.
- Lease: một hàng mỗi cluster, thời gian theo đồng hồ DB; metric `deadline_seconds` theo đồng hồ đơn điệu của process (không so với DB).
- Không đổi `go.mod`/`flakehashes.json` (thêm dependency trực tiếp đổi goModSum của nix).
- File ticket khác chạm cùng chỗ (P13b, P13c: `types/config.go`, `internal/lease`): P13f chỉ **thêm** file `internal/lease/metrics.go` + vài dòng gọi trong `manager.go`; không thêm config YAML.
- Không đụng lab, VPS; không chạy E2E. Artifact cấu hình chỉ là file + script.

## 4. Tham số mới (cờ, không YAML)

| Binary | Cờ | Mặc định | Ý nghĩa |
|---|---|---|---|
| `mesh-bootstrap` | `-metrics-listen` | `""` (tắt) | listener metric operator (`/metrics`, `/health`) |
| `mesh-bootstrap` | `-metrics-operator-cidrs` | `""` (chỉ loopback) | CIDR operator; dải Tailscale bị từ chối |
| `mesh-bootstrap` | `-proxy-protocol-from` | `""` (tắt) | CIDR proxy tin cậy (VPS HAProxy 10.99.77.1/32); bật PROXY v2 |
| `mesh-bootstrap` | `-proxy-header-timeout` | `5s` | thời gian chờ header |
| `mesh-bootstrap` | `-ip-rate` | `-1` = tự: 10/s khi bật PROXY v2, 0 khi không (forwarder che IP) | |
| `mesh-placement` | `-metrics-listen`, `-metrics-operator-cidrs` | tắt / chỉ loopback | như trên; `/health` ping DB |
| `mesh-placement` | `-revoked-certs` | `""` | file thu hồi chứng chỉ operator |
| `mesh-placement` | `-revoked-certs-reload` | `10s` | chu kỳ kiểm mtime |

## 5. Giả định còn lại

1. Unit `mesh-boot-proxy.*`, `mesh-c2-proxy.*` trên VPS có tên như suy ra từ `stage2a-public-path.md` (chỉ `mesh-cp-proxy` có trong README). Script apply kiểm sự tồn tại và dừng nếu khác.
2. VPS có binary `haproxy` (MeshGuard `mg-l4proxy` dùng HAProxy) đủ mới cho `send-proxy-v2` (≥ 1.5). Chưa đọc trên VPS (không được đụng); script apply kiểm `haproxy -v` và dừng nếu thiếu.
3. Cluster listener (8443 c1, 8445 c2) **chưa** nhận PROXY v2 trong P13f: HAProxy VPS chỉ gửi PROXY v2 cho 8444 (bootstrap); 8443/8445 là TCP thuần như hiện nay. Per-IP cho `/ts2021`, `/key`, `/verify` để ticket sau (ghi rủi ro).
4. Harness E2E khởi động `mesh-bootstrap` trên `10.99.77.2:8444` không có `-proxy-protocol-from`: sau khi áp HAProxy VPS, harness phải truyền `-proxy-protocol-from 10.99.77.1/32` (biến `BOOT_PROXY_FROM`), nếu không TLS của bootstrap hỏng (header PROXY trước ClientHello).
5. Phiên bản image monitoring (Prometheus, Alertmanager, Grafana, exporter) tồn tại trên registry; kiểm bằng `docker pull` + `promtool`/`amtool check` trên mc-ci.
