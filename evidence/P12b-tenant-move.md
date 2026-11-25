# PR P12b Evidence — chuyển tenant có kế hoạch giữa hai cluster

- **Repo commit before / after:**
  - Before: `master` `fc339e8`.
  - After: `feat/P12b-inventory` (bảng commit dưới; commit chứa file này là commit cuối).
  - Commit trước bước thông báo: `60333af` inventory, `98843e8`/`51a3bea` lịch, `8688aac`/`e016bd5` API + scheduler.
- **Verified source paths & symbols:** `evidence/source-inventory-P12b.md` (`60333af`) cộng các file sửa liệt kê dưới đây.
- **Design choice / ADR:**
  - Quyết định Q1–Q7 của chủ dự án (2026-11-26): `tickets/P12b.md` §Câu hỏi, tất cả theo đề xuất.
  - Cơ chế chuyển: **ADR 0010** (không đổi). Noise: **ADR 0005** một key mỗi cluster (không đổi). Ghi chú thiết kế mới của P12b ở §ADR note.
- **Trạng thái:** `Implemented`, phần code `Verified` bằng unit/race/servertest. **E2E lab chưa chạy** (harness viết xong, chạy cần coordinator). **B3 trên code thật chưa đo** (cần bật mc-bench). Chưa `Reviewed`.

## Tóm tắt thay đổi

| Bước | Thay đổi | Commit (RED → GREEN) |
|---|---|---|
| Q2 lịch | Bảng `tailnet_moves` (migration mới, nối cuối chuỗi v0.1, trong transaction); `Registry.ScheduleMove / CancelScheduledMove / GetMove / ListMoves / PendingMove / RunDueMoves` | `98843e8` → `51a3bea` |
| Q2 API + scheduler | Operator API (mTLS, audit, idempotent): `POST /v1/tailnets/{id}/moves`, `GET /v1/tailnets/{id}/moves`, `GET /v1/moves/{move_id}`, `POST /v1/moves/{move_id}/cancel`; `mesh-placement -schedule-interval` (mặc định 5 s) chạy `RunDueMoves` | `8688aac` → `e016bd5` |
| Q1 thông báo | MapResponse `DisplayMessages` (capver ≥ 117) hoặc `Health` (cũ hơn) cho client của tailnet sắp chuyển; xóa khi hủy | `4840114` → `8904c97` |
| Q3 API tenant | `GET /api/v2/tailnet/{tailnet}/scheduled-move` chỉ đọc; client v2 sinh lại | `289ec8d` → `adeaf2f` |
| Q5 re-bootstrap | `internal/rebootstrap`, `cmd/mesh-rebootstrap` (công cụ vận hành; test viết cùng code, không có commit RED riêng) | `996ce77` |
| Q6 B3 | `bench-db b3 -designs …,registry`: đo đường `MoveOwner` + `HandOver` thật | `3749ec8` (build/vet, chưa chạy) |
| Q7 E2E | `infra/lab/e2e/p12b/run.sh` (`bash -n` sạch; shellcheck không có trên máy; chưa chạy) | `c0667d2` |

## ADR note (ghi nhận thiết kế, theo yêu cầu coordinator)

