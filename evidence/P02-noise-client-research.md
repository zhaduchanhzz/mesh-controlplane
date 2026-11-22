# P02 — Nghiên cứu hành vi client Tailscale cho Noise key theo từng tailnet

- **Trạng thái:** `Planned` (chỉ là nghiên cứu đọc code; chưa có code, chưa chạy client thật). **Không** khẳng định client tương thích với thiết kế.
- **Ngày:** 2026-11-22
- **Repo commit:** `2558340ef63d03fd2603c6b6dfb589ef1386cd69` (nhánh `feat/P00-rebaseline`)
- **Phiên bản client đã đọc:** `tailscale.com v1.105.0-pre.0.20261112091541-a2da5fbcaa53` (`go.mod:90`; `VERSION.txt` = `1.105.0`), tại `$(go env GOMODCACHE)/tailscale.com@v1.105.0-pre.0.20261112091541-a2da5fbcaa53`.
- Mọi đường dẫn `file:line` bên dưới là tương đối với gốc module đó, trừ khi ghi `hscontrol/...` (repo này) hoặc `GOROOT/...` (thư viện chuẩn Go trên máy local).
- Nhãn: **Verified (code read)** = đã đọc đoạn code được trích; **Assumption** = suy luận/chưa kiểm chứng bằng chạy thật.
- Lưu ý: Tailscale official build dùng Go fork riêng (`tailscale/go`); hành vi `net/http` bên dưới được đọc từ stdlib local — **Assumption** là giống hệt.

---

## Tóm tắt

| # | Câu hỏi | Trả lời ngắn |
|---|---|---|
| 1 | Client lấy Noise pubkey thế nào? | `GET <ControlURL>/key?v=<capver>` qua HTTPS có xác thực cert; chỉ giữ **trong RAM** của `controlclient.Direct`; không persist. Với Headscale (không gửi legacy key) client fetch lại `/key` mỗi lần `doLogin`, nhưng Noise client đã tạo thì **giữ key cũ** cho tới khi `Direct` bị đóng. |
| 2 | Machine key theo device hay profile? | **Theo device/state store**: một khóa `_machinekey` dùng chung cho mọi profile. Node key, NetworkLockKey **theo profile** (nằm trong `Prefs.Persist`). |
| 3 | Profile có ControlURL/node key/control client riêng? | Có: mỗi profile có `Prefs` riêng (ControlURL, Persist); đổi profile hủy control client cũ và tạo mới. **Dùng chung**: machine key, disco key, dial plan (chỉ reset khi ControlURL đổi), health tracker (heuristic port 443). |
| 4 | Host/SNI? Port 80? | Upgrade `/ts2021`: thử **HTTP thường port 80 trước** (chỉ có Host header, không SNI), sau 500 ms (hoặc khi 80 lỗi) thử HTTPS 443 (có SNI + Host). Host header gồm cả port (`a.mesh.test:80`, `a.mesh.test:443`). Cert TLS của `/ts2021` **không được enforce** (chỉ log). Dial plan có thể đổi IP/ACE proxy nhưng giữ nguyên Hostname. |
| 5 | Pinning/cache theo host gây trộn A/B? | Không thấy pin Noise key theo host ở state persist. Rủi ro trộn nằm ở: machine key + disco key dùng chung, dial plan dùng chung, và dedup profile theo `ControlURL + (UserID hoặc NodeID)`. |

**Kết luận thiết kế:** server **phải xử lý cả Host header lẫn SNI**; chỉ có **Host** là luôn có mặt trên mọi đường (port 80, 443, WebSocket). SNI chỉ dùng để đối chiếu trên TLS. Ràng buộc tailnet thực sự đến từ việc handshake Noise IK chỉ thành công với private key của đúng tailnet mà client đã mã hóa tới — không phải từ Host/SNI (đều do client tự khai, không được xác thực).

---

## Q1. Client lấy Noise public key của control server như thế nào?

**Trả lời:** Qua `GET {ServerURL}/key?v={CurrentCapabilityVersion}`, dùng HTTP client thường (không phải Noise), parse `tailcfg.OverTLSPublicKeyResponse`. Kết quả lưu trong field in-memory của `Direct`, không ghi xuống state store. Không có cơ chế làm mới Noise client khi key đổi.

