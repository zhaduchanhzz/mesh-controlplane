# PR P08 Evidence — Endpoint hardening (debug/metrics, /verify, DERP verify, cluster listener)

- **Repo commit before / after:** `f74ae64` (P08 WIP, chồng trên `feat/P10-placement-fencing` @ `cc435f3` và `feat/P08a-endpoint-audit`) → `2e8b896` (test), `2862d4a` (cluster), `809fa72` (E2E harness), `6221aa8` (TestConfigFields), commit evidence (commit chứa file này).
- **Phụ thuộc merge:** nhánh chứa toàn bộ P10, nên **P10 phải được review và merge trước**. Sau đó rebase P08 lên master.
- **Ticket / ADR:** P08a `evidence/P08a-endpoint-inventory.md` (F1–F6, N1–N8, hợp đồng §5); ADR 0005, ADR 0007. Không có ADR mới, không đổi thiết kế Noise (AGENTS rule 12).
- **Máy:** code sửa trên Windows; build/test trên `mc-ci` (Go 1.27.4, `~/src/p08`, code đưa sang bằng `git bundle`); E2E: client `tailscale/tailscale:v1.104.7` trên mc-ci → VPS 116.118.3.46 → WireGuard → mc-cp.

## 1. Path và symbol đã đọc / sửa

| Path | Symbol | Thay đổi P08 |
|---|---|---|
| `hscontrol/operator_access.go` (mới, WIP) | `operatorOnly`, `isOperatorIP`, `peerIP` | Gate toàn bộ listener debug/metrics: loopback hoặc `metrics_operator_cidrs`; dải Tailscale không bao giờ là operator; bỏ `X-Forwarded-For` |
| `hscontrol/debug.go`, `hscontrol/app.go` | `debugHTTPServer`, `protectedDebugHandler` (xóa), `readVerifySecret`, `DERPVerifyURL` | Listener debug sau `operatorOnly`; secret `/verify`; URL verify DERP riêng cho mỗi runtime |
| `hscontrol/config/validate.go`, `hscontrol/types/config.go` | `metrics_operator_cidrs`, `derp.verify_secret_path` | Từ chối CIDR `/0` và CIDR chồng dải Tailscale |
| `hscontrol/handlers.go` | `VerifyHandler`, `handleVerifyRequest`, `verifyCallerAuthenticated` | Secret bắt buộc (constant-time); node hết hạn không được admit |
| `hscontrol/derp/server/derp_server.go` | `RegisterVerifyHandler`, `verifyDispatcher`, `NewDERPServer(verifyURL)` | Scheme đăng ký một lần, mỗi runtime một host ngẫu nhiên; URL lạ → lỗi → DERP từ chối |
| `hscontrol/noise.go` | `SSHActionHandler`, `sshDstRefused` | dst không tồn tại và dst của máy khác trả cùng 401 (N5) |
| `hscontrol/cluster.go` | `clusterHost`, `Cluster.hostGuard`, `misdirectedCluster`, `Cluster.MetricsHandler`, `validateClusterConfig`, `Cluster.Serve` | Guard Host/SNI (421); `server_url` phải là hostname; listener metrics riêng (chỉ `/metrics`, operator-only); từ chối tailsql (N4) |
| `hscontrol/servertest/cluster_p05.go` | `NewTestCluster` | `server_url` = `http://localhost:<port>` (listener vẫn 127.0.0.1) |
| `hscontrol/endpoint_inventory_test.go`, `hscontrol/testdata/p08a_endpoint_allowlist.txt` | `clusterRoutes`, `noiseRouterFunc`, `publicAuth`, `debugRoutes` | Thêm route listener cluster; nhãn `operator-only`, `verify-secret`; router Noise đọc từ `serveNoiseSession` |
| `infra/lab/e2e/p05/run.sh` | `t_http` | C01 không còn gọi bằng hostname khác; thêm C03 (421) và C04 (metrics loopback) |
| Sinh lại | `go generate ./hscontrol/types/` | `docs/ref/config-example.yaml`, `docs/ref/configuration-options.md`, `config_docs_generated.go`, `nix/options.generated.nix`. Golden config: `HEADSCALE_UPDATE_GOLDEN=1 go test ./hscontrol/config/` exit 0, không đổi |

## 2. Phát hiện P08a → trạng thái

