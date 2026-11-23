# PR P09a Evidence — E2E S01/S03, S16, S18; DELETING terminal; quota fix (review A2)

- **Repo commit before / after:** `d3be9e2` (master: P10 + P08 merged) → `459e6d4` (DELETING terminal, CLI, E2E), `bb3fc73` (quota fix A2), commit evidence (commit chứa file này). Nhánh `feat/P09a-e2e-lifecycle`.
- **Mục đích:** chuẩn bị gate N1 (P09): thêm vào harness client thật các ca S01, S03, S16, S18 (`06-tests-benchmarks.md`) không phụ thuộc P06/P07; sửa hai lỗi tìm ra khi làm.
- **ADR:** không có ADR mới. ADR 0005/0007 giữ nguyên.

## 1. Path và symbol

| Path | Symbol | Thay đổi |
|---|---|---|
| `hscontrol/db/platform_admin.go` | `SetTailnetStatus`, `ErrTailnetDeleting` | `DELETING` là trạng thái cuối: mọi chuyển trạng thái khác bị từ chối ngay trong câu `UPDATE` (không có cửa sổ race); `DELETING` → `DELETING` là no-op |
| `hscontrol/db/platform_test.go` | `TestPlatformDeletingIsTerminal` | SQLite + PostgreSQL |
| `cmd/headscale/cli/cluster.go` | `tailnetAuthKeyCmd`, `tailnetAuthKeyRevokeCmd` | `tailnet authkey` in thêm `id`; lệnh mới `tailnet authkey-revoke --slug --id` qua handle tenant-scoped (ID của tailnet khác → not found) |
| `hscontrol/cluster.go` | `Cluster.knownNode`, `runtimeForRegister` | **Sửa lỗi A2:** chỉ coi là "thiết bị đã có" khi `(machine key, node key/old node key)` thuộc **đúng tailnet của auth key** |
| `hscontrol/servertest/cluster_p10_test.go` | `TestClusterQuotaNotBypassedByNodeOfOtherTailnet` | RED → GREEN cho A2 |
| `infra/lab/e2e/p05/run.sh` | `t_scope`, `t_restart`, `t_lifecycle`, `set_policy`, `has_a_host`, `has_b_host` | Ca S01, S03, S16, S18 |

## 2. Lỗi tìm ra và đã sửa

| ID | Lỗi | Hậu quả | Sửa | Test |
|---|---|---|---|---|
| S18-1 | `SetTailnetStatus` cho phép `DELETING` → `ACTIVE` | Tenant đã xóa, cùng key và node cũ, có thể được phục vụ lại | `DELETING` terminal | `TestPlatformDeletingIsTerminal`, E2E S18 |
| A2 (review P10) | `knownNode` nhận cặp key có ở **bất kỳ** tailnet nào | Thiết bị có node ở A đăng ký được vào B đã đầy quota (gửi node key ở A làm `OldNodeKey`) | So tailnet tìm được với tailnet của auth key | `TestClusterQuotaNotBypassedByNodeOfOtherTailnet` |

## 3. Test (mc-ci, Go 1.27.4; PostgreSQL 16.15 tạm trên unix socket qua `initdb`, `MESH_TEST_POSTGRES_DSN`)

| Lệnh | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go build ./...`; `go vet ./hscontrol/db/ ./cmd/headscale/...` | `459e6d4` | 0 | |
| `go test -race -count=1 -v -run TestPlatformDeletingIsTerminal ./hscontrol/db/` | `459e6d4` | 0 | subtest `sqlite` và `postgres` đều PASS (không SKIP) |
| `go test -race -count=1 ./hscontrol/db/... ./internal/...` (SQLite + PG16) | `459e6d4` | 0 | 6 package ok, gồm `tenantscope`, `placement`, `placementapi`. Lần chạy đầu cùng lệnh exit 1 vì `initdb -q` không hợp lệ với PG16 nên PG không khởi động (lỗi môi trường, không phải code) |
| `go test -count=1 -tags p05cluster -run TestClusterQuotaNotBypassedByNodeOfOtherTailnet ./hscontrol/servertest/` | trước `bb3fc73` | **1 (RED)** | B đã đầy (limit 1) vẫn trả `MachineAuthorized: true` và tạo node trong B |
| như trên, `-race -count=3` | `bb3fc73` | 0 | GREEN |
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` | `bb3fc73` | 0 | 130 s |

## 4. E2E client thật (`infra/lab/e2e/p05/run.sh`; `tailscale/tailscale:v1.104.7` trên mc-ci → VPS → WireGuard → mc-cp)

- **`459e6d4`** (`artifacts/459e6d4/P05/e2e-20261123T053327Z/`): exit 0, 193 s, **19/19 PASS**, `cleanup_ok=1`, 0 key chưa redact.
- **`bb3fc73`** (`artifacts/bb3fc73/P05/e2e-20261123T054038Z/`, binary sha256 `3a7b1f32311d0192…`): exit 0, 191 s, **19/19 PASS**, `cleanup_ok=1`, 0 key chưa redact. Hai lần chạy liên tiếp cùng kết quả.

| ID | Nội dung kiểm |
|---|---|
| C01–C04, R01–R07, L01, F01–F03 | như P05/P08/P10 |
| **S01** | Trong B: b1↔b4 thấy nhau và `tailscale ping` được dưới policy allow; peer của B không có host nào của A, peer của A không có host nào của B |
| **S03** | A giữ policy wildcard `*`, B đổi sang `{"grants":[]}` (nạp live): sau 2 s b1, b4 không còn peer và không ping được nhau; a1 vẫn thấy a2; A vẫn không thấy host nào của B. Sau đó khôi phục policy allow của B cho các ca F02/F03 |
| **S16** | `systemctl restart headscale-cluster` (cùng DB, cùng Noise key): 4 client (a1, a2, b1, b4) Running trở lại **không đăng nhập lại**; node key, IP, NodeID và danh sách node phía server **giống hệt** trước restart; a4 đăng ký sau restart được a1 thấy (chứng minh stream map của a1 do process mới phục vụ); không có peer xuyên tenant. 14 s |
| **S18** | (1) key bị `authkey-revoke` → từ chối, không tạo node; (2) tailnet `e` → `DELETING`: runtime bị retire (log có `slug=e`), `DELETING → ACTIVE` bị từ chối, `tailnet create --slug e` bị từ chối (slug không tái dùng); (3) key của `e` → `invalid pre auth key` trên thiết bị mới; (4) tạo tailnet `e2`, restart container của node cũ `e1`: e1 không về `Running` (`NoState`), `e2` chỉ có node của chính nó, e2 không thấy e1 |

## 5. Known limitations

- **S18 "recreate":** slug của tenant đã xóa **không** tái dùng được (unique index trên `tailnets.slug` gồm cả dòng đã xóa). Tenant mới phải có slug mới. Chưa có thao tác purge dữ liệu của tenant `DELETING` (ticket sau).
- Revoke key **không** xóa node đã đăng ký bằng key đó (hành vi upstream, có ghi trong `RevokePreAuthKey`).
- S01/S03 dùng ACL `acls` kiểu cũ cho allow và `grants: []` cho deny; chưa kiểm grants có điều kiện (P06).
- S16 chỉ là restart cùng process; failover active → standby là F01 của N2 (P11b).
- Các phát hiện còn mở của review P10/P08 (A1, A3–A10, B1–B4): `evidence/REVIEW-2026-11-23-P10-P08.md`.
- **Reviewer approval:** chưa (review AI không thay người duyệt).