| Claim | Nguồn | Nhãn |
|---|---|---|
| URL `/key?v=%d` ghép từ `serverURL` (đã `TrimRight "/"`) | `control/controlclient/direct.go:1543-1544`, `direct.go:316` | Verified (code read) |
| Body JSON `{legacyPublicKey, publicKey}`; fallback format cũ (chỉ legacy key dạng text) | `direct.go:1562-1577`; `tailcfg/tailcfg.go:2978-2991` | Verified (code read) |
| `/key` đi qua `c.httpc` (transport chuẩn + `tlsdial.Config`), cert **được verify** (system roots → ExtraRootCAs → baked roots) | `direct.go:333-367`; `net/tlsdial/tlsdial.go:104-200` | Verified (code read) |
| Key lưu ở `Direct.serverLegacyKey`/`serverNoiseKey` (in-memory) | `direct.go:102-103`, `direct.go:693-697` | Verified (code read) |
| Chỉ fetch khi `serverLegacyKey.IsZero()` | `direct.go:682-704` | Verified (code read) |
| Headscale chỉ trả `PublicKey`, `LegacyPublicKey` = zero ⇒ điều kiện trên luôn đúng ⇒ client **fetch lại `/key` mỗi lần `doLogin`** | `hscontrol/handlers.go:244-246` + `direct.go:682` | Verified (code read) — hệ quả suy ra từ 2 đoạn code |
| Noise client (`ts2021.Client`) tạo một lần với `ServerPubKey` cố định; `getNoiseClient` trả client cũ nếu đã có, **không so sánh** với `serverNoiseKey` mới | `direct.go:1717-1760` (đặc biệt `1725-1727`, `1740`) | Verified (code read) |
| Noise client chỉ bị xóa khi `Direct.Close()` | `direct.go:425-438` | Verified (code read) |
| Không có server key trong `persist.Persist` hay `ipn.LoginProfile` | `types/persist/persist.go:20-40`; `ipn/prefs.go:1104-1152` | Verified (code read) |
| Grep `ServerPubKey|serverNoiseKey|OverTLSPublicKeyResponse|ControlKey` toàn module (bỏ test) chỉ ra `controlclient`, `controlhttp`, `ts2021`, `tailcfg`, `cli/debug.go`, `tsp` — không có ở `ipn/` | grep | Verified (code read, grep) |
| Server key "từ chối" nếu `PublicKey` zero: "control server is too old; no noise key" | `direct.go:707-709`, `direct.go:1065-1067` | Verified (code read) |

**Nếu key phục vụ cho một URL thay đổi:**
- Restart tailscaled hoặc tạo lại control client (`Start` → `DisconnectControl` → `startLocked` tạo `controlclient` mới, `ipn/ipnlocal/local.go:3210-3232`, `3427-3452`) ⇒ fetch key mới. **Verified (code read)**.
- Trong vòng đời một `Direct`: `serverNoiseKey` có thể được cập nhật ở `doLogin` kế tiếp nhưng Noise client cũ vẫn dùng key cũ ⇒ redial sẽ thất bại handshake tới khi `Direct` bị tạo lại. **Verified (code read)** về luồng; hành vi thực tế (retry/backoff, có tự tạo lại `Direct` không) là **Assumption**, cần test.
- ⇒ Xoay vòng (rotate) Noise key theo tailnet **không được làm nóng**; cần overlap hoặc yêu cầu client restart.

## Q2. Machine key theo device hay theo profile? Node key?

**Trả lời:** Machine key là **một khóa cho cả state store** (thực tế: cả device/instance tailscaled), dùng chung mọi profile. Node key theo profile.

