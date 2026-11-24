# PR P07 Evidence — source inventory: mapper, event, MapResponse delta theo tailnet

- **Trạng thái:** `Verified` (cluster suite `-race`, mutation, E2E client thật 29/29 ×2; §5–§8); chưa `Reviewed`, chưa merge. §2–§4 là inventory tại `e9c7137`.
- **Nhánh:** `feat/P07-mapper-scoped` (từ `master` `e9c7137`, đã có P00d và P06).
- **Phạm vi (PLAN dòng 276, 291; `06-tests-benchmarks.md` S12, §1c):** I07 (thay đổi ở A không đổi danh sách peer hay event của B), S12 (A gửi MapRequest/cập nhật node trong lúc B đang nhận map: B không nhận event hay delta nào), chạy lại M01/M03 của P00d với nhiều tenant, E2E không lộ key/netmap chéo kể cả khi kết nối lại.

## 1. Commit

| | Commit |
|---|---|
| Before | `e9c7137` |
| After | `fbf0e53` |

## 2. Path & symbol đã đọc (tại `e9c7137`)

Đọc rộng bởi một agent đọc nguồn; các điểm có dấu ✓ đã kiểm trực tiếp.

| Chủ đề | Path:dòng | Phát hiện |
|---|---|---|
| Gắn phiên ✓ | `cluster.go:687` `NoiseUpgradeHandler`; `noise.go:853` `PollNetMapHandler` → `bindForNode` (`session_route.go:114`) → `Cluster.runtimeForNode` (`cluster.go:657`) → `Platform.TailnetForNode` (`db/platform.go:222`, `LIMIT 2`, khác đúng 1 hàng → từ chối) | Phiên gắn runtime **một lần** (`session_route.go:45` `bind`); runtime khác sau đó → `ErrSessionTenantSwitch`, phiên bị đóng; runtime đã retire → `errRuntimeRetired` |
| Sau khi gắn | `noise.go:935` `newMapSession`, `:1225` `getAndValidateNode` | Mọi xử lý qua `ns.headscale` của phiên; batcher (`mapper/batcher.go:58`), mapper, State là trường của runtime. Không có map toàn process theo node ID |
| Node ID ✓ | `db/schema.sql:110` (`AUTOINCREMENT`), `schema_postgres.sql:83` (`bigserial`) | Một sequence cho mọi tailnet: ID không trùng giữa tailnet |
| Change | `app.go:1416` `Headscale.Change`; `cluster.go:329-428` `reconcile` | Change chỉ vào batcher/ephemeral GC của chính runtime; reconcile gọi `rt.h.Change` theo revision của chính tailnet đó |
| Ping | `cluster.go:701-721` `PingResponseHandler`; `state/ping.go:11,59` | Hỏi mọi runtime `CompletePing(id)`; ID 16 byte hex ngẫu nhiên theo tracker từng runtime; hoàn tất ping không phát change |
| Cache theo machine key | `state/state.go:249,313,321`; `node_store.go:1121` | Auth cache, `registerLocks`, `GetNodesByMachineKeyAllUsers` đều thuộc State/NodeStore từng runtime |
| Trạng thái toàn process | Prometheus (`mapper/batcher.go:35,43`, `metrics.go:25-39`, `state/node_store.go:50+`, `node_store_reconcile.go:22`, `state.go:2258`); `mapper/node_conn.go:192` `connIDCounter` | Không mang dữ liệu tenant; metric divergence P00d cộng dồn mọi tenant (không biết tenant nào lệch) |
| `metrics.go:11` `mapResponseLastSentSeconds{id}` | chỉ khi `HEADSCALE_DEBUG_HIGH_CARDINALITY_METRICS` | label node ID, không label tailnet; listener metrics chỉ cho operator (`cluster.go:795`, `operatorOnly`) |
| Dump MapResponse | `mapper/mapper.go:32` envknob `HEADSCALE_DEBUG_DUMP_MAPRESPONSE_PATH`, `:476`, `builder.go:134,435` | **Một thư mục cho mọi runtime**: MapResponse của mọi tenant ghi chung, tên thư mục con theo node ID. Chỉ khi operator bật envknob, nhưng trộn dữ liệu tenant |
| Debug | `debug.go:103,187`; `app.go:897-914` | Server debug chỉ chạy ở `serve` một tailnet; `Cluster.HTTPHandler` (`cluster.go:806-830`) không có route debug/admin/metrics |
| DERP map ✓ | `config.ForTailnet` sao chép nông (`DERP.DERPMap` chung con trỏ); `app.go:554,1599` | Chỉ sửa tại chỗ khi bật DERP nhúng, mà cluster từ chối (`errClusterEmbeddedDERP`); `derp.ApplyPolicy` clone region |
| Harness | `servertest/cluster_p05.go:112-198` | `NewTestCluster` cứng config; **không bật được** `tuning.incremental_netmap` |

