# PR P12b Evidence — chuyển tenant có kế hoạch giữa hai cluster

- **Repo commit before / after:**
  - Before: `master` `fc339e8`.
  - After: `feat/P12b-inventory` (bảng commit dưới; commit chứa file này là commit cuối).
  - Commit trước bước thông báo: `60333af` inventory, `98843e8`/`51a3bea` lịch, `8688aac`/`e016bd5` API + scheduler.
- **Verified source paths & symbols:** `evidence/source-inventory-P12b.md` (`60333af`) cộng các file sửa liệt kê dưới đây.
- **Design choice / ADR:**
  - Quyết định Q1–Q7 của chủ dự án (2026-11-26): `tickets/P12b.md` §Câu hỏi, tất cả theo đề xuất.
  - Cơ chế chuyển: **ADR 0010** (không đổi). Noise: **ADR 0005** một key mỗi cluster (không đổi). Ghi chú thiết kế mới của P12b ở §ADR note.
- **Trạng thái:** `Implemented`, phần code `Verified` bằng unit/race/servertest. **E2E lab chưa chạy** (harness viết xong, chạy cần coordinator). **B3 trên code thật đã đo** (2026-11-25 20:32 UTC, mc-bench; §Benchmark before/after) và có một phát hiện: `MoveOwner` đếm thiết bị của cluster đích trong lúc giữ lock ownership (writer của tenant đang chuyển chờ ~95–153 ms). Chưa `Reviewed`.

## Tóm tắt thay đổi

| Bước | Thay đổi | Commit (RED → GREEN) |
|---|---|---|
| Q2 lịch | Bảng `tailnet_moves` (migration mới, nối cuối chuỗi v0.1, trong transaction); `Registry.ScheduleMove / CancelScheduledMove / GetMove / ListMoves / PendingMove / RunDueMoves` | `98843e8` → `51a3bea` |
| Q2 API + scheduler | Operator API (mTLS, audit, idempotent): `POST /v1/tailnets/{id}/moves`, `GET /v1/tailnets/{id}/moves`, `GET /v1/moves/{move_id}`, `POST /v1/moves/{move_id}/cancel`; `mesh-placement -schedule-interval` (mặc định 5 s) chạy `RunDueMoves` | `8688aac` → `e016bd5` |
| Q1 thông báo | MapResponse `DisplayMessages` (capver ≥ 117) hoặc `Health` (cũ hơn) cho client của tailnet sắp chuyển; xóa khi hủy | `4840114` → `8904c97` |
| Q3 API tenant | `GET /api/v2/tailnet/{tailnet}/scheduled-move` chỉ đọc; client v2 sinh lại | `289ec8d` → `adeaf2f` |
| Q5 re-bootstrap | `internal/rebootstrap`, `cmd/mesh-rebootstrap` (công cụ vận hành; test viết cùng code, không có commit RED riêng) | `996ce77` |
| Q6 B3 | `bench-db b3 -designs …,registry`: đo đường `MoveOwner` + `HandOver` thật | `3749ec8`; sửa harness + số đo: `5bf2a71` |
| Q7 E2E | `infra/lab/e2e/p12b/run.sh` (`bash -n` sạch; shellcheck không có trên máy; chưa chạy) | `c0667d2` |
| Merge master | `44e523a` (feat/CI-green) vào nhánh; xung đột duy nhất `placementapi/api.go` (định dạng route của master + 4 route move) | `4088df8` |
| Lint | golangci-lint 2.15.0 từ flake.lock (như `ci.yml`): 46 → **0 issues**; hai `//nolint:gosec` có lý do (G204, G703) trong `cmd/mesh-rebootstrap` | `464aac3` |

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

### Kết quả test

Cây trên mc-ci: `git archive` của `4088df8` (merge master `44e523a`) chép sang `~/src/p12b` (LF, không có golden CRLF), rồi đồng bộ các file đổi tới `da93abc`. Go 1.27.4. `nix` = `nix shell --inputs-from . nixpkgs#…` như `ci.yml`.