| Claim | Nguồn | Nhãn |
|---|---|---|
| Hằng `MachineKeyStateKey = "_machinekey"` — khóa toàn cục, không có profile ID | `ipn/store.go:22-24` | Verified (code read) |
| `LocalBackend` có một field `machinePrivKey` | `ipn/ipnlocal/local.go:367` | Verified (code read) |
| `initMachineKeyLocked` đọc/ghi `_machinekey`, nếu đã có trong RAM thì giữ | `local.go:4790-4823` | Verified (code read) |
| `createGetMachinePrivateKeyFunc` cache và trả cùng key cho mọi control client | `local.go:4763-4783`; truyền vào `controlclient.Options` tại `local.go:3428` | Verified (code read) |
| Machine key chỉ bị xóa bởi `ResetAuth` (xóa toàn bộ profile) | `local.go:4830-4837`, `local.go:8880-8895` | Verified (code read) |
| `SwitchProfile`/`resetForProfileChangeLocked` **không** đụng machine key | `local.go:8734-8822` | Verified (code read) |
| `Persist` (PrivateNodeKey, OldPrivateNodeKey, NetworkLockKey, NodeID, UserProfile, AttestationKey) "persisted per ipn.LoginProfile" | `types/persist/persist.go:20-40` | Verified (code read) |
| `Prefs.Persist` (JSON field `Config`) nằm trong prefs của từng profile, ghi theo `LoginProfile.Key` | `ipn/prefs.go:336-341`; `ipn/ipnlocal/profiles.go:563-572` | Verified (code read) |
| Node key mới sinh khi regen hoặc Persist rỗng; NetworkLockKey sinh nếu rỗng | `direct.go:712-731` | Verified (code read) |

## Q3. Profiles: mỗi profile có ControlURL, node key, control client riêng? State nào dùng chung?

**Riêng theo profile (Verified (code read)):**
- `Prefs.ControlURL` (`ipn/prefs.go:75`), `LoginProfile.ControlURL` (`ipn/prefs.go:1143-1145`, gán ở `profiles.go:455`).
- Node key / NetworkLockKey / NodeID (Q2).
- Control client: `resetForProfileChangeLocked` gọi `resetControlClientLocked` rồi `startLocked` ⇒ `controlclient.New` với `ServerURL` của profile mới (`local.go:8765-8822`, `7342-7366`, `3427-3452`); `controlclient.New` → `NewDirect` (`control/controlclient/auto.go:180`) ⇒ `Direct` mới ⇒ `/key` fetch mới, Noise client mới.
- `tailscale login` luôn gọi `SwitchToEmptyProfile` trước (`cmd/tailscale/cli/login.go:24-28`) → `PUT /localapi/v0/profiles/` → `NewProfile` (`ipn/localapi/localapi.go:1562-1568`) → `resetDialPlan` + reset (`local.go:8853-8866`).
- Netmap disk cache theo profile ID (`ipn/ipnlocal/diskcache.go:74-97`, `local.go:6665-6674`); TKA state theo profile (`ipn/ipnlocal/tailnet-lock.go:532-533`).

**Dùng chung giữa các profile:**
| State | Nguồn | Nhãn | Ý nghĩa cho isolation |
|---|---|---|---|
| Machine key `_machinekey` | Q2 | Verified (code read) | Server A và B thấy **cùng machine key** ⇒ server phải coi `(tailnet_id, machine_key)` là định danh; không lookup node chỉ theo machine key. |
| Disco key: lấy từ `b.MagicConn().DiscoPublicKey()` mỗi lần start | `local.go:3410` | Verified (code read) | Trong `ipnlocal` chỉ thấy xoay disco key ở `DebugRotateDiscoKey` (`local.go:8369-8375`) ⇒ **Assumption**: disco key giống nhau giữa profile A và B. Lookups theo disco key phía server phải scoped. |
| Dial plan `LocalBackend.dialPlan` | `local.go:423-425`; chỉ reset khi ControlURL đổi khi switch (`local.go:8743-8746`, `5206-5209`), luôn reset khi logout (`local.go:7476`), `NewProfile`, `ResetAuth` | Verified (code read) | Với hostname riêng mỗi tailnet thì ControlURL khác ⇒ reset. Nếu 2 tailnet dùng chung ControlURL sẽ **giữ dial plan của tailnet trước**. |
| Health tracker (`LastNoiseDialWasRecent`: dial lại trong 2 phút ⇒ ép 443) | `health/health.go:1375-1386`; dùng ở `control/controlhttp/client.go:186-214` | Verified (code read) | Không ảnh hưởng isolation; ảnh hưởng tính tái lập POC (sau khi switch A→B trong 2 phút, B đi thẳng 443). |
| `ExtraRootCAs`, policy client, dialer, netmon | `local.go:3427-3452` | Verified (code read) | Không gắn tailnet. |

