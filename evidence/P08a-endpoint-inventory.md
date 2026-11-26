# P08a — Inventory endpoint và RED security tests

- **Trạng thái:** inventory `Implemented` + `Verified` (test sinh từ code, chạy Windows và Linux `mc-ci`); RED tests `Implemented` (FAIL đúng như mong đợi). Sửa lỗi: **chưa** (P08). Chưa `Reviewed`.
- **Nhánh:** `feat/P08a-endpoint-audit` từ `master` @ `2b3e98d` (chưa có schema `tailnet_id` của P03b, chưa có P04/P05).
- **Ticket:** PLAN.md v2.0 dòng P08 ("API/admin/metrics/debug/queue limits/DERP"; bằng chứng "authZ, DoS, endpoint audit"). Chưa có `tickets/P08.md`.
- **Phạm vi:** chỉ test + tài liệu. Không đổi code không phải test.
- **Map:** I09, I11, I15 (`03-isolation-threat-model.md` §2, §4); S14, S15 (`06-tests-benchmarks.md`); AGENTS.md rule 6, 9, 13.

> Quy tắc AGENTS 13: lỗi isolation = BLOCK MERGE. Các RED ở đây là **lỗ hổng đang mở**; P08 không được merge khi còn RED, và không được "sửa" bằng cách skip/xóa test.

## 1. Cách sinh inventory (CI)

`hscontrol/endpoint_inventory_test.go` → `TestEndpointInventory` dựng mọi bề mặt HTTP và so **chính xác** với `hscontrol/testdata/p08a_endpoint_allowlist.txt` (`<server> <METHOD|ANY> <path> <auth>`). Thêm/bớt route hoặc một operation API mất `Security` → FAIL cho tới khi cập nhật allowlist **và** tài liệu này (test kiểm mọi path xuất hiện trong file này dưới dạng `` `path` ``). In danh sách: `MESH_PRINT_ROUTES=1 go test -run TestEndpointInventory ./hscontrol/`.

| Server | Listener thật | Cách liệt kê | Giới hạn |
|---|---|---|---|
| public | `listen_addr` (TCP/TLS); cùng router cho `HEADSCALE_DEBUG_INSECURE_TLS_LISTEN_ADDR`, cho memnet của tailsql, và cho từng runtime sau front door `serve-tailnets` | `chi.Walk(createRouter)` với DERP bật và provider OIDC; mount `/api/v1/*`, `/api/v2/*` được mở bằng `chi.Walk` trên mux Huma; auth API lấy từ `Operation.Security` | Route phụ thuộc cấu hình được bật hết (bao trùm) |
| noise | Bên trong `/ts2021` (HTTP/2 trên kết nối Noise) | Router dựng **bên trong** `NoiseUpgradeHandler` cho từng kết nối → không walk được nếu không sửa code; đọc AST `noise.go` (mọi `r.Get/Post/...`, lồng `r.Route`) | Tĩnh. **P08 nên tách `noiseRouter()` để `chi.Walk` được** |
| debug | `metrics_listen_addr` (mặc định `127.0.0.1:9090`) | `http.ServeMux` không liệt kê được: đọc trang chỉ mục `/debug/` (loopback) + đăng ký tường minh (statsviz, `/metrics`) + pprof `HandleSilent` của tsweb | pprof silent liệt kê tĩnh (gọi thử sẽ chạy CPU profile) |
| unix | `unix_socket` (quyền file `unix_socket_permission`) | Cùng mux Huma v1+v2, bọc `WithLocalTrust` → mọi operation `local-trust` | — |

Ngoài 4 server trên (không phải route HTTP của control plane, ghi nhận cho đủ): ACME HTTP-01 (`tls.acme.http_listen`: challenge + redirect), STUN UDP (`derp.server.stun_listen_addr`, không auth), tailsql (node tsnet `tag:tailsql` trong tailnet, xem §4.6).

**Số route (sinh từ code @ `2b3e98d`):** public **87**, noise **12**, debug **28**, unix **65** — tổng 192 dòng allowlist.

## 2. Quy ước

- **Auth hôm nay:** `none`; `auth` = API key (v1: bearer; v2: bearer/basic hoặc OAuth token, có scope); `local-trust` = ai mở được socket là admin; `noise-session` = bất kỳ machine key nào hoàn tất handshake (không cần đăng ký); `auth-id-secret`/`ping-id-secret`/`oidc-state` = bí mật khó đoán trong URL; `tsweb-protected` = `tsweb.AllowDebugAccess` (loopback, **toàn dải Tailscale 100.64.0.0/10 và fd7a:115c:a1e0::/48**, `TS_ALLOW_DEBUG_IP`, `TS_DEBUG_TRUSTED_CIDRS`, `debugkey` nếu có `TS_DEBUG_KEY_PATH`; từ chối nếu có `X-Forwarded-For`); `private-ip` = `protectedDebugHandler` (tsweb + mọi `ip.IsPrivate()`).
- **Scope tenant hôm nay:** master không có `tailnet_id`; mỗi runtime = một DB. "Không" = endpoint đọc/ghi **toàn bộ** DB/state của process. Với P05 (một runtime nhiều tailnet trên PostgreSQL chung) mọi dòng "Không" thành truy cập xuyên tenant nếu không sửa.
- **Rủi ro:** CAO = lộ/đổi dữ liệu node/IP/key/policy của tenant khác hoặc vượt qua kiểm soát relay; TB = lộ metadata, oracle, DoS, hoặc phụ thuộc cấu hình triển khai; Thấp = dữ liệu công khai/tĩnh.

