# PR P11b Evidence — lease cluster và failover active/standby

- **Nhánh:** `feat/P11b-cluster-failover` (từ `master` `20d178e`). Trạng thái: `Verified`, chưa `Reviewed`.
- **Inventory:** `evidence/source-inventory-P11b.md`. **Ticket:** `tickets/P11b.md`. **ADR:** 0005 (Bổ sung 2 §2; một Noise key cho mỗi cluster, không đổi).
- **Lab:** VM mc-c1b 7706 (192.168.1.245) tạo ở nhánh `feat/LAB-c1b` (`fcbc4f4`, chưa merge), HAProxy trên mc-cp; chủ dự án duyệt ngày 2026-11-25.

## Repo commit before / after

| | Commit |
|---|---|
| Trước | `master` `20d178e` |
| Code + test | `68d8bfa` inventory, `110ecb5` lease + migration + fence, `1909795` cluster active/standby, `0696d15` allowlist/docs, `41e556d` metric, test xóa node/ephemeral, benchmark, harness |
| Evidence | commit chứa file này |

## Verified source paths & symbols

Xem `evidence/source-inventory-P11b.md`. Mã mới hoặc đổi chính:

| Path | Symbol |
|---|---|
| `internal/lease/lease.go` | `Store.Acquire/Renew/Release/Get`, `ErrHeld`, `ErrLost`, DDL SQLite/PostgreSQL, `PostgresNow = clock_timestamp()` |
| `internal/lease/manager.go` | `Manager.Run(ctx, onAcquire, onLose)`, `Config{TTL, RenewInterval, AcquireInterval, SafetyMargin}.Validate` |
| `hscontrol/db/cluster_leases.go`, `platform.go` | migration `202611251000-cluster-leases` (cuối chuỗi v0.1), `RequireLease/SetLease/ClearLease`, `ErrNoClusterLease` |
| `hscontrol/db/tenantscope/tenantscope.go` | `checkLease`: holder + epoch + `expires_at > now()` của DB, `FOR SHARE` trên PostgreSQL; thứ tự khóa advisory → `tailnets` → `cluster_leases` |
| `internal/tailnet/fence.go` | `Fence.WithLease`, `Fence.Lease` |
| `hscontrol/cluster.go` | `Serve` chạy lease manager; `onLeaseAcquired`/`onLeaseLost`; `/ready`; `Ready()`; standby không nạp runtime |
| `hscontrol/app.go`, `session_route.go`, `poll.go` | `servable()` = chưa retire và còn lease; kiểm ở bind, enter, requireBound và trong `writeMap`; `mapresponse_sent_total` đếm mọi frame đã ghi (update, keepalive) sau cổng này |
| `hscontrol/config/*`, `types/config.go` | `tuning.cluster_lease.{ttl, renew_interval, acquire_interval, safety_margin}` |

## Design choice / ADR

- Lease theo đồng hồ DB; tự fence theo đồng hồ monotonic tại `bắt đầu lần gia hạn thành công cuối + TTL − margin`.
- Mặc định TTL 10 s, gia hạn 3 s, standby thử 1 s, biên 2 s. Lý do: failover ≤ ~11 s; mất một lần gia hạn chưa fence; active tự fence (≤ 8 s) trước khi lease hết hạn ở DB (10 s), nên standby không thể lên khi active cũ còn phục vụ.
- Endpoint: HAProxy TCP, TLS passthrough, health check HTTPS `/ready` với SNI và Host của cluster, `on-marked-down shutdown-sessions`. Không dùng VIP/keepalived (không đụng mạng host Proxmox). Định tuyến chỉ để sẵn sàng; standby tự từ chối (fail closed).
- `cluster_leases.cluster_id` **không** có FK tới `clusters`, giống `tailnets.owner_cluster_id`: cluster chưa đăng ký vẫn bầu được một process. Inventory ban đầu ghi FK; đây là thay đổi có chủ ý, ghi trong comment `internal/lease/lease.go`.

## Tests + exact commands + exit codes

Máy: mc-ci, go 1.27.4, `GOFLAGS=-buildvcs=false`. PostgreSQL 16 tạm (`~/pgtmp.sh`, unix socket, `fsync=off`).

### RED → GREEN (fence lease ở DB)

