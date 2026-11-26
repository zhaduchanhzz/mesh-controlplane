# Runbook: xoay Noise key của cluster (bản nháp, chưa diễn tập)

- **Trạng thái:** `Planned` — bản nháp P13e (2026-11-26). Các lệnh dưới đây đã có test trong process (`evidence/P13e-noise-rotation.md`), **chưa** chạy trên lab. Drill trên c2 (P13e) sẽ sửa runbook này và điền §6.
- **Thiết kế:** ADR 0014 (xoay "lạnh" qua switchover, **không** chồng lấn hai key; mô hình Noise không đổi, AGENTS rule 12), ADR 0005 (một Noise key mỗi cluster).
- **Không thuộc mkdocs** (`mkdocs.yml` loại `ops/`): tài liệu vận hành lab, có tên VM/đường dẫn lab.

## 0. Nguyên tắc

- Mỗi cluster có **một** Noise key; mọi process của cluster (active + standby) dùng cùng key. Registry (`clusters.noise_public_key`) là nguồn sự thật: process có key khác registry **không phục vụ**.
- Active kiểm lại registry **mỗi chu kỳ reconcile (2 s)**: registry đổi sang key khác → active ngừng phục vụ, nhả lease, đứng standby (chờ một TTL rồi mới thử lại; lấy lại được cũng nhả ngay vì key vẫn lệch). Vì vậy **chính lệnh `rotate` là điểm switchover**.
- Process cluster **không bao giờ tự tạo key**. File key thiếu → process từ chối khởi động (`cluster Noise private key file is missing`). `--init-noise-key` chỉ dùng **một lần** cho process đầu tiên của cluster **mới** (registry chưa có key); registry đã có key thì cờ này cũng bị từ chối.
- Không in private key ra terminal, log, artifact, ảnh chụp (AGENTS rule 2). Chỉ dùng `--generate` hoặc pipe qua ssh; không đặt key trong argv/biến môi trường. Public key (`mkey:…`) in được.
- Không có thu hồi phía client: client chưa nối lại vẫn tin key cũ tới khi `Direct` được tạo lại (restart `tailscaled`, đổi profile) — `evidence/P02-noise-client-research.md` Q1.

Ký hiệu lab (c2): `A` = VM đang active, `S` = VM đang standby (mc-c2 `.244`, mc-c2b `.246`; xác định bằng `/ready`). Trên VM:

```sh
HSC="sudo -u headscale /usr/local/bin/headscale-cluster -c /etc/headscale/cluster/config.yaml cluster --cluster-id mc-c2"
KEY=/var/lib/headscale/cluster/noise_private.key
```

## 1. Kiểm tra trước

1. Cả hai process chạy, đúng một process trả `/ready` 200 (A):
   `curl -s -o /dev/null -w '%{http_code}\n' --resolve c2.cp.gta5web.online:8443:127.0.0.1 https://c2.cp.gta5web.online:8443/ready` trên từng VM.
2. `$HSC noise-key show` trên A và S: cùng `file_public_key`, `match: true`. Ghi lại public key cũ (`K_old`).
3. Danh sách tenant của cluster (để thông báo), chỉ đọc, trên DB nền tảng:
   `SELECT id, slug, status FROM tailnets WHERE owner_cluster_id = 'mc-c2' AND status <> 'DELETING';`
4. Phiên bản client mỗi tenant (để đo §6), ping nền giữa các client (đo khoảng mất).
5. Không có move tenant đang chờ trên cluster (`GET /v1/moves`), không có drill khác đang chạy.

## 2. Xoay có kế hoạch

| Bước | Ở đâu | Lệnh / hành động | Kỳ vọng |
|---|---|---|---|
| 1 | — | Thông báo admin **mọi** tenant (§1.3): cửa sổ, ảnh hưởng (mất control plane vài giây, có thể phải restart `tailscaled`) | |
| 2 | S | `sudo systemctl stop headscale-cluster` | A vẫn phục vụ |
| 3 | S | `$HSC noise-key install --generate` | in `public_key` = `K_new`, `replaced_public_key` = `K_old`; file 0600, ghi nguyên tử. Không đụng DB |
| 4 | S | `sudo systemctl start headscale-cluster` | S là standby (A giữ lease; S không chạm registry khi chưa lấy được lease) |
| 5 | S | `$HSC noise-key show` | `file_public_key` = `K_new`, `registered_public_key` = `K_old`, `match: false` (đúng) |
| 6 | S | **Switchover:** `$HSC noise-key rotate` (lấy public key từ file của S) | in `from` = `K_old`, `to` = `K_new`; audit `RotateNoiseKey` actor `cli:mc-c2` |
| 7 | A, S | Theo dõi log | A trong ≤ 2 s: `the registry names another Noise key for this cluster: stepping down`, rồi `cluster lease ended: standby`; S trong ~1 s sau: `cluster lease acquired: active`, `cluster active: serving` |
| 8 | ngoài | `/key` qua URL công khai trả `K_new`; `/ready` của S 200, của A 503 | |
| 9 | A | `sudo systemctl stop headscale-cluster` | |
| 10 | S→A | Chép key qua pipe, có kiểm định dạng ở A: `ssh S "sudo cat $KEY" \| ssh A "$HSC noise-key install --from /dev/stdin"` | A in `public_key` = `K_new` |
| 11 | A | `sudo systemctl start headscale-cluster`; `$HSC noise-key show` | standby; `match: true` |
| 12 | A, S | So `sha256sum $KEY` hai VM (chỉ so hash, không in nội dung) | bằng nhau |
| 13 | — | Đo §6 (1, 5, 15, 60 phút); sau ngưỡng thì yêu cầu restart `tailscaled` cho client chưa nối lại | |