## 3. Bảng route đầy đủ

| # | Server | Method | Path | Auth hôm nay | Dữ liệu tenant có thể lộ/đổi | Scope tenant hôm nay | Rủi ro |
|---|---|---|---|---|---|---|---|
| 1 | debug | ANY | `/debug/` | tsweb-protected | Chỉ mục debug, hostname máy | Không | TB |
| 2 | debug | ANY | `/debug/batcher` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 3 | debug | ANY | `/debug/config` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 4 | debug | ANY | `/debug/derp` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 5 | debug | ANY | `/debug/filter` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 6 | debug | ANY | `/debug/gc` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 7 | debug | ANY | `/debug/mapresponses` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 8 | debug | ANY | `/debug/nodestore` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 9 | debug | ANY | `/debug/overview` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 10 | debug | ANY | `/debug/ping` | tsweb-protected | Liệt kê node đang kết nối; GET ?node= gửi ping (thay đổi trạng thái qua GET) | Không | CAO |
| 11 | debug | ANY | `/debug/policy` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 12 | debug | ANY | `/debug/policy-manager` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 13 | debug | ANY | `/debug/pprof/` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 14 | debug | ANY | `/debug/pprof/cmdline` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 15 | debug | ANY | `/debug/pprof/goroutine` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 16 | debug | ANY | `/debug/pprof/profile` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 17 | debug | ANY | `/debug/pprof/symbol` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 18 | debug | ANY | `/debug/pprof/trace` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 19 | debug | ANY | `/debug/registration-cache` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 20 | debug | ANY | `/debug/routes` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 21 | debug | ANY | `/debug/runtime-metrics` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 22 | debug | ANY | `/debug/ssh` | tsweb-protected | Toàn bộ state (node, IP, key, policy, cấu hình) mọi tenant trong process | Không | CAO |
| 23 | debug | ANY | `/debug/statsviz` | private-ip | Runtime Go (heap, GC, goroutine) | Không | TB |
| 24 | debug | ANY | `/debug/statsviz/` | private-ip | Runtime Go (heap, GC, goroutine) | Không | TB |
| 25 | debug | ANY | `/debug/statsviz/ws` | private-ip | Runtime Go (heap, GC, goroutine) | Không | TB |
| 26 | debug | ANY | `/debug/vars` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 27 | debug | ANY | `/debug/varz` | tsweb-protected | Profile/biến runtime; /debug/gc ép GC (DoS nhẹ) | Không | TB |
| 28 | debug | ANY | `/metrics` | none | Prometheus của cả process; nhãn node.id nếu bật HEADSCALE_DEBUG_HIGH_CARDINALITY_METRICS | Không | CAO |
| 29 | noise | GET | `/machine/ssh/action/{src_node_id}/to/{dst_node_id}` | noise-session | Quyết định SSH check; 404/401 khác nhau → oracle node ID | Không | TB |
| 30 | noise | GET | `/machine/whoami` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 31 | noise | PATCH | `/machine/set-device-attr` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 32 | noise | POST | `/machine/audit-log` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 33 | noise | POST | `/machine/c2n` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 34 | noise | POST | `/machine/c2n-response` | noise-session | Phản hồi c2n, ràng buộc phiên Noise của node đích | Không | TB |
| 35 | noise | POST | `/machine/feature/query` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 36 | noise | POST | `/machine/id-token` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 37 | noise | POST | `/machine/map` | noise-session | Netmap: peer, key, IP, DNS, policy | Không | CAO |
| 38 | noise | POST | `/machine/register` | noise-session | Đăng ký node; nguồn gắn tenant (I01) | Không | CAO |
| 39 | noise | POST | `/machine/set-dns` | noise-session | Ghi bản ghi DNS (ACME) cho node gọi | Không | TB |
| 40 | noise | POST | `/machine/update-health` | noise-session | Chưa cài đặt (501) | N/A | Thấp |
| 41 | public | ANY | `/bootstrap-dns` | none | IP của hostname trong DERP map | Không | Thấp |
| 42 | public | ANY | `/derp` | none | Relay DERP; quyết định admit dựa trên verify toàn process | Không | CAO |
| 43 | public | ANY | `/derp/latency-check` | none | Không | N/A | Thấp |
| 44 | public | ANY | `/derp/probe` | none | Không | N/A | Thấp |
| 45 | public | DELETE | `/api/v1/apikey/{prefix}` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 46 | public | DELETE | `/api/v1/node/{nodeId}` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 47 | public | DELETE | `/api/v1/preauthkey` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 48 | public | DELETE | `/api/v1/user/{id}` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 49 | public | DELETE | `/api/v2/device/{id}` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 50 | public | DELETE | `/api/v2/tailnet/{tailnet}/keys/{keyId}` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 51 | public | GET | `/` | none | Không | N/A | Thấp |
| 52 | public | GET | `/api/v1/apikey` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 53 | public | GET | `/api/v1/config` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 54 | public | GET | `/api/v1/docs` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 55 | public | GET | `/api/v1/health` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 56 | public | GET | `/api/v1/node` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 57 | public | GET | `/api/v1/node/{nodeId}` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 58 | public | GET | `/api/v1/openapi-3.0.json` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 59 | public | GET | `/api/v1/openapi-3.0.yaml` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 60 | public | GET | `/api/v1/openapi.json` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 61 | public | GET | `/api/v1/openapi.yaml` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 62 | public | GET | `/api/v1/policy` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 63 | public | GET | `/api/v1/preauthkey` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 64 | public | GET | `/api/v1/user` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 65 | public | GET | `/api/v2/device/{id}` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 66 | public | GET | `/api/v2/device/{id}/attributes` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 67 | public | GET | `/api/v2/device/{id}/routes` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 68 | public | GET | `/api/v2/device/{id}/update-status` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 69 | public | GET | `/api/v2/docs` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 70 | public | GET | `/api/v2/openapi-3.0.json` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 71 | public | GET | `/api/v2/openapi-3.0.yaml` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 72 | public | GET | `/api/v2/openapi.json` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 73 | public | GET | `/api/v2/openapi.yaml` | none | Tài liệu OpenAPI (không có dữ liệu tenant) | N/A | Thấp |
| 74 | public | GET | `/api/v2/tailnet/{tailnet}/acl` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 75 | public | GET | `/api/v2/tailnet/{tailnet}/devices` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 76 | public | GET | `/api/v2/tailnet/{tailnet}/keys` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 77 | public | GET | `/api/v2/tailnet/{tailnet}/keys/{keyId}` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 78 | public | GET | `/api/v2/tailnet/{tailnet}/settings` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 79 | public | GET | `/api/v2/tailnet/{tailnet}/users` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 80 | public | GET | `/api/v2/users/{id}` | auth | User theo ID toàn cục | Không | CAO |
| 81 | public | GET | `/apple` | none | server_url (cấu hình client) | Không | Thấp |
| 82 | public | GET | `/apple/{platform}` | none | server_url (cấu hình client) | Không | Thấp |
| 83 | public | GET | `/auth/{auth_id}` | auth-id-secret | Trang đăng ký/duyệt theo auth_id; gắn tenant lúc duyệt (I01) | Không | TB |
| 84 | public | GET | `/favicon.ico` | none | Không | N/A | Thấp |
| 85 | public | GET | `/health` | none | Trạng thái DB (ok/lỗi) của cả process | Không | Thấp |
| 86 | public | GET | `/key` | none | Noise public key của cluster (dùng chung mọi tenant, ADR 0005) | Không | Thấp |
| 87 | public | GET | `/oidc/callback` | oidc-state | Hoàn tất OIDC, tạo user/node | Không | TB |
| 88 | public | GET | `/register/confirm/{auth_id}` | auth-id-secret | Trang đăng ký/duyệt theo auth_id; gắn tenant lúc duyệt (I01) | Không | TB |
| 89 | public | GET | `/register/{auth_id}` | auth-id-secret | Trang đăng ký/duyệt theo auth_id; gắn tenant lúc duyệt (I01) | Không | TB |
| 90 | public | GET | `/robots.txt` | none | Không | N/A | Thấp |
| 91 | public | GET | `/ts2021` | noise-handshake | Nâng cấp Noise; mọi machine key đều handshake được, không lộ dữ liệu tenant trực tiếp | Không | TB |
| 92 | public | GET | `/version` | none | Phiên bản/commit (fingerprint) | Không | Thấp |
| 93 | public | GET | `/windows` | none | server_url (cấu hình client) | Không | Thấp |
| 94 | public | HEAD | `/machine/ping-response` | ping-id-secret | Hoàn tất ping theo ping ID (bí mật), ID toàn cục | Không | Thấp |
| 95 | public | PATCH | `/api/v2/tailnet/{tailnet}/settings` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 96 | public | POST | `/api/v1/apikey` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 97 | public | POST | `/api/v1/apikey/expire` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 98 | public | POST | `/api/v1/auth/approve` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 99 | public | POST | `/api/v1/auth/register` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 100 | public | POST | `/api/v1/auth/reject` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 101 | public | POST | `/api/v1/debug/node` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 102 | public | POST | `/api/v1/node/backfillips` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 103 | public | POST | `/api/v1/node/register` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 104 | public | POST | `/api/v1/node/{nodeId}/approve_routes` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 105 | public | POST | `/api/v1/node/{nodeId}/expire` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 106 | public | POST | `/api/v1/node/{nodeId}/rename/{newName}` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 107 | public | POST | `/api/v1/node/{nodeId}/tags` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 108 | public | POST | `/api/v1/policy/check` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 109 | public | POST | `/api/v1/preauthkey` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 110 | public | POST | `/api/v1/preauthkey/expire` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 111 | public | POST | `/api/v1/user` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 112 | public | POST | `/api/v1/user/{oldId}/rename/{newName}` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 113 | public | POST | `/api/v2/device/{id}/authorized` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 114 | public | POST | `/api/v2/device/{id}/expire` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 115 | public | POST | `/api/v2/device/{id}/key` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 116 | public | POST | `/api/v2/device/{id}/name` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 117 | public | POST | `/api/v2/device/{id}/routes` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 118 | public | POST | `/api/v2/device/{id}/tags` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 119 | public | POST | `/api/v2/device/{id}/update-client` | auth | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 120 | public | POST | `/api/v2/oauth/token` | none | Đổi client credentials lấy token; token mang scope/tags | Không | TB |
| 121 | public | POST | `/api/v2/tailnet/{tailnet}/acl` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 122 | public | POST | `/api/v2/tailnet/{tailnet}/acl/validate` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 123 | public | POST | `/api/v2/tailnet/{tailnet}/keys` | auth | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 124 | public | POST | `/register/confirm/{auth_id}` | auth-id-secret | Trang đăng ký/duyệt theo auth_id; gắn tenant lúc duyệt (I01) | Không | TB |
| 125 | public | POST | `/ts2021` | noise-handshake | Nâng cấp Noise; mọi machine key đều handshake được, không lộ dữ liệu tenant trực tiếp | Không | TB |
| 126 | public | POST | `/verify` | none | Oracle tồn tại node key TOÀN CỤC; nhận cả node hết hạn | Không | CAO |
| 127 | public | PUT | `/api/v1/policy` | auth | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 128 | unix | DELETE | `/api/v1/apikey/{prefix}` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 129 | unix | DELETE | `/api/v1/node/{nodeId}` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 130 | unix | DELETE | `/api/v1/preauthkey` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 131 | unix | DELETE | `/api/v1/user/{id}` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 132 | unix | DELETE | `/api/v2/device/{id}` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 133 | unix | DELETE | `/api/v2/tailnet/{tailnet}/keys/{keyId}` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 134 | unix | GET | `/api/v1/apikey` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 135 | unix | GET | `/api/v1/config` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 136 | unix | GET | `/api/v1/docs` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 137 | unix | GET | `/api/v1/health` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 138 | unix | GET | `/api/v1/node` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 139 | unix | GET | `/api/v1/node/{nodeId}` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 140 | unix | GET | `/api/v1/openapi-3.0.json` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 141 | unix | GET | `/api/v1/openapi-3.0.yaml` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 142 | unix | GET | `/api/v1/openapi.json` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 143 | unix | GET | `/api/v1/openapi.yaml` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 144 | unix | GET | `/api/v1/policy` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 145 | unix | GET | `/api/v1/preauthkey` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 146 | unix | GET | `/api/v1/user` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 147 | unix | GET | `/api/v2/device/{id}` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 148 | unix | GET | `/api/v2/device/{id}/attributes` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 149 | unix | GET | `/api/v2/device/{id}/routes` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 150 | unix | GET | `/api/v2/device/{id}/update-status` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 151 | unix | GET | `/api/v2/docs` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 152 | unix | GET | `/api/v2/openapi-3.0.json` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 153 | unix | GET | `/api/v2/openapi-3.0.yaml` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 154 | unix | GET | `/api/v2/openapi.json` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 155 | unix | GET | `/api/v2/openapi.yaml` | local-trust | Tài liệu OpenAPI | N/A | Thấp |
| 156 | unix | GET | `/api/v2/tailnet/{tailnet}/acl` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 157 | unix | GET | `/api/v2/tailnet/{tailnet}/devices` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 158 | unix | GET | `/api/v2/tailnet/{tailnet}/keys` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 159 | unix | GET | `/api/v2/tailnet/{tailnet}/keys/{keyId}` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 160 | unix | GET | `/api/v2/tailnet/{tailnet}/settings` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 161 | unix | GET | `/api/v2/tailnet/{tailnet}/users` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 162 | unix | GET | `/api/v2/users/{id}` | local-trust | User theo ID toàn cục | Không | CAO |
| 163 | unix | PATCH | `/api/v2/tailnet/{tailnet}/settings` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 164 | unix | POST | `/api/v1/apikey` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 165 | unix | POST | `/api/v1/apikey/expire` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 166 | unix | POST | `/api/v1/auth/approve` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 167 | unix | POST | `/api/v1/auth/register` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 168 | unix | POST | `/api/v1/auth/reject` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 169 | unix | POST | `/api/v1/debug/node` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 170 | unix | POST | `/api/v1/node/backfillips` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 171 | unix | POST | `/api/v1/node/register` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 172 | unix | POST | `/api/v1/node/{nodeId}/approve_routes` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 173 | unix | POST | `/api/v1/node/{nodeId}/expire` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 174 | unix | POST | `/api/v1/node/{nodeId}/rename/{newName}` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 175 | unix | POST | `/api/v1/node/{nodeId}/tags` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 176 | unix | POST | `/api/v1/policy/check` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 177 | unix | POST | `/api/v1/preauthkey` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 178 | unix | POST | `/api/v1/preauthkey/expire` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 179 | unix | POST | `/api/v1/user` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 180 | unix | POST | `/api/v1/user/{oldId}/rename/{newName}` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |
| 181 | unix | POST | `/api/v2/device/{id}/authorized` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 182 | unix | POST | `/api/v2/device/{id}/expire` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 183 | unix | POST | `/api/v2/device/{id}/key` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 184 | unix | POST | `/api/v2/device/{id}/name` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 185 | unix | POST | `/api/v2/device/{id}/routes` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 186 | unix | POST | `/api/v2/device/{id}/tags` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 187 | unix | POST | `/api/v2/device/{id}/update-client` | local-trust | Thiết bị theo node ID toàn cục, không có `{tailnet}` trong path | Không | CAO |
| 188 | unix | POST | `/api/v2/oauth/token` | local-trust | Cấp OAuth token | Không | TB |
| 189 | unix | POST | `/api/v2/tailnet/{tailnet}/acl` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 190 | unix | POST | `/api/v2/tailnet/{tailnet}/acl/validate` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 191 | unix | POST | `/api/v2/tailnet/{tailnet}/keys` | local-trust | Dữ liệu tailnet; chỉ chấp nhận `-` (= toàn DB) | Không | CAO |
| 192 | unix | PUT | `/api/v1/policy` | local-trust | Admin toàn quyền (node/user/key/policy) trên TOÀN DB, không scope | Không | CAO |