RED = cùng tree, `checkLease` trả về ngay (diff 3 dòng). Lệnh: `go test -race -count=1 -run 'TestLeaseFence|TestFenceWithLease' -v ./hscontrol/db/tenantscope/`, SQLite và `MESH_TEST_POSTGRES_DSN=<pg tạm>`.

| Lượt | SQLite | PostgreSQL 16 |
|---|---|---|
| RED (`checkLease` tắt) | exit 1: **FAIL** `TestLeaseFenceTakenOverRollsBack`, `TestLeaseFenceExpiredByDatabaseClockRollsBack`, `TestLeaseFenceWrongEpochOrNoRowRollsBack`; 3 PASS (`LiveLeaseWrites`, `OperatorWriteNeedsNoLease`, `FenceWithLeaseValidation`) | exit 1: cùng 3 FAIL, cùng 3 PASS |
| GREEN (`41e556d`) | exit 0, 6/6 PASS | exit 0, 6/6 PASS |

Log trên mc-ci: `~/p11b-full/redbench/{red,green}-{sqlite,postgres}.log`, `red.diff`.

### Unit + race + servertest

Tree `41e556d` (đồng bộ bằng tar lên `~/src/p11b`), log `~/p11b-full/final/`:

| Lệnh | Exit | Thời gian |
|---|---|---|
| `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu -c go test -race -count=1 -timeout 45m ./hscontrol/... ./internal/... ./cmd/...` (SQLite, gồm toàn bộ servertest) | **0** | 2072 s |
| `MESH_TEST_POSTGRES_DSN=<pg tạm> go test -race -count=1 -timeout 30m ./hscontrol/db/... ./internal/...` | **0** | 163 s |
| `go test -race -count=1 -timeout 30m -tags p05cluster -run 'Cluster\|P11b' ./hscontrol/servertest/` | **0** | 559 s |

Ghi chú: package servertest cần > 10 phút dưới race (cả `master`: 1962 s so với P11b 1995 s, cùng máy). Với timeout mặc định 10 phút nó panic ở cả hai nhánh, nên phải chạy với `-timeout 45m`. Ngoài nix shell, `TestAPIv2*` lỗi vì thiếu `tscli`/`tofu` (cả `master`); trong nix shell thì PASS.

Servertest P11b (`-tags p05cluster -run TestP11b`, race): 6/6 PASS.

| Test | Kiểm | Kết quả |
|---|---|---|
| `TestP11bStandbyServesNothing` | standby `/health` 200, `/ready` 503, `/ts2021` 503, không runtime | PASS |
| `TestP11bExactlyOneActive` (F04) | 3 process, 20 mẫu, đúng 1 ready | PASS |
| `TestP11bFailoverKeepsNodesWithoutRelogin` (F01) | epoch +1, `owner_epoch` không đổi, cùng node ID/key/IP, node mới thấy được, B vẫn cô lập | PASS (standby ready 104 ms sau khi active dừng có release) |
| `TestP11bLostLeaseStopsOldActive` (F02/F03) | lease hết hạn ở DB → active cũ 503 `/ready` và `/ts2021`, standby lên | PASS |
| `TestP11bDeletedNodeStaysDeletedAfterFailover` (I02/I13) | xóa node ngay trước khi active dừng → không hồi sinh, peer không thấy | PASS |
| `TestP11bEphemeralGCAfterFailover` | GC ephemeral dựng lại ở active mới: node không quay lại bị xóa sau đúng 1 timeout (66 s; đo 65,99 s tính từ lúc thấy ready), node reconnect được giữ | PASS |

`internal/lease` (race, SQLite + PostgreSQL): store (acquire/renew/release, trigger epoch), manager F03 tự fence ~900 ms khi DB treo, F04 một bên thắng, crash, release, start failure.

## E2E artifacts / protocol map redacted

`IMAGE=<client> BIN=<headscale-cluster> infra/lab/e2e/p11b/run.sh`

```
client (docker trên mc-ci) → VPS :8443 → wg-mesh → mc-cp 10.99.77.2:8443 (HAProxy, TCP, /ready)
   → mc-cp 192.168.1.241:8443  hoặc  mc-c1b 192.168.1.245:8443   (cùng cluster mc-cp-c1, cùng Noise key)
   → PgBouncer mc-db 192.168.1.243:6432, database e2e_p11b<run> (xóa khi cleanup)
```

Binary `79d48f130a93083e…` build từ `41e556d`. TTL mặc định (10 s / 3 s / 1 s / 2 s).

