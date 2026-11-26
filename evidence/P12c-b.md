# PR P12c nhóm B Evidence (lease, tính sẵn sàng, tài nguyên)

- **Repo commit before / after:** `1ffffc1` (nhánh tích hợp `feat/P12c-review-hardening`) → tip `feat/P12c-b` (xem bảng commit §1; tip ghi trong báo cáo cuối).
- **Trạng thái:** `Verified` (test đã chạy trên mc-ci, có lệnh và exit code bên dưới). Chưa `Reviewed`. Cấu hình lab chỉ sửa file, **chưa áp** lên mc-db (coordinator áp).
- **Verified source paths & symbols:** `evidence/source-inventory-P12c-b.md`.
- **Môi trường test:** mc-ci, Go 1.27.4, `-race` với `CGO_ENABLED=1`; PostgreSQL 16.15 tạm (`initdb`, unix socket, port 55436, chỉ cho nhánh này) qua `MESH_TEST_POSTGRES_DSN`; servertest `-tags p05cluster` trong `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu`.

## 1. Commit RED → GREEN

| ID | RED commit | Lệnh RED (exit) và kết quả | GREEN commit | Lệnh GREEN (exit) |
|---|---|---|---|---|
| B1 | `f0cd3af` | `MESH_TEST_POSTGRES_DSN=<pg tạm> go test -count=1 -run 'TestLeaseRenewNotStarved\|TestLeaseStandbyPoll\|TestLeaseTakeoverWaits' -v ./hscontrol/db/tenantscope/` → **1**. Lần 1: renew 7,64 s / 5,92 s / 0,03 s / **8,00 s (timeout)** / **8,00 s (timeout)**, 2/5 renew thất bại trong hạn self-fence; lần 2: tệ nhất 5,21 s. Poll của standby: `context deadline exceeded` sau 3,0 s thay vì `ErrHeld` | `a1f06ce` (+ `50f06e3` chỉ sửa cách tính tốc độ ghi trong log test) | cùng lệnh, 3 lần → **0**: renew tệ nhất 14,6 / 14,3 / 5,4 ms; poll `ErrHeld` ngay |
| B2 | `7be8aee` | `… -run 'TestLeaseFenceIdleTransaction\|TestFenceTimeoutsAreTransactionLocal' …` → **1**: standby không lấy được lease sau **15,2 s**; trong transaction có fence cả ba timeout = `0` | `2c10bfa` | → **0**: takeover **4,01 s** sau khi transaction bị bỏ dở (TTL 2 s, cận 7 s); transaction đó COMMIT lỗi, ghi của nó không thấy; ba timeout = `4s` trong transaction, `0` sau đó (cùng kết nối) |
| B3 | `db5aa16` | `go test -count=1 -run 'TestManagerRenewsDuringSlowOnAcquire\|TestManagerSelfFencesDuringSlowOnAcquire' -v ./internal/lease/` → **1**: "lease reported lost while onAcquire was still loading", "lease ended after a slow onAcquire: self-fence deadline passed" | `c14b5e8` | `go test -race -count=3 -v ./internal/lease/` → **0** |
| B4 | `8e1a009` | `go test -tags p05cluster -count=1 -run TestP12cAPI -v ./hscontrol/servertest/` → **1**: B nhận 429 sau 40 request của A (A: 3×200, 37×429) và sau 300 key rác (300×429) | `05b8cda` | `go test -tags p05cluster -race -count=1 -run 'TestP12cAPI\|TestP08bTenantAPI' -v ./hscontrol/servertest/` → **0** |
| B5 | `d56f1f6` | `go test -tags p05cluster -count=1 -run TestP12cRuntimeNotRoutable -v ./hscontrol/servertest/` → **1**: map request trong khe load→start → **500**, log `http handler panic: nil pointer dereference` tại `mapper.(*Batcher).AddWork` ← `Headscale.Change` ← `mapSession.serve` | `1183d7e` | `-race -count=3` → **0** |
| B6 | `f5a137f` | `go test -tags p05cluster -count=1 -run TestP12cNoiseSessionsForgotten -v ./hscontrol/servertest/` → **1**: sau 3 vòng mở/đóng 10 phiên, `h.sessions` = 10 → 20 → 30 | `660cb2c` | `-race -count=2` → **0** (về 0 sau mỗi vòng) |
| L2 | `1059eec` | `… -run TestP12cReadyBody …` → **1**: body `{"status":"ready","lease_epoch":1}` | `f87194d` | → **0** |
| L1 | `6794bf7` | `go test -count=1 -run TestP12cHealth -v ./hscontrol/` → **1**: 200 request `/health` đồng thời = **200 ping DB** | `eff3158` | `go test -race -count=3 -run TestP12cHealth -v ./hscontrol/` → **0** (≤ 1 ping; DB lỗi hiện ra trong ~1 s) |
| — | | | `c9c59c4`, `6318f32` | lint (wsl, prealloc, predeclared), không đổi hành vi |