## 4. Phát hiện (đã kiểm trong code @ `2b3e98d`)

### 4.1 Đã biết trước, đã xác minh

| ID | Phát hiện | Vị trí | Map | Test |
|---|---|---|---|---|
| F1 | `protectedDebugHandler` cho qua mọi `ip.IsPrivate()`. **Hiệu chỉnh:** nó chỉ bọc `/debug/statsviz/` và `/debug/statsviz/ws`; các `/debug/*` khác đi qua `tsweb.Protected` (không nhận RFC1918, nhưng xem N1) | `hscontrol/debug.go:26-52, 274-279` | S14, I09 | `TestP08aDebugListenerRefusesNonOperator` |
| F2 | `/verify` trên router **public**, không auth, tra node key toàn cục (`GetNodeByNodeKey`), trả `{"Allow":true}` cho cả node hết hạn: oracle tồn tại node key; sau P05 là oracle xuyên tenant | `hscontrol/handlers.go:166-213`, `app.go:557` | S15, I11, I15 | `TestP08aVerifyRejectsUnauthenticatedPublicCaller`, `TestP08aVerifyRefusesUnknownExpiredRevoked` |
| F3 | `http.DefaultTransport.RegisterProtocol("headscale-derp-verify", …)` là **toàn cục process**, một scheme cố định | `hscontrol/app.go:288-293`, `derp/server/derp_server.go:37,70-71,399-427` | S15, I11 | `TestP08aDERPVerifyTwoTailnetsOneProcess` (xem N2) |
| F4 | API v1 (toàn quyền) và v2 dưới `/api` trên router public; API key v2 không gắn tailnet; `requireDefaultTailnet` chỉ nhận `-`; `/api/v2/device/{id}` và `/api/v2/users/{id}` dùng ID toàn cục, không có `{tailnet}` | `app.go:570-573`, `api/v2/api.go:289-297`, `api/v2/devices.go` | I08, I09, I15, rule 6 | `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` (**SKIP**, chặn bởi P04/P05) |
| F5 | Unix socket: mọi operation v1+v2 với `WithLocalTrust`, không auth; quyền file mặc định `0770` | `app.go:~700-760`, `config/default.go:17` | rule 6 (API nền tảng phải được bảo vệ riêng) | allowlist (`local-trust`) |
| F6 | `/machine/ping-response` chỉ xác thực bằng ping ID khó đoán; ID toàn process | `noise.go:381-401`, `app.go:576` | I15 | allowlist (`ping-id-secret`); rủi ro Thấp |

