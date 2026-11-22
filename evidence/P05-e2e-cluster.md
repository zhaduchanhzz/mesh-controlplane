# PR P05 Evidence — E2E client thật, chế độ cluster

- **Repo commit before / after:** `d0ad0f6` (master, merge P04+P05) → `7bd5a3d` (nhánh `feat/P05-e2e-cluster`).
- **Verified source paths & symbols:**
  - `hscontrol/cluster.go` (`NewCluster`, `Serve`, `HTTPHandler`);
  - `hscontrol/session_route.go` (`bind`, `refuseRoute`, `registerRefusal`);
  - `hscontrol/noise.go` (`RegistrationHandler`);
  - `hscontrol/db/platform.go`, `platform_admin.go` (`CreateTailnet`, `SetTailnetStatus`, `TailnetBySlug`);
  - `cmd/headscale/cli/cluster.go`.
- **Design choice / ADR:** ADR 0005, ADR 0007. Thêm trong PR này:
  - `headscale cluster serve` và lệnh quản trị offline `headscale cluster tailnet create|status|policy|authkey|nodes` (tạm dùng tới P10);
  - tạo tailnet = PENDING + policy `{"grants":[]}` trong **một transaction** (AGENTS rule 8);
  - từ chối register do không xác định được tenant: trả `200 + RegisterResponse.Error`, giống đường "bad key" của upstream. Một thông báo chung cho key lạ, tailnet SUSPENDED và tailnet của cluster khác (không lộ trạng thái tenant). Lỗi lookup DB vẫn trả 503.

## Đường đi và cách chạy
- **Đường đi:** client `tailscale/tailscale:v1.104.7` (docker, mc-ci) → `https://c1.cp.gta5web.online:8443` → VPS 116.118.3.46 → WireGuard `wg-mesh` → mc-cp:8443 (`headscale-cluster.service`, cluster `mc-cp-c1`).
- **Thao tác lab:**
  - harness dừng POC P02 (`headscale-tailnets`) trong lúc chạy rồi khởi động lại, kiểm `/health` 200;
  - không sửa firewall (mc-cp chỉ mở 8443 từ VPS), không đổi DNS (`*.cp` wildcard có sẵn);
  - DB E2E riêng `/var/lib/headscale/cluster-e2e/db.sqlite`, tạo mới mỗi lần, xóa bằng root và kiểm lại đã xóa.
- **Seed (cluster dừng):** tailnet `a`, `b` ACTIVE (policy accept `*` trong tailnet), `c` mint key rồi chuyển SUSPENDED.
- **Lệnh:** `SSH_KEY=~/.mesh-lab/mesh-lab bash infra/lab/e2e/p05/run.sh`

## Kết quả (`artifacts/7bd5a3d/P05/e2e-20261122T201731Z/`, exit 0, 70 s)

| ID | Kết quả | Nội dung |
|---|---|---|
| C01 | PASS | Một Noise key cho cả cluster, không phụ thuộc Host |
| C02 | PASS | Listener public không có admin API v1/v2, `/metrics`, `/debug/`, `/swagger`, `/register/…`, `/oidc/callback` (không route nào 2xx) |
| R01 | PASS | A: a1↔a2; B: b1 không thấy peer nào; cửa sổ quan sát 5 s |
| R02 | PASS | IPAM theo tailnet: node đầu của A và B cùng nhận 100.64.0.1 |
| R03 | PASS | Data plane trong A: `tailscale ping` a1→a2 |
| R04 | PASS | Một thiết bị, profile A và B: 2 node, cùng machine key, mỗi profile chỉ thấy tailnet của nó; switch về A giữ node key |
| R05 | PASS | Key không tồn tại bị server từ chối ("invalid pre auth key"), không tạo node |
| R06 | PASS | Key của tailnet SUSPENDED bị từ chối với **cùng** thông báo |
| R07 | PASS | Đăng ký web/OIDC bị từ chối với thông báo rõ, không có auth URL |

- **Lặp lại:** `2e8096d` 9/9 PASS (68 s); `7bd5a3d` 9/9 PASS (70 s).
- **Lần đầu (`d0ad0f6`+dirty):** 9/9 PASS nhưng mất 176 s, vì từ chối trả 401 khiến client retry (64 request 401). Sau khi đổi sang `200 + Error`, lần chạy cuối có 0 lần 401.
- **Redaction:** `grep -E 'hskey-auth-[A-Za-z0-9]'` trên artifact cho 0 kết quả chưa redact.
- **Cleanup:** 0 container và 0 volume nhãn `mesh-e2e=p05` trên mc-ci; DB E2E đã xóa; `headscale-cluster` inactive, `headscale-tailnets` active.
- **Không bị ảnh hưởng:** `ts-home1` vẫn chạy.

## Unit / servertest
- `go test -count=2 -race -tags p05cluster ./hscontrol/servertest/ -run Cluster` @ `65f33cc` trên mc-ci (16 vCPU): exit 0, 149.9 s.
- `go build ./...`, `go vet` (cả tag `p05cluster`): exit 0.

## Known limitations / security blockers
- Trong lúc E2E chạy, các client lab cũ trỏ vào hostname POC (`ts-home1`, `vps-ext`) gặp lỗi Noise handshake (khác key) và tự nối lại sau khi POC chạy lại. Trong log cluster thấy đây là các dòng `decrypting machine key: ... authentication failed`.
- Mọi request đến qua VPS L4 nên log chỉ thấy `remote=10.99.77.1`. Muốn có IP thật của client cần PROXY protocol (ticket sau).
- Lệnh quản trị `cluster tailnet …` phải chạy khi cluster dừng: runtime chỉ nạp tailnet và policy lúc khởi động (P10 làm reload).
- **Chưa kiểm** E2E cho I01 (đổi tenant trên cùng phiên Noise) bằng client thật: client chính thức không bao giờ gửi request như vậy. I01 được kiểm bằng phiên Noise thô trong servertest.
- **Reviewer approval:** chưa.
