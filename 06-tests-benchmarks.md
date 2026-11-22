# 06 — Test Matrix, Commands, Benchmark và Artifacts

> Cập nhật theo PLAN 2.0 và ADR 0005: một Noise key cho mỗi cluster, bootstrap, active/standby, PostgreSQL dùng chung, netmap tăng dần. Các test bị ảnh hưởng: S05, S11, S16, S17; thêm nhóm F (failover/bootstrap) và M (netmap tăng dần).

## 1. Bộ test real-client bắt buộc (S01–S18)

| Test | Kịch bản | Điều kiện pass |
|---|---|---|
| S01 | A1↔A2 cùng tailnet, grant allow; B1↔B2 tương tự | Kết nối VPN nội bộ đúng policy |
| S02 | A1 cố peer discovery B1 | Không thấy node key, peer, hostname, IP B |
| S03 | A wildcard grants `*`, B default deny | Không có visibility/traffic xuyên tailnet |
| S04 | Tạo A+grants=[] đồng thời enroll A1 | Không có MapResponse allow-all hoặc peer trái phép |
| S05 | Cùng machine key trên A/B, chuyển Tailscale profile A→B→A. Chạy **hai biến thể**: A/B trên **cùng cluster** (cùng ControlURL, cùng Noise key) và A/B trên hai cluster khác nhau | Hai node identities độc lập, A/B network maps nguyên vẹn; client không trộn hai profile cùng URL |
| S06 | Cùng IP overlay, cùng hostname và subnet route ở A/B | Không lookup nhầm, IPAM/DNS/route khác tailnet độc lập |
| S07 | B sửa DNS/extra records, A đang streaming | Không thay netmap/DNS của A |
| S08 | A route/exit-node, B trùng CIDR/advertise | Không tự approve hoặc leak default route sang B |
| S09 | A tagged device và user-owned device, B thử tag trùng | Tag ownership và grants bounded |
| S10 | A token/cookie/web-approval/preauth key được dùng tại B (cùng cluster) | Forbidden; không đổi tenant qua header/payload |
| S11 | **Gắn tenant cho phiên với key chung cluster:** (a) register bằng credential A rồi gửi map với node key của B trên cùng phiên Noise; (b) `(machine_key, node_key)` không khớp node nào hoặc khớp hai node; (c) Host lạ, SNI ≠ Host; (d) header `X-Tailnet-ID` giả; (e) credential của tenant không thuộc cluster | Fail-closed ở mọi biến thể; không phát map/peer của B; không tạo node |
| S12 | MapRequest/Node update A trong lúc B query peers | B không nhận event hoặc delta từ A |
| S13 | Tenant A gửi pending enroll flood (và bootstrap flood) vượt quota | B vẫn đăng ký/MapStream/bootstrap theo SLO lab |
| S14 | Public `/debug`, `/metrics`, gRPC admin, listener bootstrap, `/ready` | Blocked hoặc không lộ node/IP/key/tenant |
| S15 | DERP verify unknown/revoked and A/B clients (cả sau failover) | Không client không hợp lệ được relay; không relay bypass auth |
| S16 | **Restart và failover cluster:** restart cả cluster cùng DB + key; và kill active để standby lên (F01) | Client reconnect **không đăng nhập lại**, giữ NodeID/node key/IP, không double registration/cross-map |
| S17 | **Split brain và stale epoch:** active cũ sống lại với lease epoch cũ (F02); request mang `owner_epoch` cũ; tenant đã chuyển nhưng client dùng ControlURL cũ; bootstrap trả dữ liệu cũ | Chỉ process giữ lease hiện tại của cluster sở hữu tenant nhận write/registration/map |
| S18 | Delete/recreate tenant, revoke key | Old keys/nodes không thể hồi sinh trái phép |

**Test POC bắt buộc sớm:** S05 (biến thể cùng cluster), S10–S11 và S04 trước khi tối ưu performance. E2E phải dùng Tailscale client thật (pin phiên bản tối thiểu và ít nhất một phiên bản mới). Unit mock không có Noise thật thì không đủ.

### 1b. Failover và bootstrap (F01–F08)

| Test | Kịch bản | Điều kiện pass | Ticket |
|---|---|---|---|
| F01 | kill -9 active của cluster | Standby lấy lease với epoch mới trong ≤ TTL + chu kỳ thử; client thật reconnect cùng ControlURL **không đăng nhập lại**, giữ NodeID/node key/IP; ping phục hồi | P11b |
| F02 | Treo active (`SIGSTOP` > TTL) rồi `SIGCONT` | Standby tự lên trong TTL; active cũ tỉnh lại **từ chối mọi ghi** (epoch cũ), **không phát MapResponse**, quay về standby; không hai holder cùng epoch | P11b |
| F03 | Cắt mạng active ↔ PostgreSQL, client vẫn tới được active | Active tự fence theo deadline monotonic trước khi standby lấy lease; không lúc nào hai process cùng phát map | P11b |
| F04 | Hai process cùng giành lease | Đúng một thắng; bên thua không nhận traffic | P11b |
| F05 | Failover PostgreSQL (promote replica) | Lease và `owner_epoch` không lùi; không hai holder; control plane phục hồi; thời gian dừng được đo | P13 |
| F06 | Bootstrap: tenant `ACTIVE` / `MOVING` / `SUSPENDED` / `PENDING` / `DELETING` / không tồn tại, có và không có credential | Đúng URL cluster khi `ACTIVE`; còn lại bị từ chối theo bảng `05-data-api-contract.md` §8; phản hồi từ chối không lộ tồn tại (status, body, timing) | P11 |
| F07 | Client tới nhầm cluster (bootstrap cũ, tự cấu hình, tenant đã move) | Cluster từ chối register/map, không tạo node | P11, P12b |
| F08 | Restore backup PostgreSQL cũ hơn một lần failover và một lần move | `owner_epoch` và lease epoch không lùi; không cluster/process nào nhận lại quyền cũ | P13 |