### 4.2 Mới (ngoài danh sách đã biết)

| ID | Phát hiện | Vị trí | Rủi ro | Map | Test |
|---|---|---|---|---|---|
| **N1** | `tsweb.AllowDebugAccess` tin **toàn dải Tailscale** (100.64.0.0/10, fd7a:115c:a1e0::/48). Trong control plane nhiều tenant, mọi node của mọi tenant đều có địa chỉ trong dải này. Nếu listener metrics/debug tới được từ mạng tailnet (control plane cũng là node, subnet router, tsnet/tailsql nhúng trên cùng host), một node tenant đọc được `/debug/nodestore`, `/debug/config`, `/debug/policy`, `/debug/mapresponses`, `/debug/registration-cache`… của **mọi** tenant | `tailscale.com/tsweb/tsweb.go:104-127`, `debug.go:132-279` | **CAO** | S14, I09 | `TestP08aDebugListenerRefusesNonOperator` (case CGNAT/ULA Tailscale) |
| **N2** | Hệ quả F3: runtime thứ hai bật DERP nhúng + `verify_clients` (mặc định `true`) trong cùng process **panic** `protocol headscale-derp-verify already registered` → `serve-tailnets` với 2 tailnet bật DERP nhúng không khởi động được (DoS cấu hình). Handler đã đăng ký trả lời cho **mọi** DERP trong process → DERP của B dùng state của A | `app.go:288-293` | **CAO** | S15, I11 | `TestP08aDERPVerifyTwoTailnetsOneProcess` |
| **N3** | `/metrics` không có cổng nào: không qua `tsweb.Protected`, và `debugHostGuard` cố ý bỏ kiểm Host cho `/metrics`. Ai tới được `metrics_listen_addr` đều đọc được. Registry Prometheus toàn cục (`promauto`) → mọi tenant trong process chung một bộ số; với `HEADSCALE_DEBUG_HIGH_CARDINALITY_METRICS` có nhãn `id` = node ID | `debug.go:99-104, 282-283`, `metrics.go:9-21` | **CAO** khi listener không phải loopback; TB khi loopback | S14, I09, rule 9 | `TestP08aDebugListenerRefusesNonOperator` (`/metrics`) |
| **N4** | tailsql (khi `tailsql.enabled`): node tsnet `tag:tailsql` phục vụ SQL chỉ đọc trên **toàn DB** cho ai được grant `tailscale.com/cap/tailsql` trong policy. Trong runtime nhiều tenant, một grant trong policy của một tenant mở dữ liệu của mọi tenant. Phải cấm ở chế độ multi-tenant | `tailsql.go:60-130`, `app.go:~898-925` | **CAO** (khi bật) | I06, I09 | Chưa (cần P05); ghi cho P08 |
| **N5** | `/machine/ssh/action/{src}/to/{dst}`: bất kỳ machine key nào (handshake không cần đăng ký) phân biệt được "dst node not found" (404) với "machine key does not match" (401) → oracle node ID tuần tự toàn cục (đếm node của tenant khác) | `noise.go:~430-480` | TB | I15 | Chưa (ghi cho P08) |
| **N6** | `/debug/ping` đổi trạng thái qua **GET** `?node=` (gửi PingRequest tới node) và liệt kê mọi node đang kết nối; cùng cổng `tsweb-protected` → node tenant (N1) có thể ping/liệt kê node tenant khác | `debug.go:239-270` | CAO (qua N1) | S14, I09 | Gián tiếp qua N1 |
| N7 | Listener debug không có `realIP`: đặt reverse proxy cùng host trước `metrics_listen_addr` làm mọi request thành loopback = operator. Ghi vào runbook: không proxy listener debug | `debug.go:285-291` | TB (triển khai) | S14 | Không test được ở handler; runbook P08 |
| N8 | `/api/v1/openapi*`, `/api/v2/openapi*`, `/api/v1/docs`, `/api/v2/docs` công khai; `/api/v2/oauth/token` công khai (đúng thiết kế client credentials) — cần rate limit (I10) | `api/v1`, `api/v2` | Thấp/TB | I10 | allowlist |

