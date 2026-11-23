# PR P10d Evidence — giới hạn thiết bị của cluster khi đăng ký và giới hạn quan hệ peer của tailnet

- **Repo commit before / after:** `3269688` (master) → `dcb8afc` (code + test), commit evidence (commit chứa file này). Nhánh `feat/P10d-capacity-limits`.
- **Nguồn:** chủ dự án đồng ý (2026-11-23) hạ giới hạn vận hành tạm thời để làm tiếp:
  - mesh toàn phần ≤ 1 000 thiết bị;
  - `max_devices` của cluster được kiểm cả khi đăng ký thiết bị;
  - đẩy tenant mới sang cluster khác qua placement và bootstrap (P11).
- **Số đo căn cứ:** `evidence/source-inventory-P00c.md` §9. Trên `master`, 10k personal_devices/isolated đạt; 10k allow_all không đạt (6 161/10 000 node, 23,3 GB RSS).
- **ADR:** không có ADR mới. Các con số là giới hạn vận hành tạm thời, không phải capacity claim (PLAN §0).

## 1. Path / symbol đã đọc

- `internal/placement/clusters.go` `place()`: chọn cluster ACTIVE ít tải nhất, bỏ cluster có `devices >= max_devices`; `clusterUsage`, `lockClusters`.
- `internal/placement/quota.go` `EnrollmentAllowed`, `EnrollmentAllowedTx` (P10b: chạy trong transaction chèn node; advisory lock `placement/quota/<tailnet>`); `deviceCount`.
- `internal/placement/placement.go` `Quota`, `Config`, các lỗi.
- `hscontrol/cluster.go` `load` (cài `EnrollGate`), `checkQuota` (kiểm sớm khi định tuyến register).
- `hscontrol/session_route.go` `registerRefusal`; `hscontrol/auth.go` `handleRegisterWithAuthKey` (ánh xạ `ErrQuotaExceeded`).
- `hscontrol/state/node_store.go` `Snapshot`, `snapshotFromNodes`; `hscontrol/state/state.go` `createAndSaveNewNode`.
- `hscontrol/types/config.go` `Tuning`; `hscontrol/config/{default,validate}.go`; `cmd/headscale/cli/cluster.go` (`--max-devices` mặc định 5000; E2E đăng ký cluster với 5000).

**Lỗ hổng trước P10d:** `max_devices` của cluster shared chỉ được kiểm khi **đặt tailnet mới**. Các tailnet đã nằm trên cluster cứ tăng thiết bị, mỗi tailnet dưới quota 5 000 của mình, nên tổng có thể vượt `max_devices`.

## 2. Thay đổi

| Path / symbol | Thay đổi |
|---|---|
| `internal/placement/quota.go` `clusterBudget` (mới), `EnrollmentAllowedTx` | Cluster shared: sau khi tailnet qua quota, lấy advisory lock `placement/cluster/<id>` (PostgreSQL, sau lock của tailnet; thứ tự luôn tailnet → cluster), đếm thiết bị sống của mọi tailnet sống trên cluster; `>= max_devices` → `ErrQuotaExceeded` + `ErrClusterFull` (join hai lỗi). Cluster dedicated không đổi (limit của tailnet = `max_devices`) |
| `internal/placement/placement.go` | `Quota.ClusterDevices`, `ClusterLimit`, `ClusterWarn` (80%, tính cả thiết bị đang đăng ký); `ErrClusterFull` |
| `hscontrol/cluster.go` `load` | Log cảnh báo khi cluster đạt 80% |
| `hscontrol/session_route.go` `quotaRefusal`, `peerLimitRefusal`; `hscontrol/auth.go` | Thông báo cho client tách ba trường hợp: tailnet đầy ("this tailnet has reached its device limit"), cluster đầy ("no capacity for new devices right now; contact your administrator", không nêu tên cluster hay tenant khác), quá giới hạn peer |
| `hscontrol/types/config.go` `Tuning.MaxPeerEntries`; `hscontrol/config/{default,validate}.go` | `tuning.max_peer_entries`, mặc định **1 000 000** (= mesh toàn phần 1 000 node), `0` = không giới hạn, âm = lỗi cấu hình |
| `hscontrol/state/node_store.go` `Snapshot.peerEntries`, `NodeStore.PeerEntries` | Tổng `len(peersByNode[id])`, tính khi dựng lại peer map, giữ nguyên khi tái dùng |
| `hscontrol/state/state.go` `createAndSaveNewNode`, `ErrPeerEntriesLimit` | Node **mới** bị từ chối khi tailnet đã ở giới hạn, trước khi cấp IP hay ghi DB |
| `hscontrol/servertest/server.go` `WithMaxPeerEntries` | Tùy chọn test |
| Tài liệu cấu hình sinh lại | `go generate ./hscontrol/types/` (configgen): `docs/ref/config-example.yaml`, `docs/ref/configuration-options.md`, `config_docs_generated.go`, `nix/options.generated.nix` |
| `hscontrol/config/load_test.go` `compatView` | Dựng `compatTuning` theo từng trường (struct `Tuning` có thêm trường). Giống hệt thay đổi cùng chỗ ở nhánh P00d, để merge không xung đột |

**Vì sao giới hạn theo quan hệ peer, không theo tên policy "allow_all":** chi phí control plane tỉ lệ với tổng số cặp được thấy nhau, không phải số thiết bị. Với 10k thiết bị:

| Policy | Peer entries | Có bị chặn bởi giới hạn 1 000 000? |
|---|---|---|
| personal_devices | ~90 000 | Không |
| hub-and-spoke (20 server) | ~400 000 | Không |
| allow_all | ~100 000 000 | Có |

Một tailnet allow_all ~1 000 thiết bị chạm giới hạn. Phát hiện "allow_all" bằng cách đọc policy thì dễ sai (group/tag/autogroup tương đương).

## 3. Test — RED rồi GREEN (mc-ci, Go 1.27.4)

RED: cùng cây `dcb8afc` với hai chỗ kiểm bị tắt (`clusterBudget` trả `nil` ngay; điều kiện peer `&& false`).

| Test | RED | GREEN |
|---|---|---|
| `TestQuotaSharedClusterBudget` (SQLite + PostgreSQL): cluster shared `max_devices` 10, hai tailnet, cảnh báo ở 80%, đầy ở 10, thiết bị xóa mềm không tính, dedicated không đổi | FAIL: `ClusterDevices` 0 (want 6), không có `ErrClusterFull` | PASS cả hai backend |
| `TestQuotaSharedClusterBudgetConcurrent` (SQLite + PostgreSQL): 20 đăng ký đồng thời vào hai tailnet của cluster `max_devices` 5, kiểm trong transaction chèn | FAIL (chạy cùng lệnh RED) | PASS: đúng 5 nhận, 15 từ chối `ErrClusterFull`, 5 node |
| `TestPeerEntriesLimitRefusesNewNode` (servertest, không policy = allow-all, giới hạn 6) | FAIL: `An error is expected but got nil` (node thứ 4 vào được) | PASS `-race -count=3`: node thứ 4 bị từ chối với thông báo "connects too many devices", không để lại node; 3 node cũ giữ 2 peer |
| `TestPeerEntriesLimitIgnoresUnconnectedNodes` (deny-all, giới hạn 6, 6 node) | — | PASS `-race -count=3` |

| Lệnh | Commit | Exit |
|---|---|---|
| `go build ./... && go vet ./internal/placement/ ./hscontrol/state/ ./hscontrol/ ./hscontrol/config/ ./hscontrol/types/ && go vet -tags p05cluster ./hscontrol/servertest/` | `dcb8afc` | 0 |
| `gofmt -l` (các thư mục đã sửa) | `dcb8afc` | chỉ `hscontrol/state/node_store_test.go`, file này đã lỗi `gofmt` sẵn trên `3269688`, không do P10d |
| Suite cluster, full race, E2E | | xem §4 |

## 4. Suite cluster, full race, E2E

| Lệnh | Commit | Exit | Ghi chú |
|---|---|---|---|
| `go test -race -count=1 -tags p05cluster -run Cluster ./hscontrol/servertest/` (mc-ci) | `dcb8afc` | 0 | 111 s |
| `nix shell … -c go test -race -count=1 -p 8 -timeout 90m ./hscontrol/... ./internal/... ./cmd/...` (mc-ci) | `dcb8afc` | 1 | 31 package ok. `hscontrol` FAIL chỉ ở `TestACMETLSSettings/DNS-01`: `listen tcp 127.0.0.1:57066: bind: address already in use`. `freePort` lấy port rồi mới bind, nên port bị package chạy song song chiếm mất; test này không đụng tới code P10d |
| `go test -race -count=3 -run TestACMETLSSettings ./hscontrol/` (chạy lại riêng) | `dcb8afc` | 0 | Cộng lại: 32 package ok |
| E2E `infra/lab/e2e/p05/run.sh`, binary `dcb8afc` (`CGO_ENABLED=0`, sha256 `bde57dc21fa1abbf…`), cluster đăng ký `--max-devices 5000` | `dcb8afc` | 0, 0 | **19/19 PASS ×2**: `artifacts/dcb8afc/P05/e2e-20261123T155522Z/`, `…T160129Z/`; `cleanup_ok=1`, 0 key chưa redact |

## 5. Known limitations

- Giới hạn peer dựa trên snapshot NodeStore, có thể trễ một batch (≤ `node_store_batch_timeout`). Đăng ký đồng thời có thể vượt giới hạn vài entry. Đây là giới hạn capacity, không phải ranh giới bảo mật.
- Đổi policy làm tailnet vượt giới hạn **không** bị từ chối; chỉ node mới bị chặn. Chưa có metric hay log riêng cho trường hợp này.
- Một node "hub" mới có thể thấy rất nhiều peer: tổng entries sau khi nó vào có thể vượt giới hạn tới khoảng 2n.
- Kiểm thiết bị của cluster tuần tự hóa các lần đăng ký của mọi tailnet trên cùng cluster shared (PostgreSQL) cho tới commit. Chưa đo tác động lên thông lượng đăng ký.
- Tên cluster và tenant không lộ trong thông báo cho client; log phía server có cluster ID.
- Đẩy tenant sang cluster khác: tenant **mới** đi theo `place()` như trước. Tenant **đã có** chạm giới hạn cluster thì bị từ chối; dời đi là P12b (đăng nhập lại). Client chưa tự tìm cluster: P11 bootstrap.
- **Reviewer approval:** chưa.