**Dedup profile:** `findMatchingProfiles` coi 2 profile là một nếu `ControlURL` bằng nhau **và** (`UserProfile.ID` bằng nhau **hoặc** `NodeID` bằng nhau); `SetPrefs` sẽ xóa state của các profile trùng (`ipn/ipnlocal/profiles.go:286-292`, `350-372`). **Verified (code read)**. ⇒ Nếu hai tailnet dùng chung ControlURL và ID user/node trùng số, client sẽ ghi đè/xóa profile của tailnet kia. Hostname riêng mỗi tailnet tránh được; vẫn nên cấp `UserID`/`StableNodeID` duy nhất toàn cục.

**`RegisterRequest.Tailnet`:** client có gửi `prefs.Tailnet()` (`local.go:3434`, `direct.go:772`, `tailcfg/tailcfg.go:1361-1370`). Đây là giá trị client tự khai — **không** được dùng làm nguồn `TailnetID` (AGENTS.md §6). **Verified (code read)** về việc gửi; việc không tin là quyết định thiết kế.

## Q4. Transport: Host header, SNI, port 80, fallback, dial plan

**Chọn port** (`control/ts2021/client.go:133-163`) — Verified (code read):
| ControlURL | httpPort | httpsPort |
|---|---|---|
| `https://a.mesh.test` | 80 | 443 |
| `https://a.mesh.test:8443` | `""` ⇒ `controlhttp` mặc định **80** (`constants.go:114-119`, `client.go:251-255`) | 8443 |
| `http://a.mesh.test:8080` (host public) | 8080 | 443 |
| `http://10.x` / `localhost` / loopback | 80 hoặc port chỉ định | không thử HTTPS |

⇒ Kể cả ControlURL là `https://...:port`, client **vẫn thử `http://<host>:80/ts2021`** trước. Verified (code read).

**Thứ tự dial** (`control/controlhttp/client.go:239-344`) — Verified (code read):
1. Bắt đầu port 80 ngay (`client.go:291-293`), trừ khi `forceNoise443()`.
2. Timer 500 ms (`client.go:87-94`, `297-305`) rồi thử HTTPS; nếu port 80 lỗi sớm thì thử HTTPS ngay (`client.go:320-329`). Kết nối thành công đầu tiên thắng.
3. `forceNoise443()` = true nếu plan9, `TS_FORCE_NOISE_443`, hoặc lần dial Noise trước < 2 phút (`client.go:186-214`).
4. ACE host ⇒ bỏ HTTPS, chỉ port 80 qua ACE dialer (`client.go:261-263`, `427-436`).

**Upgrade request** (`client.go:539-548`) — Verified (code read): `POST /ts2021`, header `Upgrade: tailscale-control-protocol`, `Connection: upgrade`, `X-Tailscale-Handshake: base64(Noise IK initiation)`. HTTP/2 tắt cho upgrade (`client.go:479-481`). Initiation message được mã hóa tới `ControlKey` (Noise_IK: `MixHash(controlKey)`, `es` DH với control key, encrypt machine pubkey) (`control/controlbase/handshake.go:31`, `68-90`).

**Host header:** `req.URL = u` với `u.Host = JoinHostPort(Hostname, port)` (`client.go:251-259`); stdlib ghi Host từ `r.URL.Host` khi `r.Host` rỗng, **không** bỏ port mặc định (`GOROOT/src/net/http/request.go:611-622`). ⇒ Host = `a.mesh.test:80` hoặc `a.mesh.test:443`. Verified (code read, stdlib local). Qua HTTP proxy môi trường (`client.go:465-476`), Host vẫn như vậy — **Assumption** proxy không rewrite.

**SNI:** `dnscache.TLSDialer` đặt `ServerName = host` (tách port) nếu trống (`net/dnscache/dnscache.go:694-710`) ⇒ SNI = `a.mesh.test`. Verified (code read). Port 80: không có TLS ⇒ không có SNI.

**TLS cert của `/ts2021` không được enforce:** `VerifyConnection` bị bọc để lỗi chỉ log rồi `return nil` (`client.go:494-502`). Verified (code read). (Khác với `/key`, nơi cert được verify — Q1.)