## 5. Hợp đồng dự kiến (P08 cài đặt, cần người duyệt)

1. **Operator của listener debug/metrics (S14, I09, rule 9):** chỉ peer TCP là loopback, hoặc nằm trong allowlist CIDR operator tường minh trong cấu hình (mặc định rỗng). Không dùng `ip.IsPrivate()`, không tin dải Tailscale CGNAT/ULA (thuộc tenant), không tin `X-Forwarded-For`. `/metrics` cùng quy tắc (bỏ ngoại lệ). Dài hạn: mTLS operator (threat model §4).
2. **`/verify` (S15, I11, I15):** chỉ DERP server đã xác thực (mTLS hoặc secret verify theo từng DERP, ánh xạ sang tập tailnet DERP đó phục vụ) được gọi. Caller public không xác thực → 401/403, body không phụ thuộc node key. DERP đã xác thực chỉ nhận `Allow` cho node tồn tại, **chưa hết hạn**, thuộc tailnet trong scope của DERP; unknown/expired/deleted → từ chối. Verify nhúng phải là hàm gắn với runtime/scope, không đăng ký trên `http.DefaultTransport` toàn cục. Không xác định được scope → fail closed. P08 phải sửa test upstream `TestHandleVerifyRequest_AdmitsByNodeKey` (đang ghim `expired → true`).
3. **API (I08, rule 6):** API key/OAuth token tenant gắn đúng một `TailnetID` đã xác minh. `/api/v2/tailnet/{id}/…` khác tailnet của key → 404 (giống tailnet không tồn tại). `-` = tailnet của key. `/api/v2/device/{id}`: tailnet của device phải bằng tailnet của key, không thì 404 và không đổi gì. `/api/v1` = API quản trị nền tảng, không nhận key tenant. Unix socket = quản trị nền tảng, không mount vào container/VM của tenant.