## 3. Test có sẵn

`cluster_p05_test.go`: `TestClusterTwoTailnetsPeersIsolated`, `TestClusterSameMachineKeyTwoTailnets` (xóa node A → B không có frame), `TestClusterSessionCannotSwitchTenant`, `TestClusterOverlapAndConfigIndependence` (policy/rename ở A im lặng ở B, `assertQuiet` 3 s). `cluster_p06_test.go`: exit node của B → A không frame; derpMap của A → B không frame. `cluster_p06_dns_test.go`: DNS của A → B không frame.

**Chưa có:** cập nhật node do MapRequest của A (hostinfo, endpoint, DERP home) và online/offline trong lúc B nhận map (lõi của S12); join/xóa ở A với B quan sát; ephemeral GC; kết nối lại hàng loạt; hai tenant với netmap tăng dần (M01/M03 nhiều tenant), full map định kỳ và đối chiếu khi có hai tenant; race stress hai batcher.

## 4. Kết luận và kế hoạch

Về cấu trúc, đường map/event đã tách theo runtime; inventory không tìm thấy đường rò. P07 vì vậy chủ yếu **chứng minh bằng test** (kèm mutation chứng minh test bắt được), và sửa một điểm nhỏ:

1. **Harness:** `NewTestClusterWith(tb, ClusterOptions{IncrementalNetmap: bool}, seeds...)`.
2. **S12 / I07 (cluster, client thật in-process, chạy cả công tắc OFF và ON):** ở A lần lượt: đổi hostinfo/endpoint qua MapRequest, ngắt/kết nối lại, join, xóa node, node ephemeral bị dọn; sau mỗi thao tác B **không nhận frame nào** và netmap của B không chứa gì của A; A nhận đúng.
3. **M01 nhiều tenant:** chuỗi thao tác ngẫu nhiên (rapid, số bước nhỏ) trên A và B với công tắc ON; sau mỗi bước mỗi client có đúng tập peer của runtime mình, không có node tailnet khác, và tailnet không bị thao tác thì im lặng.
4. **M03 nhiều tenant:** tiêm lệch vào adjacency của A, đối chiếu sửa; chỉ node của A nhận full map, B im lặng; full map định kỳ của A không gửi tới B.
5. **Kết nối lại:** 20 lần ngắt/kết nối lại liên tiếp ở A trong lúc B nhận map: B im lặng, A hội tụ đúng.
6. **Race:** test 2–5 chạy dưới `-race`.
7. **Sửa nhỏ:** thư mục dump MapResponse thêm cấp thư mục theo tailnet ID ở cluster mode (`<dir>/<tailnet>/<node>`), để dữ liệu các tenant không lẫn.
8. **Mutation** (bản sao tạm, không commit): ví dụ change của A cũng được đẩy vào batcher B; NodeStore/batcher dùng chung; reconcile gửi full map cho mọi runtime; ListPeers không lọc tailnet.
9. **E2E (harness P05):** client của A đổi hostinfo (route quảng bá) và kết nối lại nhiều lần trong lúc b1 quan sát: b1 không thấy node hay key nào của A.

## 5. Thay đổi

| Commit | Nội dung |
|---|---|
| `f642741` | inventory |
| `ec9d9d2` | `servertest.NewTestClusterWith(ClusterOptions{IncrementalNetmap, FullMapInterval})`; 4 test P07; `Headscale.ReconcileNetmap` (tác vụ định kỳ gọi lại nó); `State.TailnetID`; dump MapResponse `<dir>/<tailnet>/<node>` ở cluster mode (`serve` một tailnet giữ `<dir>/<node>`); test `TestDebugDumpPerTailnet` |
| `fbf0e53` | E2E P05 ca S12 |

Không đổi đường map/event chạy thật: inventory không tìm thấy rò rỉ và test xác nhận điều đó.

## 6. Tests + lệnh + exit code (mc-ci)

