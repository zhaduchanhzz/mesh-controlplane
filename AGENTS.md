# Coding Agent Policy — Mesh Control Plane

Đọc `README.md`, `PLAN.md` và tài liệu liên quan trước mỗi PR. **Không** tự suy luận Headscale đã hỗ trợ multi-tailnet hoặc coi nhiều user là nhiều tailnet.

## Tuyệt đối phải tuân thủ

1. Không làm việc trên `main`; tạo nhánh `feat/Pxx-short-name` từ commit upstream baseline đã pin; push PR, **không tự merge**.
2. Không ghi secrets, service token, Proxmox credentials, SSH private key vào Git, logs, artifact, screenshot. Dùng test credentials ngắn hạn; output đã redact.
3. Không cho phép agent `tofu apply/destroy`, xóa VM, sửa firewall thật, reset DB có dữ liệu, expose API metrics, deploy Internet public nếu chưa có người xác nhận rõ phạm vi và diff. Có thể chạy `tofu fmt/validate/plan` và trình bản plan.
4. Chỉ bật privilege Proxmox tại pool lab và trong phiên thao tác có duyệt; hạn chế SSH. Tránh `insecure = true` và không cài token root `root@pam` vào repo.
5. Mỗi PR phải bắt đầu bằng một file `evidence/source-inventory-Pxx.md`: commit hash, danh sách path/symbol đã đọc, constraints hiện có, những điểm còn giả định.
6. Mỗi API truy cập dữ liệu node phải nhận verified `TailnetScope`/`TailnetID` từ authenticated context. Nếu chưa có scope, **fail closed**. Không có API unscoped ngoài các thao tác quản trị nền tảng được bảo vệ riêng.
7. Khi thêm trường `tailnet_id`, kiểm tra schema, unique constraints, FK, partial indexes, caches, events, map deltas, admin/API, DERP verify và background jobs. Không chỉ thêm `WHERE` ở tầng DB.
8. Không cho policy `*` vượt tailnet. `{"grants":[]}` và PENDING phải cùng transaction khi tạo tailnet. Không fallback sang allow-all vì parsing policy lỗi.
9. Tất cả endpoint debug hoặc metric lộ node/IP/keys phải bị từ chối trên public listener. Metrics quản trị chạy listener riêng hạn chế truy cập.
10. Không sửa generated `gen/` trực tiếp: sửa `proto/` rồi chạy generator được pin theo upstream.
11. Không reorder migration upstream. Không chạy test destructively trên dữ liệu không thuộc lab.
12. Không tự ý đổi thiết kế Noise multi-identity nếu client không tương thích; ghi ADR, log protocol đã redact và dừng PR đến khi được duyệt.
13. Lỗi isolation = **BLOCK MERGE** bất kể unit tests khác pass.

## Chu trình bắt buộc cho mỗi ticket

```
READ source+docs → Inventory symbols → ADR/contract → RED security test
→ Small implementation → Unit/race → Integration real client
→ Benchmark (nếu hot path) → Evidence + PR → Human review
```

Báo cáo bằng chứng theo mẫu:

```markdown
# PR Pxx Evidence
- Repo commit before / after:
- Verified source paths & symbols:
- Design choice / ADR:
- Tests + exact commands + exit codes:
- E2E artifacts / protocol map redacted:
- Negative tenant isolation tests:
- DB migrations and downgrade constraints:
- Benchmark before/after (nếu hot path):
- Known limitations / security blockers:
- Reviewer approval:
```

## Nghiệm thu không được tuyên bố sai

- `Not started` — chưa có code; `Planned` — có hợp đồng; `Implemented` — code đã đổi; `Verified` — đã chạy test và cung cấp artifact; `Reviewed` — người duyệt đã xác nhận.
- Go race unit tests **không** chứng minh Noise, relay hoặc client profile security. Cần E2E thực.
- Proxmox VM isolation là underlay security test aid, không thay thế logical tenant isolation trong process.
- Docker build smoke test không đồng nghĩa với production readiness.
