# Source Inventory — P00 (rebaseline v0.1)

- **Trạng thái:** `Implemented` một phần (evidence + .gitignore). Chưa `Verified` trên Linux runner. Chưa `Reviewed`.
- **Nhánh:** `feat/P00-rebaseline` tạo từ `master` @ `2558340ef63d03fd2603c6b6dfb589ef1386cd69`
- **Ngày:** 2026-11-22

## 1. Baseline và provenance

| Mục | Giá trị đã xác minh | Nguồn |
|---|---|---|
| Commit repo | `2558340` "first commit" (squash, không có lịch sử upstream) | `git log` |
| Nguồn gốc | Snapshot `juanfont/headscale` nhánh `main`, đang phát triển 0.30 | `hscontrol/db/db.go:108` ("0.30 development"), `db.go:258` |
| Migration upstream cuối | `202611171200-node-key-expiry-disabled` | `hscontrol/db/db.go:377` |
| Phiên bản docs upstream | `0.29.6` | `mkdocs.yml:116` |
| Go | `go 1.27.4` (không có dòng `toolchain`) | `go.mod` |
| Tailscale dependency | `tailscale.com v1.105.0-pre.0.20261112091541-a2da5fbcaa53` | `go.mod:90` |
| License | BSD-3-Clause, giữ nguyên | `LICENSE` |

**Quyết định (người dùng, 2026-11-22):** bỏ tag `v0.29.7`, dùng snapshot này làm hard fork và đánh version dự án từ **v0.1**. v0.1 **không** nâng cấp từ DB Headscale: mở chuỗi migration mới, có `tailnet_id` ngay từ đầu.

**Hệ quả:** SHA upstream chính xác không khôi phục được từ repo. Bản vá bảo mật của upstream phải cherry-pick thủ công và ghi lại nhật ký.

## 2. Path và symbol đã đọc

| Path:line | Symbol | Ghi chú |
|---|---|---|
| `hscontrol/app.go:91-114` | `Headscale` | Singleton: `noisePrivateKey`, `state`, `mapBatcher`, `authProvider` |
| `hscontrol/app.go:151` | `readOrCreatePrivateKey` | Một Noise key cho cả process |
| `hscontrol/app.go:288-293` | `RegisterProtocol` | Sửa `http.DefaultTransport` toàn cục (DERP verify) |
| `hscontrol/app.go:514-582` | `createRouter` | Danh sách route public |
| `hscontrol/app.go:874` | `MetricsAddr` | Debug và metrics chạy trên listener riêng |
| `hscontrol/app.go:897` | TailSQL | Embedded node join qua memnet |
| `hscontrol/noise.go:78-129` | `noiseServer`, `NoiseUpgradeHandler` | Tạo per-connection; `AcceptHTTP` nhận private key theo từng lời gọi |
| `hscontrol/handlers.go:221-254` | `KeyHandler` | Trả về một key duy nhất |
| `hscontrol/handlers.go:166-189` | `handleVerifyRequest` | Tra node key toàn cục, vẫn admit node expired |
| `hscontrol/debug.go:30-45` | `protectedDebugHandler` | Cho qua mọi IP `IsPrivate()` |
| `hscontrol/state/state.go:214-300` | `State` | `nodeStore`, `ipAlloc`, `polMan`, `authCache`, `sshCheckAuth`, `pings`, `registerLocks` |
| `hscontrol/state/node_store.go:200-221` | `Snapshot` | `nodesByMachineKey` key theo user; `nodesByNodeKey`, `routes`, `allNodes` là global |
| `hscontrol/state/node_store.go:776-821` | `snapshotFromNodes` | Gọi `peersFunc(allNodes)` mỗi lần topology đổi |
| `hscontrol/policy/v2/policy.go:341` | `noRulesFilter` | `pol == nil` hoặc ACLs/Grants đều nil → allow-all |
| `hscontrol/policy/v2/policy.go:831-939` | `BuildPeerMap` | O(n²) theo cặp; bộ nhớ khoảng n²×8B khi allow-all |
| `hscontrol/policy/v2/filter.go:135` | `noRulesFilter` | → `allowAllPolicy()` |
| `hscontrol/policy/v2/types.go:2140-2141` | `Policy.ACLs/Grants` | Tag `omitempty`: marshal lại sẽ làm mất `[]` |
| `hscontrol/policy/v2/types.go:3427` | `unmarshalPolicy` | Input rỗng → `nil` → allow-all |
| `hscontrol/policy/v2/policy_test.go:85-98` | `TestPolicyManager` | `{}` = allow-all; `{"grants":[]}` = deny-all |
| `hscontrol/mapper/batcher.go:277-300, 786-802` | `Batcher`, `addToBatch` | Broadcast tới mọi node qua `b.nodes.Range` |
| `hscontrol/types/change/change.go:19-57` | `Change` | Không có trường tailnet |
| `hscontrol/types/node.go:92` | `NodeID.StableID` | StableID = NodeID dạng thập phân |
| `hscontrol/db/schema.sql:37-39, 69` | unique indexes | `users.name` và `provider_identifier` unique toàn cục; `credentials(kind, identifier)` |
| `hscontrol/api/v2/api.go:292` | `requireDefaultTailnet` | `{tailnet}` bắt buộc là `-` |
| `hscontrol/scope/scope.go` | `scope.Scope` | OAuth scope của API v2, **không** phải tailnet scope |
| `hscontrol/servertest/README.md` | `TestScale` | Control client thật + Noise in-memory; `HEADSCALE_STRESS_NODES` |
| `hscontrol/mapper/batcher_scale_bench_test.go:30-33` | scale counts | Benchmark tới 10k–50k |

