# Source Inventory — P02 (POC Noise key per tailnet, chọn tailnet theo Host)

- **Trạng thái:** `Verified` (POC, E2E với client thật v1.84.3 và v1.104.7). Gate P02 theo AGENTS rule 12 đạt: client tương thích với mỗi tailnet một Noise key, nên **không cần ADR thay thế**. Chưa `Reviewed` bởi reviewer bảo mật là người.
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

## 6. Kết quả

### Unit và race
- `go test ./hscontrol/frontdoor/` trước khi viết code: build fail (RED). Sau khi viết: 7/7 pass.
- Trên `mc-ci` (Linux): `CGO_ENABLED=1 go test -race ./hscontrol/frontdoor/ ./internal/tailnet/` cả hai ok.

### Triển khai trên `mc-cp` (endpoint public, chủ lab đã duyệt)
- Sao lưu DB và Noise key cũ: `/var/backups/mesh-cp-20261122T112224`.
- `headscale-tailnets.service`: một process, 3 runtime (`hs` 127.0.0.1:18440 giữ DB và key cũ; `a` :18441; `b` :18442), front door TLS :8443. Metrics bind 127.0.0.1:9090–9092. `headscale.service` cũ đã disable.
- Smoke test qua Internet: `/key` cho ra 3 key khác nhau (`hs` giữ nguyên `8218b05e…`). Host lạ → 421. SNI A + Host B → 421. Hai client cũ `vps-ext` và `home-ci1` vẫn kết nối direct, ping 3/3.

### E2E client thật (`infra/lab/e2e/p02/run.sh`, artifacts `artifacts/80c02af/P02/`, gitignored)

| Test | Baseline (RED) | POC P02 |
|---|---|---|
| K01 `/key` A ≠ B | FAIL | **PASS** |
| K02/K03 host lạ, IP thô | FAIL | **PASS** (421) |
| K04 chuẩn hóa `Host:port` | SKIP | **PASS** |
| K05 SNI ≠ Host trên `/key` | FAIL | **PASS** |
| K06/K07/K08 `/ts2021` (SNI≠Host, host lạ, IP, WebSocket) | FAIL (101) | **PASS** (421 trước upgrade) |
| K09 | SKIP | **PASS** |
| K10 bắt tay Noise A và B (đối chứng) | PASS | PASS |
| R01 netmap A và B tách biệt | FAIL | **PASS** |
| R02 profile A→B→A cùng machine key: 2 node, quay lại A sau switch và restart (v1.84.3 và v1.104.7) | FAIL | **PASS** |
| R03 key của A bị từ chối ở B | FAIL | **PASS** |
| N8 xóa node A không ảnh hưởng node B | SKIP | **PASS** |
| K10m, K11, N5, N7, N10, S10w | SKIP | SKIP (lý do như baseline: cần Noise client tự viết, cổng 80 là HAProxy, không có OIDC) |

- **Lần chạy 1** (`20261122T112349Z`): 2 FAIL ở R02. Nguyên nhân là **lỗi trong script** do agent chính thêm vào (`n['id']` nằm trong chuỗi bash bọc nháy đơn → `KeyError`). Lỗi này làm hỏng cả bước dọn node: 6 node `p02-*` bị sót, đã xóa bằng tay sau khi kiểm tra.
- **Lần chạy 2** (`ONLY="R02 N8"`): exit 0. R02 PASS cho cả hai phiên bản. N8 PASS. 0 traceback. Script tự dọn sạch.

### Giới hạn còn lại
- Lúc khởi động, front door nhận kết nối trước khi runtime nghe xong, gây vài lỗi 502 ngắn. Cần chờ runtime sẵn sàng trước khi mở front door.
- Cổng 80 vẫn là HAProxy MeshGuard (N10).
- Chưa có quota hay rate limit theo tailnet (P08). Chưa có DERP verify theo tailnet (S15, P08).
- Mỗi tailnet là một runtime đầy đủ: cô lập tốt, nhưng chi phí theo tailnet cao. P03 sẽ chuyển sang runtime theo tailnet trên một DB của shard (ADR 0001/0002).

## 7. Tăng tốc harness E2E (nhánh `agent/e2e-fast`, 2026-11-22)

