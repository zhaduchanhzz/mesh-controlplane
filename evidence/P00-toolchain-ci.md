# P00 — Toolchain gate (Nix) và bản nháp CI tier 1

- **Trạng thái:** toolchain gate `Verified` trên `mc-ci` (xem bảng kết quả). CI `Planned` + `Implemented` dạng bản nháp (`.github/workflows/ci.yml`), **chưa chạy trên GitHub**, nên chưa `Verified`. Chưa `Reviewed`.
- **Nhánh:** `agent/toolchain-ci` (worktree `wt-toolchain-ci`), chưa commit.
- **Commit được kiểm tra:** `4f8af3fc9dcbc724910904fbb1007da9f153921f` (HEAD của `feat/P01-module-tailnet-types` lúc tạo worktree; code Go giống `2558340` + các commit lab/docs của P00).
- **Ngày:** 2026-11-22
- **Phạm vi thay đổi:** chỉ `.github/**` và file này. Không đổi code Go, không đổi `flake.nix`/`flake.lock`/`Makefile`.

## 1. Môi trường

| Mục | Giá trị |
|---|---|
| Máy | `mc-ci` (VMID 7701, 192.168.1.240), Ubuntu 24.04.5, kernel 6.8.0-147, 8 vCPU, 23 GiB RAM, `/` 116 G, Docker 29.1.3 |
| Tải dùng chung | Trong lúc đo, một agent khác cũng biên dịch/chạy test trong `~/src/p01` trên cùng VM, và container `ts-home1` đang chạy. Thời gian dưới đây vì thế là **cận trên**, không phải số đo sạch |
| Nix | Determinate Systems `nix-installer` **v3.24.2** (`nix-installer-x86_64-linux`, SHA-256 `22ed14ee1b24d100d388f1fc245e3062bd5982433d159cffa6dec34c0eb15dab`, khớp digest GitHub ghi cho release asset; `sha256sum -c` OK). Cài bằng `sudo ./nix-installer install linux --no-confirm`, exit 0, 10 s. Kết quả: `nix (Determinate Nix 3.24.2) 2.35.3` |
| Nguồn | `git bundle create` từ worktree → `scp` → `git clone` vào thư mục mới `~/src/p00-toolchain`. `git status` sạch; 0 file chứa CR |
| Không đụng | `~/src/mesh-controlplane`, `~/baseline*`, container `ts-home1`, container `p02-*`, các VM khác, Proxmox |

### Công cụ trong `nix develop` (theo `flake.lock`)

| Công cụ | Phiên bản | Store path |
|---|---|---|
| go | 1.27.4 (`nixpkgs-stable` `go_1_27`) | `/nix/store/nh17h6gh…-go-1.27.4` |
| golangci-lint | 2.15.0 (build với go1.27.3) | `/nix/store/0hbvnysb…-golangci-lint-2.15.0` |
| tscli | 0.3.0 | `/nix/store/cd1n7xvw…-tscli-0.3.0` |
| OpenTofu | 1.13.5 | `/nix/store/w0vw790c…-opentofu-1.13.5` |
| gofumpt | 0.12.0 (go1.27.4) | |
| gcc (cho `-race`) | 16.2.0 wrapper | |

Lưu ý: OpenTofu trong dev shell là **1.13.5**, khác bản 1.13.9 đã cài tay vào `~/.local/bin` cho baseline trước. Trong `nix develop`, bản Nix đứng trước trên `PATH`.

`nix eval --inputs-from . nixpkgs#{tscli,opentofu,golangci-lint}.outPath` cho ra **đúng cùng store path** với bản trong dev shell (đã so sánh). Đây là cơ sở cho thiết kế CI ở mục 4.

## 2. Lệnh, exit code, thời gian

Mọi lệnh chạy bằng `nix develop -c bash -c '<lệnh>'` trong `~/src/p00-toolchain`, tuần tự, log ở `~/p00tc-logs/` trên `mc-ci`.

