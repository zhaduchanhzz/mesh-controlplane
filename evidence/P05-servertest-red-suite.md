# P04+P05 servertest — bộ test RED cho cluster nhiều tailnet (ADR 0007)

- **Trạng thái:** bộ test `Implemented` (đã viết, compile được với stub hợp đồng). **Chưa** `Verified` trên code thật: `hscontrol.Cluster`, `db.Platform`, `db.ErrUnknownTenant`, `hscontrol.ErrSessionTenantSwitch` chưa tồn tại ở `ce75c1b`. Đây không phải bằng chứng P04/P05 đã xong.
- **Repo:** worktree `wt-p05-tests`, nhánh `agent/p05-tests`, tách từ `feat/P05-multitenant-runtime` @ `ce75c1b6cebb372fc63d0ebfcc624b951d73882f`. Toolchain `go1.27.4 windows/amd64`.
- **Hợp đồng:** `design/adr/0007-multi-tenant-runtime-and-session-routing.md` (mục "Hợp đồng code" và "Test bắt buộc").

## Build tag `p05cluster`

Mọi file mới đều có `//go:build p05cluster`. Không có tag thì `go build ./...`, `go vet ./...` và CI không thấy chúng, nên nhánh vẫn xanh trong lúc chưa có implementation. PR hiện thực ADR 0007 phải bỏ tag (hoặc thêm tag vào CI) để bộ test thành gate.

```bash
go test -tags p05cluster ./hscontrol/servertest/ -run Cluster -count=1 -v
```

Trên `ce75c1b` lệnh này **không compile** (thiếu các symbol hợp đồng). Đó là trạng thái mong đợi.

## File mới

| File | Nội dung |
|---|---|
| `hscontrol/servertest/cluster_p05.go` | `TestCluster`: seed bảng `tailnets` (owner/status/epoch), mint key và node cho tailnet không có runtime qua handle scoped, rồi chạy `hscontrol.NewCluster` + `(*Cluster).Serve(ctx)` trên một listener loopback thật, một Noise key, một hostname. `Tenant(slug)` trả view `*TestServer` của runtime (`Cluster.Runtime(id)`); `NodeRows` đọc bảng `nodes` không qua scope làm oracle "không có node nào được tạo ở đâu cả"; `Platform()` mở `db.OpenPlatform`. |
| `hscontrol/servertest/cluster_client_p05.go` | `NewClusterClient`/`TryNewClusterClient`: `controlclient.Direct` thật, cho phép **dùng chung machine key** (hai profile của một thiết bị), resume `persist.Persist`, hoặc bỏ qua login. `RawNoiseSession`: client Noise thô dựa trên `tailscale.com/control/ts2021`, **đúng một kết nối** (không redial), theo dõi việc server đóng phiên; gửi `LiteMap` và `Register` với header tùy ý. |
| `hscontrol/servertest/cluster_p05_test.go` | 7 test dưới đây. |

Không sửa file harness có sẵn (`harness.go`, `server.go`, `client.go`, ...).

## Danh sách test

Nguyên tắc chung (ADR để ngỏ mã HTTP và nội dung lỗi): assert **thuộc tính an ninh** — bị từ chối, không có node row mới/đổi trong bất kỳ tailnet nào, response/netmap không chứa dữ liệu tenant khác — không assert chuỗi lỗi chính xác. Mọi tenant đều có `PolicyEmptyDenies`; test cài policy allow tường minh khi cần thấy peer.

