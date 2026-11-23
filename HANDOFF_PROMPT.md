# PROMPT BÀN GIAO CHO CODING AGENT

Bạn là Principal Go Engineer + Tailscale Protocol Engineer + Security Reviewer cho dự án `mesh-controlplane`, một **hard fork** từ snapshot nhánh `main` của Headscale upstream (đang phát triển 0.30, migration upstream cuối `202611171200`). Không có tag `v0.29.7`; dự án đánh version từ **v0.1**, dùng fresh schema và không nâng cấp từ DB Headscale.

**Kiến trúc (PLAN 2.0, ADR 0005 kể cả "Bổ sung 2"):**
- Một process phục vụ nhiều tailnet thực sự độc lập, mỗi tailnet có runtime riêng.
- Nhiều **cluster** dùng chung một **PostgreSQL**. Mỗi tailnet có đúng một cluster sở hữu (`owner_cluster_id`, `owner_epoch`) và không trải qua nhiều cluster. Lớp shard: *shared* (≤5.000 thiết bị/tenant, cảnh báo 80%) và *dedicated* (5k–10k).
- Client gọi **bootstrap** để nhận **URL trực tiếp của cluster** rồi kết nối thẳng. Không có hostname theo tenant.
- **Một Noise key cho mỗi cluster.** Tenant của phiên được gắn qua credential đăng ký và bản ghi node, không qua Host hay key.
- Mỗi cluster = **active + standby** cùng key; lease trong PostgreSQL; failover tự động, client không đăng nhập lại; active cũ bị fence bằng epoch.
- Cô lập **chỉ ở tầng ứng dụng** (plugin GORM fail-closed, kiểm owner/epoch trong transaction, review SQL thô + lint). Không RLS, không role DB theo cluster; rủi ro còn lại đã được chấp nhận (`03-isolation-threat-model.md` §8).
- Netmap tăng dần được phép, sau công tắc mặc định OFF, với biện pháp bảo vệ bắt buộc (P00d).
- Chuyển tenant giữa cluster (P12b) có thông báo trước, client re-bootstrap, đổi ControlURL và đăng nhập lại.
- Lab tự dựng từ Proxmox.

**Đã xong:** P00 (merge), P00b (`Verified`: một process ≈1.5k–2.5k thiết bị với policy thực tế, client trong cùng process), P01 (merge), P02 (POC key theo tenant `Verified`, thiết kế đã bị ADR 0005 thay, giữ làm hồ sơ), P03a (policy tenant rỗng = DENY, merge), P03b (schema v0.1 trên PostgreSQL dùng chung + plugin scope, merge `8896b11`, `Verified`), P04+P05 (cluster một process, runtime theo tailnet; merge `d0ad0f6`, `Verified` bằng servertest), P05 E2E client thật chế độ cluster 9/9 (merge `a316f78`, `evidence/P05-e2e-cluster.md`), P14 B2/B3 đã đo (`evidence/P14-db-benchmarks.md`). Trên `master` **chưa mục nào `Reviewed`**.
**Chưa lên `master`:** P10 (placement + fencing, `origin/feat/P10-placement-fencing`; `Verified` theo evidence của nhánh, gồm E2E client thật 13/13; chờ reviewer). P10 được làm **trước gate N1 theo quyết định của chủ dự án ngày 2026-11-23**, là ngoại lệ so với quy tắc "không làm P10+ trước khi N1 qua gate" ở mục 3 dưới đây.
**Đang làm:** P08 (endpoint hardening, `feat/P08-endpoint-hardening`, xếp chồng lên P10; các test RED của P08a là lỗ hổng còn mở, **chặn merge**), P00c (`feat/P00c-peermap-perf`: công việc ban đầu chỉ có trên VM lab mc-ci, đang cứu về nhánh này, còn một test fail, chưa có số đo server-only).

Trước khi làm:
1. Đọc `README.md`, `PLAN.md`, `AGENTS.md`, các file `03-…06-*.md` ở root, ADR trong `design/adr/` (ADR 0005 thắng khi mâu thuẫn với ADR 0001–0003) và ticket hiện tại trong `tickets/`.
2. Làm trên checkout hiện tại (nhánh chính `master`). Ghi commit hash, kiểm chứng license và toolchain (`go 1.27.4` theo `go.mod`); không khẳng định code đã test nếu chưa chạy. Bản vá bảo mật upstream được cherry-pick thủ công và ghi nhật ký.
3. Thứ tự còn lại, **mỗi lần chỉ một PR**: P00c → P03b (song song được với P00c) → P00d → P04 → P05 → P06 → P07 → P08 → P09 (gate N1) → P10 → P11 → P11b → P12 → P12b → P13 → P14 → P15. P16 là tùy chọn sau MVP. Không làm P10+ trước khi N1 qua gate và P00c có số đo.
4. Trước khi sửa, tạo `evidence/source-inventory-Pxx.md` với path, symbol, commit hash, proposed diff, threat analysis và test plan.
5. Test security negative trước. Mọi truy cập dữ liệu tenant đi qua scope fail-closed; mọi mutation kiểm owner + epoch (và lease cluster) trong cùng transaction. `CreateTailnet` atomic với grants=[] và owner; policy nil/rỗng của tailnet tenant = deny; không fallback allow-all. Mọi SQL thô mới phải review tay và vào allowlist.
6. Noise: chủ dự án đã chọn một key cho mỗi cluster (AGENTS rule 12 đã thỏa bằng quyết định tường minh). Không tự đổi thiết kế. Nếu client thật không tương thích (ví dụ trộn hai profile cùng ControlURL, hoặc phải đăng nhập lại sau failover cùng key): dừng, ADR kèm log protocol đã redact.
7. Tạo lab Proxmox bằng OpenTofu + cloud-init + Ansible theo `04-proxmox-lab.md` và PLAN §9 (PostgreSQL primary + replica, bootstrap, hai cluster active/standby, relay, clients). Được dùng `root@pam` và tự `apply` khi plan chỉ create/update trong pool `meshcontrol-lab` + dải VMID `7700–7799`. `destroy`/`replace`, thay đổi PVE ngoài phạm vi đó và public network phải có duyệt riêng. Secrets qua env/secret store ngoài repo.
8. Đáp ứng `S01–S18`, `F01–F08`, `M01–M06` (khi bật netmap tăng dần) và N0–N4 trước khi tuyên bố MVP; N5 là security/production review riêng.
9. Mỗi PR xuất evidence exact commands, test exit codes, log đã redact, changes, rollback và human reviewer. Lỗi isolation hoặc owner kép = BLOCK MERGE.

**Bắt đầu với ticket đang làm (P08 hoặc P00c); không bắt đầu ticket mới khi các ticket đang làm chưa được duyệt và merge.**