| ID | Nội dung | Trạng thái P08 | Test |
|---|---|---|---|
| F1, N1, N3 | statsviz cho mọi IP private; `/debug/*` cho dải Tailscale; `/metrics` cho mọi peer | **Đã sửa** | `TestP08aDebugListenerRefusesNonOperator`, `TestMetricsRouterIsolation`, `TestP08ClusterMetricsOperatorsOnly` |
| F2 | `/verify` public không auth, oracle node key, admit node hết hạn | **Đã sửa** | `TestP08aVerifyRejectsUnauthenticatedPublicCaller`, `TestP08aVerifyRefusesUnknownExpiredRevoked`, `TestVerifyHandler_SuccessSetsJSONContentType`, `TestHandleVerifyRequest_AdmitsByNodeKey` |
| F3, N2 | transport verify DERP toàn process, một URL cố định | **Đã sửa** | `TestP08aDERPVerifyTwoTailnetsOneProcess` |
| N4 | tailsql lộ DB | **Đã sửa cho cluster mode** (từ chối khi khởi động) | `TestP08ClusterConfigRefusesTailSQL` |
| N5 | SSH action là oracle node ID | **Đã sửa** | `TestSSHActionHandler_RejectsUnknownDst` |
| S11c | listener cluster nhận Host/SNI bất kỳ | **Đã sửa** | `TestP08ClusterHostGuard`, `TestP08ClusterServerURLMustBeHostname`, E2E C03 |
| F4 | API admin v2 chưa scope theo tailnet | **Chưa — P08b.** Cluster mode không mở API admin nào (fail closed, E2E C02) | `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` vẫn **SKIP — không phải PASS** |
| F5, F6, N6–N8 | unix socket local trust; ping ID; reverse proxy trước listener debug; openapi công khai | Không đổi trong P08 (đã allowlist / runbook) | — |

## 3. Test (mc-ci, Go 1.27.4)