### 1c. Netmap tăng dần (M01–M06)

Bắt buộc trước khi bất kỳ môi trường nào bật công tắc (`tickets/P00d.md`). P07 chạy lại M01/M03 với nhiều tenant.

| Test | Kịch bản | Điều kiện pass |
|---|---|---|
| M01 | Property test (`rapid`): chuỗi thay đổi ngẫu nhiên node/user/tag/route/exit node/policy/xóa/thu hồi | Peer map tăng dần ≡ full rebuild sau **mỗi** bước |
| M02 | Đổi policy | Luôn đi full rebuild |
| M03 | Tiêm lệch vào snapshot tăng dần | Đối chiếu định kỳ phát hiện, metric tăng, thay bằng full rebuild, node bị ảnh hưởng nhận full map |
| M04 | Thu hồi/xóa node khi hàng đợi delta đầy | Node bị ảnh hưởng vẫn nhận bản không còn peer; full map định kỳ đúng chu kỳ |
| M05 | Công tắc OFF | Chuỗi MapResponse giống hệt full rebuild (golden) |
| M06 | Client thật, công tắc ON: thu hồi node, xóa user, đổi policy chặn | Peer biến mất khỏi netmap client trong thời gian giới hạn |

## 2. Command baseline từ upstream

```bash
# Phải chạy trong source checkout, với Go/nix toolchain được tag pin.
nix develop
make build
make test
CGO_ENABLED=1 go test -race ./...
# Đọc cmd/hi/README.md trước khi chạy:
go run ./cmd/hi doctor
go run ./cmd/hi run "TestPingAllByIP"
```

Khi rename `cmd/hi` hoặc module, cập nhật lệnh trong Makefile và tài liệu ngay PR cùng thay đổi. Các lệnh trên là **baseline**, không phải tuyên bố đã chạy trong môi trường hiện tại. Từ P03b, test DB chạy trên cả SQLite và PostgreSQL; integration ưu tiên PostgreSQL (`--postgres`).

### Phân loại test

- Go unit: policy parsing, typed scope, plugin scope (`ErrNoScope`), lint SQL thô, uniqueness, auth claims, owner/lease epoch CAS, IPAM per tenant, equivalence netmap (M01–M05).
- `servertest`: handler/state mapping, register+poll, default-deny transaction, auth replay, gắn tenant cho phiên (S11).
- Docker integration: two real tailscale clients/tailnet, multiple profiles cùng cluster, SSH/routing/DNS, DERP/exit node; hai process + PostgreSQL container cho F02–F04.
- Proxmox E2E: isolated underlay, TLS thật, bootstrap, hai cluster active/standby, PostgreSQL primary/replica, faults, latency.
- Race/fuzz: MapResponse events + concurrent writes, blocked node update, malformed hostinfo/routes/keys.
- Security: debug/metrics route scan (cả bootstrap); API authorization matrix; sensitive logs redaction; cross-tenant grants.

## 3. E2E fixture

```yaml
# Khái niệm, agent phải biến thành test code
postgres: {primary: mc-pg-01, replica: mc-pg-02}
bootstrap: mc-bootstrap
clusters:
  cluster-1: {url: "https://cluster-1.mesh.test:8443", processes: [mc-c1-a, mc-c1-b]}
  cluster-2: {url: "https://cluster-2.mesh.test:8443", processes: [mc-c2-a, mc-c2-b]}
tailnets:
  alpha: {cluster: cluster-1, nodes: [a1, a2], policy: {grants: []}}
  beta:  {cluster: cluster-1, nodes: [b1, b2], policy: {grants: []}}   # cùng cluster với alpha
  gamma: {cluster: cluster-2, nodes: [c1, c2], policy: {grants: []}}
intentional_collisions: [machine_key, hostname, overlay_ip, advertised_subnet, tag]
client_matrix: [pinned_stable, current_supported]
netmap_incremental: [off, on]
```

Sau khi tạo tailnet phải **không** có peer traffic cho đến khi policy được cập nhật rõ ràng. Test traffic cross tailnet cần phân biệt direct underlay với overlay: dùng tcpdump và firewall underlay.

## 4. Benchmark test regimes (không phải performance claim)