**Dial plan:** nhận từ `MapResponse.ControlDialPlan` (`direct.go:1332-1339`); mỗi candidate dial tới IP cố định qua resolver tĩnh nhưng **giữ `Hostname`** (`client.go:98-170`, `409-417`) ⇒ Host/SNI không đổi; hết candidate thì fallback DNS (`client.go:160-165`). `TS_USE_CONTROL_DIAL_PLAN` mặc định bật (`client.go:96`, `104`). Verified (code read). Headscale hiện không set `ControlDialPlan` (grep `DialPlan` trong `hscontrol/**/*.go` không test: không có kết quả) — Verified (grep).

**WebSocket:** chỉ ở build `js/wasm` (`control/controlhttp/client_js.go:22-57`): `wss://host/ts2021?X-Tailscale-Handshake=...` (hoặc `ws://host:port` khi NoPort). Server `acceptWebsocket` đọc handshake từ form (`control/controlhttp/controlhttpserver/controlhttpserver.go:126-165`). Verified (code read). Không thấy fallback DERP trong `control/controlhttp` (grep `derp` không kết quả) — Verified (grep); không có fallback control qua DERP ở nơi khác là **Assumption**.

**Request bên trong Noise:** URL `serverURL + "/machine/register"` (đổi `http:`→`https:`), header `Ts-Lb: <nodekey>` (`direct.go:813-827`, `control/ts2021/client.go:314-318`) ⇒ `:authority` HTTP/2 bên trong là `a.mesh.test` — Verified (code read) về URL; `:authority` cụ thể là **Assumption** (hành vi Go HTTP/2).

## Q5. Pinning/cache theo ControlURL host có thể trộn A và B?

- Không có pin Noise key theo host trong state persist (Q1). Verified (code read).
- `dnsCache` tạo riêng mỗi `Direct` (`direct.go:326-331`); health `SetTLSConnectionError` theo ServerName (`tlsdial.go:150-160`) chỉ là cảnh báo. Verified (code read).
- Điểm có thể trộn: machine key + disco key dùng chung; dial plan dùng chung nếu ControlURL trùng; dedup profile theo ControlURL+ID (Q3). Verified (code read) trừ disco key (Assumption).
- Phía server (không phải client) là nơi nguy hiểm nhất: cùng machine key đến từ 2 tailnet. Headscale hiện có `noisePrivateKey` đơn (`hscontrol/app.go:151`, `hscontrol/noise.go:116-122`) và lookup theo machine key — cần audit ở P02 implementation. **Assumption** (chưa đọc lại phần DB lookup trong ticket này).

---

## Hệ quả thiết kế

1. **Bind tailnet theo Host header là bắt buộc; SNI chỉ là kiểm tra bổ sung.** Port 80 (đường mặc định đầu tiên) không có SNI. Nếu không mở port 80, client vẫn chạy qua 443 sau ≤500 ms/khi 80 lỗi (Verified code; thời gian thực tế là Assumption).
   - Chuẩn hóa Host: bỏ port, lowercase, bỏ dấu `.` cuối, từ chối IP literal/host lạ ⇒ **fail closed** (không có "default tailnet").
   - Trên TLS: yêu cầu `r.TLS.ServerName` (sau chuẩn hóa) == Host; lệch ⇒ từ chối trước handshake (đề xuất 421/400).
   - Nếu có reverse proxy TLS phía trước: app không thấy SNI; proxy phải chuyển Host nguyên vẹn và tự kiểm tra SNI==Host. Phải ghi ADR.
2. **Host/SNI không phải xác thực.** Cả hai do client tự khai và Noise không ràng buộc chúng. Bảo đảm thật: client đã mã hóa initiation tới pubkey của tailnet nó lấy từ `/key` của hostname đó; server chọn sai key ⇒ `decrypting machine key` thất bại (`handshake.go:255-280`). MITM sửa Host trên port 80 chỉ gây DoS, không chuyển tenant (Assumption cần negative test N2).
   - `TailnetID` gắn vào phiên Noise = tailnet của private key đã hoàn tất handshake; truyền vào context của mọi handler `/machine/*`. **Không** trial-decrypt lần lượt các key (tốn O(N), mờ ràng buộc).
   - Đối chiếu `:authority`/Host bên trong Noise với tailnet của phiên (defense in depth).