## 3. Docs lệch so với code (cần sửa trong P00)

1. PLAN, HANDOFF, AGENTS ghi baseline `v0.29.7`. Thực tế là snapshot main 0.30-dev, nay chuyển thành v0.1.
2. Không có `tickets/` (HANDOFF yêu cầu bắt đầu ở `tickets/P00.md`).
3. PLAN trỏ tới `docs/04-proxmox-lab.md`, nhưng file nằm ở root. Không có file 01, 02.
4. AGENTS rule 10 nhắc tới `proto/`. Repo không có `proto/`; codegen hiện là Huma + OpenAPI (`cmd/gen-openapi`, `gen/client/v1|v2`).
5. AGENTS rule 11 ("không reorder migration upstream") cần viết lại, vì v0.1 mở chuỗi migration mới.
6. Hai tên cho file inventory: `evidence/source-inventory-Pxx.md` và `verified-symbols.md`.
7. Thiếu `CONTRIBUTING.md`, `AI_POLICY.md`, `CHANGELOG.md`, `.github/` (không có CI).
8. PLAN ghi Go 1.27.3; `go.mod` ghi 1.27.4.

## 4. Constraints hiện có

- `hscontrol/db/AGENTS.md`: không tắt FK, không AutoMigrate trong migration, migration chạy trong transaction.
- `integration/AGENTS.md`: ưu tiên `servertest` khi không cần Docker.
- Debug và metrics chỉ chạy trên `metrics_listen_addr`.

## 5. Giả định chưa kiểm chứng

- Tailscale client lấy `/key` theo control URL của từng profile và dùng chung machine key giữa các profile. **Phải chứng minh ở P02.**
- Bộ nhớ peer map ở 5k và 10k node chỉ là ước tính từ cấu trúc dữ liệu. **Phải đo ở P00b.**
- Test chạy trên Windows chưa đại diện cho Linux CI.

## 6. Lệnh đã chạy và exit code

