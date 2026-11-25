# Source Inventory — P12b (chuyển tenant có kế hoạch giữa hai cluster)

- **Commit:** `master` `fc339e8`, ngày 2026-11-26.
- **Nhánh:** `feat/P12b-inventory`.
- **Trạng thái:** `Planned`. Chưa có code. Chạy song song với N1 theo duyệt của chủ dự án ngày 2026-11-26; chỉ merge sau khi N1 đạt.

## 1. Đã đọc

| Nguồn | Nội dung liên quan |
|---|---|
| `tickets/P12b.md` | Bản trước ADR 0010: `status=MOVING` cộng **hai lần** tăng epoch, drain theo đợt, operator API `BeginMove/AbortMove` |
| `PLAN.md:288`, §7 N2 | P12b là điều kiện cuối của N2 ("tenant move P12b không có owner kép"); benchmark B3/L11 |
| `design/adr/0010-tailnet-serving-claim.md` | Trao tay: `MoveOwner` sang cluster khác chỉ đặt `moving_to_cluster_id`; nguồn retire runtime rồi `HandOver` (owner = đích, **epoch + 1 một lần**); nguồn chết thì đích `TakeOver`; `CancelMove`; R10. Dòng 108: "P12b dùng lại nguyên cơ chế, chỉ thêm thông báo, re-bootstrap và đổi ControlURL" |
| ADR 0005 (Bổ sung, Bổ sung 2), ADR 0003 | Mỗi cluster một Noise key; không có hostname ổn định theo tenant; client đổi ControlURL và đăng nhập lại |
| `internal/bootstrap/bootstrap.go:150–196` | `Resolve`: key hợp lệ và `MovingTo != nil` → `OutcomeMoving` (trước nhánh URL); sau trao tay → URL của đích |
| `internal/bootstrap/http.go:35, 183, 216` | `MOVING` = 503 `{"error":"tenant is moving; retry later"}`, `Retry-After: 30` |
| `hscontrol/cluster.go` | `handOverRequested`/`handOver`/`takeOverOrphans`; `retire` → `closeSessions`; `runtimeFor` (`:730–744`): tailnet biết nhưng không phục vụ ở đây → `ErrUnknownTenant`; `:772`: register với `OldNodeKey` tra tailnet theo `(machine_key, node_key)` |
| `hscontrol/session_route.go:185–200` | Từ chối tenant → **401 `unauthorized`**; đổi tenant trong session → 403 và đóng kết nối |
| `hscontrol/noise.go:878–892` | Node bị xóa → 404 trần; "client từ 1.106 coi đó là node bị gỡ và chuyển `NeedsLogin`" |
| `hscontrol/db/handover.go` | `HandOver`/`TakeOver`: CAS dưới `LockOwnership` cùng lease sống |
| `internal/placement/tailnets.go` | `MoveOwner` (yêu cầu chuyển; kiểm lease và sức chứa của đích từ P12 bước 5), `CancelMove` |
| `infra/lab/e2e/p12/run.sh` (ca P10) | Chuyển y c1 → c2: bootstrap 200 → 503 → 200, c1 trao tay trước khi c2 nạp, client đăng nhập lại bằng `tailscale up --login-server <URL2> --force-reauth` |
| `evidence/E2E-two-cluster.md` N07, `artifacts/15dd9f3/two-cluster/*/logs/N07-relogin.txt` | Sau khi chuyển và đăng nhập lại ở đích: **NodeID 1 → 1, IP 100.64.0.1 → 100.64.0.1** (cùng machine key), không tạo node trùng |
| `evidence/P12-placement-reconcile.md` | R11 = 0 sau ADR 0010; thời gian nạp ở đích 1,9–4 s; bất thường P08 (S17, đang đo trong P09) |
| `tailscale.com@v1.105.0-pre…/cmd/tailscale/cli/up.go:441–445` | Đổi `--login-server` khi đang `Running` **bắt buộc `--force-reauth`** |
| `…/control/controlclient/direct.go:1205–1225`, `errors.go` | Map khác 200 → lỗi rồi retry với backoff; 404 → `ErrNodeRemoved`; 401 không đưa client sang trạng thái nào đặc biệt |
| `…/tailcfg/tailcfg.go:2156, 2172` | MapResponse có `Health []string` và `DisplayMessages` (capability version ≥ 117, 2025-05-28). Headscale hiện chưa gửi hai trường này (`hscontrol/mapper`) |
| `evidence/P14-db-benchmarks.md` §B3 | CAS một dòng 2–5 ms so với ghi lại mọi dòng 70–619 ms (lần đo 2026-11-22, câu SQL mô phỏng, chưa phải code thật) |

