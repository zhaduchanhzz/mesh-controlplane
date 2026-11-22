# Source Inventory — P02 (POC Noise key per tailnet, chọn tailnet theo Host)

- **Trạng thái:** `Implemented` (POC), chờ E2E client thật (`infra/lab/e2e/p02/run.sh`). Chưa `Reviewed` bởi reviewer bảo mật là người.
- **Nhánh:** `feat/P02-noise-per-tailnet-poc` từ `master` @ `c783425`
- **Ticket:** `tickets/P02.md`. Đầu vào: `evidence/P02-noise-client-research.md`, `evidence/P02-e2e-red-baseline.md`

## 1. Ràng buộc đã kiểm chứng

| Ràng buộc | Nguồn |
|---|---|
| Client thử `http://<host>:80/ts2021` trước, kể cả khi ControlURL có `:8443` | `controlhttp/client.go:253` (`strDef(HTTPPort,"80")`), log E2E v1.84.3 và v1.104.7 |
| Cổng 80 không có SNI; TLS của `/ts2021` không được verify. Ràng buộc thật nằm ở Noise IK | `controlhttp/client.go:493-502` |
| `/key` lấy qua HTTPS có verify | `controlclient/direct.go:1543` |
| Machine key dùng chung mọi profile; node key riêng theo profile | research Q2 |
| `/ts2021` hiện trả `101` trước mọi kiểm tra Host (K06–K08) | E2E RED |
| URL đăng nhập lại dùng `server_url` toàn cục | E2E RED (R02) |
| `ServerURL` được dùng ở auth, noise, debug, platform_config, state, dns, api v2 c2n | `grep cfg.ServerURL` |
| `config.Load(path, isFile, environ) (*Loaded, error)` không dùng biến toàn cục | `hscontrol/config/load.go:437` |
| Global duy nhất `NewHeadscale` có thể đụng là `http.DefaultTransport.RegisterProtocol` (chỉ khi bật embedded DERP verify) | `hscontrol/app.go:290` |

## 2. Thiết kế POC (theo hướng ADR 0001)

```
client ──TLS──▶ front door :8443 (1 process)
                 1. Host chuẩn hóa (lowercase, bỏ port, bỏ dấu chấm cuối), từ chối IP và host lạ → 421
                 2. Có TLS: SNI chuẩn hóa phải == Host → nếu không thì 421
                 3. Bỏ header client tự đặt (X-Tailnet-ID…); không có tailnet mặc định
                 4. Reverse proxy (giữ Upgrade/101) → instance của tailnet đó
                     ├─ instance A: Headscale đầy đủ, 127.0.0.1:18441, server_url https://a.cp…:8443, DB A, Noise key A
                     └─ instance B: … 127.0.0.1:18442, DB B, Noise key B
```

- **Vì sao chọn nhiều instance thay vì refactor state ngay:** P02 phải chứng minh được **client thật tương thích** với mỗi hostname một Noise key, và profile A→B→A. Dùng instance riêng thì cô lập được ngay từ cấu trúc (DB, NodeStore, policy, batcher, authCache, `server_url` riêng), không cần đụng state và mapper (P03–P07).
- **Đây là POC, không phải thiết kế cuối.** P03 sẽ chuyển sang một DB cho mỗi shard với `tailnet_id` và runtime theo tailnet. Front door giữ lại làm lớp chọn tailnet.
- **Không** thử lần lượt Noise key của các tailnet. Mỗi kết nối chỉ tới đúng một instance.

## 3. Thay đổi

| Path | Thay đổi |
|---|---|
| `hscontrol/frontdoor/` (mới) | `NormalizeHost`, `Door` (bảng host → backend, kiểm SNI, 421 fail closed, xóa header client tự đặt, reverse proxy hỗ trợ Upgrade) |
| `cmd/headscale/cli/serve_tailnets.go` (mới) | Lệnh `headscale serve-tailnets`: nạp N config, chạy N instance trong một process, thêm một listener TLS cho front door |
| `infra/lab/mc-cp/` | Config cho 2 instance A/B và cập nhật service, để chạy E2E |

## 4. Kiểm thử

- **RED trước:** unit test `hscontrol/frontdoor`.
- **Nghiệm thu:** `infra/lab/e2e/p02/run.sh` với client thật v1.104.7 và v1.84.3. Kỳ vọng K01–K03, K05–K08, R01–R03 chuyển từ RED sang GREEN.

## 5. Giới hạn đã biết

- Chưa áp quota hay rate limit theo tailnet tại front door (P08).
- Mỗi instance có một bộ worker riêng, nên chi phí theo tailnet cao hơn so với runtime chung. Benchmark P00b và ADR 0001 sẽ quyết định cách gộp.
- Cổng 80 của lab thuộc HAProxy MeshGuard; edge thật ở P10 phải phục vụ hoặc từ chối `/ts2021` trên cổng 80.
