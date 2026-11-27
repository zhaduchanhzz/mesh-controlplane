# ADR 0015 — Netmap tăng dần mặc định, chỉ gửi phần thay đổi (delta-first)

- **Trạng thái:** **Accepted** (hướng) bởi chủ dự án, 2026-11-26. Chi tiết triển khai ở `tickets/P14f.md`; mỗi pha cần RED → GREEN và review bảo mật trước khi bật.
- **Quyết định của chủ dự án (2026-11-26, nguyên văn):** "Dừng toàn bộ các task sau task này và triển khai lại theo hướng vừa bàn. netmap sẽ tăng dần và gửi lại phần thay đổi. chuyển định hướng sang hướng đó. Update document trước sau đó thực hiện code triển khai và test."
- **Liên quan:** ADR 0005 §"Netmap tăng dần" (biện pháp 1–5, vẫn hiệu lực), `tickets/P00d.md` + `evidence/source-inventory-P00d.md` (Q1–Q4, R1–R10, S1–S5), `evidence/P00b-capacity-baseline.md`, `evidence/source-inventory-P00c.md` §6/§9, `tickets/P14.md` §P14f (thay bởi ticket này).
- **Thay đổi so với trước:** ADR 0005 và P00d để công tắc **OFF mặc định** và chỉ tăng dần cho join/xóa node dưới policy toàn cục (Q4). ADR này đổi thành **ON mặc định** và mở rộng nguyên tắc "chỉ gửi phần thay đổi" cho mọi loại thay đổi, kể cả đổi policy.

## Bối cảnh

Số đo hiện có (mc-bench 8 vCPU / 24 GiB):

- 10k `allow_all` không chạy được: 6 161/10 000 node kết nối trong 20 phút, RSS 23,3 GB (P00c §9).
- Peer map giữ lại 938 MB ở 10k `allow_all`, tăng theo n² (P00b).
- Nguồn tốn chính (P00d inventory §3.4, S2): mỗi lần một node **join**, server phát `PolicyChange` tới mọi node đang kết nối, và mỗi node nhận lại **toàn bộ** danh sách peer (`policyChangeResponse`: PacketFilters + Self + mọi peer trong `PeersChanged`). Một join = n MapResponse, mỗi cái O(n) → O(n²) byte. Đổi tag/route/user/policy cũng đi đường này; `UserAdded`/`UserRemoved` ép full map cho mọi node.
- P00d với công tắc ON (phạm vi hẹp Q4) đã cho: 10k `isolated` đăng ký 500 node nhanh hơn ~36 %, byte gửi ít hơn ~10 lần (289,5 → 28,6 MB). `personal_devices` và `allow_all` không đổi vì nằm ngoài phạm vi hẹp.

Giao thức Tailscale vốn hỗ trợ delta trong một phiên map: `PeersChanged`, `PeersRemoved`, `PeersChangedPatch`, `OnlineChange`/`PeerSeenChange`; `PacketFilters` có thể bỏ trống (giữ cái cũ) hoặc gửi mới; `DNSConfig`, `DERPMap`, `SSHPolicy` tương tự. Client 1.84.3 và 1.104.7 đã dùng các trường này với Headscale upstream.

## Quyết định

### D1. Công tắc `tuning.incremental_netmap.enabled` mặc định **ON**

- OFF giữ nguyên làm **công tắc khẩn cấp** (kill switch): hành vi giống hệt full rebuild P00c (golden M05 tiếp tục chạy trong CI).
- Điều kiện của P00d "chỉ bật sau gate N1 và review bảo mật" đã thỏa: N1 đạt, P00d `Reviewed` (2026-11-26, review độc lập + P12c), N1 E2E chạy mỗi lượt ở cả ON và OFF từ P09 tới P12d.
- `reconcile_interval` (60 s) và `full_map_interval` (15 m) giữ nguyên mặc định; P14f đo lại chi phí ở các điểm nhỏ/vừa (R9).

### D2. Delta-first cho **mọi** thay đổi: chỉ gửi phần thay đổi

Sau khi một batch thay đổi được áp dụng (node join/xóa, online/offline, tag, route, exit node, đổi tên, expiry, user thêm/xóa, đổi policy, đổi DNS), mỗi node đang kết nối R chỉ nhận:

| Phần | Gửi khi | Nội dung |
|---|---|---|
| `PeersChanged` | peer mới thấy, hoặc trường của peer đổi | chỉ các peer đó |
| `PeersRemoved` | R **không còn** được thấy peer | ID các peer đó |
| `PeersChangedPatch` / online | chỉ trạng thái/endpoint đổi | patch |
| `PacketFilters` | filter **của R** đổi (so hash theo người nhận) | filter mới của R |
| `SSHPolicy`, `DNSConfig`, `DERPMap`, `Self` | phần đó của R đổi | bản mới của phần đó |
| (không gửi gì) | R không bị ảnh hưởng | — |