| Lệnh | Exit | Ghi chú |
|---|---|---|
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run --timeout 20m` (2.15.0) | 0 | **0 issues** (tại `464aac3`) |
| `go vet ./...` | 0 | |
| `go vet -tags benchdb ./cmd/bench-db` | 0 | |
| `go test -race -count=1 -timeout 60m $(go list ./... \| grep -v -e /integration -e /hscontrol/servertest)` | 1 | 40 package `ok`; chỉ `internal/placement` FAIL: `TestScheduleMoveValidates` panic ở `moves.go:162` (lỗi do merge, xem dưới) |
| `go test -race -count=1 -timeout 30m ./internal/placement/...` (sau `da93abc`) | 0 | `internal/placement` ok 72,7 s; `placementapi` ok 4,6 s |
| `nix shell … nixpkgs#tscli nixpkgs#opentofu -c go test -race -count=1 -timeout 60m ./hscontrol/servertest/...` | 0 | ok 1990 s, gồm `TestAPIv2` (GoClient, TSCLI, Terraform) |
| `go test -race -count=1 -timeout 60m -tags p05cluster ./hscontrol/servertest/` | 1 | Chỉ `TestAPIv2`, `TestAPIv2OAuthScopes` FAIL: `exec: "tscli"/"tofu": executable file not found` (lần này quên bọc `nix shell`); mọi test cluster, gồm `TestScheduledMoveNotifiesOnlyItsClients`, `TestClusterP07IncrementalAcrossTenants`, pass |
| `nix shell … nixpkgs#tscli nixpkgs#opentofu -c go test -race -count=1 -timeout 30m -tags p05cluster -run '^(TestAPIv2\|TestAPIv2OAuthScopes)$' ./hscontrol/servertest/` | 0 | ok 50,8 s |

**Lỗi do merge, đã sửa (`da93abc`, coordinator):** trước merge, `lockClusters(tx, mode, where, args…)` nhận chế độ khóa; master `44e523a` đổi thành `lockClusters(db, where, args…)`. Lời gọi `lockClusters(tx, "SHARE", "id = ?", to)` trong `ScheduleMove` vẫn biên dịch (tham số variadic) nhưng `"SHARE"` thành mệnh đề WHERE, không trả cluster nào, và truy cập `target[0]` panic. Các lần chạy trước merge trên mc-ci đều pass; GitHub CI run 45691631954 bắt được lỗi. Bài học: sau merge phải chạy lại cả suite, không chỉ lint.

**Ngoài P12b:** `TestACMETLSSettings/TLS-ALPN-01` "bind: address already in use" trên GitHub CI là flake cổng có từ trước (coordinator xác nhận), không xuất hiện trong các lần chạy trên mc-ci ở đây.

**Chạy trước merge (tại `adeaf2f`, mc-ci, `-race`):** `TestMoveNoticeInMapResponse`, `TestMoveNoticesOnlyOwnOpenMoves` (SQLite + PostgreSQL), `TestAPIv2ScheduledMove`, `TestEndpointInventory`, `TestScheduledMoveNotifiesOnlyItsClients` (p05cluster), `./internal/rebootstrap`, `./cmd/mesh-rebootstrap`: exit 0. RED trước GREEN: `TestMoveNoticeInMapResponse` và `TestScheduledMoveNotifiesOnlyItsClients` fail tại `4840114`; `TestAPIv2ScheduledMove`, `TestEndpointInventory` và assertion API trong `TestScheduledMoveNotifiesOnlyItsClients` fail (404) tại `289ec8d`.

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

B3/L11 trên đường code thật (Q6). "Before" = số đo 2026-11-22 (`tickets/P12b.md` §B3, `evidence/P14-db-benchmarks.md` §4). "After" = lần đo này. Trạng thái: `Verified` cho đúng phép đo dưới đây; chưa `Reviewed`.

### Môi trường và lệnh

