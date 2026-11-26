# Source inventory — P14f F3 (delta-first cho danh sách peer và PacketFilters)

- **Commit đọc:** `570321b` (`feat/P14f-netmap-delta`, = `master` `483afb0` + tài liệu ADR 0015 / ticket P14f).
- **Ticket / ADR:** `tickets/P14f.md` §F3; ADR 0015 D2, D3; ADR 0005 §"Netmap tăng dần"; `evidence/source-inventory-P00d.md` §3.4–3.5, R5–R7, S1.

## Path & symbol đã đọc

| Path | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/mapper/batcher.go` | `generateMapResponse` (84–170) | `RequiresRuntimePeerComputation` → `computePeerDiff` (removal theo cái đã gửi) + `policyChangeResponse` với **mọi** peer hiện tại; `resp.Peers != nil` → removal theo full list |
| | `withRemovals` (194) | removal đi frame riêng, trước |
| | `handleNodeChange` (209–244) | `nc.send` rồi `updateSentPeers(resp)` (theo node) |
| | `AddNode` (306–477) | initial map gửi thẳng vào `c` (không qua `mc.send`); ghi `lastSSHPolicy`, `lastSelf` của connection; `pendingInitial` chặn broadcast tới khi xong |
| `hscontrol/mapper/node_conn.go` | `connectionEntry` (61–89), `withSelfDelta` (93), `withSSHPolicyDelta` (108) | **mẫu có sẵn**: trạng thái đã gửi theo **từng connection**, bỏ phần bằng nhau trong `send` |
| | `multiChannelNodeConn.send` (467–609) | mỗi connection một bản đã lược; ghi `lastSelf`/`lastSSHPolicy` chỉ khi gửi thành công; connection bị backpressure không ghi |
| | `updateSentPeers`, `computePeerDiff` (679–730) | `lastSentPeers`/`partialPeers` theo node: nguồn removal (giữ nguyên) |
| `hscontrol/mapper/mapper.go` | `policyChangeResponse` (317–339), `buildFromChange` (343–) | PacketFilters + SSH + Self + `UserProfiles(currentPeers)` + `PeersChanged(currentPeers)` |
| `hscontrol/mapper/builder.go` | `WithPacketFilters` (242) | `PacketFilters = {"base": filter}`; client: `PacketFilters == nil` = giữ filter cũ |
| | `WithUserProfiles` (204) | profile của chính node + user của các peer truyền vào |
| `hscontrol/mapper/incremental.go` | `incrementalNetmap`, `SendFullMaps`, `removalsFromSent` | công tắc P00d đọc từ `mapper.cfg` |
| `hscontrol/types/change/change.go` | `FullUpdate` (422), `PolicyChange` (504), `UserAdded`/`UserRemoved` (656–670) | user thêm/xóa/đổi profile = `FullUpdate` → `addToBatch` ép full map mọi node |
| `hscontrol/state/state.go` | `UpdateUser` (774–857), `DeleteUser` (871–915) | phát `UserAdded` khi profile đổi; `UserRemoved` khi policy không đổi |
| `hscontrol/mapper/batcher_test.go` | `setupBatcherWithTestData`, `TestSSHPolicySentOnlyWhenChanged` | harness dùng cho RED |
| `tailscale.com/control/controlclient` (map session) | `updateStateFromResponse` | `PeersChanged` thay entry; `UserProfiles` cộng dồn theo ID; `PacketFilters` nil = giữ |

## Thiết kế chọn

- Lược theo **từng connection**, cùng chỗ và cùng kiểu với `withSelfDelta`/`withSSHPolicyDelta`: mỗi connection giữ hash của từng entry peer, từng `UserProfile` và `PacketFilters` đã **giao thành công** cho nó. Connection bị backpressure hoặc lỗi không ghi, nên lần sau so với cái nó thật sự có. Connection chưa có mốc (chưa ghi initial map) → không lược (gửi đủ).
- Patch (`PeersChangedPatch`, `OnlineChange`, `PeerSeenChange`) và `PeersRemoved` xóa hash của peer đó → lần sau gửi đủ entry.
- `Peers` (full list) **không bao giờ bị lược**: là lưới an toàn (ADR 0015 D3); chỉ dùng để đặt lại mốc.
- `UserAdded`/`UserRemoved`: khi công tắc ON, `State` trả change đi đường policy-delta (`RequiresRuntimePeerComputation`), không `FullUpdate`.
- **Gắn với công tắc ON**: OFF giữ nguyên đầu ra (golden M05).
- Không đổi điều kiện tính adjacency tăng dần (Q4), không đổi `computePeerDiff`/`removalsFromSent`.

## Giả định chưa kiểm

- A1: JSON của `tailcfg.Node`/`UserProfile`/`FilterRule` tất định (map được sort khi encode) → hash ổn định giữa các lần render cùng nội dung.
- A2: Client 1.84.3 và 1.104.7 cộng dồn `UserProfiles` (không thay cả tập) — đúng với `controlclient` hiện hành; kiểm lại bằng E2E F6.
- A3: Không có trường của entry peer mà client sửa ngoài `PeersChanged`/patch (nếu có, hash sẽ lệch với client).