### Two-tenant verify sau P05

`TestP08aDERPVerifyTwoTailnetsOneProcess` hiện dùng mô hình `serve-tailnets` (2 runtime, 2 DB, 1 process), mô hình multi-tailnet duy nhất trên master. Khi P05 có runtime một-process-nhiều-tailnet: thêm biến thể (cùng file, cùng S15) tạo tailnet A và B trên **một** runtime, đăng ký node ở A, gọi verify với danh tính DERP scope cho B → phải `Allow=false`; danh tính DERP scope cho A → `Allow=true` (đối chứng). Fixture `p08aTwoTailnets` trong `hscontrol/servertest/p08a_apiv2_tailnet_scope_test.go` là chỗ nối cho cả API v2 và verify. Không dùng nhiều user thay cho nhiều tailnet.

## 6. Kết quả RED/GREEN

Go 1.27.4 ở cả hai nơi. Windows: `go test -count=1 -v -run '^<Test>' <pkg>`. Linux `mc-ci` (`~/src/p08a`, clone từ `git bundle` của commit `608dc39`, không đụng thư mục khác): `go test -race -count=1 -v -run '^<Test>' <pkg>`. "Sub FAIL/PASS" đếm dòng `--- FAIL/PASS` (gồm test cha).

| Test | Pkg | S/I | Mong đợi | Windows exit | Linux -race exit | Sub FAIL / PASS | Kết luận |
|---|---|---|---|---|---|---|---|
| `TestEndpointInventory` | `./hscontrol/` | S14, I09, §4 | GREEN (cổng CI) | 0 | 0 | 0 / 1 | GREEN. Đối chứng âm: xóa dòng `public POST /verify none` khỏi allowlist → exit 1, báo "new or changed (not in allowlist)" |
| `TestP08aPublicRouterHasNoDiagnosticRoutes` | `./hscontrol/` | S14, I09 | GREEN (regression guard) | 0 | 0 | 0 / 1 | GREEN |
| `TestP08aDebugListenerRefusesNonOperator` | `./hscontrol/` | S14, I09 | RED | 1 | 1 | 18 / 13 | **RED**: statsviz từ 192.168/10/fd00:: (F1); mọi `/debug/*` từ 100.64.0.7 và fd7a:115c:a1e0::7 (N1); `/metrics` từ mọi peer kể cả 203.0.113.9 (N3). GREEN: `/debug/{nodestore,config,pprof}` từ RFC1918/ULA/public; đối chứng loopback 200 |
| `TestP08aVerifyRejectsUnauthenticatedPublicCaller` | `./hscontrol/` | S15, I11, I15 | RED | 1 | 1 | 1 / 0 | **RED**: caller public nhận `200 {"Allow":true}` cho node key đã đăng ký, body khác với key lạ (F2) |
| `TestP08aVerifyRefusesUnknownExpiredRevoked` | `./hscontrol/` | S15, I11 | RED (expired) | 1 | 1 | 2 / 3 | **RED** expired → `Allow=true` (F2). GREEN: live (đối chứng), unknown, deleted |
| `TestP08aDERPVerifyTwoTailnetsOneProcess` | `./hscontrol/endpointaudit/` | S15, I11 | RED | 1 | 1 | 1 / 0 | **RED**: runtime B panic `protocol headscale-derp-verify already registered`; đường verify dùng chung admit node key của tailnet A (F3, N2) |
| `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` | `./hscontrol/servertest/` | I08, I09, I15 | SKIP (chặn P04/P05) | 0 | 0 | 0 / 0 (1 SKIP) | **SKIP — không phải pass.** Chưa có hai tailnet trên một runtime |

