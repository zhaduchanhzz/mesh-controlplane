# P02 E2E — kết quả RED trên baseline (một tailnet, một Noise key)

- **Trạng thái:** harness `Implemented`. Kết quả RED `Verified` trên baseline (đã chạy với client thật, có artifact). Đây **không** phải bằng chứng P02 đã làm xong; nó chỉ chứng minh các test phát hiện được việc server chưa hỗ trợ.
- **Ngày chạy:** 2026-11-22, run `20261122T104943Z`.
- **Repo:** worktree `wt-p02-e2e`, nhánh `agent/p02-e2e`, HEAD `4f8af3f` (harness chưa commit).
- **Server:** `mc-cp`, `headscale v0.0.0-20261122064815-2558340ef63d+dirty` (commit `2558340ef63d`, go1.27.4). Config `infra/lab/mc-cp/config.yaml`, `server_url: https://hs.cp.gta5web.online:8443`, policy `grants * → *`.
- **Đường mạng:** client → `116.118.3.46:8443` (VPS, `systemd-socket-proxyd`, TCP passthrough) → WireGuard → `mc-cp:8443`. Host và SNI tới server nguyên vẹn.
- **Client thật:** `tailscale/tailscale:v1.104.7` (bản mới) và `tailscale/tailscale:v1.84.3` (bản tối thiểu, capver 116), chạy trong Docker trên `mc-ci`.
- **Probe HTTP:** `curl 8.7.1 (x86_64-w64-mingw32) Schannel` trên máy operator (Git Bash). SNI lấy từ host trong URL; `-H Host:` chỉ đổi header Host.
- **Artifact (gitignored):** `artifacts/4f8af3f/P02/e2e-20261122T104943Z/` gồm `results.tsv`, `env.txt`, `keys.txt`, log `tailscaled`/CLI đã redact, `*.noise-dials.log` và `K10-debug-ts2021-*.log`.

## Lệnh

```bash
# Từ gốc worktree, Git Bash
bash infra/lab/e2e/p02/run.sh ; echo exit=$?      # exit=1 (11 FAIL)
```

Pre-auth key được tạo trên `mc-cp` bằng `headscale preauthkeys create --user lab-test --expiration 30m -o json`, nằm trong thư mục tạm mode 700 (file mode 600) và chỉ được truyền qua `tailscale up|login --auth-key=file:/dev/stdin`. Không key nào được in ra. Sau khi chạy, harness đã xóa container `p02-*` và volume, xóa node 14–18 (node do run này tạo), expire rồi xóa các key của mình. Đã kiểm tra lại: trên server chỉ còn node `vps-ext` và `home-ci1`, key 1 (không phải của harness) vẫn giữ nguyên, trên `mc-ci` chỉ còn container `ts-home1`.

Có hai lần chạy trước đó (`104101Z`, `104528Z`) cho cùng kết quả test. Chúng làm lộ hai lỗi cleanup của harness: `ssh` trong vòng `while read` nuốt stdin, và Python trên Windows in ra CRLF nên ID bị dính `\r`. Node và key còn sót từ hai lần đó đã được xóa tay; cả hai lỗi đã sửa trong `run.sh`. Artifact của hai lần đó đã bị xóa vì có log ID chưa redact.

## Kết quả từng test

Quy ước: test kiểm tra **hành vi đích** của P02. FAIL = RED.

