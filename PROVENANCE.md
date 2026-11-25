# Provenance — mesh-controlplane

Dự án là **hard fork** của Headscale, đánh version từ **v0.1**. Quyết định của chủ dự án ngày 2026-11-22: bỏ tag `v0.29.7`; v0.1 không nâng cấp từ DB Headscale mà mở chuỗi migration mới.

## Nguồn

| Mục | Giá trị | Nguồn kiểm chứng |
|---|---|---|
| Upstream | `juanfont/headscale`, nhánh `main`, đang phát triển 0.30 | Commit gốc `2558340`: `hscontrol/db/db.go:108` ("0.30 development") |
| Migration upstream cuối cùng trong snapshot | `202611171200-node-key-expiry-disabled` | `2558340`: `hscontrol/db/db.go:377` |
| Phiên bản docs upstream | `0.29.6` | `mkdocs.yml:116` |
| Commit đầu của repo | `2558340` "first commit": snapshot squash, không có lịch sử upstream | `git log` |
| **SHA upstream chính xác** | **Không khôi phục được** từ repo (snapshot squash) | `evidence/source-inventory-P00.md` §1 |
| Go | `go 1.27.4` | `go.mod` |
| `tailscale.com` | `v1.105.0-pre.0.20261112091541-a2da5fbcaa53` | `go.mod:90` |
| License | BSD-3-Clause, giữ nguyên | `LICENSE` |
| Module | `github.com/zhaduchanhzz/mesh-controlplane` (đổi tên ở P01) | `go.mod` |

## Nhật ký cherry-pick bản vá upstream

Vì không có SHA upstream chính xác, mọi bản vá (nhất là bản vá bảo mật) của `juanfont/headscale` phải cherry-pick thủ công và ghi vào bảng này.

| Ngày | Advisory / commit upstream | File bị ảnh hưởng | Commit trong fork | Test |
|---|---|---|---|---|
| — | Chưa có bản vá upstream nào được cherry-pick tính tới 2026-11-26. Kiểm bằng `git log --all -i --grep=cherry --grep=upstream`: không có commit cherry-pick nào. | | | |

**Quy trình khi cherry-pick:**
1. Một nhánh `fix/upstream-<id>`.
2. Ghi một dòng vào bảng trên.
3. Có test chứng minh lỗi trước khi sửa (RED), nếu tái hiện được.
4. Chạy CI.

Bản vá chạm vùng cách ly tenant phải theo AGENTS 13.

## Phần đã khác upstream

Fork đổi kiến trúc sang nhiều tailnet trên một cluster. Xem `PLAN.md`, `design/adr/0001`–`0010` và các ticket `P0x`–`P12`.

Các khác biệt lớn:
- Một runtime cho mỗi tailnet; một Noise key cho mỗi cluster.
- Schema v0.1 có `tailnet_id`.
- Placement, fence owner/epoch, lease cluster, trao tay tailnet.
- API v2 theo tenant.
- `/verify` cho DERP ngoài.