| # | Lệnh | Exit | Thời gian | Ghi chú |
|---|---|---|---|---|
| 0 | `nix develop -c …` (lần đầu, store lạnh) | 0 | 698 s | Tải ~220 path từ cache.nixos.org. **Build từ source** 5 gói do overlay ép build với Go 1.27: `gotools-0.52.0` (kèm checkPhase chạy test, chiếm phần lớn thời gian), `gofumpt-0.12.0`, `gotestsum-1.13.0`, `gotests-1.9.0`, `golangci-lint-langserver-0.0.12`; cộng 2 script nhỏ |
| 1 | `make build` | **0** | 96 s | `check-deps` qua (golangci-lint, gofumpt, mdformat, prettier có trên `PATH`). Binary `headscale` version `4f8af3f` |
| 2 | `make lint` (`golangci-lint run --timeout 10m`) | **0** | 157 s | `0 issues.` |
| 3 | `go run ./cmd/hi doctor` | **0** | 36 s | Mọi check pass. Kéo image `golang:1.27.4` và `ghcr.io/k3s-io/k3s:v1.35.5-k3s1`. 1 cảnh báo: không có Docker Hub credentials (pull ẩn danh, giới hạn 100/6h). Không cấu hình credential (đúng quy tắc không đưa secret) |
| 4 | `make test` (= `CGO_ENABLED=1 go test -race ./...`, timeout mặc định 10 phút) | **2** | 788 s | 33 package ok. `hscontrol/servertest`: `panic: test timed out after 10m0s` (đang chạy `TestLogoutReloginWithPollChurn`). Không có `--- FAIL` hay `DATA RACE` trước khi timeout |
| 5 | `CGO_ENABLED=1 go test -race -timeout 90m ./...` (kèm `-json` để lấy thời gian từng test) | **đang chạy** | — | Bắt đầu ~11:00Z; kết quả ở `~/p00tc-logs/summary.txt` và `race-full.json` trên `mc-ci`. **Chưa có kết quả lúc bàn giao** |

## 3. Test fail và nguyên nhân

- `make test` fail chỉ vì `Makefile` không đặt `-timeout`: `servertest` dưới `-race` cần hơn 10 phút (không race đã mất 2246 s). Đây là lỗi cấu hình target, không phải lỗi logic. Không sửa `Makefile` trong P00 (ngoài phạm vi).
- `TestAPIv2/TSCLI` (fail ở baseline cũ vì thiếu `tscli`): trong `nix develop` đã có `tscli` 0.3.0; xác nhận pass phụ thuộc kết quả lệnh #5 (chưa có).

## 4. Thiết kế CI tier 1 (`.github/workflows/ci.yml`)

Trigger: `pull_request` và `push` vào `master`, `feat/**`. `permissions: contents: read` ở mức workflow, không job nào nâng quyền. Không dùng secret nào (kể cả `GITHUB_TOKEN` ngoài quyền đọc mặc định). `actions/checkout` với `persist-credentials: false`. Mọi action pin theo commit SHA đầy đủ:

| Action | Tag | SHA |
|---|---|---|
| `actions/checkout` | v7.0.1 | `3d3c42e5aac5ba805825da76410c181273ba90b1` |
| `actions/setup-go` | v7.0.0 | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` |

(SHA lấy bằng `git ls-remote`, tag đã dereference. Không chọn `setup-go` v7.1.0 vì mới phát hành 5 ngày.)

Nix không cài qua action mà qua `.github/scripts/install-nix.sh`: tải đúng `nix-installer` v3.24.2 và `sha256sum -c` với checksum ghi trong workflow, tắt diagnostics (`NIX_INSTALLER_DIAGNOSTIC_ENDPOINT=""`), không đăng nhập FlakeHub.

### Các job

| Job | Nội dung | Nguồn công cụ | Timeout |
|---|---|---|---|
| `build` | `go build ./...`, build `cmd/headscale` với cùng cờ như `make build` (`-buildmode=pie`, `-X main.version`), `headscale version`, `go vet ./...` | `setup-go` (go.mod → 1.27.4) | 20 phút |
| `test-race` | `go test -race -count=1 -timeout 40m` mọi package **trừ** `hscontrol/servertest` | `setup-go` | 45 phút |
| `servertest` (matrix 3 shard) | `go test -count=1 ./hscontrol/servertest/...` **không** `-race`, chia theo tên test | Go từ `setup-go`; `tscli` + `tofu` từ `nix shell --inputs-from . nixpkgs#tscli nixpkgs#opentofu` | 60 phút |
| `lint` | `golangci-lint run --timeout 20m` | `nix shell --inputs-from . nixpkgs#golangci-lint` (2.15.0, đúng bản của flake) | 30 phút |