| ID | Ánh xạ | Kết quả | Chi tiết | RED dự kiến "chưa làm"? |
|---|---|---|---|---|
| K01 | S11, N2, I01 | **FAIL** | `/key?v=116` trên A và B trả **cùng** key `mkey:8218b05e…` | Có (một key cho cả process, `hscontrol/app.go:151`) |
| K02 | S11, N3 | **FAIL** | Host lạ `zz.cp…` → 200 kèm key mặc định | Có (`KeyHandler` bỏ qua Host) |
| K03 | S11, N3 | **FAIL** | URL raw IP (không SNI) → 200 có key; SNI A + `Host: 116.118.3.46:8443` → 200 có key | Có |
| K04 | N4 | SKIP | 4/4 biến thể Host (`:8443`, không port, chữ hoa, dấu `.` cuối) đều nhận key của A, nhưng A==B nên test không phân biệt được | Không áp dụng (cần 2 tailnet) |
| K05 | S11, N1 | **FAIL** | SNI A + Host B → 200 có key; SNI B + Host A → 200 có key | Có (chưa đối chiếu SNI == Host) |
| K06 | S11, N1 | **FAIL** | `POST /ts2021` với SNI A + Host B → `101 Switching Protocols` | Có |
| K07 | S11, N3 | **FAIL** | `/ts2021` với Host lạ → 101; với raw IP → 101 | Có |
| K08 | S11, N9 | **FAIL** | WebSocket `/ts2021` với SNI A + Host B → 101 | Có |
| K09 | S11 | SKIP | `X-Tailnet-ID`, `X-Forwarded-Host`, `Forwarded: host=` trỏ B không làm đổi key, nhưng A==B nên không phát hiện được nếu tailnet bị đổi | Không áp dụng |
| K10 | P02 POC bước 1 | PASS | v1.104.7 `tailscale debug ts2021 --host <h>:8443` bắt tay Noise thành công trên A và B (positive control). Cả hai lần đều thử `:80` trước | — |
| K10m | P02 POC bước 1 | SKIP | `debug ts2021` của v1.84.3 không dial được `host:port` (resolve chuỗi `"a.cp…:8443"` như một tên miền). Handshake v1.84 đã được R02 phủ | — |
| K11 | S11, N2 | SKIP | Host B + initiation mã hóa tới key A cần client Noise tự viết; ngoài ra A==B | Cần harness Go riêng |
| N5 | N5, I01 | SKIP | `:authority` bên trong phiên Noise: cần client tự viết | Cần harness Go riêng |
| N7 | N7 | SKIP | Map request bằng node key A qua phiên B: cần client tự viết và 2 tailnet | Cần harness Go riêng |
| N10 | N10 | SKIP | Cổng 80 của `116.118.3.46` không phải control plane (HAProxy, trả reply rỗng) | Hạn chế của lab |
| S10w | S10 | SKIP | Lab chưa cấu hình OIDC hay web approval | Hạn chế của lab |
| R01 | S11, N7, I01 | **FAIL** | Client đăng ký qua A và client đăng ký qua B thấy nhau trong `tailscale status` (cả hai chiều) | Có (một tailnet, policy `*`) |
| R02/v1.84.3 | S05, I03 | **FAIL** | Server chỉ có **1 node** cho machine key của thiết bị (node A bị đăng ký B ghi đè và đổi tên thành `…-b`). Switch về A → `NeedsLogin`, node key mất hiệu lực. Sau `docker restart`: vẫn profile A, `NeedsLogin` | Có (lookup theo machine key chưa có `tailnet_id`) |
| R02/v1.104.7 | S05, I03 | **FAIL** | Giống hệt v1.84.3 | Có |
| N8 | N8 | SKIP | Server gộp A và B thành một node nên không còn gì độc lập để xóa | Không áp dụng (cần R02 GREEN trước) |
| R03 | S10, N6 | **FAIL** | Key cấp "cho A" đăng ký được qua B (`rc=0`, `Running`, có node mới) | Có (key chưa gắn tailnet) |

Tổng: 11 FAIL, 1 PASS, 9 SKIP. Exit 1.

**Cả 11 RED đều là RED dự kiến "chưa làm":** server baseline có một Noise key, không đọc Host/SNI và chỉ có một tailnet. Không có FAIL nào do lỗi hạ tầng hay harness. K10 PASS cho thấy đường TLS, Noise và client thật hoạt động, nên các FAIL không phải do mất kết nối.

Phần client của R02 khớp với nghiên cứu: client giữ hai profile riêng (`tailscale switch --list` có 2 dòng), mỗi profile có node key riêng, và mỗi profile dial Noise tới đúng host của mình (`*.noise-dials.log`: sau `login` qua B thì dial `b.cp…:8443`, sau `switch` thì dial `a.cp…:8443`). Phần FAIL nằm ở phía server.

## Quan sát mâu thuẫn hoặc bổ sung so với `evidence/P02-noise-client-research.md`