Test an toàn không RED (đúng cả trước và sau, giữ làm hồi quy): `TestLeaseTakeoverWaitsForInFlightFencedWrite` (B1), `TestManagerSelfFencesDuringSlowOnAcquire` (B3).

## 2. Thiết kế (ghi chú ADR)

### B1 — kiểm lease không khóa dòng; takeover dưới advisory lock công bằng theo cluster

Không đổi ngữ nghĩa ADR 0005 (lease CAS trên `cluster_leases`, đồng hồ DB, kiểm lease trong cùng transaction của mọi ghi có fence) và ADR 0006 (chỉ `pg_advisory_xact_lock*`, không lock phiên).

- **Ghi có fence (PostgreSQL), `checkLease`:** câu riêng `SELECT pg_advisory_xact_lock_shared(hashtextextended('lease/cluster/' || $1, 0))` (giữ tới hết transaction), rồi một câu **sau đó** đọc `holder, epoch, expires_at > clock_timestamp()` **không** `FOR SHARE`.
- **`Acquire`:** đọc thường; lease còn sống → `ErrHeld` ngay, không khóa gì (standby poll mỗi giây không còn chặn hay bị chặn bởi ghi). Lease hết hạn/chưa có → `SET LOCAL lock_timeout` (thời gian còn lại của ctx), `pg_advisory_xact_lock` (exclusive) trên cùng khóa, rồi câu CAS cũ `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at <= now`.
- **`Renew`, `Release`:** không đổi (UPDATE một dòng, không advisory). Chúng không đổi holder hay epoch; ghi không còn giữ khóa dòng nên Renew không bao giờ chờ ghi.
- SQLite không đổi (SQLite tuần tự hóa người ghi).

**Vì sao vẫn an toàn.** Chỉ `Acquire` đổi holder/epoch (trigger `trg_cluster_leases_epoch_monotonic`; `Renew`/`Release` không đổi hai cột này), và chỉ khi `expires_at <= clock_timestamp()`. Xét một ghi W của holder cũ A và takeover K của B:
1. W lấy khóa shared trước K: K chờ tới khi transaction của W kết thúc. Kiểm của W chạy khi đã giữ khóa; nếu lease A đã hết hạn theo đồng hồ DB thì W `ErrFenced`, nếu chưa thì W commit **trước** K. ⇒ không takeover nào commit khi một ghi đã qua kiểm của holder cũ còn đang bay (`TestLeaseTakeoverWaitsForInFlightFencedWrite`: K trả về sau thời điểm W commit).
2. K giữ (hoặc đã xếp hàng giữ) khóa exclusive trước: W chờ sau K (hàng đợi lock manager công bằng: shared tới sau không vượt exclusive đang chờ). Câu đọc lease của W bắt đầu **sau** khi W có khóa, nên snapshot (READ COMMITTED) và `clock_timestamp()` đều sau khi K commit/rollback: W thấy holder B (`ErrFenced`), hoặc nếu K không lấy được (lease còn sống) thì thấy A như cũ. Kể cả nếu snapshot cũ hơn, dòng của A mà K đã thay có `expires_at <= thời điểm K` < bây giờ, nên không bao giờ "live" ⇒ `ErrFenced` (trong test: ghi T2 xếp hàng sau K → `ErrFenced`, mọi kiểu ghi của A sau takeover → `ErrFenced`).
3. Renew đồng thời với W: W có thể đọc phiên bản dòng trước Renew (expiry sớm hơn) ⇒ chỉ có thể từ chối thừa, không bao giờ nhận thừa.
- Thứ tự khóa: advisory tailnet (shared) → dòng `tailnets` (FOR SHARE) → advisory cluster (shared). `Acquire` chỉ giữ advisory cluster (exclusive); `LockOwnership`/`HandOver` không lấy khóa cluster. Một transaction có fence chỉ thuộc một tailnet nên luôn lấy T trước C; câu sau lấy lại khóa đã giữ (không chờ). Không có chu trình. Mọi chờ bị chặn bởi `lock_timeout` (B2, Acquire).
- **Đánh đổi:** một takeover làm ghi mới của cluster đó xếp hàng sau nó trong lúc chờ các ghi đang bay (chỉ xảy ra khi lease đã hết hạn, tức active cũ đã ngừng). Kiểm lease thêm một round trip (khóa + timeout trong cùng câu, xem B2), xem §4.