| Mục | Giá trị |
|---|---|
| Thời điểm | 2026-11-25 20:32:12–20:33:58 UTC (lần chạy 2). Load average VM 0,10 → 0,99; **không** có tải nền nào khác (lần 2026-11-22 có benchmark P00c chạy song song) |
| VM | `mc-bench` (VMID 7703, 192.168.1.242), 8 vCPU, 24 GiB; bật/tắt bằng OpenTofu (dưới) |
| PostgreSQL | `postgres:17.6-bookworm`, digest `sha256:f3bd19c606e442c3d7bdfa8002e03fe260a1023351e0ea4598032022b68dd6e3` (cùng image với P14). PostgreSQL 17.6. Container `--cpuset-cpus 0-3 --memory 8g`; `shared_buffers=2GB`, `effective_cache_size=6GB`, `max_connections=400`, `synchronous_commit=on`, `wal_level=replica`, `max_wal_size=4GB`, `fsync=on`, `full_page_writes=on`, `pg_stat_statements`. Chỉ `127.0.0.1:55432`, kết nối trực tiếp (không PgBouncer). DB mới `bench_b3_p12b` (tạo bằng tay sau khi PG sẵn sàng) |
| Mật khẩu | Sinh trên VM (`openssl rand -hex 24`) vào `~/benchdb-p12b/pgpass` (thư mục 700), không in ra; harness đọc bằng `-passfile`. Xóa khi dọn. Đã grep giá trị mật khẩu trong mọi file kết quả: không có |
| Script | `stack.sh` của P14 không còn (không được commit, đã xóa khỏi VM), nên dựng lại theo §1 của P14: `b3-stack.sh`, `b3-run.sh`, `b3-summarize.py`, `b3-explain.sh` |
| Harness | `cmd/bench-db` từ cây `git archive` của nhánh (`15525e7` + sửa b3.go của `5bf2a71`; `da93abc`, commit vào nhánh trong lúc đo, chỉ đổi `ScheduleMove` trong `internal/placement/moves.go`, không nằm trên đường được đo), build trên mc-ci (Go 1.27.4, `CGO_ENABLED=0 GOFLAGS=-buildvcs=false`), binary sha256 `8ad386e7114d5b851ca093f035638692bad6677db399d23d3d7368eedf6d6c3b`. Ghim CPU 5–7 |
| Artifact | `artifacts/P12b-b3/` trong main checkout (gitignored, như mọi artifact E2E): `b3-p12b.json`, `b3-p12b.log`, `b3-p12b-attempt1.log`, `b3-p12b-summary.txt`, `b3-p12b-explain.txt`, các script, `tofu-plan-start.txt`, `tofu-plan-stop.txt` |

Lệnh và exit code:

| Bước | Lệnh | Exit |
|---|---|---|
| Bật VM | (WSL, main checkout `infra/lab`) `tofu fmt -check` / `validate` / `plan -var mc_bench_running=true -out=b3-true.tfplan` / `check-plan.py` / `apply b3-true.tfplan` | 0 / 0 / 0 / 0 / 0 |
| Build | (mc-ci) `go vet -tags benchdb ./cmd/bench-db`; `go build -tags benchdb -o bench-db ./cmd/bench-db`; `gofmt -l cmd/bench-db` rỗng | 0; 0; 0 |
| PG | `./b3-stack.sh up` | 0 |
| Lần 1 | `taskset -c 5-7 ./bench-db b3 -passfile <file> -port 55432 -db bench_b3_p12b -others 9 -other-devices 5000 -sizes 1000,5000,10000 -users 20 -trials 7 -designs owner_cas,row_rewrite,registry -out results/b3-p12b.json` (binary của `15525e7`) | **1**: pha cô lập xong, pha đồng thời dừng ở `probe registry mv-01000: MoveOwner: sql: database is closed` |
| Lần 2 | cùng lệnh, binary đã sửa, DB `bench_b3_p12b` drop và tạo lại | **0** |
| EXPLAIN | `b3-explain.sh` (chỉ đọc) | 0 |
| Dọn | `./b3-stack.sh down` (container, volume, file mật khẩu) | 0 |
| Tắt VM | `tofu plan -out=b3-false.tfplan` (mặc định `mc_bench_running=false`) / `check-plan.py` / `apply b3-false.tfplan` | 0 / 0 / 0 |

