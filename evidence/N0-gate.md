# Gate N0 — Upstream Verified

- **Ngày:** 2026-11-26.
- **Trạng thái:** `Verified`. Chờ chủ dự án nghiệm thu.
- **Định nghĩa:** `PLAN.md` §7 (N0).

| Điều kiện N0 | Bằng chứng | Kết quả |
|---|---|---|
| Commit snapshot, `PROVENANCE.md`, nhật ký cherry-pick | `PROVENANCE.md`: nguồn, SHA upstream không khôi phục được, nhật ký cherry-pick (chưa có bản vá nào) | Có (file tạo ngày 2026-11-26; P00 đã lên kế hoạch nhưng chưa tạo) |
| Baseline build, unit test, integration smoke pass trên Linux | CI GitHub trên `master` `44e523a`, [lượt 45687997498](https://github.com/zhaduchanhzz/mesh-controlplane/actions/runs/45687997498): build, lint (golangci-lint ghim theo `flake.lock`), test race (không gồm servertest), servertest (apiv2, rest, scale-ha) | **6/6 job xanh** |
| | Toolchain trên `mc-ci` (Linux): `evidence/P00-toolchain-ci.md` | Verified (P00) |
| P00b có số đo baseline | `evidence/P00b-capacity-baseline.md` | Có |
| P00c có số đo server-only | `evidence/source-inventory-P00c.md`; PLAN §9 | Có. Evidence của P00c tự ghi `Implemented`, chưa `Reviewed`. |
| Test lab không chứa production secret | Secret chỉ nằm ngoài repo (`%USERPROFILE%\.mesh-lab`). Trước mỗi lần push đều quét: không có key hay mật khẩu thật. Artifact E2E được quét (ca S99/P12/N09). | Đạt |

## Việc đã làm để CI xanh

Nhánh `feat/CI-green`, merge `44e523a`. Chi tiết trong `evidence/source-inventory-CI-green.md`.

- **golangci-lint:** 271 lỗi → 0.
  - Không tắt linter nào; không đổi `.golangci.yaml`.
  - 41 chỗ `//nolint` theo từng dòng, mỗi chỗ có lý do.
- **`TestPolicyChanges/multiple_policy_changes`:** là **lỗi của test**. Hai vòng áp dụng cùng một policy; `SetPolicy` coi đó là không có thay đổi nên đúng ra không gửi gì. Test nay dùng hai policy khác nhau, và kiểm thêm rằng áp lại đúng nội dung cũ thì không bị tính là thay đổi.

## Giới hạn

- CI đã đỏ liên tục từ trước P11b tới 2026-11-25. Các ticket đã merge trong khoảng đó không qua cổng CI, chỉ qua bộ test trên mc-ci.
- Chưa có người duyệt (`Reviewed`).

## Kiểm tra lại (2026-11-26, sau khi merge P12c `f868e22`)

| Điều kiện | Kiểm lại | Kết quả |
|---|---|---|
| Snapshot, `PROVENANCE.md`, nhật ký cherry-pick | `PROVENANCE.md` có trên `master` `f868e22`; chưa cherry-pick bản vá upstream nào kể từ P00 | Không đổi, đạt |
| Build, unit, integration smoke trên Linux | CI GitHub: `master` `f868e22` **success**; `feat/P12d-subnet-api` `06c9f7b` **success** | Đạt (CI còn xanh sau P12c) |
| P00b, P00c có số đo | `evidence/P00b-capacity-baseline.md`, `evidence/source-inventory-P00c.md` | Có; P00c vẫn `Implemented` |
| Lab không chứa production secret | Quét lại `git grep` (khóa riêng, token PVE, `tskey-`/`hskey-`, AWS, GitHub) trên `master`, `feat/P12d-subnet-api`, `feat/P12d-exitnode`, `feat/P13-integration`, `feat/P14a-capload`: chỉ có (1) fixture upstream `hscontrol/db/testdata/sqlite/headscale_0.29.3_dump.sql` (khóa tạo bởi binary v0.29.3 trong DB thử, không dùng ở đâu), (2) biến `$TOKEN` trong ví dụ `LAB00-operator.md`, (3) khóa giả `hskey-auth-AAAA…` trong `infra/lab/e2e/p05/run.sh`. `artifacts/` bị `.gitignore` | Đạt |

N0 vẫn `Verified`, chờ chủ dự án nghiệm thu (`Reviewed`).