## 2. Hiện trạng so với ticket

| Mục ticket (bản cũ) | Hiện trạng sau P12 / ADR 0010 |
|---|---|
| `MOVING` + epoch tăng hai lần; thêm `MOVING` vào CHECK của `status` | **Không còn cần.** `moving_to_cluster_id` là cờ "đang chuyển"; trao tay tăng epoch đúng một lần. `status` vẫn `ACTIVE`; bootstrap đã trả `MOVING` dựa trên `moving_to` |
| Fence nguồn | Đã có: fence owner/epoch/lease ở mọi lần ghi (P10b/P11b); nguồn retire runtime trước khi trao tay |
| Không owner kép | Đã có: R11 = 0 (ADR 0010, servertest và E2E P12) |
| Bootstrap `MOVING` → URL đích | Đã có (P11, P12) |
| Client cũ tới nguồn bị từ chối | Đã có: 401 (`runtimeFor`); E2E P12 ca P10: key cũ bị từ chối ở c1 |
| NodeID/UserID giữ nguyên, không node trùng | Đã có bằng chứng (N07: cùng NodeID và IP) |
| Abort | `CancelMove` trước khi trao tay; sau khi trao tay chỉ đi tiếp được |
| **Thông báo trước** (tenant admin, client) | **Chưa có.** Không có lịch chuyển, không có thời điểm hẹn, không có event hay API cho tenant, chưa dùng `DisplayMessages` |
| **Re-bootstrap và đăng nhập lại có kiểm soát; đo thời gian X% client** | **Chưa có.** Client v1.104.7 và v1.84.3 **không tự chuyển**: bị 401 thì retry mãi. Phải có người hoặc công cụ chạy `tailscale up --login-server <mới> --force-reauth` |
| Drain theo đợt | Nguồn đóng mọi stream của tenant cùng lúc khi retire. Client không tự re-bootstrap, nên không có storm tự phát vào bootstrap hay đích; storm chỉ có khi công cụ của tenant chạy đồng loạt |
| Pending web/OIDC/SSH auth | Cluster mode tắt đăng ký web/OIDC. State SSH-check nằm trong runtime, mất khi retire, client làm lại ở đích |
| Restore backup không lùi epoch (I14, F08) | Thuộc N3/P13 |
| Runbook và hướng dẫn theo loại client | Chưa có |
| B3/L11 | Đã đo lần đầu bằng SQL mô phỏng; chưa đo đường code thật (`MoveOwner` + `HandOver`) |

## 3. Ràng buộc

- Không đổi Noise; mỗi cluster một key (ADR 0005, AGENTS 12). Đổi ControlURL nghĩa là đăng nhập lại; chủ dự án đã chấp nhận.
- Giữ nguyên cơ chế trao tay của ADR 0010: không có hai owner, kể cả trong bộ nhớ.
- Migration nếu có chỉ thêm cuối chuỗi v0.1, chạy trong transaction, không dùng FK hay `AutoMigrate` (AGENTS 11).
- API cho tenant phải nhận tailnet từ context đã xác thực và fail closed (AGENTS 6). Không lộ cluster hay tenant khác.
- Lỗi isolation hoặc owner kép = BLOCK MERGE (AGENTS 13).

## 4. Giả định chưa kiểm

- Client v1.84.3 (capability version của nó cần đối chiếu) và v1.104.7 hiển thị `DisplayMessages` trong `tailscale status`. Nếu không, dùng `Health []string`.
- Đăng nhập lại ở đích bằng **pre-auth key cũ** (reusable, chưa hết hạn) dùng lại node theo machine key, như N07. Key không reusable đã dùng thì cần key mới do tenant tạo.
- Kéo dài thời gian từ lúc thông báo tới lúc trao tay (hàng phút, hàng giờ) không ảnh hưởng gì khác: trong khoảng đó tailnet vẫn `ACTIVE` ở nguồn.

## 5. Câu hỏi cho chủ dự án

Xem `tickets/P12b.md` §Câu hỏi (Q1–Q7, mỗi câu có đề xuất).
