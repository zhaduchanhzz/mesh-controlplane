# PR P13e Evidence — xoay Noise key của cluster (code + runbook nháp)

- **Repo commit before / after:** `c4533aa` (`feat/P13-plan`) → tip `feat/P13be-db-noise` (bảng §1; tip trong báo cáo cuối).
- **Trạng thái:** code `Verified` (unit, PostgreSQL/SQLite, servertest `-tags p05cluster -race`). Runbook `docs/ops/noise-key-rotation.md` `Planned` (chưa diễn tập). Drill c2 (đo phạm vi ảnh hưởng client) **`Not started`**. Chưa `Reviewed`.
- **Verified source paths & symbols:** `evidence/source-inventory-P13be.md` §2.2, quyết định §3.7–3.11.
- **Design choice / ADR:** ADR 0014 §1–§3 (D8: xoay lạnh qua switchover, không chồng lấn hai key, kiểm định kỳ). **Mô hình Noise không đổi** (một key mỗi cluster, ADR 0005; AGENTS rule 12): không trial-decrypt, không key kế tiếp, `/key` vẫn trả đúng một key. Không migration, không đổi OpenAPI/`gen/` (kiểm định dạng ở tầng registry).

## 1. Commit RED → GREEN

| Hành vi | RED commit | Lệnh RED (exit) và kết quả | GREEN commit | Lệnh GREEN (exit) |
|---|---|---|---|---|
| `noise_public_key` phải là `mkey:` hợp lệ (POST/PATCH) | `200a04d` | `go test -count=1 -run 'TestClusterNoiseKeyFormat\|TestOperatorReconcile' ./internal/placement/placementapi/` → **1**: `mkey:c1`, `not-a-key`, `nodekey:…` được lưu (201/200) | `b40d296` | `go test -count=1 ./internal/placement/...` → **0** |
| Active kiểm lại registry mỗi reconcile, step down khi lệch | `200a04d` | `go test -tags p05cluster -count=1 -run TestP13e -v ./hscontrol/servertest/` → **1**: `Condition never satisfied — a process whose key is no longer the registered one must stop serving` (6 s) | `87343f0` | → **0**: ngừng phục vụ và nhả lease **1,95 s** sau khi sửa registry (≤ 1 chu kỳ 2 s); không phục vụ khi còn lệch; registry không bị ghi đè; phục vụ lại ở epoch mới khi key trở lại. `go test -race -count=3 -run TestManager ./internal/lease/` → **0** (mới `TestManagerStepDown`) |
| Không tự tạo key ở cluster mode | `200a04d` | cùng lệnh servertest → **1**: `An error is expected but got nil — a cluster process must not start without its Noise key file` (file key mới được tạo) | `c84b854` | → **0**; thêm: `--init-noise-key` bị từ chối khi registry có key (lỗi nêu key đã đăng ký), file key hỏng không bị thay |
| Lệnh xoay (registry, audit) + CLI `noise-key show/install/rotate` | `5664888` | `go test -run TestRotateNoiseKey ./internal/placement/` và `go test -run TestNoiseKey ./cmd/headscale/cli/` → **1** (build failed: `RotateNoiseKey`, `rotateNoiseKeyCLI`, `noiseKeyStatus` chưa có) | `47e5544` | → **0** (SQLite + PostgreSQL); mới `TestP13ePlannedRotationSwitchover`: process có key mới phục vụ **0,81–0,84 s** sau `RotateNoiseKey`, `/key` trả key mới; `go test -race -tags p05cluster -count=2 -run TestP13e ./hscontrol/servertest/` → **0** |
| Lint | — | `golangci-lint run ./...` ở `47e5544` → **1** (2 issue trong file test, xem P13b §1) | `1886808` | §4 |
| Runbook nháp | — | — | `4520d80` | — |

Thông báo khi thiếu file key (log test):

```
cluster Noise private key file is missing: /tmp/…/noise_private.key; copy the key file of this cluster from its other process (or, for the first process of a new cluster, start it once with --init-noise-key)
```

## 2. Thiết kế