Tóm tắt plan (cả hai lần `check-plan.py` = "In lab scope"; mọi resource khác `no-op`):
- Bật: `proxmox_virtual_environment_vm.mc_bench will be updated in-place`, `~ started = false -> true`, `Plan: 0 to add, 1 to change, 0 to destroy.`
- Tắt: `proxmox_virtual_environment_vm.mc_bench will be updated in-place`, `~ started = true -> false`, `Plan: 0 to add, 1 to change, 0 to destroy.`

Sửa harness trong commit này (`cmd/bench-db/b3.go`, build tag `benchdb`, không đụng code chạy thật):
1. **Lỗi:** pha 1 đóng pool admin rồi mở lại (để flush table stats), nhưng `Registry` vẫn giữ pool cũ, nên pha 2 của `registry` lỗi `database is closed`. Sửa: tạo lại `Registry` trên pool mới.
2. **Probe của `registry` dùng fence thật** của `tenantscope.checkFence` (`pg_advisory_xact_lock_shared` trên `'tenantscope/ownership/' || id`, rồi `FOR SHARE OF t`). Bản `3749ec8` chỉ có `FOR SHARE`, nên không chờ `LockOwnership` (advisory lock exclusive) của move như một cluster thật. Probe của `owner_cas` và `row_rewrite` giữ nguyên như 2026-11-22 để so được.
3. Ghi `target` (cluster đích) cho mỗi lần đo `registry`, vì thời gian `MoveOwner` phụ thuộc cluster đích (dưới).

### Workload

9 tenant khác × 5.000 + tenant được chuyển 1.000 / 5.000 / 10.000 = 61.000 node, 20 user/tenant, seed qua handle có scope (26,6 s). Ba thiết kế mỗi lần đo:
- `owner_cas`: một câu CAS `tailnets` (mô phỏng như 2026-11-22);
- `row_rewrite`: ghi lại `cluster_id` trên mọi dòng của tenant (schema `blogsim`, mô phỏng thiết kế bài blog);
- `registry`: **đường thật** `placement.Registry.MoveOwner` (transaction 1: `LockOwnership`, `FOR UPDATE` cluster đích, đếm thiết bị tenant, kiểm sức chứa + lease đích, `UPDATE moving_to`, audit) rồi `hsdb.Platform.HandOver` của cluster nguồn (transaction 2: kiểm lease nguồn, owner = đích, epoch + 1). Hai cluster bench `src`, `dst`, mỗi cái giữ lease 1 ngày.

Mọi tenant bắt đầu ở `src`. `owner_cas` và `registry` đều đổi owner, thứ tự thiết kế đảo mỗi lần, nên ở pha cô lập `registry` đi `src → dst` ở lần lẻ (đích giữ 0 thiết bị) và `dst → src` ở lần chẵn (đích giữ 61.000 − kích thước tenant). Pha đồng thời luôn `dst → src`.

### Pha cô lập (đo; median của 7 lần [min–max]; `CHECKPOINT` trước mỗi lần, WAL gồm full-page image)

| Tenant | Thiết kế | Tổng ms | Bước 1 ms | Bước 2 ms | Dòng đổi | WAL |
|---|---|---|---|---|---:|---|
| 1.000 | CAS (mô phỏng) | 1,89 [1,71–4,87] | 0,73 | 1,17 (COMMIT) | 1 | 6,2 KB |
| 1.000 | **registry (thật)** | **111,3 [10,4–123,5]** | 108,3 (`MoveOwner`) | 5,0 (`HandOver`) | 2 | 9,3 KB [6,4–13,4] |
| 1.000 | ghi lại mọi dòng | 90,4 [82,6–106,1] | 80,3 | 11,1 (COMMIT) | 1.021 | 8,9 MB |
| 5.000 | CAS (mô phỏng) | 1,99 [1,77–5,12] | 0,64 | 1,21 | 1 | 9,7 KB |
| 5.000 | **registry (thật)** | **110,5 [15,8–114,7]** | 107,4 | 6,1 | 2 | 13,7 KB [11,8–16,1] |
| 5.000 | ghi lại mọi dòng | 426,8 [382,9–450,5] | 410,7 | 16,5 | 5.021 | 30,7 MB |
| 10.000 | CAS (mô phỏng) | 1,83 [1,77–2,23] | 0,61 | 1,21 | 1 | 9,8 KB |
| 10.000 | **registry (thật)** | **110,2 [68,2–154,2]** | 107,2 | 3,1 | 2 | 15,0 KB [12,6–17,9] |
| 10.000 | ghi lại mọi dòng | 845,3 [736,2–950,8] | 841,9 | 4,7 | 10.021 | 51,3 MB |