| Lần | Client | Kết quả | Thời gian | Artifact (không commit) |
|---|---|---|---|---|
| 1 | `tailscale/tailscale:v1.104.7` | **9/9 PASS**, cleanup_ok=1 | 411 s | `artifacts/41e556d/P11b/e2e-20261124T202254Z` |
| 2 | `tailscale/tailscale:v1.84.3` | **9/9 PASS**, cleanup_ok=1 | 413 s | `artifacts/41e556d/P11b/e2e-20261124T202954Z` |

| Ca | Kiểm | Lần 1 | Lần 2 |
|---|---|---|---|
| H01 | standby gọi thẳng (bỏ qua HAProxy): `/health` 200, `/ready` 503, `/ts2021` 503, không `/metrics` | PASS | PASS |
| C00 | a1↔a2 qua HAProxy, ping; b1 một mình | PASS | PASS |
| F01 | `kill -9` active → standby ready; a1 Running + ping; node mới a3 được thấy; cùng node ID/IP/key; số `NeedsLogin` không tăng; số node a +1 | ready 11,4 s, ping 13,1 s, thấy a3 19,9 s | ready 11,9 s, ping 13,6 s, thấy a3 22,8 s |
| F02 | `SIGSTOP` active quá TTL → process kia ready; `SIGCONT` → process cũ không ghi frame nào (`mapresponse_sent_total`), `/ready` 503, `/ts2021` 503, log kết thúc lease | ready 12,5 s; frame 15 → 15 | ready 10,4 s; frame 9 → 9 |
| F03 | cắt active ↔ PostgreSQL (nft output trên host active), client vẫn tới active → active ngừng trước khi standby ready, không ghi frame sau đó | ngừng 6,1 s, standby 8,9 s; frame 23 → 23 | ngừng 7,4 s, standby 10,1 s; frame 22 → 22 |
| F04 | lease hết hạn, khởi động đồng thời hai process → đúng một ready trong 10 s, epoch +1 | PASS | PASS |
| I01 | tailnet b không thấy node nào của a qua F01–F04 | PASS | PASS |
| L01 | epoch theo thời gian tăng nghiêm ngặt, không dùng lại | [1, 2, 3, 4, 5] | [1, 2, 3, 4, 5] |
| S01 | artifact không có auth key, private key, machine/node key đầy đủ | PASS | PASS |

Tỉ lệ client phải đăng nhập lại sau failover: **0** (F01–F04, 2 phiên bản client).

F03 đo trên đồng hồ của mc-cp, poll ~50–100 ms; "ngừng" = `/ready` của active cũ khác 200 lần đầu.

Audit lease (lần 1, `logs/lease-events.log`): epoch 1 mc-cp → 2 mc-c1b (F01) → 3 mc-cp (F02) → 4 mc-c1b (F03) → 5 mc-c1b (F04). Mỗi lần kết thúc ghi `reason="self-fence deadline passed"`.

## Negative tenant isolation tests

- E2E I01, và `check_b_isolated` sau F01–F03.
- Servertest F01: B không thấy node A sau failover.
- RED lease fence: ghi của holder cũ hoặc epoch cũ, ghi khi lease đã hết hạn theo `now()` của DB (dù chưa tới deadline monotonic), và ghi khi không có hàng lease đều rollback (SQLite và PostgreSQL).
- `TestP11bDeletedNodeStaysDeletedAfterFailover`.
- Không có lỗi isolation.

## DB migrations and downgrade constraints

- Thêm `202611251000-cluster-leases` vào **cuối** chuỗi v0.1. DDL giống hệt đuôi `schema.sql`/`schema_postgres.sql` (`TestClusterLeasesDDLInSchemaFiles`). Chạy trong transaction, không `AutoMigrate`, không disable FK.
- Trigger: `epoch` không giảm; đổi `holder` phải tăng `epoch`.
- Downgrade: binary cũ không biết `cluster_leases` và không bao giờ kiểm lease, nên **không được** chạy binary cũ song song với binary P11b trên cùng DB. Bootstrap/CLI đòi `LatestMigrationID`: nâng mọi process của cluster trước, rồi mới tới bootstrap.

## Benchmark before/after (hot path ghi)

`go test -count=6 -run '^$' -bench 'BenchmarkFenceWrite$' -benchtime 1s -benchmem ./hscontrol/db/tenantscope/`, mc-ci 16 vCPU (load 1–3.4), PostgreSQL 16 qua unix socket, `fsync=off`. Trung bình 6 lần (ns/op):