- `placement.ValidateNoisePublicKey` (`key.MachinePublic.UnmarshalText`, khác zero) trong `CreateCluster`/`UpdateCluster`; `""` trong PATCH vẫn xóa (P12).
- `Registry.RotateNoiseKey(ctx, op, id, mkey)`: kiểm định dạng, khác key hiện tại, CAS trên key đã đọc (`ErrStaleRevision` nếu đổi giữa chừng), `revision + 1`, audit `RotateNoiseKey` `{"from","to"}` (chỉ public key; test kiểm không có `privkey:` trong audit).
- `Cluster.reconcile` → `noiseKeyRotated()`: `RecordNoiseKey` (đọc; chỉ ghi khi registry rỗng, như lúc lấy lease). `ErrNoiseKeyMismatch` → `ready=false`, `ClearLease`, `lease.Manager.StepDown`. Lỗi khác chỉ log (ghi vẫn bị fence ở DB).
- `lease.Manager.StepDown(reason)`: kênh theo từng lease, không chặn; `Run`: `onLose` (retire mọi runtime) → **release** → chờ **TTL** rồi mới acquire lại (process có key đúng lấy trước). Nếu process lệch key lấy lại được, `onLeaseAcquired` thấy lệch và nhả ngay (đường P12) — không bao giờ phục vụ.
- `NewCluster(cfg, opts...)`: file key thiếu → `ErrNoiseKeyMissing` trước khi mở DB; `WithNoiseKeyInit()` / `cluster serve --init-noise-key` chỉ tạo khi registry chưa có key; `WriteNoiseKeyFile` ghi nguyên tử (temp 0600 cùng thư mục, fsync, rename, fsync thư mục). `headscale serve` (không cluster) giữ hành vi upstream.
- CLI `headscale cluster --cluster-id X noise-key`: `show` (public key file vs registry), `install --from <file>|--generate` (không đụng DB; `--from /dev/stdin` cho pipe ssh), `rotate [--public-key mkey:…]` (actor `cli:<cluster>`). Không in private key.
- servertest: process đầu của cluster test dùng `WithNoiseKeyInit` (như lần đầu khởi động cluster mới); `TryNewCluster`, `StartProcessWithNoiseKey`.

## 3. Drill lab cần gì (P13e, chưa chạy)

- Binary từ nhánh này trên mc-c2, mc-c2b (và mc-cp/mc-c1b cho c1 không bị ảnh hưởng). Unit systemd **không** cần `--init-noise-key` (key đã có).
- ≥ 2 tenant trên c2, client 1.104.7 + 1.84.3 (VM A/B và container), ping nền; client trên c1 làm đối chứng.
- Theo runbook §2 (có kế hoạch) và §3 (khẩn cấp) ×2; điền runbook §6 và ADR 0014 §Hệ quả: thời gian tới `/ready`, tới ping/`tailscale status` phục hồi, tỉ lệ tự phục hồi không restart (1/5/15/60 phút), có phải đăng nhập lại không, NodeID/IP giữ nguyên.
- Log protocol đã redact nếu client không hồi phục (điều kiện dừng, AGENTS rule 12).

## 4. Lint, vet, race, servertest (P13b + P13e, cùng lượt)

Lượt `~/p13be-verify.sh 47e5544` trên mc-ci (tuần tự, `-p 4`, PostgreSQL/PgBouncer tạm qua `MESH_TEST_*`), lint chạy lại ở tip sau khi sửa:

| Lệnh | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go vet ./hscontrol/... ./internal/... ./cmd/...` | `47e5544` | 0 | |
| `go vet -tags p05cluster ./hscontrol/ ./hscontrol/servertest/` | `47e5544` | 0 | |
| _(điền sau)_ | | | |

## 5. Known limitations / security blockers

- Không có blocker isolation. Không đổi mô hình Noise; không có xoay "nóng"; client chưa nối lại vẫn tin key cũ (không thu hồi được, ADR 0014 §2).
- Ảnh hưởng client **chưa đo** (drill).
- Cửa sổ giữa lúc registry đổi và lúc active nhận ra: ≤ 1 chu kỳ reconcile (2 s) + thời gian một lần đọc DB; trong cửa sổ đó active (key cũ) còn phục vụ từ bộ nhớ; ghi vẫn bị fence bình thường (lease còn hợp lệ — xoay key không phải fence).
- Sau khi active step down, nếu chưa có process nào mang key mới thì control plane cluster dừng (fail closed, có chủ ý, runbook §3.4).
- Thay đổi hành vi: dựng lại cluster lab từ đầu (VM mới, chưa có key) cần `cluster serve --init-noise-key` một lần; harness E2E hiện tại không bị ảnh hưởng (đều kiểm `test -s` key hoặc chép key qua pipe).

## 6. Reviewer approval

Chưa có.