- **Phạm vi:** chỉ sửa `infra/lab/e2e/p02/run.sh` và README của nó. Không đổi Go, server, cấu hình `mc-cp`, VPS, DNS hay firewall. Assertion của từng test giữ nguyên. Thay đổi gồm: chạy song song có giới hạn (`JOBS`, mặc định 4), chế độ `--fast` (chỉ K01–K09), `results.json`, polling thay cho `sleep` cố định, kiểm tra image một lần, ControlMaster có probe, cleanup có xác minh (kể cả khi bị ngắt).
- **Lệnh** (Git Bash, gốc worktree):
  ```bash
  HS='sudo -u headscale /usr/local/bin/headscale'
  HS_A="$HS -c /etc/headscale/tailnets/a.yaml" HS_B="$HS -c /etc/headscale/tailnets/b.yaml" \
    HEADSCALE="$HS -c /etc/headscale/tailnets/hs.yaml" bash infra/lab/e2e/p02/run.sh [--fast]
  ```

| Lần chạy | Wall | Chi tiết |
|---|---|---|
| Trước: `run.sh` tuần tự @ `7adf255`, run `20261122T124514Z` | **98 s** | test 81 s, cleanup ~12 s. 15 PASS, 6 SKIP, exit 0 |
| Sau: full, `JOBS=4`, run `20261122T125815Z` | **40 s** | setup 2.7 s, test 28.0 s (đường găng: R02 ~23.5 s), cleanup có xác minh 10.7 s. 15 PASS, 6 SKIP, exit 0 |
| Sau: `--fast`, run `20261122T130013Z` | **3.4 s** (lần trước đó 4.1 s) | 9 PASS (K01–K09), exit 0. Không dùng `mc-ci`, không tạo gì trên `mc-cp` |
| Ngắt: `timeout -s INT 14 bash run.sh`, run `20261122T125909Z` | 28 s | exit 2. Cleanup giết các test đang chạy, xóa 6 node `p02-*` và 7 key, xác minh sạch (12.8 s) |
| `ONLY="K02 R03"` / `ONLY="K05 R01" --fast` | 11.6 s / 1.9 s | chỉ chạy đúng các test được chọn; với `--fast`, R01 bị loại |

- **So khớp từng test (trước → sau):** K01–K09 PASS → PASS; K10 PASS → PASS; R01 PASS → PASS; R02/v1.84.3 PASS → PASS; R02/v1.104.7 PASS → PASS; N8 PASS → PASS; R03 PASS → PASS; K10m, K11, N5, N7, N10, S10w SKIP → SKIP (cùng lý do). Thứ tự trong summary, `results.tsv` và `results.json` cố định, không phụ thuộc test nào xong trước.
- **Kiểm tra tĩnh:** `bash -n` OK. WSL không có `shellcheck`, nên dùng ShellCheck 0.10.0 (`shellcheck-py` trong venv tạm): 0 finding. Bốn mã mức info đã có từ bản cũ (SC2015, SC2018, SC2019, SC2029) được tắt kèm lý do ở đầu file.
- **Sau khi chạy, đã kiểm tra trên `mc-cp`:** tailnet `a` và `b` có 0 node và 0 pre-auth key. Trên `mc-ci` chỉ còn container `ts-home1`, không còn volume `p02-*`. Artifact không chứa chuỗi `tskey-`/`hskey-` nào.
- **SSH multiplexing:** trên Git Bash (OpenSSH 9.7p1 msys), master khởi động được, nhưng session báo `mux_client_request_session: read from master failed` rồi tự mở kết nối mới. Harness probe trước và tắt mux trên Git Bash. Mỗi kết nối LAN trực tiếp mất ~0.35 s. Trên WSL/Linux, mux được dùng.
- **Giới hạn còn lại:** R01 vẫn giữ cửa sổ quan sát 5 s, vì đây là test âm tính: peer rò rỉ (nếu có) đến bất đồng bộ. Nếu lần ngắt rơi đúng lúc phản hồi SSH của `preauthkeys create` đang truyền về, harness không biết ID của key đó; key ấy chưa dùng và hết hạn sau ≤30 phút. Thời gian chạy bị chặn bởi R02, vì các bước của nó vốn tuần tự (up A → login B → switch → restart).