3. **`/key` phải theo Host và qua HTTPS có cert hợp lệ cho từng hostname.** Đây là điểm duy nhất client xác thực server key bằng WebPKI. ControlURL `http://` cho phép MITM thay key ⇒ chỉ dùng trong lab. `/key` với Host lạ ⇒ 404/400, không trả key mặc định.
4. **Cùng machine key ở nhiều tailnet là bình thường** (profile A/B trên một device). Unique constraint phải là `(tailnet_id, machine_key)`; mọi lookup/cache/event theo machine key hoặc disco key phải có `tailnet_id`. Lưu ý: nhà vận hành nền tảng có thể liên kết thiết bị giữa các tailnet qua machine key — cần nêu trong threat model.
5. **Rotate Noise key theo tailnet:** client không làm mới Noise client trong vòng đời `Direct` ⇒ cần chiến lược overlap/thông báo restart; ghi ADR.
6. **Dial plan (nếu sau này bật):** phải per-tailnet; không cần đổi Host. Client giữ dial plan qua switch profile nếu ControlURL trùng ⇒ không dùng chung ControlURL giữa tailnet.
7. **UserID/StableNodeID duy nhất toàn cục** để tránh dedup profile phía client nếu có cấu hình ControlURL trùng.

**Blocker hiện tại:** chưa có blocker cứng từ code client — client chọn key theo URL/hostname, nên thiết kế "một hostname ↔ một Noise key" khả thi về mặt luồng code. Rủi ro mở (cần E2E, không thể khẳng định tương thích): (a) port 80 không SNI ⇒ bắt buộc bind Host; (b) cert `/ts2021` không enforce; (c) key rotation không tự hồi phục; (d) machine/disco key dùng chung giữa profile.

---

## POC với client thật (chỉ lab, credentials ngắn hạn, log đã redact)

Chuẩn bị: hai hostname `a.mesh.test`, `b.mesh.test` trỏ về server lab; cert hợp lệ cho từng hostname (CA lab nạp vào client); server stub log `r.Host`, `r.TLS != nil`, `r.TLS.ServerName`, tailnet được chọn, `ShortString()` của machine key (không log full key/secret).

1. **Key theo hostname:** `tailscale debug ts2021 --host a.mesh.test` rồi `--host b.mesh.test` (`cmd/tailscale/cli/debug.go:308-319`, `/key` qua `https://` tại `debug.go:1068`, machine key tạm `debug.go:1087`). Kỳ vọng: 2 pubkey khác nhau, cả hai handshake OK. Thử thêm `--connect-ip <IP>` để kiểm tra Host/SNI giữ hostname.
2. **Đường port 80 vs 443:** chạy tailscaled với `TS_DEBUG_NOISE_DIAL=1`; lần đầu kỳ vọng thấy `http://a.mesh.test:80/ts2021` (Host `a.mesh.test:80`, không TLS) rồi/hoặc `https://...:443` (SNI `a.mesh.test`, Host `a.mesh.test:443`). Lặp lại với `TS_FORCE_NOISE_443=1`, và với port 80 bị chặn.
3. **Profile A→B→A cùng machine key:**
   - `tailscale login --login-server https://a.mesh.test --auth-key <A-short-lived>`
   - `tailscale login --login-server https://b.mesh.test --auth-key <B-short-lived>`
   - `tailscale switch <profile A>`; rồi `tailscale switch <profile B>`; restart tailscaled giữa chừng.
   - Kỳ vọng/đo: machine key (short) giống nhau ở A và B; node key khác nhau; DB có 2 node khác `tailnet_id` cùng machine key; quay lại A dùng lại node key cũ (không register mới); netmap A không chứa peer/route/DNS/DERP của B và ngược lại; `/key` được fetch lại sau mỗi switch.
4. **Rotate key A** khi client đang kết nối: ghi lại client có tự hồi phục hay cần restart (so với Q1).

## Negative tests (lỗi nào cũng là BLOCK MERGE)