| Lệnh | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go build ./...`; `go vet ./hscontrol/ ./hscontrol/derp/... ./hscontrol/endpointaudit/ ./hscontrol/config/` | `f74ae64` | 0 | mốc |
| `go test -race -count=1 -run "P08a\|EndpointInventory\|Verify\|SSHAction\|DERP\|Debug\|Metrics" ./hscontrol/ ./hscontrol/endpointaudit/ ./hscontrol/derp/... ./hscontrol/config/` | `f74ae64` | **1** (mốc) | FAIL: `TestEndpointInventory` (router Noise đã chuyển sang `serveNoiseSession`), `TestVerifyHandler_SuccessSetsJSONContentType` (403, thiếu secret), `TestSSHActionHandler_RejectsUnknownDst` (401 thay 404), `TestMetricsRouterIsolation` (403 cho peer không phải operator), `TestHandleVerifyRequest_AdmitsByNodeKey/expired`, `TestP08aDERPVerifyTwoTailnetsOneProcess` (test còn gọi URL cố định cũ). Bốn test RED P08a khác: 3 đã GREEN, 1 SKIP |
| `go test -race -count=1 ./hscontrol/ ./hscontrol/endpointaudit/ ./hscontrol/derp/... ./hscontrol/config/` | `2e8b896` | **0** | `hscontrol` 562 s |
| `go test -race -count=1 -run TestP08Cluster ./hscontrol/` | `2862d4a` | 0 | |
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | `2862d4a` | **0** | 124 s; servertest dùng `localhost` |
| `go test -count=1 -run TestEndpointInventory ./hscontrol/` | `2862d4a` | 0 | Đối chứng âm: trước khi thêm 11 dòng `cluster …` vào allowlist → exit 1, liệt kê đúng 11 route "new or changed (not in allowlist)" |
| `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -race -count=1 -p 6 -timeout 60m ./hscontrol/... ./internal/... ./cmd/...` | `809fa72` | **1** | 30 package ok, 2 FAIL. (a) `hscontrol/types` `TestConfigFields`: danh sách kỳ vọng thiếu 2 trường mới của P08 (`metrics_operator_cidrs`, `derp.verify_secret_path`), lỗi của P08, đã sửa ở `6221aa8`. (b) `hscontrol/servertest` `TestReloginFollowsNewHostname` (`lifecycle_test.go:735`: tên vẫn là "second" sau 10 s): chạy cùng lúc với 2 lần E2E trên cùng mc-ci; P08 không đụng đường đổi hostname. Xem 3 dòng dưới |
| `go test -race -count=1 ./hscontrol/types/` | `6221aa8` | 0 | |
| `go test -race -count=20 -run '^TestReloginFollowsNewHostname$' ./hscontrol/servertest/` | `809fa72` | 0 | 20/20 |
| `nix shell … -c go test -race -count=1 -timeout 60m ./hscontrol/servertest/` (chạy riêng, không có E2E song song) | `809fa72` | **0** | 2023 s. Kết luận: (b) là flake khi máy quá tải, ghi lại để theo dõi; không phải hồi quy của P08 |

## 4. E2E client thật (`infra/lab/e2e/p05/run.sh`, binary build từ `809fa72` trên mc-ci, sha256 `94143fc3…8082`)

- **Lệnh** (Windows Git Bash, `python3` = Python 3.12 qua shim trong scratchpad): `SSH_KEY=~/.mesh-lab/mesh-lab BIN=<binary> bash infra/lab/e2e/p05/run.sh`.
- **Lần 1 (`e2e-20261123T032657Z`):** setup lỗi ở bước đọc auth key, do shim `python3` chưa vào PATH (`Python was not found`); không chạy ca nào; cleanup OK, POC trả `/health` 200. Lỗi môi trường chạy, không phải lỗi code.
- **Lần 2 (`artifacts/809fa72/P05/e2e-20261123T032723Z/`):** exit 0, 129 s, **15/15 PASS**, `cleanup_ok=1`.
- **Lần 3 (`artifacts/809fa72/P05/e2e-20261123T032945Z/`):** exit 0, 137 s, **15/15 PASS**, `cleanup_ok=1`.

| ID | Kết quả (cả hai lần) | Nội dung |
|---|---|---|
| C01 | PASS | Một Noise key cho cluster ở `c1.cp.gta5web.online` |
| C02 | PASS | Listener public không có API admin, `/metrics`, `/debug/`, `/swagger`, `/register/…`, `/oidc/callback` |
| **C03 (mới)** | PASS | Chỉ `c1.cp…` được phục vụ. Hostname khác (`a.cp…` qua `--connect-to`, `/health` và `/key`), IP thô `116.118.3.46:8443` (không SNI), SNI=c1/Host=a và SNI=a/Host=c1 đều nhận **421**; đối chứng `$URL/health` 200 |
| **C04 (mới)** | PASS | Metrics cluster `127.0.0.1:9095`: `/metrics` 200 từ loopback trên mc-cp, `/debug/` 404, từ LAN (mc-ci → 192.168.1.241:9095) không kết nối được |
| R01–R07 | PASS | Như P05 (isolation A/B, IPAM, data plane, hai profile một thiết bị, từ chối key lạ / SUSPENDED / web login) |
| L01, F01–F03 | PASS | Như P10 (tailnet mới khi đang chạy, tăng epoch, suspend/resume, chuyển cluster) |

- **Redaction:** `grep -rlE 'hskey-auth-[A-Za-z0-9]{6}'` trên thư mục artifact: 0 file.
- **Cleanup:** harness tự chứng minh (DB E2E xóa bằng root, 0 container/volume nhãn `mesh-e2e=p05`), POC P02 `/health` 200 sau mỗi lần.
- Artifact nằm dưới `artifacts/` (gitignored), không commit.

## 5. Known limitations / security blockers

- **F4 (API admin theo tailnet) → P08b.** Cluster mode hiện không mở API admin (C02), nên fail closed; nhưng chế độ một tailnet (`headscale serve`) vẫn mount API v1/v2 trên router public như upstream.
- **DERP verify theo tenant cho DERP ngoài ở cluster mode → P08b** (cần lab giai đoạn 2). Cluster mode từ chối DERP nhúng.
- Rate limit / queue limit (I10, S13) → P14.
- Guard Host chỉ kiểm hostname; port trong Host không bị so (client gửi `:8443`, VPS forward cùng port).
- Request đi qua VPS L4 nên log chỉ thấy `10.99.77.1` (PROXY protocol: ticket sau).
- Runbook (N7): không đặt reverse proxy trước `metrics_listen_addr`; mọi request qua proxy cùng host sẽ thành loopback = operator.
- `TestReloginFollowsNewHostname` flake một lần khi mc-ci quá tải (§3); nên theo dõi trong CI.
- Go race/servertest không chứng minh Noise/relay security; E2E ở §4 là bằng chứng client thật.
- **Reviewer approval:** chưa.