1. **`tailnet_moves` là bảng nền tảng, không phải bảng tenant.** Cột `tailnet_id` chỉ tailnet bị chuyển, không phải phạm vi tenant. Plugin `tenantscope` tự scope mọi model có cột `tailnet_id`, nên bảng này **chỉ được đọc/ghi bằng SQL tường minh** (`Raw(...).Rows()` + scan tay, `Exec`). Không có model GORM.
2. **Một move mở mỗi tailnet:** unique partial index `idx_tailnet_moves_open ON (tailnet_id) WHERE state IN ('SCHEDULED','REQUESTED')`. CHECK trên `state`; FK `tailnet_id → tailnets(id)`.
3. **Không đổi cơ chế ADR 0010.** Scheduler chỉ gọi `MoveOwner` (đặt `moving_to`, giữ owner/epoch). Cluster nguồn vẫn tự retire rồi `HandOver` (epoch + 1 đúng một lần). Scheduler ghi `HANDED_OVER` khi thấy owner = đích và `moving_to` trống. Owner/epoch đổi giữa lúc lên lịch và lúc tới hạn → `FAILED` (không chuyển). Request đã có sẵn trùng đích (crash giữa hai bước) → nhận lại, không fail.
4. **Abort:** `SCHEDULED` → `CANCELLED`. `REQUESTED` → `CancelMove` (xóa `moving_to` khi owner vẫn là nguồn) rồi `CANCELLED`. Sau trao tay: `ErrConflict`, chỉ đi tiếp (chuyển ngược là một move mới).
5. **Thứ tự nâng cấp:** `LatestMigrationID` đổi thành `202611261000-tailnet-moves`. Bootstrap mở DB read-only và đòi đúng migration mới nhất, nên **cluster, mesh-placement và mesh-bootstrap phải nâng cấp cùng nhau** (migration do process đầu tiên mở DB ghi chạy).
6. **Thông báo không lộ đích:** text nói thời gian, lý do, lệnh cần chạy; không có cluster ID hay URL đích (client hỏi bootstrap sau khi chuyển, ADR 0005).
7. **Giới hạn client cũ (capver < 117):** nhận `Health`. Khi hủy, gửi `Health = []`, xóa **mọi** health message từ control. Control plane không gửi Health nào khác, nên hiện tại không mất gì; nếu sau này thêm Health khác thì phải gộp.
8. **API tenant đọc từ state của runtime** (`State.MoveNotice`), không truy vấn chéo tailnet; trễ tối đa một chu kỳ reconcile (2 s) so với DB.
9. **Re-bootstrap không cầm secret:** tool chỉ gửi key id + SHA-256 proof cho bootstrap; lệnh mỗi target nhận `{target}`/`{login_server}`, key tới client qua `--auth-key=file:` do operator đặt sẵn. Target và URL bị giới hạn ký tự (không chèn được cú pháp shell). Output giữ phần đuôi đã redact.

## Runbook — chuyển tenant có kế hoạch

**Trước khi lên lịch**
1. Đích `ACTIVE`, có lease sống, còn sức chứa (`GET /v1/clusters`). Tailnet `ACTIVE`, không có move mở (`GET /v1/tailnets/{id}/moves`).
2. Chọn thời điểm; báo trước cho admin tenant (mẫu thông báo dưới).

**Lên lịch** (operator, mTLS):
```
POST /v1/tailnets/{id}/moves   Idempotency-Key: <khóa>
{"to_cluster":"<đích>","scheduled_at":"2026-11-27T10:00:00Z","reason":"rebalance"}
```
- Trong ≤ 1 chu kỳ reconcile, mọi client của tailnet thấy thông báo; `GET /api/v2/tailnet/-/scheduled-move` của tenant trả thời gian + lý do.

**Tới giờ** (tự động): mesh-placement yêu cầu move → nguồn retire và trao tay → bootstrap trả 503 MOVING rồi URL đích. Client vẫn trỏ nguồn nhận 401 và thử lại mãi, **không tự đổi ControlURL** (cần re-login).

**Re-bootstrap client** (chạy ngay trước giờ, tool tự chờ tới khi bootstrap trả URL khác nguồn):
```
mesh-rebootstrap -bootstrap https://boot.<domain>:8444 -authkey-file <key reusable của tenant> -tenant <slug> \
  -from <URL nguồn> -poll 1s -targets hosts.txt -concurrency 8 -timeout 3m -out result.json \
  -cmd '<lệnh theo loại client, dưới>'
```
| Loại client | `-cmd` / thao tác |
|---|---|
| Linux (systemd tailscaled), qua SSH | `ssh {target} sudo tailscale up --login-server {login_server} --auth-key=file:/etc/mesh/authkey --force-reauth` (giữ nguyên các cờ `up` khác đã dùng, như `--hostname`) |
| Docker container `tailscale/tailscale` | `docker exec -i {target} tailscale up --login-server={login_server} --auth-key=file:/dev/stdin --hostname={target} --force-reauth <keyfile` |
| macOS / Windows (người dùng tự làm) | Mở bootstrap/URL do admin gửi; `tailscale up --login-server <URL mới> --force-reauth` (CLI) hoặc đăng xuất và đăng nhập lại server mới trong GUI |
| iOS / Android | Đổi "custom coordination server" sang URL mới rồi đăng nhập lại (không tự động được) |

