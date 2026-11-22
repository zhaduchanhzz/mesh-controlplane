# Source Inventory — P10 (fencing owner/epoch, cluster theo ownership, quota)

- **Trạng thái:** `Implemented`, `Verified` (unit/race trên SQLite và PostgreSQL 16 ở mc-ci; servertest; E2E client thật). Chưa `Reviewed` (PLAN §10 yêu cầu reviewer bảo mật duyệt fencing).
- **Nhánh:** `feat/P10-placement-fencing`, từ `master` `a316f78`. Đã merge phần placement từ `agent/p10-placement` (`evidence/source-inventory-P10-placement.md`).
- **Lệch so với PLAN:** PLAN nói không làm P10 trước khi N1 (P09) qua gate và P00c có số đo. Chủ dự án yêu cầu làm P10 ngay (2026-11-23), nên đã làm trước. Giới hạn quota 5000 lấy theo ticket, chưa dựa trên số đo P00c.

## 1. Path/symbol đã đọc (phần kỹ sư chính)

| Path | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/db/tenantscope/tenantscope.go` | `Register`, `scopeMutation`, `stampCreate` | Mọi Create/Update/Delete trên bảng tenant đi qua đây. Đây là điểm đặt fence, không phụ thuộc người gọi nhớ dùng helper |
| `hscontrol/db/db.go` | `Write`, `Read`, `openDB` | GORM transaction mặc định bật (không `SkipDefaultTransaction`), nên fence nằm cùng transaction với mutation |
| `hscontrol/db/tenant.go` | `scopeTo`, `ScopedContext` | Nơi duy nhất tạo handle tenant; gắn fence ở đây |
| `internal/tailnet/tailnet.go` | `Scope`, `Epoch`, `WithScope` | Thêm `Fence` riêng (cluster + epoch) |
| `hscontrol/cluster.go`, `session_route.go`, `noise.go`, `app.go` | `Cluster`, `bind`, `refuseRoute`, `serveNoiseSession`, `Headscale` | Vòng đời runtime, retire, đóng phiên |
| `hscontrol/state/state.go` | `ReloadPolicy`, `reloadPolicyLocked` | Đọc policy từ DB khi `policy.mode=database` |
| `internal/placement/quota.go` (agent) | `EnrollmentAllowed` | Fail closed |

## 2. Thiết kế

### 2.1 Fencing (I12, S17)
- `tailnet.Fence{cluster, epoch}` nằm trong context cạnh `Scope`. `scopeTo` (dùng cho runtime và `Platform.TenantDB`) gắn fence theo `owner_epoch` đọc được lúc nạp; `ScopedContext` giữ fence.
- Callback `tenantscope:fence_{create,update,delete}` chạy sau `gorm:begin_transaction`, trước câu lệnh. Trong chính transaction đó nó đọc `tailnets(owner_cluster_id, owner_epoch)`; trên PostgreSQL đọc bằng `FOR SHARE`.
  - Lệch owner hoặc epoch → `ErrFenced`, rollback.
  - Không có fence → `ErrNoFence`.
  - Đọc dữ liệu không bị fence: runtime bị retire sẽ thôi phục vụ ở tầng cluster (§2.2).
- `FOR SHARE` khiến lệnh đổi owner (`UPDATE tailnets`) phải chờ writer đang chạy commit xong, nên một lần ghi không thể commit dưới owner đã đổi sau khi kiểm (`TestFenceBlocksConcurrentMove`, PostgreSQL).
- Kết hợp với trigger của agent (epoch không giảm; đổi owner bắt buộc tăng epoch).
- **Bảng đường ghi tenant** (`TestNoRawTenantWritesOutsideFence` duyệt AST toàn repo):
  - mọi ghi qua GORM đều đi qua callback fence;
  - SQL thô ghi bảng tenant chỉ có 2 chỗ trong allowlist: `hscontrol/db/platform_admin.go` (policy deny của tailnet đang tạo, cùng transaction, chưa có runtime) và `cmd/bench-db/b3.go` (benchmark, build tag `benchdb`, không có trong binary server);
  - DDL và migration không ghi dữ liệu tenant.

### 2.2 Cluster theo ownership
- `reconcile()` chạy mỗi 2 giây:
  - nạp tailnet ACTIVE mà cluster sở hữu nhưng chưa phục vụ (không cần restart);
  - retire runtime có tailnet đã đổi owner, đổi epoch, không còn ACTIVE hoặc đã bị xóa;
  - tăng epoch nhưng vẫn cùng cluster → retire rồi nạp lại ở epoch mới.
- **Retire**, theo thứ tự:
  1. gỡ khỏi định tuyến (phiên mới bị từ chối như tenant lạ);
  2. đặt cờ `retired`;
  3. đóng mọi phiên Noise đang gắn với runtime;
  4. đóng batcher (kết thúc map stream) và dừng GC/scheduled tasks;
  5. đóng state.
- Request trên phiên đã gắn với runtime bị retire → 503 và đóng phiên; client nối lại và được phân giải lại.
- **Policy:** CLI tăng `tailnets.policy_revision` sau khi ghi policy; reconcile gọi `ReloadPolicy`. Policy lỗi thì giữ policy cũ (không bao giờ allow-all), chỉ log một lần.

### 2.3 Quota (I10)
- Chỉ kiểm khi đăng ký **thiết bị mới** bằng auth key; node đã có (đăng nhập lại, xoay key) không bị tính.
- Vượt giới hạn → client nhận `RegisterResponse.Error` "this tailnet has reached its device limit".
- Cluster chưa đăng ký, tailnet lạ hoặc không ACTIVE → từ chối (fail closed).
- `headscale cluster register` ghi cluster vào registry.
- **Giới hạn:** kiểm quota nằm ngoài transaction tạo node nên có thể vượt vài thiết bị khi nhiều đăng ký đồng thời ở sát ngưỡng. `EnrollmentAllowedTx` đã có sẵn để làm chính xác (ticket sau).

### 2.4 CLI operator tạm (tới khi `mesh-placement` chạy trên lab)
- `headscale cluster register`
- `cluster tailnet reassign --to <cluster>`: CAS trên `(owner, epoch)`
- `cluster tailnet policy`: ghi policy và tăng revision
- các lệnh `tailnet …` giờ chạy được khi cluster đang chạy

## 3. Test

| Lệnh | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go test -run Fence ./hscontrol/db/tenantscope/` với `tenantscope.go` của master | (RED) | 1 | `StaleEpoch`, `ForeignCluster`, `Required` fail |
| `go test -race -v ./hscontrol/db/tenantscope/` (PostgreSQL 16, `MESH_TEST_POSTGRES_DSN`) | `50a72f2` | 0 | 6 test fence pass, kể cả `TestFenceBlocksConcurrentMove` (không skip) |
| `go test -race ./hscontrol/db/... ./internal/...` (SQLite + PostgreSQL) | `50a72f2` | 0 | `TestTenantDBWritesFencedAfterOwnershipChange` trên schema thật |
| `TestNoRawTenantWritesOutsideFence` | `9f315ec` | 0 | Bắt được đúng file khi gỡ khỏi allowlist |
| `go test -count=2 -race -tags p05cluster -run Cluster ./hscontrol/servertest/` | `50a72f2` | 1 | `EpochBump` fail: `controlclient.Direct` của test không tự nối lại (tailscaled thì có). Đã sửa test: chờ server đóng stream, rồi `Reconnect` |
| như trên, `-count=3` | `1da1d71` | 0 | 330 s |
| Quota servertest khi bỏ `checkQuota` (RED) | `64b3941`* | 1 | 2 test quota fail |
| Quota + P10 servertest (local) | `64b3941` | 0 | |
| Full `go test -race ./hscontrol/...` | xem §3.1 | | |