| ID | Kịch bản | Kỳ vọng |
|---|---|---|
| N1 | TLS: SNI=`b.mesh.test`, Host=`a.mesh.test` (client Go tự viết, đặt `tls.Config.ServerName` riêng) cho cả `/key` và `/ts2021` | Từ chối trước handshake; không log/tạo node |
| N2 | Host=`b`, initiation mã hóa tới pubkey A (port 80 và 443) | Handshake thất bại; không thử key khác; không tạo node |
| N3 | Host thiếu / IP literal / hostname lạ / `a.mesh.test.evil` | `/key` và `/ts2021` fail closed; không có default tailnet |
| N4 | Biến thể Host: `A.MESH.TEST`, `a.mesh.test.`, `a.mesh.test:80`, `a.mesh.test:443`, `a.mesh.test:8443` | Chuẩn hóa về cùng tailnet A; port không phải của listener ⇒ chính sách rõ ràng (ghi ADR) |
| N5 | Phiên Noise A, request bên trong có `:authority`=`b.mesh.test` | Từ chối |
| N6 | Phiên Noise B, register bằng pre-auth key của A | Từ chối; không tạo node ở A hay B |
| N7 | Phiên Noise B gửi `/machine/map` với node key thuộc tailnet A (cùng machine key) | Từ chối; không lộ netmap A |
| N8 | Xóa/expire node ở A | Node cùng machine key ở B không đổi |
| N9 | WebSocket upgrade (`Upgrade: websocket`) với Host/SNI lệch | Cùng quy tắc như N1–N3 |
| N10 | Plain HTTP port 80 qua HTTP proxy, Host=`a` | Chọn đúng A hoặc từ chối theo ADR; không fallback |

## Path & symbol đã đọc

- `control/controlclient/direct.go`: `Options`, `NewDirect`, `Close`, `doLogin`, `loadServerPubKeys`, `getNoiseClient`, xử lý `ControlDialPlan`, `sendMapRequest` (đoạn đầu).
- `control/controlclient/auto.go:180`.
- `control/ts2021/client.go`: `NewClient`, `dial`, `AddLBHeader`.
- `control/controlhttp/{client.go,constants.go,client_js.go}`, `controlhttpserver/controlhttpserver.go`.
- `control/controlbase/handshake.go`: `ClientDeferred`, `Server`.
- `net/dnscache/dnscache.go:TLSDialer`, `net/tlsdial/tlsdial.go:Config`.
- `ipn/store.go`, `ipn/prefs.go` (`Prefs`, `LoginProfile`), `types/persist/persist.go`.
- `ipn/ipnlocal/local.go`: `Start/startLocked`, `createGetMachinePrivateKeyFunc`, `initMachineKeyLocked`, `clearMachineKeyLocked`, `SwitchProfile`, `switchToBestProfileLocked`, `resetDialPlan`, `resetForProfileChangeLocked`, `resetControlClientLocked`, `NewProfile`, `ResetAuth`, logout path, `profileDataPathLocked`.
- `ipn/ipnlocal/profiles.go`: `findMatchingProfiles`, `SetPrefs`, `setProfilePrefs`, `writePrefsToStore`.
- `ipn/ipnlocal/diskcache.go`, `ipn/ipnlocal/tailnet-lock.go:chonkPathLocked`, `ipn/localapi/localapi.go` (profiles), `cmd/tailscale/cli/{login.go,up.go,debug.go}`, `health/health.go:LastNoiseDialWasRecent`.
- Repo: `hscontrol/handlers.go:221-254` (`KeyHandler`), `hscontrol/noise.go:91-129` (`NoiseUpgradeHandler`).
- Stdlib: `GOROOT/src/net/http/request.go:611-622`.

## Giới hạn

- Chỉ đọc code, chưa chạy client thật; không có artifact E2E. Không được dùng tài liệu này để tuyên bố `Verified` cho tương thích client.
- Chưa đọc client macOS/iOS/Android/Windows GUI (có thể có store/profile riêng; ví dụ Windows có `LocalUserID` per profile).
- Chưa đọc chi tiết `ipn/ipnlocal` cho mọi đường "implicit profile switch" (TODO `tailscale/corp#28014`, `profiles.go:374-389`).
