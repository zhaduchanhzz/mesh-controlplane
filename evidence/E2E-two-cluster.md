# E2E hai cluster, client thật (lab giai đoạn 2a, 2026-11-24)

- **Nhánh:** `feat/E2E-two-cluster` (từ `master` `15dd9f3`). Trạng thái: `Verified`, chưa `Reviewed`.
- **Inventory:** `evidence/source-inventory-E2E-two-cluster.md`.
- **Phạm vi:** kiểm trên hai cluster thật những gì đã có code: P10 (owner/epoch, reconcile, fencing), P11 (bootstrap trả URL theo owner, F06), F07 (cluster không sở hữu tailnet từ chối), chuyển tailnet bằng `tailnet reassign` với client đăng nhập lại (ADR 0005). **Không** thay P12 (placement tự động, reconcile giữa cluster) hay P12b (thủ tục move có kế hoạch, số đo).

## Cách chạy

`BIN=<headscale-cluster> BOOT_BIN=<mesh-bootstrap> infra/lab/e2e/twocluster/run.sh`

| Thành phần | Chi tiết |
|---|---|
| c1 | `mc-cp-c1` trên mc-cp, `https://c1.cp.gta5web.online:8443` (VPS → wg-mesh → mc-cp) |
| c2 | `mc-c2` trên mc-c2, `https://c2.cp.gta5web.online:8445` (VPS → wg-mesh → relay mc-cp → mc-c2) |
| DB | PostgreSQL 16 + PgBouncer 1.24.1 trên mc-db; database riêng mỗi lần `e2e_n2c<run>`, xóa khi cleanup; mc-c2 tạm chuyển sang DB đó rồi khôi phục về `meshlab` |
| Bootstrap | `mesh-bootstrap` trên mc-cp, `https://boot.cp.gta5web.online:8444` |
| Tailnet | `a`, `c` do c1 sở hữu; `b` do c2 sở hữu; policy allow-all |
| Client | `tailscale/tailscale:v1.104.7` (docker trên mc-ci) |
| Binary | build trên mc-ci từ `15dd9f3`: cluster `c6270a4f168e8cac…`, bootstrap `15a68e78540c47ed…` |

## Kết quả

| Lần | Kết quả | Thời gian | Artifact (không commit) |
|---|---|---|---|
| 1 | **9/9 PASS**, cleanup_ok=1 | 528 s | `artifacts/15dd9f3/two-cluster/e2e-20261124T154855Z` |
| 2 | **9/9 PASS**, cleanup_ok=1 (N06 21 s, N07 49 s) | 477 s | `artifacts/15dd9f3/two-cluster/e2e-20261124T163110Z` |

| Ca | Kiểm | Kết quả (lần 1; lần 2 giống, trừ thời gian) |
|---|---|---|
| N01 | Hai Noise key; mỗi listener 421 với Host của cluster kia; không có `/metrics` | c1 `18e221a5…`, c2 `7738de34…` |
| N02 | 3 tailnet / 2 cluster: peer, IPAM theo tailnet, data plane | a1↔a2, b1↔b2, c1 một mình; node đầu mỗi tailnet 100.64.0.1; ping trong a và b |
| N03 | F07 register: key của `b` ở c1, key của `a`, `c` ở c2 | `invalid pre auth key`, không tạo node (a=2 b=2 c=1) |
| N04 | F07 map: state của node a1 (đã đăng ký ở c1, cùng machine/node key) đổi ControlURL sang c2 | `NoState`, không peer, không URL đăng nhập, không tạo node, a1 không đổi |
| N05 | Bootstrap hai cluster | a, c → c1; b → c2; key của a khai tenant b → 404; b3 enrol bằng URL trả về vào b |
| N06 | Dừng c2 khi c1 đang phục vụ | c1 vẫn phục vụ (a1 Running, ping a2, node mới ở c); c2 lại: b1 cùng key/IP, không đăng nhập lại (22 s) |
| N07 | Chuyển `a` c1 → c2 | c1 retire và từ chối a; c2 nạp a; bootstrap trả c2; a1, a2 đăng nhập lại ở c2, thấy a3; b, c không đổi (55 s) |
| N08 | Client còn trỏ c1 sau khi chuyển | a2 không biết a3 trong 15 s |
| N09 | Artifact không có secret | không auth key, private key hay machine/node key đầy đủ |

## Quan sát

- **Đăng nhập lại giữ node:** a1 đăng nhập lại ở c2 bằng key cũ, giữ node ID 1 và IP 100.64.0.1 (hai cluster chung DB; node được nhận lại theo machine key trong cùng tailnet). Không phải tiêu chí pass.
- **Client cũ chạy bằng netmap đệm:** a2 (còn trỏ c1) vẫn `Running` và còn thấy a1 từ netmap đã có, chỉ không nhận cập nhật. Không có netmap cũ sau move là việc của P16; P12b phải đo cửa sổ này.
- **Guard CLI theo owner:** lần chạy đầu (bỏ, không tính) dừng ở setup vì CLI của c1 từ chối tạo key cho `b` (`tailnet is owned by another cluster`). Đây là fencing đúng; harness sửa để chạy lệnh tenant trên cluster sở hữu.

## Hạn chế

- Chưa đo cửa sổ chồng lấn giữa retire ở c1 và load ở c2, chưa đo thời gian đăng nhập lại hay số reconnect (P12b).
- Chuyển tailnet do operator gọi `reassign`; placement tự động và thông báo client (re-bootstrap) chưa có (P12, P12b).
- Một PostgreSQL, một PgBouncer, `ssl: disable` trên LAN lab (P13).
- Data plane vẫn dùng DERP công cộng của Tailscale.