Khác:

- `gofmt -l` (Linux) trên file mới: sạch sau commit evidence (commit đầu có một lỗi căn lề struct trong `endpointaudit`, đã sửa). `go vet ./hscontrol/ ./hscontrol/endpointaudit/ ./hscontrol/servertest/`: exit 0.
- Hồi quy test có sẵn (Linux): `go test -race -count=1 -skip TestP08a ./hscontrol/` → exit 0 (`ok`, 528.5s). Test mới không làm hỏng test cũ.
- Package `./hscontrol/` và `./hscontrol/endpointaudit/` sẽ FAIL trong CI cho tới P08 — có chủ đích (AGENTS rule 13). Nếu CI của master không được phép đỏ, PR này phải giữ ở trạng thái draft/không merge cho tới khi P08 sửa xong; không được skip RED.

## 7. PR P08a Evidence

- **Repo commit before / after:** `2b3e98d` / xem `git log feat/P08a-endpoint-audit` (commit tests+inventory và commit evidence).
- **Verified source paths & symbols:** `hscontrol/app.go` (`NewHeadscale` 278-300, `serveHumaMux`/`createRouter` 506-582, `Serve`: socket, HTTP, insecure TLS, ACME, metrics, tailsql ~700-925, `HTTPHandler` 1404), `hscontrol/noise.go` (`NoiseUpgradeHandler` router 131-210, `PingResponseHandler` 381-401, `SSHActionHandler` ~430-500), `hscontrol/debug.go` (`protectedDebugHandler`, `debugHostGuard`, `isAllowedDebugHost`, `debugHTTPServer`), `hscontrol/handlers.go` (`handleVerifyRequest`, `VerifyHandler`, `KeyHandler`), `hscontrol/metrics.go`, `hscontrol/tailsql.go`, `hscontrol/derp/server/derp_server.go` (`DerpVerifyScheme`, `NewDERPVerifyTransport`), `hscontrol/api/v1/api.go` (`Handler`, `WithLocalTrust`, `authMiddleware`), `hscontrol/api/v2/api.go` (`Handler`, `WithLocalTrust`, `authMiddleware`, `requireDefaultTailnet`), `cmd/headscale/cli/serve_tailnets.go`, `tailscale.com/tsweb` (`AllowDebugAccess`, `Protected`, `Debugger`, `addProfilingHandlers`), test có sẵn `app_test.go` (`TestDebugHostGuard`, `validConfig`), `noise_test.go` (`TestMetricsRouterIsolation`, `putTestNodeInStore`), `handlers_test.go` (`TestHandleVerifyRequest_AdmitsByNodeKey`), `servertest/server.go`.
- **Design choice / ADR:** không ADR mới; hợp đồng §5 là đề xuất cho P08.
- **Tests + exact commands + exit codes:** §6.
- **E2E artifacts / protocol map redacted:** không có (test handler/in-process, không có client thật; không chứng minh Noise/relay E2E — AGENTS "Nghiệm thu").
- **Negative tenant isolation tests:** `TestP08aDERPVerifyTwoTailnetsOneProcess` (RED); `TestP08aAPIv2AdminKeyCannotAddressOtherTailnet` (SKIP, chặn bởi P04/P05, **không phải pass**).
- **DB migrations and downgrade constraints:** không có.
- **Benchmark:** không áp dụng.
- **Known limitations / security blockers:** F2, N1, N2, N3 (và N4 khi bật tailsql) là blocker cho gate N1 (P09). Router Noise liệt kê bằng AST; debug liệt kê bằng trang chỉ mục + danh sách tĩnh pprof. Chưa có test cho N4, N5, N7. `gofmt`/lint chưa chạy trên Windows (CRLF), xem §6.
- **Reviewer approval:** chưa.