### B2 — giới hạn transaction có fence

- Mỗi transaction có fence trên PostgreSQL (lease hay operator), từ lần kiểm lease đầu: `set_config('idle_in_transaction_session_timeout' | 'statement_timeout' | 'lock_timeout', d/2, true)` (= `SET LOCAL`, hết khi transaction kết thúc; an toàn với PgBouncer transaction mode, ADR 0006 §3.2), chung một câu với khóa shared của B1. `d` = `ttl − safety_margin` của `tuning.cluster_lease` (`tenantscope.SetTransactionBound`, đặt trong `NewCluster`); mặc định 8 s → 4 s.
- 4 s ≤ `ttl − renew_interval` = 7 s: transaction bị bỏ dở (SIGSTOP, mất mạng) bị PostgreSQL kết thúc trước khi lease có thể hết hạn, nên takeover không bị trễ thêm ở cấu hình mặc định (đo: TTL 2 s → takeover 4,01 s, đúng bằng cận idle).
- `Acquire` đặt `lock_timeout` theo ctx; `LockOwnership` giữ 5 s của nó (nếu một transaction gọi cả hai, giá trị đặt sau thắng: ≤ 5 s).

### B3 — gia hạn ngay khi lấy lease

`Manager.Run`: `hold` (renew) chạy trong goroutine ngay sau `acquire`; `onAcquire` chạy song song với context kết thúc cùng lease; `Current()` thành false ngay khi `hold` kết thúc; `onLose` chỉ gọi sau khi `onAcquire` trả về. `onLeaseAcquired` dừng nạp khi context kết thúc. `waitStopping` giữ chỗ cũ: `onLeaseLost` đã chờ (có giới hạn) trước khi `Run` acquire lại, và nay lease được gia hạn trong lúc chờ lần hai.

### B4 — thứ tự rate limit `/api/v2`

standby → có token → key đã xác thực gần đây (SHA-256 của token, ≤ 1 phút, tối đa 10 000, chỉ key đã qua xác thực mới vào) **hoặc** bucket tiền xác thực mới (`tuning.tenant_api.preauth_rate/preauth_burst`, mặc định 50/s, burst 100) trước khi tra DB → bucket key → bucket tailnet → bucket toàn cục. Tập key đã biết chỉ dùng để định tuyến và chọn bucket: runtime vẫn xác thực lại key ở mọi request qua handle có scope (thu hồi, hết hạn), và `tailnet_id` của key không đổi được. Tài liệu cấu hình sinh lại bằng `tools/configgen`.

Giới hạn còn lại: trong lúc bị flood key rác, một key **chưa** được process này xác thực trong 1 phút gần nhất vẫn tranh bucket tiền xác thực (429 + `Retry-After: 1`). Không có giới hạn theo IP (forwarder TCP che IP client; cùng gốc với B7, chuyển P13/P14).

### B5, B6, L1, L2