Quy tắc tính:

1. **Tính toán phía server vẫn an toàn trước:** đường tăng dần của adjacency giữ phạm vi đã chứng minh (P00d Q4). Mọi trường hợp khác (đổi policy, autogroup:self, via, route/exit, capability) **dựng lại adjacency toàn bộ** như ADR 0005 §2. Delta-first là thay đổi về **cái gửi đi**, không nới điều kiện tính tăng dần.
2. **Phần gửi đi = hiệu (diff)** giữa trạng thái mới của R (dựng đầy đủ hoặc tăng dần đã kiểm) và **cái client R đã nhận** (`lastSentPeers`/`partialPeers`, hash filter/SSH/DNS đã gửi), không phải giữa hai snapshot server. Vì thế removal không phụ thuộc vào adjacency cũ có đúng hay không (R5, R6 của P00d).
3. **Thu hồi/xóa không bao giờ bị gộp hay bỏ:** hàng đợi đầy → full map cho riêng R (đã có, M04).
4. **Không gửi chéo tenant:** diff tính trong runtime của tailnet; batcher, cache và hàng đợi theo runtime (ADR 0007). Một delta chứa peer của tailnet khác = lỗi isolation = BLOCK MERGE (AGENTS 13).

#### D2 bổ sung (review bảo mật độc lập P14f, 2026-11-27; `evidence/P14f-review.md`)

- **Lưới an toàn gửi đủ (R3):** một frame có danh sách `Peers` đầy đủ (phiên mới, full map định kỳ, đối chiếu, hàng đợi gộp thành full) **không bao giờ** bị lược `PacketFilters` hay `UserProfiles`, và đặt lại mốc so sánh của kết nối. Nếu trạng thái đã ghi lệch với client vì bất kỳ lý do nào (kể cả trùng hash), lần full map kế tiếp sửa cả filter.
- **Bộ nhớ theo kết nối (R2):**
  - mốc so sánh nén thành một từ 8 byte cho mỗi peer: 48 bit hash entry và 16 bit hiện diện (Online chính xác, dấu vân tay 14 bit của LastSeen; trùng dấu vân tay chỉ có thể giấu thay đổi LastSeen, không phải quyền truy cập);
  - đo được 29,6 B/peer ở 10k peer và 23,6 B/peer ở 50k (trước: 65,6 và 52,5);
  - kết nối giữ quá `maxBaselinePeers` (100 000) peer thì không giữ mốc nữa (gửi đủ, an toàn) và tăng `headscale_mapper_delta_baseline_dropped_total`.
- **Giới hạn quan hệ peer áp cho ghi policy (R2):**
  - `tuning.max_peer_entries` trước chỉ chặn node mới join. Nay một policy làm tổng peer entry của tailnet vượt giới hạn **và** lớn hơn mức hiện tại bị từ chối ở mọi đường ghi: `setACL`/`validate` của tenant (400 / 200 kèm lý do), `UpdatePolicy` (API v1, operator), CLI `headscale tailnet policy` (chặt: không có số hiện tại).
  - Đếm bằng bản dựng peer map theo lớp ở chế độ chỉ đếm, dừng khi vượt giới hạn, nên chi phí bị chặn bởi giới hạn, không theo n².
  - Ghi bị từ chối không lưu gì, không đổi revision và không ghi audit (theo hợp đồng P12d: audit chỉ ghi lần ghi đã lưu).
- **Memo hash của entry dùng chung (R1)** thuộc cache của từng mapper (đi cùng runtime), không còn biến toàn process.

### D3. Lưới an toàn gửi lại bản đầy đủ (giữ và kiểm lại)

Full map vẫn được gửi khi:

- **mở phiên map mới**: client khởi động lại, đổi mạng, server restart, failover, chuyển tenant;
- hàng đợi delta của node bị đầy;
- **định kỳ** (`full_map_interval`, trải đều theo node);
- **đối chiếu nền** phát hiện lệch (`reconcile_interval`): metric `…_netmap_divergence_total{kind}`, log đã redact, thay snapshot, full map cho các node bị ảnh hưởng (M03);
- trạng thái đã gửi của R không xác định (ví dụ sau lỗi encode/gửi).

### D4. Dựng danh sách peer một lần theo lớp, dùng chung (pha sau)