## 8. Cập nhật bởi P08 (allowlist `hscontrol/testdata/p08a_endpoint_allowlist.txt`)

Bảng §3 giữ nguyên làm hồ sơ P08a. Các thay đổi P08 so với bảng đó (chi tiết và lệnh test: `evidence/source-inventory-P08.md`):

| Thay đổi | Trước (P08a) | Sau (P08) |
|---|---|---|
| Mọi route của listener debug (`metrics_listen_addr`), kể cả `/metrics` và statsviz | `tsweb-protected` / `private-ip` / `none` | `operator-only`: `operatorOnly` bọc cả listener; chỉ loopback hoặc `metrics_operator_cidrs`; dải Tailscale không bao giờ là operator (F1, N1, N3) |
| `public POST /verify` | `none` | `verify-secret`: cần secret trong `derp.verify_secret_path`; không cấu hình thì mọi caller nhận cùng 403; node hết hạn không được admit (F2) |
| Listener public của cluster (`headscale cluster serve`) | chưa có trong inventory | Thêm 11 dòng `cluster …`: chỉ route không gắn tenant và `/ts2021`; mọi route sau guard Host/SNI (421). `/register/{auth_id}`, `/auth/{auth_id}` = `disabled-404`. Không có API admin, debug, metrics |
| Listener metrics của cluster | chưa có | Chỉ `GET /metrics`, sau `operatorOnly`; không có trang debug (test `TestP08ClusterMetricsOperatorsOnly`, không liệt kê trong allowlist vì không phải router chi) |
| cluster GET `/ready` (P11b) | chưa có | `none`: 200 `{"status":"ready","lease_epoch":N}` chỉ ở process giữ lease cluster và đã nạp tailnet, 503 `{"status":"standby"}` ở standby; dùng làm health check của front door (HAProxy). Không lộ holder, node, tenant; sau guard Host/SNI như mọi route cluster |
| cluster POST `/verify` (P09, ADR 0009) | chưa có | `verify-secret`: secret `derp.verify_secret_path` của cluster, so constant-time; thiếu hoặc sai secret: mọi caller nhận cùng 403, body không phụ thuộc node key. `Allow` chỉ khi node key thuộc node còn sống, chưa hết hạn, của tailnet mà cluster sở hữu và đang phục vụ; process standby trả `Allow=false`. Do DERP ngoài của cluster gọi qua URL công khai |
| cluster ANY `/api/v2/*` (P08b) | chưa có | `api-key-tailnet`: bucket toàn process trước khi tra key (`tuning.tenant_api.global_*`), rồi `Platform.TailnetForAPIKey` (chỉ key `hskey-api-`, chưa thu hồi, chưa hết hạn) chọn tailnet, bucket theo key và theo tailnet (429), runtime của tailnet xác thực key lần nữa qua handle scoped. Tailnet không do process phục vụ → 404; standby → 503. Không có `/api/v1`, không có `/api/v2/oauth/token`, không nhận OAuth access token |
| `cluster-v2 …` (29 dòng, P08b) | chưa có | API v2 của một runtime ở cluster mode (`apiv2.Backend.Tailnet`): `{tailnet}` chỉ nhận `-` hoặc ID tailnet của key, khác → 404; device của tailnet khác → 404. Bị từ chối 403 (`clusterForbidden`): POST `/api/v2/tailnet/{tailnet}/acl` (setACL), PATCH `/api/v2/tailnet/{tailnet}/settings`, POST `/api/v2/device/{id}/authorized`, POST `/api/v2/device/{id}/key`, POST `/api/v2/device/{id}/update-client`; tạo/xóa OAuth client (`keyType: client`) → 403. Các trang `/api/v2/docs`, `/api/v2/openapi*` ghi `none` ở tầng runtime nhưng chỉ tới được sau bước tra key của cluster |
| `cluster-unix …` (5 dòng, P08b) | chưa có | `local-trust` (quyền file `unix_socket_permission`), chỉ trên unix socket, không ở listener public hay metrics: `/cluster/v1/tailnet/{tailnet}/nodes` (GET), `/cluster/v1/tailnet/{tailnet}/node/{node}` (DELETE), `/cluster/v1/tailnet/{tailnet}/node/{node}/expire` (POST), `/cluster/v1/tailnet/{tailnet}/node/{node}/routes` (POST), `/cluster/v1/tailnet/{tailnet}/user/{user}` (DELETE). `{tailnet}` chỉ là ID; tailnet không phục vụ → 404; node/user của tailnet khác → 404; standby → 503; mỗi lệnh ghi một dòng audit log |
| Router Noise | đọc từ `NoiseUpgradeHandler` | đọc từ `serveNoiseSession` (P05 đã chuyển router sang hàm này); tập route không đổi |