### Vì sao chọn cách này

**Câu hỏi:** cài pin `tofu`/`tscli` thủ công, hay dùng Nix?

- **Chọn Nix, nhưng chỉ `nix shell --inputs-from .` cho từng công cụ, không `nix develop`.**
  - Phiên bản lấy thẳng từ `flake.lock` (nixpkgs `f6bf1059…`). Khi nâng `flake.lock`, CI tự theo; không có danh sách version thứ hai phải đồng bộ tay. Đã kiểm tra store path trùng với dev shell.
  - Ba gói này có sẵn trên cache.nixos.org (không build). Trên `mc-ci`, `nix shell` cho cả ba khi đã có store: 2 s.
  - `nix develop` trên runner lạnh tốn ~12 phút (đo được 698 s) vì overlay build lại `gotools` (cả test), `gofumpt`, `gotestsum`… CI tier 1 không cần các gói này. Không có binary cache riêng (không dùng FlakeHub/Cachix vì cần secret/token), nên mỗi lần chạy đều phải build lại.
- **Go từ `actions/setup-go`, không từ Nix:** cùng 1.27.4 (go.mod và `go_1_27` của flake), có cache module/build sẵn của action, nhanh hơn. `GOTOOLCHAIN=local` để không tự tải toolchain khác.
- **Không dùng `golangci-lint-action`:** bản chính thức tải binary release từ GitHub, có thể khác bản nixpkgs (2.15.0 build với go1.27.3). Dùng đúng bản của flake để kết quả lint giống `make lint` trên máy dev.
- **Không dùng `DeterminateSystems/nix-installer-action`:** action mặc định `determinate: true` sẽ cố đăng nhập FlakeHub (cần `id-token: write`); chỉ ghim được tag installer (`source-tag`) mà không có checksum (release không kèm file checksum, `source-checksums-url` cần file đó). Script tự viết ngắn, ghim cả version lẫn SHA-256.
- **`servertest` tách khỏi `-race`:** giống thiết kế upstream (comment trong `flake.nix`: check `gotest`/`gorace` loại `hscontrol/servertest`, servertest chạy workflow riêng không `-race`). Lần chạy không race trước đó mất 2246 s trên 8 vCPU; runner `ubuntu-latest` chỉ 4 vCPU nên chia 3 shard theo tên test.

TBD (shard theo số đo)

### Thời gian dự kiến trên GitHub

TBD

## 5. Khác biệt còn lại so với CI upstream

TBD

## 6. Việc còn mở

TBD

## Kết quả `-race` đầy đủ (bổ sung bởi agent chính, 2026-11-22)

- Lệnh: `CGO_ENABLED=1 go test -race -timeout 90m -json ./...` trong `nix develop` trên mc-ci, commit `4f8af3f` (trước khi đổi module path). Bắt đầu khoảng 11:05Z, kết thúc 11:44Z (sự kiện cuối: `servertest` pass).
- **34/34 package pass, 0 package fail, 0 báo cáo `WARNING: DATA RACE`.** `TestAPIv2/TSCLI` pass vì `tscli` có trong dev shell.
- Ghi chú: một tác vụ chờ của agent chính bị kẹt vì `pgrep -f` khớp nhầm thread kernel `rcu_tasks_trace`. Đã dừng và đọc kết quả từ `race-full.json`.
- **Kết luận N0 toolchain:** `make build` 0, `make lint` 0, `hi doctor` 0, `-race` 34/34 pass. `make test` chỉ vượt timeout mặc định 10 phút ở `servertest`; CI dùng `-timeout` riêng.
