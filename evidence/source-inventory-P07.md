# PR P07 Evidence — source inventory: mapper, event, MapResponse delta theo tailnet

- **Trạng thái:** `Planned` (inventory + kế hoạch). Chưa đổi code.
- **Nhánh:** `feat/P07-mapper-scoped` (từ `master` `e9c7137`, đã có P00d và P06).
- **Phạm vi (PLAN dòng 276, 291; `06-tests-benchmarks.md` S12, §1c):** I07 (thay đổi ở A không đổi danh sách peer hay event của B), S12 (A gửi MapRequest/cập nhật node trong lúc B đang nhận map: B không nhận event hay delta nào), chạy lại M01/M03 của P00d với nhiều tenant, E2E không lộ key/netmap chéo kể cả khi kết nối lại.

## 1. Commit

| | Commit |
|---|---|
| Before | `e9c7137` |
| After | chưa có |

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

## 5. Tests / E2E / migration / benchmark / review

Chưa chạy. Không có migration. Không phải hot path mới (benchmark không bắt buộc). Reviewer approval: chưa có.