Cho các full map ở D3 (nhất là bão kết nối lại sau failover/restart): các node có **cùng lớp hiển thị** (cùng tập peer, cùng quyền, P00c `peerClass`) dùng chung một danh sách peer đã dựng và mã hóa; mỗi người nhận chỉ bỏ chính mình. Khóa của lớp phải gồm mọi yếu tố làm entry khác theo người xem (cap của grants, route `via`, autogroup:self). Cache theo runtime và theo thế hệ snapshot/policy; không bao giờ dùng chung giữa tailnet.

### D5. Ngoài phạm vi

- Không đổi giao thức client, không đổi mô hình Noise (AGENTS 12).
- Không ẩn peer offline khỏi netmap (giữ hành vi Tailscale); nếu cần, là quyết định riêng của chủ dự án.

## Bất biến và test bắt buộc

| ID | Bất biến | Test |
|---|---|---|
| M01 | Adjacency tăng dần ≡ dựng toàn bộ | giữ (rapid), thêm chạy dài trong CI đêm |
| **M07** (mới) | **Replay delta ≡ full map**: với mọi chuỗi thay đổi ngẫu nhiên (node, user, tag, route, exit, via, autogroup:self, policy, DNS, xóa, thu hồi, online/offline) trên **hai tailnet cùng process**, trạng thái của một client mô phỏng (áp full map đầu + mọi delta theo ngữ nghĩa `tailcfg`/`controlclient`) bằng đúng full map dựng mới cho node đó tại mọi điểm tĩnh: tập peer, trường từng peer, PacketFilter, SSH, DNS, DERP | rapid, hai runtime, oracle = full map mới |
| **M08** (mới) | Delta không bao giờ **thêm** peer mà full map không cho, không bao giờ chứa peer của tailnet khác | trong M07 + test âm tính chéo tenant |
| M02 | Đổi policy → adjacency dựng lại toàn bộ | giữ |
| M03 | Lệch → phát hiện, thay, full map cho node bị ảnh hưởng | giữ |
| M04 | Thu hồi khi hàng đợi đầy vẫn tới; full map định kỳ đúng chu kỳ | giữ |
| M05 | OFF ≡ hành vi P00c (golden) | giữ |
| M06 | Client thật: thu hồi, xóa user, policy chặn, **đổi tag/route, đổi policy qua API tenant** → peer biến mất ≤ 10 s, chỉ qua đường delta (hai interval đặt 1 h) | mở rộng, 1.104.7 + 1.84.3 |
| **M09** (mới) | Số byte/CPU giảm đúng như kỳ vọng ở các ô đo nhỏ/vừa; không xấu đi ở ô khác | capload, mô hình ước lượng (quyết định "ước lượng đúng") |

## Hệ quả

- N1 E2E: mặc định chạy ON; lượt OFF giữ để kiểm công tắc khẩn cấp.
- P14 (dung lượng): netmap delta-first là tối ưu chính; quota/SLO tính trên ON.
- Rủi ro chính: bỏ sót một phần của MapResponse khi chỉ gửi diff (client giữ trạng thái cũ). M07 là cổng chặn; thiếu M07 xanh thì không bật pha tương ứng.

## Kết quả kiểm chứng (2026-11-27)

| Hạng mục | Kết quả | Nguồn |
|---|---|---|
| M07/M08 (oracle replay hai tailnet, chạy dài và race, cả ON và OFF) | Đạt | `evidence/P14f-f3.md`, `P14f-f4.md`, `P14f-review.md` |
| Review bảo mật độc lập | Không có BLOCKER. R1–R8 đã sửa có RED→GREEN | `evidence/P14f-review.md` |
| N1 client thật, binary cuối | ON 1.104.7 / 1.84.3 đạt mọi case cô lập; OFF 43/43; P12b 13/13 | `evidence/P14f-f6.md` |
| M06 mở rộng (M06x, chỉ qua đường delta) | Chậm nhất 3,8 s, ngưỡng 10 s | như trên |
| Soak (interval 60 s / 15 m) | 90 vòng: divergence 0 qua 80 lần đối chiếu. Lượt ≥ 2 h đang chạy | như trên |
| M09 | Byte gửi ở 5k: isolated ít hơn 14 lần, personal_devices 24 lần, shared_service 31 lần. `allow_all` ON đạt ở 1k–2k, nơi OFF trượt | `evidence/P14f-f5.md` §2 |

**Interval mặc định giữ nguyên** (`reconcile_interval` 60 s, `full_map_interval` 15 m). Đối chiếu tốn dưới 0,1 s mỗi lần ở 5k với policy không full mesh, và khoảng 3,9 s với `allow_all` 5k (`evidence/P14f-f5.md` §4).