| Test | S/I | Chứng minh gì |
|---|---|---|
| `TestClusterTwoTailnetsPeersIsolated` | S02, I02 | A (2 client) và B (3 client) cùng URL; policy allow-all + SSH. Mỗi client thấy **đúng** peer của tenant mình (so theo node key với state của runtime), địa chỉ peer khớp IPAM của tenant, `ListPeers` phía server không ra ngoài tenant. Quét JSON netmap (self, peers, packet filter, SSH policy, DNS, user profiles) không có node key/machine key/disco key/hostname/tên MagicDNS/user/IP riêng của tenant kia. B có client thứ ba để tập IP "chỉ B có" khác rỗng (IPAM độc lập làm hai node đầu trùng IP). |
| `TestClusterAuthKeyPicksTailnet` | S10, S11d, S11 (key lạ) | Key A → node ở A, key B → node ở B, cùng URL/Noise key; row trong DB mang đúng `tailnet_id`; `Platform.TailnetForAuthKey/TailnetForNode` trả đúng tenant. Header giả `X-Tailnet-ID`/`X-Tailnet` không đổi tenant. Key rác, key đúng định dạng nhưng không tồn tại, secret bị sửa, ghép id của A với secret của B (và ngược lại) đều bị từ chối, bảng `nodes` không đổi; key không tồn tại → `db.ErrUnknownTenant`. |
| `TestClusterSameMachineKeyTwoTailnets` | S05 (cùng cluster), I03 | Một machine key: profile A → profile mới ở B (key B) → quay lại A chỉ bằng persist (không key). Hai node độc lập (ID khác, cùng machine key, node key khác), mỗi map stream chỉ có tenant của nó; quay lại A dùng lại node cũ, không tạo node. Hai profile stream **đồng thời** từ cùng machine key vẫn tách biệt. Xóa node A: node B, row B và stream B không bị đụng (không frame nào trong 3 s); `TailnetForNode` của node đã xóa → `ErrUnknownTenant`. |
| `TestClusterSessionCannotSwitchTenant` | I01, S11a, S11b | Trên **một** phiên Noise (một kết nối, không redial): map A rồi map B; map B rồi map A; map A rồi register bằng key B; register bằng key A rồi map B. Request thứ hai bị từ chối, server **đóng phiên**, request bị từ chối không áp endpoint vào node tenant kia, không tạo/đổi node; body không chứa id/slug/node key/hostname của B. Có positive control (request đầu tiên thành công và được áp dụng). `(machine_key, node_key)` không tồn tại, hoặc node key của máy khác → từ chối. Tham chiếu `hscontrol.ErrSessionTenantSwitch` chỉ để log, không assert chuỗi. |
| `TestClusterOverlapAndConfigIndependence` | S06, S07, I07 | Cùng user `alice`, cùng hostname `laptop`/`phone` ở A và B: tên MagicDNS không bị thêm hậu tố; IPAM độc lập (node thứ n của A và B có cùng địa chỉ); lookup "phone" ở mỗi tenant ra đúng node của tenant đó. Đổi policy ở A, đổi tên node (DNS) ở A: client A nhận thay đổi, client B **không nhận frame nào** trong 3 s và filter/tên của B giữ nguyên; chiều ngược lại cho policy B. |
| `TestClusterRefusesForeignOrInactiveTailnet` | I01 (owner fence), S11e | Tailnet của cluster khác, `SUSPENDED`, `PENDING`, `DELETING`: không có runtime; `OwnedTailnets` không chứa tailnet của cluster khác; key của chúng bị từ chối (client thật và Noise thô, không có auth URL); node đã có trong các tailnet đó không map được; body lỗi không lộ id/slug; bảng `nodes` không đổi. Positive control: tailnet ACTIVE của cluster có runtime. |
| `TestClusterWebAuthDisabledInMultiTenant` | P04b scope (ADR 0007 §4) | Đăng ký không có auth key (`LoginDefault`, `LoginInteractive`, và register Noise thô) bị từ chối rõ ràng, **không** trả auth URL đang chờ; body không lộ tenant; không tạo node. |

## Kiểm tra compile và harness (không commit)

1. Nhánh không tag: `go vet ./hscontrol/servertest/` → exit 0; `go build ./...` → exit 0.
2. Bản sao scratch (`git archive HEAD` vào scratchpad, chép 3 file mới vào) cộng **stub hợp đồng** không commit: `hscontrol/cluster_stub_p05.go` (`Cluster`, `NewCluster`, `Serve`, `Runtime`, `ErrSessionTenantSwitch`) và `hscontrol/db/platform_stub_p05.go` (`Platform`, `OpenPlatform`, `TailnetForAuthKey`, `TailnetForNode`, `OwnedTailnets`, `TenantDB`, `ErrUnknownTenant`) đúng chữ ký ADR 0007.
   - `go vet -tags p05cluster ./hscontrol/servertest/` → exit 0. `go list -tags p05cluster` xác nhận 3 file được build.
   - `go test -tags p05cluster -run Cluster` với stub trả lỗi → cả 7 test FAIL tại `NewCluster` (RED). Phần seed (schema, insert `tailnets`, mint key và node qua handle scoped cho tailnet foreign/suspended/pending/deleting) chạy qua trước đó.