- **B5:** `serveTailnet` = `load` (không công bố) → `start` → công bố vào `c.runtimes` dưới `c.mu`. Lỗi start → runtime bị đánh dấu retired và dừng nền, không bao giờ định tuyến. Hook test `loadHook` chỉ có setter khi build tag `p05cluster`.
- **B6:** kết nối giao cho HTTP/2 server được bọc; `Close` đầu tiên (client đi, idle timeout, hoặc `closeSessions` đóng kết nối Noise bên dưới) gọi `unbind`; `unbind` đánh dấu phiên `closed` và `bind` từ chối phiên đã đóng (request đang bay lúc kết nối đóng không gắn lại được).
- **L1:** kết quả ping DB của `/health` dùng lại 1 s; request thấy cũ thì ping (giới hạn 3 s, không bị hủy khi client bỏ đi), request đồng thời chờ kết quả đó.
- **L2:** `/ready` 200 `{"status":"ready"}`, 503 `{"status":"standby"}`; mã trạng thái giữ nguyên (HAProxy và harness E2E chỉ đọc mã; `lease_epoch` đọc từ `cluster_leases` hoặc log).

## 3. Cấu hình lab (chỉ sửa file `infra/lab/db/setup-db.sh`, chưa áp)

| Dịch vụ | Thiết lập | Giá trị | Áp dụng |
|---|---|---|---|
| PgBouncer `mc-pgb` (`/opt/mc-db/pgbouncer.ini`) | `query_wait_timeout` | `5` (s) | chạy lại `setup-db.sh` trên mc-db (ghi lại ini, tạo lại container `mc-pgb`) |
| PgBouncer | `idle_transaction_timeout` | `5` (s) | như trên |
| PostgreSQL `mc-pg` | `ALTER ROLE headscale SET idle_in_transaction_session_timeout = '5s'` | 5 s | cùng lần chạy `setup-db.sh` (khối SQL idempotent); có hiệu lực với kết nối server mới, PgBouncer được tạo lại nên kết nối cũ đã bị thay |

Mọi giá trị < hạn self-fence 8 s (ADR 0006 §3.4). Transaction có fence tự đặt chặt hơn (4 s, `SET LOCAL`).

## 4. Benchmark hot path (kiểm lease trong mọi ghi có fence)

`BenchmarkFenceWrite` (`hscontrol/db/tenantscope/fence_bench_test.go`), PostgreSQL 16 tạm nối thẳng qua unix socket, **`synchronous_commit = off`** (để bỏ nhiễu fsync: lần chạy đầu với fsync bật bị IO của hai agent khác chi phối, ngay cả psql `BEGIN; UPDATE; COMMIT` trống cũng 9,3 ms/tx, số đó bị loại). Binary test của base `1ffffc1` và tip `6318f32` chạy **xen kẽ** 3 vòng, `-test.benchtime 2s`. Trung bình 3 vòng (ms/op):

| Benchmark | base `1ffffc1` | tip | Chênh |
|---|---|---|---|
| `nofence/update/serial` | 0,316 | 0,300 | nhiễu |
| `fence/update/serial` (operator, không lease) | 0,452 | 0,550 | +0,10 |
| `fence+lease/update/serial` | 0,557 | 0,679 | +0,12 (+22 %) |
| `nofence/tx10/serial` | 1,603 | 1,543 | nhiễu |
| `fence/tx10/serial` | 2,887 | 3,861 | +0,97 (≈ +0,10 mỗi câu) |
| `fence+lease/tx10/serial` | 3,942 | 4,906 | +0,96 (≈ +0,10 mỗi câu) |

Đúng dự kiến: **một round trip thêm cho mỗi câu ghi có fence** (câu khóa shared + `set_config`, phải tách khỏi câu đọc lease để snapshot/đồng hồ của câu đọc đi sau khóa). Qua PgBouncer, round trip lớn hơn (ADR 0006 §4: +0,3–0,6 ms p50). Đổi lại: renew không còn bị bỏ đói (từ tới 8 s xuống ≤ 15 ms) và poll của standby không còn khóa dòng lease. Hướng tối ưu (cần sửa `checkFence`, ngoài phạm vi nhóm B trong `tenantscope.go`): lấy khóa shared của cluster và `set_config` ngay trong câu fence tailnet (câu 1), khi đó câu đọc lease (câu 2) vẫn đi sau khóa và không còn round trip thêm.

Số đo renew dưới tải (B1, §1) là số đo hot path quan trọng hơn: 16 writer, mỗi transaction 3 câu ghi có fence: renew tệ nhất 5,2–8,0 s (timeout) → 5–15 ms.