| PostgreSQL | nofence | fence (P10) | fence+lease (P11b) | lease thêm |
|---|---|---|---|---|
| update/serial | 309 061 | 418 536 | 536 511 | +118 µs (+28 %) |
| create/serial | 319 283 | 443 166 | 553 223 | +110 µs (+25 %) |
| tx10/serial (10 update / tx) | 1 538 940 | 2 679 209 | 3 768 634 | +1,09 ms (+41 %), 10 lần đọc lease |
| update/parallel-one-tailnet | 41 768 | 62 048 | 77 053 | +15 µs (+24 %) |

SQLite: ba chế độ nằm trong nhiễu (~9,2–10,4 ms/op, bị chi phối bởi fsync của file).

Đánh giá: mỗi câu ghi thêm một round-trip `SELECT … FOR SHARE` trên `cluster_leases` (~110 µs trên socket cục bộ; qua PgBouncer/mạng sẽ hơn). Lần đọc này có trên mọi đường ghi (register, MapRequest có thay đổi được lưu như endpoint/hostinfo, key, admin), nhưng không có trên đường dựng và gửi MapResponse (chỉ đọc). Tối ưu được nếu cần: gộp kiểm lease vào cùng câu đọc `tailnets` của fence, hoặc kiểm một lần mỗi transaction. Chưa làm, để reviewer quyết định (benchmark B2/B3 của P14 làm chuẩn).

## Runbook

- **Cấu hình:** `tuning.cluster_lease.ttl` (10 s), `renew_interval` (3 s), `acquire_interval` (1 s), `safety_margin` (2 s); validate: `ttl ≥ 1s`, `0 < safety_margin ≤ ttl/2`, `0 < renew_interval < ttl − safety_margin`, `acquire_interval > 0`.
- **Tự động:** HAProxy kiểm `/ready` mỗi 500 ms, `fall 2 rise 2`, `on-marked-down shutdown-sessions`. Active chết → ≤ TTL + 1 s standby lên → HAProxy chuyển sau ≤ ~1 s → tailscaled reconnect cùng URL, không đăng nhập lại.
- **Thủ công (bảo trì):** `systemctl stop` active → nó release lease, standby lên sau ≤ `acquire_interval` (servertest: 104 ms). Không cần sửa DB.
- **Noise key:** cùng file trên cả hai host (0600, owner headscale), chép qua pipe, so sha256; không ghi DB, không log. Mất key ở một host thì host đó không được làm active.
- **State mất khi failover:** `authCache`, `sshCheckAuth`, ping, `registerLocks`, phiên web auth đang chờ: làm lại. Ephemeral GC dựng lại từ DB với timer mới (xóa sau 1 inactivity timeout kể từ lúc lên active).
- **PostgreSQL mất hẳn:** không ai giữ được lease; cả cluster dừng, fail closed (P13 xử lý HA DB).

## Known limitations / security blockers

- **Không có security blocker.** Không khoảng thời gian nào hai process cùng phục vụ trong F01–F04 (×2), servertest và unit.
- F03: sau khi tự fence (`/ready` 503, không ghi frame), runtime/stream của active cũ đóng chậm ~4 s khi DB không tới được (log `retiring tailnet runtime` 4 s sau `lease ended`). Cổng `servable()` trong `writeMap` chặn mọi frame trong khoảng đó (frame 23 → 23).
- `mapresponse_sent_total` trước đây chỉ đếm keepalive; nay đếm mọi frame (`type="update"|"keepalive"`). Dashboard dựa trên số cũ sẽ thấy tăng.
- Một process duy nhất (lab, harness P05/hai cluster) giờ mở listener ở trạng thái standby rồi lấy lease sau ≤ 1 s; trong khoảng đó `/ts2021` trả 503 (client tự thử lại). Harness cũ chờ `/health`: nên đổi sang `/ready` khi sửa lại.
- Chỉ một standby; không active-active (ngoài phạm vi).
- PostgreSQL trong test unit dùng `fsync=off`: số benchmark chỉ để so tương đối.
- Lab: HAProxy và hai process chạy như unit tạm trong E2E; chưa có cấu hình lâu dài cho c1 HA (cần merge `feat/LAB-c1b` và quyết định của chủ dự án).

## Reviewer approval

Chưa có. Cần reviewer bảo mật duyệt fencing (PLAN §10).
