# 06 — Test Matrix, Commands, Benchmark và Artifacts

## 1. Bộ test real-client bắt buộc (S01–S18)

| Test | Kịch bản | Điều kiện pass |
|---|---|---|
| S01 | A1↔A2 cùng tailnet, grant allow; B1↔B2 tương tự | Kết nối VPN nội bộ đúng policy |
| S02 | A1 cố peer discovery B1 | Không thấy node key, peer, hostname, IP B |
| S03 | A wildcard grants `*`, B default deny | Không có visibility/traffic xuyên tailnet |
| S04 | Tạo A+grants=[] đồng thời enroll A1 | Không có MapResponse allow-all hoặc peer trái phép |
| S05 | Cùng machine key trên A/B, chuyển Tailscale profile A→B→A | Hai node identities độc lập, A/B network maps nguyên vẹn |
| S06 | Cùng IP overlay, cùng hostname và subnet route ở A/B | Không lookup nhầm, IPAM/DNS/route khác tailnet độc lập |
| S07 | B sửa DNS/extra records, A đang streaming | Không thay netmap/DNS của A |
| S08 | A route/exit-node, B trùng CIDR/advertise | Không tự approve hoặc leak default route sang B |
| S09 | A tagged device và user-owned device, B thử tag trùng | Tag ownership và grants bounded |
| S10 | A token/cookie/web-approval được dùng tại B | Forbidden; không đổi tenant qua header/payload |
| S11 | Host A + key B, SNI mismatch và forged X-Tailnet-ID | Fail-closed, không phát key/map của B |
| S12 | MapRequest/Node update A trong lúc B query peers | B không nhận event hoặc delta từ A |
| S13 | Tenant A gửi pending enroll flood vượt quota | B vẫn đăng ký/MapStream theo SLO lab |
| S14 | Public `/debug`, `/metrics`, gRPC admin | Blocked; không leak node/IP/key |
| S15 | DERP verify unknown/revoked and A/B clients | Không client không hợp lệ được relay; không relay bypass auth |
| S16 | Shard restart cùng DB + identities | Client reconnect, không double registration/cross-map |
| S17 | Split brain, stale epoch, expired signed route manifest | Chỉ owner đúng epoch nhận write/registration |
| S18 | Delete/recreate tenant, revoke key | Old keys/nodes không thể hồi sinh trái phép |

**Test POC bắt buộc sớm**: S05, S10–S11 và S04 trước khi bắt đầu tối ưu performance. E2E phải dùng Tailscale client thật (pin phiên bản tối thiểu và ít nhất một phiên bản cập nhật). Noiseless unit mock không đủ.

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

Khi rename `cmd/hi` hoặc module, cập nhật lệnh trong Makefile và tài liệu ngay PR cùng thay đổi; các lệnh trên là **baseline**, không phải tuyên bố đã chạy trong môi trường hiện tại.

### Phân loại test

- Go unit: policy parsing, typed scope, uniqueness, auth claims, revision CAS, IPAM per tenant.
- `servertest`: handler/state mapping, register+poll, default-deny transaction, auth replay.
- Docker integration: two real tailscale clients/tailnet, multiple profiles, SSH/routing/DNS, DERP/exit node.
- Proxmox E2E: isolated underlay, real edge/TLS, two shards, faults, latency, signed manifests.
- Race/fuzz: MapResponse events + concurrent writes, blocked node update, malformed hostinfo/routes/keys.
- Security: debug/metrics route scan; API authorization matrix; sensitive logs redaction; cross-tenant grants.

## 3. E2E fixture

```yaml
# Khái niệm, agent phải biến thành test code
shards: [shard-1, shard-2]
tailnets:
  alpha: {shard: shard-1, nodes: [a1, a2], policy: {grants: []}}
  beta:  {shard: shard-1, nodes: [b1, b2], policy: {grants: []}}
  gamma: {shard: shard-2, nodes: [c1, c2], policy: {grants: []}}
intentional_collisions: [machine_key, hostname, overlay_ip, advertised_subnet, tag]
client_matrix: [pinned_stable, current_supported]
```

Sau khi tạo tailnet phải **không** có peer traffic cho đến khi policy được cập nhật rõ ràng. Test traffic cross tailnet cần phân biệt direct underlay với overlay: dùng tcpdump và firewall underlay.

## 4. Benchmark test regimes (không phải performance claim)

| Profile | Workload | Chỉ số |
|---|---|---|
| L1 | 10 tailnet × 10 node, idle | RAM idle per tailnet, open streams |
| L2 | 100 tailnet × 10 node, node join/leave | p95/p99 enrollment, map update |
| L3 | 10 tailnet × 100 node, endpoint churn | p95 map build, delta bytes, CPU |
| L4 | 1 tailnet × 1.000 node | netmap hot-spot, memory |
| L5 | 200 concurrent enroll trên tenant A | fairness, queue, tenant B latency |
| L6 | 1000 concurrent reconnects / 2 shards | handshake, error, DB lock |
| L7 | DERP fallback forced, increasing bandwidth | relay saturation, CPU, drop, latencies |
| L8 | Kill shard / partition registry / delayed placement cache | time to recovery, split brain prevention |

Các số lượng là **tập mục tiêu thử nghiệm theo cấp độ**, không phải năng lực chắc chắn đạt. Ngưỡng SLO phải được định nghĩa cùng nhóm vận hành sau baseline, trước signoff. Report bắt buộc lưu workload config, Git SHA, host vCPU/RAM/storage, test duration, Tailscale version, p50/p95/p99, RSS, CPU, SQLite busy, backlog, error and replay counts.

## 5. Artifacts + redaction

Mỗi run lưu:

```text
artifacts/<gitsha>/<testid>/
  inventory.txt       # chỉ test VMs
  environment.json    # không secrets
  commands.txt        # args đã redact
  test-result.json
  client-status.json  # node IDs/IP đã mask nếu chia sẻ
  map-response.json   # keys redacted, checksum preserved
  edge-routes.json
  metrics.prom
  pcap-metadata.txt   # không xuất full traffic thô ra ngoài lab
```

No-Go nếu test S01–S18 chưa có bằng chứng (test not run ≠ pass), hoặc bất kỳ leak security nào chưa giải quyết.