- Q4: node cũ được giữ; re-login cùng machine key dùng lại node (cùng NodeID, IP).
- `result.json`: thời gian từng target, t50/t90/t100, target lỗi (chạy lại riêng các target đó).

**Abort:**
- Trước giờ hoặc khi đã `REQUESTED` mà nguồn chưa trao tay: `POST /v1/moves/{move_id}/cancel`. Thông báo trên client biến mất.
- Sau `HANDED_OVER`: không hủy được; muốn về lại thì lên lịch move ngược.
- `FAILED` (đích mất lease, owner/epoch đổi): tailnet không bị đụng; xem `error`, sửa nguyên nhân, lên lịch lại.

**Kiểm tra sau:** `GET /v1/moves/{move_id}` = `HANDED_OVER`; `GET /v1/reconcile` = `[]`; số node ở đích bằng trước; không còn client 401 ở nguồn (log).

**Mẫu thông báo cho admin tenant:**
> Tailnet `<slug>` sẽ chuyển sang control cluster khác lúc `<YYYY-MM-DD HH:MM UTC>` (lý do: `<reason>`). Kết nối giữa các thiết bị vẫn chạy trong lúc chuyển, nhưng thiết bị không nhận thay đổi mới cho tới khi đăng nhập lại. Sau giờ trên, chạy công cụ re-bootstrap hoặc `tailscale up --login-server <URL lấy từ bootstrap> --force-reauth` với pre-auth key của tailnet. Thiết bị giữ nguyên tên và IP.

## Tests + exact commands + exit codes

Chạy trên mc-ci trong `~/src/p12b` (cây từ `git archive` của nhánh, CRLF đã bỏ), Go của mc-ci, `GOFLAGS=-buildvcs=false`.

(xem §Kết quả test)

## E2E artifacts / protocol map redacted

Chưa chạy. Harness `infra/lab/e2e/p12b/run.sh` (§Q7). Ngưỡng: theo Q7, cố định sau lần đo đầu.

## Negative tenant isolation tests

- `TestScheduledMoveNotifiesOnlyItsClients` (servertest, hai tailnet trên một cluster): client của B không bao giờ nhận thông báo move của A (3 s); thông báo không chứa cluster ID đích; key API của B thấy `scheduled:false`, không thấy lý do của A.
- `TestMoveNoticesOnlyOwnOpenMoves`: chỉ move mở của tailnet ACTIVE do chính cluster sở hữu; không lấy move của cluster khác, move đã đóng, tailnet đã xóa.
- `TestAPIv2ScheduledMove`: `{tailnet}` khác `-`/ID của key → 404; POST → 405.
- `TestEndpointInventory`: thêm đúng 3 dòng (`cluster-v2`, `public`, `unix`) cho route mới; `evidence/P08a-endpoint-inventory.md` §8 cập nhật.

## DB migrations and downgrade constraints

- Migration `202611261000-tailnet-moves`: thêm cuối chuỗi v0.1, trong `db.Transaction`, không `AutoMigrate`, không tắt FK. Rollback = `DROP TABLE IF EXISTS tailnet_moves` (cũng trong transaction).
- Downgrade: binary cũ mở DB đã có migration mới sẽ từ chối (migration lạ); bootstrap cũ cũng vậy. Nâng/hạ cả bộ cùng lúc (ADR note 5).

## Benchmark before/after

B3 cũ (câu SQL mô phỏng, 2026-11-22) trong `tickets/P12b.md`. Đo lại trên code thật: **chưa chạy** (mc-bench đang tắt). Lệnh:
```
bench-db b3 -host <pg> -port 5432 -db bench_b3_p12b -user bench -passfile <file> \
  -sizes 1000,5000,10000 -others 9 -other-devices 5000 -trials 7 -designs owner_cas,row_rewrite,registry -out b3-p12b.json
```

## Known limitations / security blockers

- Client không tự đổi ControlURL: mọi client phải re-login (Q1 đã chọn không đổi protocol). iOS/Android cần người dùng thao tác.
- Health của client cũ (capver < 117) bị xóa toàn bộ khi hủy thông báo (ADR note 7).
- Trễ API tenant ≤ 1 chu kỳ reconcile.

## Reviewer approval

Chưa.
