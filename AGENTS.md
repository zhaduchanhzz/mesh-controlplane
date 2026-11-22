# Coding Agent Policy — Mesh Control Plane

Đọc `README.md`, `PLAN.md`, ticket hiện tại trong `tickets/`, ADR trong `design/adr/` và tài liệu liên quan trước mỗi PR. **Không** tự suy luận Headscale đã hỗ trợ multi-tailnet hoặc coi nhiều user là nhiều tailnet.

## Tuyệt đối phải tuân thủ

1. Không làm việc trực tiếp trên `master` (nhánh chính của repo; không có `main`). Tạo nhánh `feat/Pxx-short-name` từ `master`, bắt đầu từ commit baseline v0.1 đã ghi trong `evidence/source-inventory-P00.md`; push PR, **không tự merge**.
2. Không ghi secrets, service token, Proxmox credentials, SSH private key vào Git, logs, artifact, screenshot. Dùng test credentials ngắn hạn; output đã redact.
3. Không cho phép agent `tofu destroy`, xóa VM, sửa firewall thật, reset DB có dữ liệu, expose API metrics, deploy Internet public nếu chưa có người xác nhận rõ phạm vi và diff. Agent **được tự `tofu apply`** một plan đã lưu (`tofu plan -out`) khi plan đó chỉ **create/update** VM trong pool `meshcontrol-lab` và dải VMID `7700–7799`. Plan có `destroy`, `replace`, hoặc chạm tới bất cứ thứ gì ngoài phạm vi đó thì phải trình người duyệt. Luôn chạy `tofu fmt/validate/plan` và lưu tóm tắt plan vào evidence.
4. Chủ lab cho phép agent dùng `root@pam` trên Proxmox lab (quyết định ngày 2026-11-22), nhưng chỉ để thao tác trong pool `meshcontrol-lab` và dải VMID lab. Không đụng tới VM, storage, network, firewall, user hay ACL ngoài phạm vi ticket LAB. Credential root chỉ nằm trong file env ngoài repo (`%USERPROFILE%\.mesh-lab\proxmox.env`) và không bao giờ được in ra log, output, artifact hay screenshot. Tránh `insecure = true`; dùng certificate PVE đã pin.
5. Mỗi PR phải bắt đầu bằng một file `evidence/source-inventory-Pxx.md`: commit hash, danh sách path/symbol đã đọc, constraints hiện có, những điểm còn giả định.
6. Mỗi API truy cập dữ liệu node phải nhận verified `TailnetScope`/`TailnetID` từ authenticated context. Nếu chưa có scope, **fail closed**. Không có API unscoped ngoài các thao tác quản trị nền tảng được bảo vệ riêng.
7. Khi thêm trường `tailnet_id`, kiểm tra schema, unique constraints, FK, partial indexes, caches, events, map deltas, admin/API, DERP verify và background jobs. Không chỉ thêm `WHERE` ở tầng DB.
8. Không cho policy `*` vượt tailnet. `{"grants":[]}` và PENDING phải cùng transaction khi tạo tailnet. Với tailnet tenant, policy nil/rỗng/`{}` = DENY (upstream coi là allow-all). Không fallback sang allow-all vì parsing policy lỗi.
9. Tất cả endpoint debug hoặc metric lộ node/IP/keys phải bị từ chối trên public listener. Metrics quản trị chạy listener riêng hạn chế truy cập.
10. Không sửa tay bất kỳ file nào trong `gen/`. Repo **không có** `proto/`: API dùng Huma + OpenAPI. Sửa định nghĩa API trong code Go, rồi chạy generator đã pin: `make openapi` (`cmd/gen-openapi`) để xuất spec, `make generate` để sinh code và client `gen/client/v1/client.gen.go`, `gen/client/v2/client.gen.go`. Commit code sinh ra cùng thay đổi nguồn.
11. v0.1 mở **chuỗi migration mới** (fresh schema, không nâng cấp DB Headscale). Không sửa, không reorder migration của fork đã được commit; chỉ thêm vào cuối. Theo `hscontrol/db/AGENTS.md`: không disable FK, không `AutoMigrate` trong migration, migration chạy trong transaction. Không chạy test destructively trên dữ liệu không thuộc lab.
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