`registry` tách theo chiều (median; số lần trong ngoặc):

| Tenant | → `dst` (đích giữ 0 thiết bị) (3): tổng = `MoveOwner` + `HandOver` | → `src` (đích giữ 60.000 / 56.000 / 51.000 thiết bị) (4) |
|---|---|---|
| 1.000 | 11,5 = 5,9 + 5,6 ms | 113,3 = 110,1 + 3,2 ms |
| 5.000 | 16,8 = 10,6 + 6,2 ms | 112,0 = 107,6 + 3,0 ms |
| 10.000 | 71,1 = 67,1 + 3,1 ms | 152,4 = 149,6 + 2,9 ms |

Thời gian đi vào đâu (`EXPLAIN ANALYZE` trên DB sau khi đo, `b3-p12b-explain.txt`):
- Kiểm sức chứa `admits` (`internal/placement/tailnets.go`): `count(n.id)` trên mọi node của các tailnet thuộc cluster đích. Đích `src`: **Seq Scan `nodes` 61.000 dòng**, 93–119 ms. Đích `dst`: 0,1–0,2 ms.
- `deviceCount` của tenant được chuyển: 1,8 ms (1.000), 5,4 ms (5.000), 42 ms (10.000; planner chọn Seq Scan).
- `HandOver`: 2,9–6,2 ms, không phụ thuộc kích thước.

Lock: `pg_locks` không được lấy cho `registry` (hai transaction chạy trong thư viện, harness không chen được trước COMMIT). Theo code: `MoveOwner` giữ advisory lock exclusive của tailnet (`LockOwnership`) và `FOR UPDATE` dòng `clusters` của đích suốt transaction 1, kể cả lúc đếm.

Dead tuple sau 21 lần move mỗi thiết kế: `blogsim.nodes` 112.000 (0 HOT); `public.tailnets` 61 update (21 HOT), 48 dead; `public.nodes` 0.

### Pha có writer đồng thời (đo; 5 lần mỗi ô; median [min–max])

Hai writer mỗi 2 ms ghi một thiết bị ngẫu nhiên: một trên tenant đang chuyển, một trên tenant `other-00`. Writer `registry` dùng fence thật (advisory shared + `FOR SHARE`); writer CAS chỉ `FOR SHARE` (như 2026-11-22). Mọi move `registry` ở pha này là `dst → src` (đích giữ 51.000–60.000 thiết bị).

| Tenant | Thiết kế | Move ms | Writer cùng tenant: nền p50 / max trong lúc move | Writer tenant khác: max trong lúc move |
|---|---|---|---|---|
| 1.000 | CAS | 3,79 [3,35–5,29] | 1,89 / 2,86 [2,18–5,96] | 3,99 [2,21–5,51] |
| 1.000 | **registry** | **121,2 [102,9–133,3]** | 1,81 / **118,1 [96,5–124,9]** | 3,08 [2,75–4,23] |
| 1.000 | ghi lại | 100,6 [98,4–114,0] | 1,20 / 81,1 [54,2–96,3] | 2,99 [1,54–5,61] |
| 5.000 | CAS | 3,52 [3,40–4,79] | 1,83 / 4,30 [2,27–6,51] | 3,90 [2,42–4,31] |
| 5.000 | **registry** | **120,9 [102,8–129,5]** | 1,86 / **115,5 [94,2–125,7]** | 2,84 [2,48–9,24] |
| 5.000 | ghi lại | 356,6 [344,0–481,0] | 1,26 / 316,2 [307,9–449,7] | 3,79 [1,79–41,1] |
| 10.000 | CAS | 3,58 [3,42–4,57] | 1,81 / 4,47 [2,01–4,58] | 2,53 [1,94–3,94] |
| 10.000 | **registry** | **122,0 [121,6–159,4]** | 1,76 / **118,6 [115,6–153,3]** | 4,01 [2,69–5,50] |
| 10.000 | ghi lại | 736,6 [699,9–768,4] | 1,21 / 692,1 [637,5–737,4] | 35,2 [28,5–42,2] |

