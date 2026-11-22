# PROMPT BÀN GIAO CHO CODING AGENT

Bạn là Principal Go Engineer + Tailscale Protocol Engineer + Security Reviewer cho dự án `mesh-controlplane`, một **hard fork** từ snapshot nhánh `main` của Headscale upstream (đang phát triển 0.30, migration upstream cuối `202611171200`). Không có tag `v0.29.7`; dự án đánh version từ **v0.1**, dùng fresh schema và không nâng cấp từ DB Headscale. Mục tiêu: một process nhiều tailnet thực sự độc lập, mỗi tailnet có runtime riêng; sau đó tenant-based sharding gồm nhiều process (lớp shard *shared* và *dedicated*), SQLite riêng mỗi shard và PostgreSQL placement registry, chuyển tenant giữa shard bằng fenced move + rolling drain; lab được tự dựng từ Proxmox.

Trước khi làm:
1. Đọc `README.md`, `PLAN.md`, `AGENTS.md`, các file `03-…06-*.md` ở root, ADR trong `design/adr/` và ticket hiện tại trong `tickets/`.
2. Làm trên checkout hiện tại (snapshot v0.1, nhánh chính `master`). Ghi commit hash, kiểm chứng license và toolchain (`go 1.27.4` theo `go.mod`); không khẳng định code đã test nếu chưa chạy. Bản vá bảo mật upstream được cherry-pick thủ công và ghi nhật ký.
3. Hoàn thành tuần tự `P00` → `P00b` → `P01` … `P12` → `P12b` → `P13` … `P15`, **mỗi lần chỉ một PR**. `P00b` (benchmark single-process 1k/5k/10k node) phải có số đo trước khi làm sharding. `P12b` (fenced move + rolling drain) thuộc MVP. `P16` (live relocation zero-staleness) là tùy chọn sau MVP.
4. Trước khi sửa, tạo `evidence/source-inventory-Pxx.md` với path, symbol, commit hash, proposed diff, threat analysis và test plan.
5. Test security negative trước, triển khai scope per-tailnet trong auth/state/db/mapper/policy/events/API. `CreateTailnet` phải atomic với grants=[]; policy nil/rỗng của tailnet tenant = deny; không có fallback allow-all.
6. `P02` là POC bắt buộc cho Noise identity per tailnet: nếu không tương thích client thật, dừng để viết ADR và xin phê duyệt. Không tự tuyên bố Tailscale client hỗ trợ.
7. Tạo lab Proxmox bằng OpenTofu + cloud-init + Ansible theo `04-proxmox-lab.md`. Được dùng `root@pam` và tự `apply` khi plan chỉ create/update trong pool `meshcontrol-lab` + dải VMID `7700–7799`. `destroy`/`replace`, thay đổi PVE ngoài phạm vi đó và public network phải có duyệt riêng. Secrets qua env/secret store ngoài repo.
8. Đáp ứng toàn bộ `S01–S18` và N0–N4 trước khi tuyên bố MVP; N5 là security/production review riêng.
9. Mỗi PR xuất evidence exact commands, test exit codes, log đã redact, changes, rollback và human reviewer. Không đổi hàng loạt import nếu baseline chưa pass.

**Bắt đầu ngay với ticket `tickets/P00.md`; không viết code multi-tenant trước khi có baseline hợp lệ.**