## 5. Lint, vet, race, servertest

| Lệnh (mc-ci) | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go vet ./hscontrol/ ./hscontrol/db/... ./internal/lease/... ./hscontrol/config/ ./hscontrol/types/` và `go vet -tags p05cluster ./hscontrol/ ./hscontrol/servertest/` | `eff3158` | 0 | |
| `nix shell --inputs-from . nixpkgs#golangci-lint -c golangci-lint run ./hscontrol/ ./hscontrol/db/... ./internal/lease/ ./internal/ratelimit/ ./hscontrol/config/ ./hscontrol/types/` | `6318f32` | 0 | **0 issues** |
| như trên với `--build-tags p05cluster ./hscontrol/ ./hscontrol/servertest/` | `6318f32` | — | 0 issue trong file nhóm B sửa/thêm; các issue còn lại của `servertest` có sẵn từ trước (không chạy lint với tag này trong CI) |
| `MESH_TEST_POSTGRES_DSN=<pg tạm> go test -race -p 4 -count=1 ./internal/lease/... ./hscontrol/db/... ./internal/placement/... ./internal/bootstrap/... ./internal/ratelimit/...` | `eff3158` | 0 | `hscontrol/db` 325 s, `tenantscope`, `placement`, `placementapi`, `bootstrap`, `ratelimit` ok (lần trước ở `2c10bfa` cũng 0) |
| `go test -race -p 4 -count=1 ./hscontrol/ ./hscontrol/config/ ./hscontrol/types/ ./internal/ratelimit/ ./internal/lease/... ./hscontrol/db/tenantscope/` (SQLite) | `6318f32` | 0 | `hscontrol` 460 s |
| `MESH_TEST_POSTGRES_DSN=<pg tạm> go test -race -p 4 -count=1 ./internal/lease/... ./hscontrol/db/tenantscope/` | `6318f32` | 0 | |
| `go test -count=1 ./hscontrol/types/ ./hscontrol/config/ ./tools/configgen/...` | `05b8cda` | 0 | trước đó exit 1 ở bản đầu của B4: `TestExamples` đòi example khác default → example 100/200 |
| `nix shell … tscli opentofu -c go test -race -tags p05cluster -count=1 -run 'TestP11b\|TestP12c\|TestP08bTenantAPI\|TestP10\|TestP12\|TestTwoClusters' -v ./hscontrol/servertest/` | `6318f32` | 0 | 17 test PASS (gồm 6 `TestP11b*`, 4 `TestP08bTenantAPI*`, 3 `TestTwoClusters*`, 4 `TestP12c*`), 214 s |
| như trên, `-run 'TestCluster\|TestFailoverInOneClusterLeavesTheOtherAlone\|TestScheduledMoveNotifiesOnlyItsClients\|TestN1Probe'` | `6318f32` | 0 | 39 test PASS (P05, P06, P07, P09 verify/probe, P10, P12, P12b notice), 532 s |

Không chạy toàn bộ suite `p05cluster` (coordinator chạy khi tích hợp).

## 6. Known limitations / security blockers

- Không có blocker isolation. Không đổi schema, không migration, không đổi mô hình Noise, không đổi ngữ nghĩa ADR 0005/0006/0010.
- B4: xem giới hạn còn lại ở §2 (key chưa biết trong lúc flood; không có giới hạn theo IP — B7, P13/P14).
- B2: transaction có fence dài nhiều câu vẫn có thể giữ khóa shared lâu (mỗi câu ≤ 4 s, mỗi khoảng nghỉ ≤ 4 s, tổng không giới hạn); `transaction_timeout` cần PostgreSQL 17 (lab là 16). Code hiện tại không có transaction như vậy.
- Chưa chạy E2E thật (N1, P12b) trên nhánh này: coordinator chạy ở nhánh tích hợp. Go race/servertest không chứng minh hành vi qua PgBouncer thật; test PostgreSQL ở đây nối thẳng PostgreSQL 16.
- `nixos-options` (flake check) chưa chạy cho hai option mới `preauth_rate/preauth_burst`; file sinh `nix/options.generated.nix` được tạo bằng `tools/configgen/files`.

## 7. Reviewer approval

Chưa có.