### Kết luận trong phạm vi số đo

- Đường thật đổi **2 dòng**, WAL ≤ 18 KB, không phụ thuộc số dòng của tenant. `HandOver` 3–6 ms.
- Nhưng thời gian `MoveOwner` **không** cố định như CAS mô phỏng: nó tăng theo **số thiết bị cluster đích đang giữ** (kiểm sức chứa quét `nodes`) và theo kích thước tenant (`deviceCount`). Ở đây: 6–67 ms khi đích rỗng, 107–150 ms khi đích giữ 51.000–60.000 thiết bị. Lớn hơn đích này thì chưa đo; kế hoạch hiện tại (Seq Scan `nodes`) cho thấy nó tăng theo tổng số node trong bảng (**ngoại suy**, không đo).
- Vì việc đếm nằm sau `LockOwnership`, writer của **chính tenant đang chuyển** (fence thật) bị chặn gần bằng cả `MoveOwner`: 94–153 ms. Writer của **tenant khác** không bị chặn (≤ 9,2 ms).
- So với ghi lại mọi dòng: nhanh hơn 4–7 lần ở 5.000/10.000; ở 1.000 thì chậm hơn (113 so với 90 ms) khi đích lớn.
- So với 2026-11-22: CAS mô phỏng nhanh hơn (1,8–2,0 so với 2,3–4,8 ms; lần này VM không có tải nền). Ghi lại mọi dòng chậm hơn và WAL gấp ~2 lần (5.000: 427 ms / 30,7 MB so với 292 ms / 15,8 MB): lần này `pg_locks` cho thấy 8 index trên `blogsim.nodes`; số index lúc 2026-11-22 không được ghi, nên nguyên nhân (nhiều khả năng là schema đã thêm index) **chưa xác minh**. Hai lần đo không cùng schema, không so thẳng được.
- **Quyết định của chủ dự án (2026-11-26): chấp nhận, ghi giới hạn.** Giữ nguyên code của P12b. Tối ưu (bộ đếm thiết bị theo cluster, hoặc đếm sức chứa trước `LockOwnership`) để sang P14. Không phải lỗi isolation; thời gian chặn ~0,1–0,15 s mỗi move, xảy ra một lần mỗi move.

## Known limitations / security blockers

- Client không tự đổi ControlURL: mọi client phải re-login (Q1 đã chọn không đổi protocol). iOS/Android cần người dùng thao tác.
- Health của client cũ (capver < 117) bị xóa toàn bộ khi hủy thông báo (ADR note 7).
- Trễ API tenant ≤ 1 chu kỳ reconcile.
- B3: `MoveOwner` đếm thiết bị của cluster đích (Seq Scan `nodes`) trong lúc giữ `LockOwnership`; writer của tenant đang chuyển chờ 94–153 ms khi đích giữ 51.000–60.000 thiết bị, tăng theo kích thước đích (§Benchmark). Chưa sửa; cần quyết định.
- B3 giới hạn đo: một VM cho PG và harness; 7/5 lần mỗi ô, không có khoảng tin cậy; `registry` không lấy `pg_locks`; chỉ hai cluster và đích lớn nhất 60.000 thiết bị; CAS và ghi lại mọi dòng vẫn là mô phỏng.

## Reviewer approval

Chưa.