**Rollback** (trước bước 9, khi A vẫn còn file `K_old`): `$HSC noise-key rotate --public-key K_old` chạy ở bất kỳ VM có cấu hình DB → S (key mới) tự ngừng phục vụ trong ≤ 2 s, A lấy lại lease (sau tối đa một TTL nếu A vừa step down). Sau bước 10 không còn file `K_old` nào: rollback = xoay tiếp sang key mới.

## 3. Xoay khẩn cấp (key bị coi là lộ)

1. Làm ngay §2 bước 2–12, **bỏ** cửa sổ thông báo trước. Nếu VM A bị nghi bị chiếm: **không** chép key mới sang A (bỏ bước 9–12, để A dừng), xử lý A riêng (dựng lại VM cần duyệt riêng). Nếu cả hai VM bị nghi: sinh key trên máy tin cậy rồi `noise-key install --from /dev/stdin` qua pipe, không để file key ngoài VM đích.
2. Thông báo admin mọi tenant **cùng lúc** với xoay: key cũ bị coi là lộ; client chưa nối lại vẫn tin key cũ và có thể bị giả control server nếu kẻ tấn công chặn được đường mạng (client không enforce TLS của `/ts2021`, P02); phải restart `tailscaled` sớm nhất.
3. Điều tra nguồn lộ: quyền file key trên hai VM (`stat $KEY`: `headscale:headscale 0600`), ai có quyền đọc, log truy cập; xoay luôn secret `/verify` của DERP cluster đó và cert TLS nếu VM bị chiếm.
4. Nếu chỉ cần chặn ngay process đang giữ key lộ (chưa có key mới sẵn sàng): `$HSC noise-key rotate --public-key <K_new đã sinh ở nơi tin cậy>` → mọi process key cũ ngừng phục vụ trong ≤ 2 s (control plane cluster dừng, data plane chạy tiếp theo netmap cuối) tới khi một process với `K_new` khởi động. Đây là dừng có chủ ý (fail closed).
5. Rủi ro còn lại: không có danh sách thu hồi trong giao thức; client không restart vẫn có thể bị giả mạo tới khi restart.

## 4. Cluster mới (lần khởi động đầu)

`$HSC serve --init-noise-key` **một lần** trên process đầu tiên (registry chưa có key): process tạo key (ghi nguyên tử, 0600), ghi public key vào registry khi lấy lease. Sau đó chép key sang process thứ hai bằng §2 bước 10 và **không** dùng cờ này trong unit systemd lâu dài.

## 5. Chẩn đoán

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| Process không khởi động: `cluster Noise private key file is missing` | File key thiếu (VM dựng lại, sai đường dẫn) | Chép key của cluster từ process kia (§2 bước 10). Không dùng `--init-noise-key` cho cluster đã có key (bị từ chối, lỗi nêu key đã đăng ký) |
| Process ở standby mãi, log `cluster Noise key: … differs from the registered one` | File key khác registry | `$HSC noise-key show`; cài đúng key hoặc xoay registry có chủ đích |
| Sau `rotate`, không process nào phục vụ | Không process nào có `K_new` đang chạy | Khởi động process đã cài `K_new`, hoặc rollback (§2) |
| `rotate` báo `stale cluster revision` | Registry đổi giữa lúc đọc và ghi | Chạy lại `noise-key show`, rồi `rotate` |
| `rotate` báo `already the registered Noise key` | Đã xoay rồi | Không làm gì |
| `install` báo `parsing private key` | File nguồn không phải private key (`privkey:` + 64 hex) | Sinh lại bằng `--generate` |

## 6. Ảnh hưởng tới client (đo trong drill P13e — chưa có số)

Điền sau drill trên c2 (≥ 2 tenant, client 1.104.7 và 1.84.3, VM A/B và container), ×2 lần, có kế hoạch và khẩn cấp:

| Chỉ số | 1.104.7 | 1.84.3 |
|---|---|---|
| Thời gian từ `rotate` tới `/ready` của S | | |
| Thời gian tới ping/`tailscale status` phục hồi (không restart) | | |
| Tỉ lệ tự phục hồi không restart sau 1 / 5 / 15 / 60 phút | | |
| Có phải đăng nhập lại? (dự kiến: không) | | |
| NodeID / IP giữ nguyên? | | |
| Client tenant cluster khác (c1) bị ảnh hưởng? (dự kiến: không) | | |

Điều kiện dừng drill (ticket P13e): client không hồi phục kể cả sau restart `tailscaled`, hoặc phải đổi cách xử lý handshake phía server (nhiều key) → dừng, ghi ADR, log protocol đã redact (AGENTS rule 12).
