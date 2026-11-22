# ADR 0001 — Runtime riêng cho mỗi tailnet

- **Trạng thái:** Proposed
- **Ngày:** 2026-11-22
- **Người quyết định:** chủ dự án (hướng đã chọn); chờ review bảo mật
- **Liên quan:** PLAN §1, §4; tickets P02, P05, P00b

## Bối cảnh

Upstream Headscale là single-tailnet. Toàn bộ state nằm trong singleton (inventory P00 §2):

- `Headscale` (`hscontrol/app.go:91-114`) giữ một `noisePrivateKey`, một `state`, một `mapBatcher`, một `authProvider`. Noise key đọc một lần cho cả process (`app.go:151`).
- `State` (`hscontrol/state/state.go:214-300`) giữ một `nodeStore`, `ipAlloc`, `polMan`, `authCache`, `sshCheckAuth`, `pings`, `registerLocks`.
- `Snapshot` của NodeStore (`hscontrol/state/node_store.go:200-221`): `nodesByNodeKey`, `routes`, `allNodes` là toàn cục; `snapshotFromNodes` (`node_store.go:776-821`) gọi `peersFunc(allNodes)` mỗi lần topology đổi.
- `BuildPeerMap` (`hscontrol/policy/v2/policy.go:831-939`) là O(n²) theo cặp node.
- Batcher broadcast tới mọi node qua `b.nodes.Range` (`hscontrol/mapper/batcher.go:786-802`); `change.Change` (`hscontrol/types/change/change.go:19-57`) không có trường tailnet.

Nếu giữ cấu trúc này và chỉ thêm `tailnet_id` vào từng chỗ, mỗi cache, event và vòng lặp đều phải được lọc đúng; chỉ cần sót một chỗ là lộ dữ liệu giữa tenant. Ngoài ra peer calculation vẫn chạy trên toàn bộ node của process.

## Quyết định

Mỗi tailnet có **runtime riêng** trong process:

| Theo tailnet | Dùng chung trong process |
|---|---|
| NodeStore, PolicyManager, Batcher, IPAM, authCache (và `sshCheckAuth`, `pings`, `registerLocks`), cấu hình DERP/DNS, Noise key | DB handle (qua repository đã scoped theo `TailnetID`), listeners (HTTP/TLS, metrics riêng), lease client tới placement registry |

- Listener chọn runtime bằng `VerifiedScope` (binding edge + Noise session). Không có runtime mặc định; không tìm thấy → fail closed.
- Repository DB được tạo với `TailnetID` cố định; không có method unscoped trên đường dữ liệu tenant.
- Runtime được dựng khi tailnet `ACTIVE` và hủy khi `SUSPENDED`/`DELETING`/sau khi move (P12b).

**Chi phí lý thuyết:** peer calculation là Σnᵢ² thay vì (Σn)². Ví dụ 10 tailnet × 1.000 node: 10 × 10⁶ = 10⁷ cặp so với 10⁸. Đây là phép tính, **không** phải số đo; P00b và L1–L4 phải xác nhận.

## Phương án thay thế

1. **Một runtime chung, thêm `tailnet_id` vào mọi cache/query/event.** Gần upstream nhất, ít thay đổi cấu trúc. Bị loại: peer calc vẫn (Σn)²; isolation phụ thuộc việc không sót bộ lọc nào; khó chứng minh I02/I07.
2. **Một process cho mỗi tailnet.** Isolation mạnh nhất (kể cả crash). Bị loại cho lớp *shared*: chi phí process, cổng, cấu hình và vận hành tăng theo số tenant. Vẫn tương thích về mặt khái niệm với lớp *dedicated* (một tenant mỗi process).
3. **Runtime chung nhưng partition NodeStore theo tailnet, còn Batcher/PolicyManager chung.** Giảm một phần chi phí nhưng vẫn giữ các đường dùng chung dễ lẫn (broadcast, policy cache). Bị loại vì không đạt "isolation do cấu trúc".

## Hệ quả

- Tốt: isolation có sẵn do cấu trúc; một thay đổi ở A không chạm vào dữ liệu của B (I07). Chi phí peer tách theo tailnet. Move một tailnet (P12b) chỉ cần dựng/hủy một runtime.
- Chi phí: mỗi runtime có goroutine, worker và bộ nhớ nền riêng; với nhiều tenant nhỏ cần giới hạn worker và dùng fair scheduling (I10). Phải đo RAM idle mỗi tailnet (L1).
- Còn rủi ro toàn process: `RegisterProtocol` sửa `http.DefaultTransport` toàn cục (`app.go:288-293`), TailSQL (`app.go:897`), debug handler (`hscontrol/debug.go:30-45` cho qua mọi IP private), DERP verify tra node key toàn cục (`hscontrol/handlers.go:166-189`). Các điểm này phải được xử lý riêng (P08).
- Panic trong một runtime vẫn có thể làm sập cả process: **không** được quảng bá là hard isolation (PLAN §0).
- Refactor lớn ở `hscontrol/app.go`, `state/`, `mapper/`; phải làm từng PR nhỏ (P02, P05–P07).
