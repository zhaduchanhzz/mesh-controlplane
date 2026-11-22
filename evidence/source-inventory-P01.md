# Source Inventory — P01 (module rename, `internal/tailnet`)

- **Trạng thái:** `Verified` (full suite Linux, §5). Review: agent tự review theo tiêu chí merge chủ repo đặt ra 2026-11-22. Chưa có reviewer bảo mật là người.
- **Nhánh:** `feat/P01-module-tailnet-types` từ `master` @ `aef014f`
- **Ticket:** `tickets/P01.md`

## 1. Quyết định

| Mục | Giá trị | Nguồn |
|---|---|---|
| Module path | `github.com/zhaduchanhzz/mesh-controlplane` | Khớp repo `origin`. Chủ repo chưa phản đối; agent chọn mặc định 2026-11-22 |
| Tên binary | **Giữ `headscale`** (lệch so với ticket) | `integration/*` và `cmd/hi` gọi tên binary ở rất nhiều chỗ; đổi trong P01 dễ làm tăng test fail (điều kiện dừng). Tách thành ticket riêng |
| `internal/tailnet` | `ID` (UUID canonical, zero không hợp lệ), `Scope`, `Source`, `Epoch`, `WithScope`, `FromContext`, `Require` | `05-data-api-contract.md` §1, ADR 0001 |

## 2. Path/symbol đã đọc

| Path | Ghi chú |
|---|---|
| `go.mod:1` | module cũ `github.com/juanfont/headscale` |
| 338 file `*.go` | import string có module path |
| `.golangci.yaml:62` | message nhắc import path `zf` |
| `.golangci.yaml:121-124` | gci/gofmt/gofumpt/goimports mặc định, không có prefix riêng → chỉ cần gofmt sắp xếp lại |
| `Makefile:42` | `-ldflags "-X main.version=$(VERSION)" -o headscale ./cmd/headscale`: không phụ thuộc module path |
| `gen/client/v1|v2/client.gen.go` | **không** chứa module path → không cần `make generate` |
| `hscontrol/scope/scope.go` | OAuth scope API v2; không import `internal/tailnet` (có test) |
| `hscontrol/api/v2/api.go:292` | `requireDefaultTailnet`: chỉ ghi nhận, sửa ở P08 |

## 3. Commit

| Commit | Nội dung | Kiểm chứng |
|---|---|---|
| `4f8af3f` | lab: VM `mc-bench` (7703), `.gitattributes` ép LF cho script | plan 2 create, gate exit 0, apply exit 0 |
| `46e6c21` | Đổi module path | 341 file, +821/−821. 0 dòng thay đổi không chứa module path. URL upstream 135 → 135. Còn sót 0. gofmt: 266 file sắp lại import, sau đó 0. `go build ./...` OK |
| (commit này) | `internal/tailnet` + test + evidence | §4 |

## 4. Test `internal/tailnet` (RED → GREEN)

- **RED:** `go test ./internal/tailnet/` trước khi viết code → exit 1 (`undefined: MustParseID`, `Scope`…).
- **GREEN:** exit 0, 11/11 test pass:
  - ID: rỗng, nil UUID, chữ hoa, thiếu gạch, sai độ dài, không phải hex, khoảng trắng, `-`, slug, `../` đều bị từ chối; ID canonical được nhận; zero ID không marshal được; JSON round-trip có validate.
  - Scope: zero ID, source không hợp lệ, principal rỗng → `ErrInvalidScope`; zero `Scope` không `Valid`.
  - Context: `FromContext` trên context rỗng hoặc nil → `ErrNoScope` (fail closed); rebind A→B → `ErrScopeConflict` (I01); A→A idempotent; `Require(B)` khi ctx là A → `ErrScopeMismatch`; `Require(zero)` → mismatch.
  - Import: package chỉ được import `context`, `errors`, `fmt` (không `net/http`, không `hscontrol/*`); `hscontrol/scope` không import `internal/tailnet`.
- **Mutation check** (đã khôi phục sau khi kiểm):
  - Thêm file import `net/http` → `TestTailnetPackageImportsAreMinimal` FAIL.
  - Bỏ kiểm tra conflict trong `WithScope` → `TestWithScopeCannotSwitchTailnet` FAIL.
- `go vet ./internal/tailnet/` exit 0. `gofmt -l internal/` rỗng.

## 5. Full suite trên Linux

Chạy trên `mc-ci` từ `git bundle` @ `4c2cda8` (0 file CRLF), Go 1.27.4 linux/amd64. Lúc chạy, máy đang chia tải với job nix/race của agent khác.

| Lệnh | Exit | Thời gian | Kết quả |
|---|---|---|---|
| `go build ./...` | 0 | 28 s | |
| `go test -count=1 -timeout 120m ./...` | 1 | 2199 s | **34 ok** (baseline 33 + `internal/tailnet`), **1 FAIL**: `servertest` `TestAPIv2/TSCLI` (thiếu `tscli`, giống baseline P00) |

**Kết luận:** không tăng test fail so với baseline. Trong `nix develop` (agent toolchain), `make build` và `make lint` exit 0 trên cùng mã nguồn.

## 6. Giả định và giới hạn

- `internal/tailnet` chưa được nối vào handler, state hay DB. P02 sẽ nối vào `/key` và Noise, P05 nối vào state.
- Go không có cách chặn code khác trong module gọi `NewScope` với dữ liệu chưa xác thực. Ràng buộc nằm ở review, cộng với test import (không có kiểu HTTP trong package).
- AGENTS rule 1 vẫn là "không tự merge". Agent đã thử cập nhật rule theo chỉ đạo của chủ repo, nhưng bị harness chặn (nới lỏng cơ chế giám sát). Chủ repo cần tự sửa hoặc cấp quyền.