| Profile | Workload | Chỉ số |
|---|---|---|
| L1 | 10 tailnet × 10 node, idle | RAM idle per tailnet, open streams |
| L2 | 100 tailnet × 10 node, node join/leave | p95/p99 enrollment, map update |
| L3 | 10 tailnet × 100 node, endpoint churn | p95 map build, delta bytes, CPU |
| L4 | 1 tailnet × 1.000 node (và lớn hơn theo P00c) | netmap hot-spot, memory; netmap tăng dần OFF/ON |
| L5 | 200 concurrent enroll trên tenant A | fairness, queue, tenant B latency |
| L6 | 1000 concurrent reconnects / 2 cluster | handshake, error, PG connections, PG lock wait |
| L7 | DERP fallback forced, increasing bandwidth | relay saturation, CPU, drop, latencies |
| L8 | Kill active / treo active / partition PostgreSQL / failover PostgreSQL | thời gian tới khi standby giữ lease, tới MapResponse đầu tiên, tỉ lệ client phải đăng nhập lại, split brain prevention |
| L9 (B1) | Nhiều tenant trong **một process**: 20 / 50 / 100 tenant × 200 thiết bị; so (a) runtime/NodeStore riêng mỗi tenant (ADR 0001) với (b) một NodeStore chung lọc theo tenant | CPU, RSS, thời gian dựng peer map, độ trễ map p50/p95/p99 (ticket P05) |
| L10 (B2) | Tải **PostgreSQL dùng chung** khi tăng số cluster process (1→16) và tenant; có và không có PgBouncer | reads/s, writes/s, p50/p95/p99 các câu chính, kích thước index, số kết nối, `cl_waiting` của PgBouncer, CPU PostgreSQL, WAL (ticket P14) |
| L11 (B3) | **Chuyển tenant** 5.000 thiết bị: CAS một dòng `tailnets` (owner_cluster_id/owner_epoch) so với ghi lại cột `cluster_id` trên mọi dòng của tenant (thiết kế blog, mô phỏng trên bản sao schema) | thời gian transaction, WAL, lock giữ, thời gian writer khác bị chặn (ticket P12b) |

**L9–L11 chi tiết:**
- **L9 / B1** — `Planned` (P05 chưa có code). Thiết kế harness ở `tickets/P05.md` §B1. Phải dùng cùng policy và ma trận P00b/P00c để so được. Biến thể (b) chỉ để đo, không phải hướng triển khai (ADR 0001 đã loại).
- **L10 / B2** — harness `cmd/bench-db` (build tag `benchdb`), chạy lần đầu trên `mc-bench`: `evidence/P14-db-benchmarks.md`. Mỗi cluster process là một OS process riêng, mở DB bằng `NewHeadscaleDatabase` thật, ghi/đọc bằng hàm `HSDatabase` thật. Mỗi cluster kèm một standby pool 2. Fencing owner+epoch (P10) được **mô phỏng** bằng `SELECT … FOR SHARE` trong mọi transaction ghi.
- **L11 / B3** — cùng harness (`bench-db b3`). Câu CAS là **mô phỏng** câu P12b (chưa có code). Thiết kế blog mô phỏng bằng schema `blogsim` (bản sao bảng tenant + cột `cluster_id NOT NULL` + index).
- Ba bài đều phải ghi: commit, phần cứng, tải khác trên VM lúc đo, phiên bản PostgreSQL/PgBouncer, cấu hình pool, và nhãn **đo** / **ngoại suy** cho từng con số.

Số lượng là **tập mục tiêu thử nghiệm theo cấp độ**, không phải năng lực chắc chắn đạt. Baseline hiện có: P00b đo một process ≈1.5k–2.5k thiết bị với policy thực tế, client chạy trong cùng process (`evidence/P00b-capacity-baseline.md` §7); P00c đo server-only. Ngưỡng SLO phải được định nghĩa cùng nhóm vận hành sau baseline, trước signoff. Report bắt buộc lưu workload config, Git SHA, host vCPU/RAM/storage, test duration, Tailscale version, trạng thái công tắc netmap tăng dần, p50/p95/p99, RSS, CPU, PostgreSQL (kết nối, lock wait, WAL), PgBouncer (chế độ pool, `default_pool_size`, `max_prepared_statements`, `cl_waiting`, `maxwait`), backlog, error and replay counts.

## 5. Artifacts + redaction

Mỗi run lưu:

```text
artifacts/<gitsha>/<testid>/
  inventory.txt            # chỉ test VMs
  environment.json         # không secrets
  commands.txt             # args đã redact
  test-result.json
  client-status.json       # node IDs/IP đã mask nếu chia sẻ
  map-response.json        # keys redacted, checksum preserved
  bootstrap-responses.json # URL cluster + status, không credential
  lease-audit.json         # chuỗi (cluster_id, holder, epoch, thời điểm) cho F-tests
  metrics.prom
  pcap-metadata.txt        # không xuất full traffic thô ra ngoài lab
```

No-Go nếu test S01–S18, F01–F08 (theo gate) hoặc M01–M06 (khi bật netmap tăng dần) chưa có bằng chứng (test not run ≠ pass), hoặc bất kỳ leak security nào chưa giải quyết.