1. **Port 80 được thử trước kể cả khi ControlURL có `https://…:8443`: research ĐÚNG, `infra/lab/README.md` SAI.** README (mục DNS/TLS) ghi "The client never tries port 80 when ControlURL has an explicit https port (`ts2021/client.go:137-142`)". Log thật (`TS_DEBUG_NOISE_DIAL=1`) của cả v1.84.3 lẫn v1.104.7 cho thấy `trying noise dial (http://a.cp.gta5web.online:80/ts2021)` rồi mới tới `https://…:8443`. Nguyên nhân: `ts2021/client.go` đặt `httpPort=""`, sau đó `controlhttp/client.go` dùng `strDef(a.HTTPPort, "80")`. K10 cũng thấy `Dial("tcp", "116.118.3.46:80")`.
   - **Hệ quả bảo mật:** initiation Noise (đã mã hóa) và `Host: a.cp…:80` được gửi tới HAProxy `mg-l4proxy` của MeshGuard trên VPS, một dịch vụ không liên quan. HAProxy trả reply rỗng nên client chuyển sang 8443. Chỉ lộ metadata, không lộ secret, nhưng README cần sửa và edge thật ở P10 phải phục vụ hoặc từ chối `/ts2021` trên cổng 80 một cách có chủ đích.
2. **Heuristic "dial gần đây → ép 443" (research Q3/Q4) được xác nhận:** sau `login` qua B trong vòng 2 phút, client bỏ qua cổng 80 (`forcing port 443 dial due to recent noise dial`). Sau `docker restart`, client lại thử cổng 80 trước.
3. **Machine key dùng chung giữa các profile (Q2) được xác nhận gián tiếp phía server:** đăng ký B dùng lại đúng node của A (cùng `machineKey`). Đó chính là lỗi S05 trên baseline.
4. **Chưa được research dự đoán:** khi switch về A, client gửi node key cũ, server không còn biết node key đó và trả `AuthURL` trỏ tới `https://hs.cp.gta5web.online:8443/register/<redacted>`. URL này dùng `server_url` toàn cục chứ không phải host của profile (`a.cp…`). P02 cần sinh URL đăng ký/redirect **theo tailnet**, nếu không web-auth sẽ đưa client sang host của tailnet khác (liên quan S10).
5. **`/ts2021` trả `101` ngay cả với handshake rác (`AAAA`).** Server chuyển giao thức trước khi kiểm tra initiation. Vì vậy, nếu kiểm tra Host/SNI, đoạn kiểm tra đó **phải** chạy trước `controlhttpserver.AcceptHTTP`. Nếu kiểm tra sau `101`, K06/K07/K08 vẫn RED.
6. **POC bước 1 của research (`tailscale debug ts2021 --host a.mesh.test`) mặc định cổng 443.** Với cổng 8443 chỉ v1.104.7 dùng được `--host host:8443`. v1.84.3 lỗi khi resolve tên. Cần ghi chú khi pin client tối thiểu.
7. **Chưa kiểm chứng:** research Q1 nói client fetch lại `/key` mỗi lần `doLogin`. Harness không có log `/key` theo Host phía server (chỉ đọc, không bật debug log server), nên chưa xác nhận. Việc "dial Noise theo host của profile" thì đã thấy.

## Giới hạn và việc còn thiếu

- N2, N5, N7 và K11 cần một client Noise tự viết (Go, chỉ dùng trong test). Ticket này không có thay đổi Go nên chưa có.
- K04, K09 và N8 chỉ có ý nghĩa khi A và B có key hoặc node khác nhau. Trên baseline chúng SKIP và sẽ tự chạy thật khi K01/R02 GREEN.
- Khi server hỗ trợ nhiều tailnet: phải đặt `KEYCMD_B` sang tailnet B, nếu không R03 và R01 vẫn tạo key B trong tailnet A.
- Proxy TCP trên VPS che IP nguồn của client (`10.99.77.1`). Test không phụ thuộc vào IP nguồn.
- Kết quả này không chứng minh isolation; nó chỉ ghi lại baseline RED. Theo AGENTS.md, lỗi isolation là BLOCK MERGE.
- Reviewer approval: chưa có.