### 3.1 E2E client thật (`infra/lab/e2e/p05/run.sh`; tailscale v1.104.7 trên mc-ci → VPS → WireGuard → mc-cp)

| ID | Kết quả (`64b3941`, 123 s, exit 0) |
|---|---|
| C01–C02, R01–R07 | PASS (như P05) |
| L01 | PASS: tailnet tạo lúc đang chạy được phục vụ không cần restart; ban đầu deny-all, policy operator được nạp live (11 s) |
| F01 | PASS: tăng epoch → runtime nạp lại; a1 tự nối lại, không đăng nhập lại (cùng node key), thấy a3 vào sau (10 s) |
| F02 | PASS: B SUSPENDED từ chối thiết bị mới; resume → b1 tự nối lại và thấy b2 |
| F03 | PASS: A chuyển sang cluster khác → bị từ chối ở đây, runtime bị retire; B vẫn nhận thiết bị mới |

- **Lặp lại:** `1da1d71` 13/13, `7073d49` 13/13.
- **Lỗi tìm ra trong vòng E2E này và đã sửa:**
  - policy ghi vào DB không được runtime đang chạy nạp → thêm `policy_revision` + reload;
  - lỗi trong harness: `grep -q` + `pipefail`, và tái dùng container sau khi bị từ chối (tailscaled giữ auth key rồi tự đăng ký khi key hợp lệ lại).
- **Redaction:** 0 key chưa redact trong mọi artifact.
- **Cleanup:** đã chứng minh (DB E2E xóa bằng root, 0 container/volume nhãn `mesh-e2e=p05`, POC P02 trả `/health` 200).

## 4. Giới hạn / blocker
- Chưa có integration test hai process cluster (khác cluster ID) cùng một DB PostgreSQL; fence được kiểm bằng test DB và servertest.
- Chưa đo overhead của fence: một `SELECT … FOR SHARE` cho mỗi câu lệnh ghi. Ticket yêu cầu số đo before/after (để P14/benchmark).
- `mesh-placement` (mTLS) chưa chạy trên lab; E2E dùng CLI operator tạm.
- Thiết bị đang kết nối của một tailnet đã bị chuyển đi vẫn giữ data plane WireGuard với nhau cho tới khi key hết hạn (bản chất của Tailscale). Control plane đã dừng phục vụ chúng.
- **Reviewer approval:** chưa.