| Test | Kiểm | OFF | ON |
|---|---|---|---|
| `TestClusterP07NodeActivityInASilentInB` (S12, I07) | hostinfo + endpoint qua MapRequest, offline/online, join, xóa ở A: A nhận đúng; client B **không frame nào** trong 3 s sau mỗi thao tác; netmap B không có node/key/IP/FQDN nào của A | pass | pass |
| `TestClusterP07ReconnectStormInASilentInB` | 20 lần ngắt/kết nối ở A: B im lặng, A hội tụ | pass | pass |
| `TestClusterP07IncrementalAcrossTenants` (M01 nhiều tenant) | 12 bước ngẫu nhiên có seed (join/xóa/hostinfo ở A hoặc B): mỗi client = peer của runtime mình, tenant kia im lặng; frame bất ngờ được ghi kèm delta peer; `P07_SEED` tái hiện | — | pass (nhiều seed) |
| `TestClusterP07ReconcileStaysInTailnet` (M03 nhiều tenant) | lệch tiêm vào A, `ReconcileNetmap` của A sửa (đúng 2 node), của B không có gì; B im lặng | — | pass |

| Lệnh | Exit |
|---|---:|
| `go build ./... && go vet` (hscontrol, mapper, state, servertest `-tags p05cluster`); `gofmt -l` các file đổi | 0 |
| `go test -race -count=1 ./hscontrol/mapper/ ./hscontrol/state/` | 0 |
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | 0 (444 s; mọi test cluster P05/P06/P07/P10) |

**Điều tra frame "bất ngờ" (không phải rò rỉ):**
1. Lần chạy đầu, với `full_map_interval = 1m` thì B nhận frame trong cửa sổ im lặng. Bộ làm mới chia chu kỳ thành 60 phần, nên với 1m nó gửi full map mỗi giây cho node của **chính** B. Ngay cả 1h cũng gửi mỗi phút: tái hiện đúng seed `1795513686994701536` bước 9 (node-1 của A nhận netmap "no peer change" ở giây thứ ~60 trong lúc B join). Với 24h, seed đó pass 2/2. Harness giờ đặt 24h.
2. Báo "leak" token `"a2"`, `"b4"`: tên client ngắn khớp chuỗi con trong key hex; JSON netmap in ra chỉ có node của chính tenant. Đổi sang tên dài thì hết.

## 7. Mutation

Bản sao tạm trên mc-bench, không commit; lần đối chứng không đột biến: pass.

| Mutation | Bị bắt bởi |
|---|---|
| P1 `Headscale.Change` của một runtime cũng đẩy vào batcher mọi runtime khác | `TestClusterP07NodeActivityInASilentInB` |
| P2 đối chiếu của A gửi full map cho node của runtime khác | `TestClusterP07ReconcileStaysInTailnet` |
| P3 dump bỏ cấp tailnet | `TestDebugDumpPerTailnet` |

Ghi chú P1: test kết nối lại hàng loạt và test ngẫu nhiên **không** bắt P1, vì change patch/`NodeAdded` của A lọt vào batcher B bị B lọc theo adjacency của B và không sinh frame. Không rò dữ liệu, nhưng chỉ test S12 (có đổi hostinfo, sinh change hiển thị) phát hiện được đường lọt.

## 8. E2E với client thật

Harness `infra/lab/e2e/p05/run.sh` thêm ca S12: a2 (tailnet A) đổi `--advertise-routes` và `tailscale down/up` 3 vòng; mỗi vòng b1 không có peer, `tailscale status --json` của b1 không chứa node key hay hostname nào của A; a2 về `Running`, a1 thấy lại a2. Binary `hs` sha256 `cde8c6904e5d89c1…`, `mesh-bootstrap` `3b1821814b5699bd…`, client `tailscale/tailscale:v1.104.7`.

| Lần | Kết quả | Artifact (không commit) |
|---|---|---|
| 1 | **29/29 PASS**, cleanup_ok=1, 0 key chưa redact | `artifacts/fbf0e53/P05/e2e-20261124T102343Z` |
| 2 | **29/29 PASS**, cleanup_ok=1, 0 key chưa redact | `artifacts/fbf0e53/P05/e2e-20261124T103159Z` |

## 9. Migration, hạn chế, review

- Không có migration. Không đổi đường nóng (không benchmark).
- Hạn chế:
  - E2E không đếm được frame ở client thật (CLI không lộ); "không frame" chỉ kiểm ở servertest.
  - Metric divergence P00d vẫn cộng dồn mọi tenant.
  - Chưa có race test riêng ép hai batcher song song ngoài các test trên (chạy `-race`).
  - Ephemeral GC, HA prober/route failover, SSH-check ở A khi B quan sát: chưa có test riêng.
- Race toàn repo (`./...`) chưa chạy lại cho P07 (đã chạy các package đổi + cluster suite).
- Reviewer approval: chưa có.