3. Smoke harness (không commit): stub `NewCluster` chạy **một** `Headscale` thật cho tailnet `tenant-a` qua `Serve(ctx)`. Một test scratch dùng chính các helper: enroll 3 client, chờ mesh + SSH, `assertTenantView`, tên MagicDNS, `NodeRows`, client chung machine key, `LiteMap` thô áp endpoint và giữ đúng một kết nối, register thô, key rác bị từ chối, policy thu hẹp đổi frame → PASS (12 s). Đã kiểm tra bộ quét leak thật sự thấy node key, IP, hostname, SSH principal và user profile trong JSON netmap (không quét rỗng). Cũng thấy upstream hiện **trả auth URL** khi không có key, nên `TestClusterWebAuthDisabledInMultiTenant` là RED thật.

## Inventory nguồn đã đọc

- `design/adr/0007-multi-tenant-runtime-and-session-routing.md`; `03-isolation-threat-model.md` (I01–I03, I07, quy tắc gắn tenant); `06-tests-benchmarks.md` (S02, S05, S06, S07, S10, S11).
- `hscontrol/servertest/{README.md,harness.go,server.go,client.go,assertions.go,tenant_default_deny_test.go,policy_test.go,sshcheck_test.go}`.
- `hscontrol/app.go` (`NewHeadscale`, `Serve`, `HTTPHandler`, `Change`, `GetState`), `hscontrol/noise.go` (`NoiseUpgradeHandler`, `PollNetMapHandler`), `hscontrol/poll.go` (`serve`, lite update), `hscontrol/handlers.go` (`KeyHandler`).
- `hscontrol/db/{db.go,tenant.go,tenant_test.go,preauth_keys.go,schema.sql}`, `hscontrol/types/{tailnet.go,config.go}`, `hscontrol/config/default.go`, `hscontrol/derp/derp.go`, `internal/tailnet/tailnet.go`.
- `tailscale.com/control/ts2021/client.go`, `tailscale.com/net/tsdial` (pin `v1.105.0-pre.0.20261112091541-a2da5fbcaa53`).

## Giả định (cần lead xác nhận)

1. `db.OpenPlatform(cfg *types.Config) (*db.Platform, error)`; `Platform` có thể có `Close() error` (test gọi nếu có).
2. `Cluster.Serve(ctx)` tự mở listener `cfg.Addr`, unix socket và nạp DERP map như `Headscale.Serve`; runtime có sẵn khi `/health` trả 200. Harness đặt `PolicyEmptyDenies=true`, `ClusterID`, `TailnetID=""`.
3. Tailnet đầu tiên được tạo qua `db.NewHeadscaleDatabase` (tạo schema + tailnet mặc định), các tailnet khác insert trực tiếp như `addTailnet` trong `db/tenant_test.go`, vì chưa có API CreateTailnet (P10).
4. IPAM độc lập theo tailnet với prefix chung của cluster: node đầu tiên của A và B cùng địa chỉ (S06 "IP overlap allowed"). Nếu lead muốn IPAM toàn cluster, assert này phải đổi.
5. Request bị từ chối do đổi tenant làm server **đóng kết nối Noise** (ADR: "phiên bị đóng"); request bị từ chối theo kiểu khác (key lạ) không bắt buộc đóng phiên.
6. Map lite (`Stream=false, OmitPeers=true`) là request map hợp lệ để gắn phiên.

## Khoảng trống — cần harness Docker/client thật (`infra/lab/e2e/p02/run.sh`)

- Đây là client điều khiển trong cùng process, không có `tailscaled`, không data plane: không chứng minh traffic WireGuard/DERP bị chặn giữa A/B, cũng không kiểm tra client thật có trộn hai profile cùng ControlURL không (S05, AGENTS rule 12).
- Chỉ SQLite; đường PostgreSQL dùng chung + PgBouncer (ADR 0006) chưa được chạy bởi bộ này.
- Không có TLS/SNI/Host allowlist (S11c, 421), cổng 80 `/ts2021`, DERP verify theo tenant (S15), debug/metrics (S14).
- S07 với `extra_records`/DNS config theo tenant: chưa có cấu hình DNS per-tenant trong hợp đồng; test chỉ dùng đổi tên node (MagicDNS) và policy.
- Kiểm tra "không có frame" dựa trên cửa sổ 3 s (`assert.Never`), không chứng minh tuyệt đối; I07 bằng hash event/map diff (M01, M03) vẫn cần.
- Thêm/bớt tailnet khi đang chạy (P10), failover/epoch (F-series) ngoài phạm vi.

## Reviewer approval

Chưa có.