| Lệnh | Môi trường | Exit | Ghi chú |
|---|---|---|---|
| `go version` | Windows 11, Go tự tải | 0 | `go1.27.4 windows/amd64` |
| `go build ./...` | Windows 11 | 0 | 142 s (gồm thời gian tải module) |
| `go test ./...` | Windows 11 | **1** | 639 s. 23 package ok, 11 package FAIL, 420 test fail. `hscontrol` và `servertest` panic `test timed out after 10m0s` (chưa chạy hết). Phần lớn lỗi do Windows: TempDir cleanup bị khóa file SQLite ("being used by another process"), đường dẫn `C:\` trong YAML gây lỗi escape `\U`, golden file so path. **Không dùng làm baseline**; phải chạy lại trên Linux (`mc-ci`). |
| `git ls-files -ci --exclude-standard` | sau khi sửa `.gitignore` | 0 | 0 file đang track bị ignore |

### Baseline Linux trên `mc-ci` (Ubuntu 24.04, 8 vCPU, 23 GiB, Go 1.27.4 linux/amd64)

| Lệnh | Exit | Kết quả |
|---|---|---|
| `go build ./...` | 0 | 100 s |
| `make build` | 2 | `check-deps`: thiếu `golangci-lint` (công cụ của `nix develop`). Lỗi công cụ, không phải lỗi code |
| `go test -timeout 30m ./...` (lần 1) | 1 | 31 ok. FAIL: `config`, `configgen` (golden file bị CRLF do copy cây thư mục từ Windows), `servertest` (timeout 30 phút; thiếu `tofu`) |
| `CGO_ENABLED=1 go test -race -timeout 45m ./...` (lần 1) | 1 | 31 ok. Cùng 3 package như trên; `servertest`: `exec: "tofu": executable file not found` |
| Sửa môi trường | — | `git -c core.autocrlf=false checkout -f -- .` (1953 → 0 file CRLF); cài OpenTofu 1.13.9 (SHA-256 OK) vào `~/.local/bin` |
| `go test -count=1 ./hscontrol/config/... ./tools/configgen/...` | 0 | Cả hai pass, nên lỗi là do CRLF |
| **`go test -count=1 -timeout 90m ./...` (baseline sạch)** | **1** | **33 ok, 1 FAIL**: `servertest` `TestAPIv2/TSCLI`: `exec: "tscli": executable file not found` ("provided by the nix dev shell"). Không có lỗi logic nào. 2246 s |

**Kết luận N0 (code):** với commit `2558340` trên Linux, mọi test pass, trừ những test cần công cụ của `nix develop` (`tscli`, `golangci-lint`). **Còn lại:** dùng `nix develop` (hoặc cài pin `tscli` + `golangci-lint` theo `flake.nix`) để chạy `make build`/`make test` đầy đủ, chạy lại `-race` sau khi đã sửa môi trường, và `go run ./cmd/hi doctor`.

**Bài học cho CI:** source phải lấy từ git (clone/archive), không copy cây thư mục từ Windows, vì CRLF làm fail test golden.

## 7. Thay đổi trong P00 (tới thời điểm này)

- `.gitignore`: thêm pattern cho state, tfvars, inventory, artifacts, key của lab. Giữ lại `.terraform.lock.hcl` và test fixture `*.env` của upstream.
- `evidence/source-inventory-P00.md`: file này.

## 7b. Lab track (LAB00–LAB04), cùng nhánh

Chủ lab quyết định (2026-11-22):
- Cho phép agent dùng `root@pam` và tự apply khi plan chỉ create/update trong pool `meshcontrol-lab` + VMID `7700–7799`.
- Cho phép tải cloud image vào `local:import` và bật `snippets` trên `local`.
- **Không** cho cấp lại cert PVE.

Đã cập nhật `AGENTS.md` rule 3–4, `04-proxmox-lab.md` §5/§6/§8, `PLAN.md` §9/§10, `HANDOFF_PROMPT.md`.

| Bước | Kết quả |
|---|---|
| Host | PVE 9.2.27, node `hanh`, 2× Xeon E5-2696 v3 (36 thread), 125.7 GiB RAM, `nvme01` lvmthin 1.86 TB, `local-lvm` 348 GB |
| Mạng | `vmbr0` (nic0, host lấy IP 192.168.1.14 qua **DHCP**), `lan10` (192.168.10.0/24, không có port vật lý). Gateway LAN 192.168.1.1. Các IP đang dùng: `.1–.17`. Lab dùng `.240–.249`. |
| TLS | Cert node: SAN có `hanh`, `hanh.promox`, `192.168.0.101` (IP cũ), **không** có 192.168.1.14. Đã lấy PVE root CA qua API (SHA-256 `7E:F5:89:…:8A:05`, hết hạn 2036), `openssl verify` OK. Dùng tên `hanh.promox` qua `/etc/hosts` trong WSL. |
| VM có sẵn ngoài pool (trước apply) | 100 OPNsense, 101 pandownloader, 102 VPN; không thuộc pool nào |
| Thay đổi host | `local` content: `iso,import,backup,vztmpl` → thêm `snippets` (PUT có digest). Tạo pool `meshcontrol-lab`. |
| Runner | WSL Ubuntu 24.04, OpenTofu 1.13.9 (SHA-256 OK), provider `bpg/proxmox` 0.123.1 |
| `tofu fmt -check` / `validate` | exit 0 / exit 0 |
| `tofu plan -out` + `scripts/check-plan.py` | 3 create, 0 change, 0 destroy; gate exit 0 |
| `tofu apply` lần 1 | exit 1: `/etc/hosts` của WSL bị ghi lại, mất dòng `hanh.promox`. State rỗng (không tạo gì). Đã sửa `scripts/tofu.sh`. |
| `tofu plan -out` lần 2 + gate | 3 create, 0 change, 0 destroy; gate exit 0 |
| `tofu apply lab.tfplan` lần 2 | **exit 0**: snippet (0 s), image `local:import/mc-ubuntu-2404-release-20261017.qcow2` (44 s, SHA-256 kiểm tra bởi PVE), VM 7701 (1 phút 26 s) |
| Kiểm tra `mc-ci` qua SSH (key `mesh-lab`, SHA256:Y1kP…FvV8) | Ubuntu 24.04.5, 8 vCPU, 23 GiB, `/` 116 G; `cloud-init status: done`; qemu-guest-agent + docker active (Docker 29.1.3); user `mesh` ∈ sudo, docker; `PasswordAuthentication no` |
| VM ngoài pool trước/sau | 100, 101, 102 giống hệt (diff rỗng) |

### Public test endpoint (2026-11-22, chủ lab yêu cầu và duyệt)

| Bước | Kết quả |
|---|---|
| `mc-cp` (VMID 7702, 192.168.1.241, 8 vCPU / 24 GiB) | plan 2 create, gate exit 0, apply exit 0; `mc-ci` no-op |
| Tunnel WireGuard `wg-mesh` VPS:51821 ↔ mc-cp | Handshake OK sau khi chủ lab duyệt thêm `udp dport 51821` và `tcp dport 8443` (toàn Internet) vào `/etc/meshguard/vps-firewall.nft` (có backup, nạp atomic, các luật khác giữ nguyên). Ping 0% loss, ~23 ms |
| VPS forward | `mesh-cp-proxy.socket` 116.118.3.46:8443 → 10.99.77.2:8443 |
| Firewall mc-cp | nftables input drop; chỉ 8443 từ wg-mesh, 22 từ 192.168.1.0/24 |
| Cloudflare | Token thứ hai (Edit zone DNS). Tạo A `cp` và `*.cp.gta5web.online` → 116.118.3.46, DNS only. Không sửa record có sẵn (MeshGuard `*.mesh`, CAA…) |
| Cert | Let's Encrypt DNS-01, SAN `cp.gta5web.online`, `*.cp.gta5web.online`, hết hạn 2027-02-20, `certbot.timer` enabled, có deploy hook |
| Control plane | Binary baseline build trên `mc-ci` (`CGO_ENABLED=0 -trimpath`, sha256 `dfbeeda3…`, commit 2558340+dirty). `headscale check all` OK. Policy file tường minh. Embedded DERP tắt. Dùng DERP map công khai của Tailscale (tạm thời; không có verify) |
| Kiểm tra public (TLS thật, không `-k`) | `/health` 200 (verify=0); `/key?v=116` trả Noise key; `/debug/`, `/metrics`, `/debug/nodestore` trên :8443 → 404; :9090 trên IP public → không kết nối được |
| Client thật | Tailscale 1.104.7. `vps-ext` (VPS, `--netfilter-mode=off`) = 100.64.0.1; `home-ci1` (container trên mc-ci, sau NAT Docker + NAT nhà) = 100.64.0.2 |
| Kết nối | `tailscale ping`: 3 gói đầu qua DERP(hkg) 77–255 ms, sau đó **direct** 22 ms (hai chiều). ICMP qua tailnet 4/4, avg 29 ms. Netcheck: UDP true, MappingVariesByDestIP false |

**Giới hạn:** đây là bản baseline **một tailnet**. Chưa chứng minh multi-tenant, Noise key theo tailnet hay isolation (P02 trở đi). Hostname lạ vẫn được phục vụ nếu TLS khớp wildcard, vì chọn tailnet theo `Host` là việc của P02.

## 8. Còn lại của P00

- Sửa các lệch docs ở mục 3, thêm `PROVENANCE.md`.
- Dựng CI tối thiểu (build, unit, lint), sau đó self-hosted runner trên `mc-ci`.
- Chạy baseline đầy đủ trên Linux và ghi lại các test đang fail sẵn.
