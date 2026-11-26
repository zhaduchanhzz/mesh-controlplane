# Source inventory — P14f F4 (danh sách peer dựng một lần, dùng chung)

- **Commit đọc:** `fd6008d` (`feat/P14f-f3`: F1 + F2 + F3).
- **Ticket / ADR:** `tickets/P14f.md` §F4, ADR 0015 D4.

## Path & symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/mapper/builder.go` | `WithPeers`, `WithPeerChanges`, `buildTailPeers`, `WithUserProfiles` | Mọi danh sách peer (full map, `PeersChanged`, policy change) đi qua `buildTailPeers`: mỗi người nhận gọi `peer.TailNode(...)` cho **từng** peer → O(n) render mỗi người nhận, O(n²) cho cả tailnet |
| `hscontrol/mapper/mapper.go` | `buildFromChange` (`SendAllPeers` → `ListPeers` + `WithPeers`), `policyChangeResponse`, `fullMapResponse` (unused) | |
| `hscontrol/types/node.go` | `NodeView.TailNode` | Không nhận người xem. Đầu vào: node (trường snapshot), `cfg` (base domain, taildrop, cert, autoupdate: cố định theo process), `selfPolicyCaps` (chỉ đọc `disable-ipv4` của **peer**), `routeFunc`, `time.Now` qua `IsExpired` |
| `hscontrol/state/state.go` | `RoutesForPeer` | **Phụ thuộc người xem**: `ViaRoutesForPeer(viewer, peer)`, `ReduceRoutes(viewer, routes, matchers(viewer))`, co-router theo `viewer.SubnetRoutes()`. Peer **không có route** (không subnet route, không exit route, không primary) → kết quả rỗng với mọi người xem |
| `hscontrol/policy/v2/policy.go` | `ViaRoutesForPeer` | "A peer without routes has nothing to steer": `SubnetRoutes()` rỗng và `!IsExitNode()` → rỗng |
| `hscontrol/policy/v2/tailnet_state_caps.go` | `PeerCapMap` | Peer không phải exit node → `nil` |
| `buildTailPeers` (phần sau `TailNode`) | `ipv4Disabled` của **người xem** sửa `AllowedIPs`/`Addresses`; peer hết hạn: sửa `Online`/`LastSeen`/`Endpoints`/`HomeDERP` theo `ActiveSessions` (trường node) | |
| `hscontrol/state/node_store.go` | `Snapshot`, `applyBatch`, `snapshotFromNodesWith` | Mỗi snapshot tạo **con trỏ mới cho mọi node** (`n.View()` trên biến vòng lặp), dù chỉ một node đổi → không dùng con trỏ làm định danh nội dung được nếu không sửa |
| `hscontrol/mapper/delta_first.go` (F3) | `hashPeer`, `hashResponse`, `sentState.record` | Chỉ **đọc** entry peer; `hashPeer` mã hóa JSON từng peer mỗi response → O(n²) cho full map của cả tailnet |
| `hscontrol/mapper/batcher.go`, `node_conn.go` | `computePeerDiff`, gửi | Chỉ đọc `peer.ID` |

## Kết luận thiết kế

- Entry peer **không phụ thuộc người xem** khi: peer không có route (subnet/exit/primary), peer chưa hết hạn tại lúc dùng, người xem không có `disable-ipv4`. Khi đó entry = hàm của (nội dung node, cờ `disable-ipv4` của peer, base domain). Mọi trường hợp khác render riêng như cũ.
- "Lớp hiển thị" thực tế: entry dùng chung **theo peer** (mọi người xem cùng nhận đúng một entry), nên mọi lớp có cùng peer dùng chung entry; danh sách của mỗi người nhận chỉ là danh sách con trỏ.
- Định danh nội dung: snapshot giữ lại con trỏ view của node **không bị ghi** trong batch (put/updateMulti/setName/del) → con trỏ view đổi khi và chỉ khi node bị ghi.
- Cache theo mapper (một mapper cho mỗi runtime tailnet, ADR 0007) → không bao giờ dùng chung giữa tailnet.
- Hash F3 của entry dùng chung được nhớ theo con trỏ entry (bảng phụ, xóa khi entry bị thay).

## Giả định chưa kiểm chứng

- A1: `cfg` của mapper không đổi trong đời process (base domain theo tailnet cố định). Vẫn giữ base domain trong entry để kiểm.
- A2: Không chỗ nào sửa entry `*tailcfg.Node` sau khi dựng response (grep: chỉ đọc). Test race với nhiều worker.
