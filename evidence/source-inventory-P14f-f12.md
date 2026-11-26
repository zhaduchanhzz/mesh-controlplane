# Source inventory P14f F1 + F2 (netmap ON mặc định; oracle replay M07/M08)

- **Commit trước:** `570321b` (`feat/P14f-netmap-delta`, = `master` `483afb0` + tài liệu F0).
- **Ticket / ADR:** `tickets/P14f.md` §F1, §F2; ADR 0015 (D1, bảng bất biến M07/M08); ADR 0005 §"Netmap tăng dần"; `evidence/source-inventory-P00d.md`.
- **Đọc bằng** `git grep`/`sed` trên cây sạch tại `570321b`; module Tailscale đọc trên mc-ci (`tailscale.com@v1.105.0-pre.0.20261112091541-a2da5fbcaa53`).

## Path & symbol

| Path | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/config/default.go:92` | `Default().Tuning.IncrementalNetmap` | `Enabled: false`, 60 s / 15 m |
| `hscontrol/config/default_test.go:45` | `TestDefault…` | khẳng định OFF (đổi thành ON = RED của F1) |
| `hscontrol/config/validate.go:334` | ràng buộc interval chỉ khi bật | không đổi |
| `hscontrol/types/config.go:1038,1136` | `Tuning.IncrementalNetmap`, `IncrementalNetmapConfig` | chú thích "Off by default", "Experimental: keep off" |
| `hscontrol/types/config.go:3-4` | `go:generate` `tools/configgen` (bảng docs, file ví dụ) | sinh `config_docs_generated.go`, `docs/ref/*`, `nix/options.generated.nix` |
| `hscontrol/app.go:453` | ticker đối chiếu + `RunFullMapRefresh` khi bật | log "(experimental)" |
| `hscontrol/state/state.go:477,543` | `SetIncrementalPeers`, `WithLocalNodeChanges` khi bật | |
| `hscontrol/mapper/incremental.go:17` | `Batcher.incrementalNetmap()` | |
| `hscontrol/mapper/netmap_golden_test.go:99` | M05 golden | cấu hình riêng, đặt `Enabled` tường minh: không bị ảnh hưởng bởi mặc định |
| `hscontrol/servertest/server.go:184` | `NewServer` dùng `config.Default()` | sau F1 chạy ON; `Serve` không chạy nên không có ticker |
| `hscontrol/servertest/cluster_p05.go:179,286` | `NewTestClusterWith`, `ClusterOptions.IncrementalNetmap` | bật = 1 h / 24 h; sau F1 mặc định cũng bật (ON = mặc định production) |
| `hscontrol/servertest/cluster_p07_test.go:31` | `p07Modes` (ON/OFF) | cần OFF tường minh |
| `hscontrol/servertest/client.go` | `TestClient` bọc `controlclient.Direct` thật; `UpdateFullNetmap` nhận netmap của map session | client mô phỏng của M07 = map session thật của Tailscale |
| `hscontrol/mapper/batcher.go:930` | `Batcher.MapResponseFromChange(id, change.FullSelf(id))` | dựng full map đồng bộ, không ghi `lastSentPeers` (đường initial map, `batcher.go:613-660`) = oracle |
| `tailscale.com/control/controlclient/map.go` | `mapSession.updateStateFromResponse` (368-), `updatePeersStateFromResponse` (550-) | `PacketFilter` → filter tên `base`; `PacketFilters`: `"*": nil` xóa hết, `k: nil` xóa, còn lại đặt; netmap = nối các filter theo tên đã sort; `DNSConfig`, `SSHPolicy` nil = giữ; `DERPMap` hợp nhất; `PeersRemoved`, `PeerSeenChange`, `OnlineChange`, `PeersChangedPatch` |

## Thiết kế F2 (lý do chọn harness)

- Harness: `servertest` cluster mode (`-tags p05cluster`), **hai tailnet trên một process**, client là `controlclient.Direct` thật. Trạng thái client sau mọi delta là netmap do map session thật của Tailscale dựng ⇒ không phải viết lại ngữ nghĩa client (một client tự viết có thể sai giống server). Oracle: full map mới dựng cho từng node bằng `MapResponseFromChange(FullSelf)` trên runtime của tailnet.
- Thay đổi ngẫu nhiên bằng `pgregory.net/rapid`; mỗi check dựng một cluster mới (đắt) ⇒ số check/bước nhỏ mặc định, chạy dài qua biến môi trường.

## Giả định chưa kiểm

- A1: `MapResponseFromChange(FullSelf)` trên node đang kết nối không đổi trạng thái gửi (đọc code: đúng, `batcher.go:640-643`).
- A2: Các trường client tự sửa (`upgradeNode`, tên hiển thị) không làm lệch các trường được so; chuẩn hóa ghi trong test.
- A3: Tailscale client giữ `UserProfiles` cũ; chỉ so profile của user đang được tham chiếu.
