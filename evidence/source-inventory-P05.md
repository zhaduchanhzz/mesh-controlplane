# Source Inventory — P04 + P05 (một process nhiều tailnet, định tuyến phiên Noise)

- **Trạng thái:** `Implemented`, đang kiểm (servertest RED suite `p05cluster`).
- **Nhánh:** `feat/P05-multitenant-runtime`, từ `master` 82f9b36 + ADR 0007 (ce75c1b).
- **Hợp đồng:** `design/adr/0007-multi-tenant-runtime-and-session-routing.md` (Accepted).
- **Song song:**
  - `agent/p05-db`: `db.Platform`, unique `(machine_key, node_key)`;
  - `agent/p05-tests`: servertest 2 tailnet trong 1 cluster.

## 1. Path/symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/app.go` | `Headscale`, `NewHeadscale`, `Serve`, `createRouter`, `getTLSSettings`, `readOrCreatePrivateKey` | `Serve` trộn runtime (batcher, GC, DERP map, scheduled tasks) với listener; `getTLSSettings` chỉ cần `cfg` |
| `hscontrol/noise.go` | `noiseServer`, `NoiseUpgradeHandler`, `PollNetMapHandler`, `RegistrationHandler`, `SetDNSHandler`, `SSHActionHandler`, `C2NResponseHandler`, `getAndValidateNode` | Phiên Noise chạy HTTP/2, nên các request trên cùng phiên chạy **song song** |
| `hscontrol/handlers.go` | `KeyHandler`, `HealthHandler`, `VersionHandler` | `KeyHandler` chỉ cần Noise key, `HealthHandler` cần `state.PingDB` |
| `hscontrol/state/state.go` | `NewState` | Tự mở DB. Tách thành `NewStateWithDB` |
| `hscontrol/types/config.go` | `PolicyMode`, `PolicyEmptyDenies`, `ClusterID`, `liveDNS` | Config có mutex (`liveDNS`), nên các runtime dùng chung con trỏ, không copy |
| `hscontrol/db/platform.go` (agent) | `OpenPlatform`, `TailnetForAuthKey`, `TailnetForNode`, `OwnedTailnets`, `TenantDB` | Lookup không scope bằng raw SQL, chỉ đọc `tailnet_id` |

## 2. Thiết kế đã làm
- **`newHeadscale`/`startRuntime`:** phần per-tailnet (state, batcher, GC, scheduled tasks) tách khỏi listener.
- **`serveNoiseSession` + `sessionRouter`** (`hscontrol/session_route.go`):
  - process một tenant: `router == nil`, phiên gắn sẵn runtime, giữ hành vi upstream;
  - cluster: request register/map/set-dns đầu tiên phân giải tenant rồi gắn phiên (`bind`, có mutex). Tenant khác thì trả 403 `ErrSessionTenantSwitch` và đóng phiên (I01);
  - SSH action và c2n-response cần phiên đã gắn;
  - lỗi tenant không tồn tại, không ACTIVE hoặc không thuộc cluster: 401, không lộ thông tin;
  - lỗi lookup DB: 503 + `Retry-After`.
- **Register không có auth key:** tra `(machine_key, node_key)`; nếu không thấy thì thử `OldNodeKey` (xoay node key).
- **`hscontrol.Cluster`** (`cluster.go`):
  - từ chối OIDC, DERP nhúng, policy file và ACME (fail closed; P04b và P08 sẽ làm);
  - bật `PolicyEmptyDenies`;
  - listener chỉ phục vụ `/ts2021`, `/key`, `/health`, `/version`, ping-response; không admin API, debug hay metrics (rule 9).

## 3. Giả định / giới hạn
- DNS, prefix IP và DERP map dùng chung giữa các tailnet trong một cluster (theo config). DNS/IP riêng từng tenant làm theo `config_revision` ở ticket sau.
- Auth key được verify hai lần (platform để tìm tenant, runtime để đăng ký): tốn thêm một lần hash. Cần đo ở benchmark.
- Thêm/bớt tailnet khi đang chạy: P10.
- Go race unit test và servertest **không** chứng minh an ninh Noise với client thật. Cần E2E thật trên lab (theo AGENTS).
