# Source Inventory — P04 + P05 (một process nhiều tailnet, định tuyến phiên Noise)

- **Trạng thái:** `Verified` (servertest trong process, mc-ci). **Chưa** E2E client thật trên lab.
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

## 4. Test (mc-ci, Go 1.27.4, nguồn qua `git bundle`, 0 file CRLF)

| Lệnh | Commit | Exit | Kết quả |
|---|---|---|---|
| `go test -count=1 -race -tags p05cluster ./hscontrol/servertest/ -run Cluster` (lần đầu) | `eeb2f64` | 1 | 6/7 pass. `policy_change_in_A_is_silent_in_B` fail: client B nhận frame của **chính B** khi setup chưa ổn định. Kiểm chứng: không có thay đổi nào ở A, B vẫn nhận 1 frame trong 3 s chờ. Đây là lỗi thời điểm của test, không phải rò rỉ |
| Sửa test: `settledFrames` chờ 1 s không có frame trước khi lấy mốc | `4784627` | | |
| `go test -count=3 -race -tags p05cluster ./hscontrol/servertest/ -run Cluster` | `4784627` | **0** | 7 test × 3 lần, pass, 232.7 s |
| `go test -count=1 -race -timeout 90m ./hscontrol/...` | `4784627` | 1 | 25/26 package ok. `servertest`: chỉ `TestAPIv2`, `TestAPIv2OAuthScopes` fail vì thiếu `tofu`/`tscli` trong PATH (môi trường) |
| `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -count=1 -race ./hscontrol/servertest/ -run "TestAPIv2$\|TestAPIv2OAuthScopes"` | `4784627` | **0** | pass (giống CI) |

**Test isolation âm** (`cluster_p05_test.go`), đều pass:
- **S02/I02:** peer A/B không thấy nhau.
- **S10/S11d:** auth key chọn tailnet; key giả, sửa hoặc ghép bị từ chối; header tenant giả bị bỏ qua.
- **S05/I03:** cùng machine key ở hai tailnet.
- **I01:** một phiên Noise không đổi được tenant.
- **S06/S07/I07:** IP/tên trùng nhau, policy và rename không lan sang tenant khác.
- Tailnet của cluster khác, SUSPENDED, PENDING hoặc DELETING bị từ chối.
- Đăng ký web/OIDC tắt.

## 5. Còn thiếu trước `Reviewed`
- E2E client `tailscale` thật qua front door của lab ở chế độ cluster (thay `serve-tailnets` POC trên mc-cp). Race test **không** chứng minh an ninh Noise với client thật.
- Benchmark chi phí verify auth key hai lần.
