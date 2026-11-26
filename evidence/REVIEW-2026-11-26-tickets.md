# Review bảo mật các ticket đã merge — 2026-11-26

- **Commit được review:** `master` `dece058` (sau Gate N1 và Gate N2).
- **Lý do:** Gate N1 và N2 được chủ dự án duyệt ở mức gate (hành vi đầu cuối). Các ticket đã merge (trừ P09, P12b) vẫn `Verified`, chưa `Reviewed`. PLAN §10 yêu cầu reviewer kiểm invariant bảo mật trong code của các ticket rủi ro cao. Chủ dự án chọn: review bảo mật các ticket này, sửa trong P12c trước P13 (2026-11-26).
- **Cách làm:** 5 reviewer độc lập (agent), mỗi người một vùng; chỉ đọc code, ADR, threat model; **không** đọc `evidence/` (để không bám theo kết luận cũ); không sửa file; review tĩnh (không có Go trên máy review). Coordinator (agent chính) kiểm lại từng phát hiện quan trọng trong code trước khi ghi.
- **Mức độ:** BLOCKER = đọc/ghi xuyên tenant, fail-open, owner kép hoặc ghi bởi process đã bị fence khai thác được; HIGH; MEDIUM; LOW. "Tiềm ẩn" = code hiện tại không có đường gọi tới, nhưng một dòng code mới hợp lệ sẽ biến nó thành BLOCKER.
- **Kết luận:** **không có BLOCKER**. Không có đường xuyên tenant, owner kép hay ghi bởi process đã bị fence nào khai thác được với code hiện tại. Có 4 lỗ HIGH tiềm ẩn ở plugin scope (P03b), 1 HIGH về DoS xuyên tenant ở `/api/v2`, 3 HIGH về tính sẵn sàng của lease, và các MEDIUM. Sửa trong `tickets/P12c.md`.

| Reviewer | Phạm vi | Ticket |
|---|---|---|
| R1 | Schema v0.1, plugin `tenantscope`, kiểm soát SQL thô | P03b |
| R2 | Gắn tenant cho phiên Noise, runtime theo tailnet | P04, P05 |
| R3 | Netmap/delta, policy, DNS, routes, netmap tăng dần | P00d, P06, P07 |
| R4 | Mọi endpoint công khai/quản trị/operator, bootstrap | P08, P08b, P11 |
| R5 | Placement, fencing owner/epoch, quota, lease, failover, hand-over, move | P10, P10b–d, P11b, P12, P12b |

Cột "Kiểm lại": **đã** = coordinator đọc lại code và xác nhận; **hợp lý** = suy từ code và hành vi thư viện/PostgreSQL, chưa chạy.

## Nhóm A — cô lập dữ liệu và policy (sửa trước, có test RED)

| ID | Mức | Ticket | Vị trí | Vấn đề và kịch bản | Kiểm lại |
|---|---|---|---|---|---|
| A1 | HIGH tiềm ẩn | P03b | `hscontrol/db/tenantscope/tenantscope.go:128` (cả `:151` cho UPDATE/DELETE) | `scopeWhere` thêm `"<table>"."tailnet_id" = ?` bằng `AddClause(clause.Where{...})`, GORM nối vào danh sách WHERE có sẵn mà không bọc ngoặc. Dưới scope A, `Where("hostname = ?", h).Or("given_name = ?", h).Find(&nodes)` thành `hostname=? OR given_name=? AND tailnet_id=A`: trả node của mọi tenant có hostname h. Với `Delete` thì xóa được node của B. Code hiện tại không dùng `.Or()`/`.Not()` | đã (cách chèn WHERE; không có lời gọi `.Or(`) |
| A2 | HIGH tiềm ẩn | P03b | `tenantscope.go:83-93` (`tenantField` nil khi `Statement.Schema` nil), thoát sớm ở `:119`, `:141`, `:183`, `:312` | `db.Table("nodes")…Find(&dto)`, `db.Table("credentials").Create(map…)`, `Updates(map)` qua `Table`, `Model(&Tailnet{}).Joins("JOIN nodes …")` đều **không bị scope và không bị fence**, không báo lỗi. Guard SQL thô không bắt các đường này. Code hiện tại: `Table()` chỉ trên `migrations`, `Joins` chỉ association `AuthKey` | đã (thoát sớm; các lời gọi hiện có) |
| A3 | HIGH tiềm ẩn | P03b | `tenantscope.go:135-152` (`scopeMutation` chỉ thêm WHERE); schema không chặn đổi `tailnet_id` | Dưới scope A, `Model(&Credential{ID:k}).Update("tailnet_id", B)` qua được fence (chỉ kiểm A); key của A từ đó xác thực vào B (`TailnetForAPIKey`, `TailnetForAuthKey` lấy tailnet từ dòng). Tương tự với node có tag, policy, `Save`/`Updates(map)`. Code hiện tại không gán `TailnetID`/`"tailnet_id"` | hợp lý |
| A4 | HIGH | P03b | `hscontrol/db/raw_write_guard_test.go:20-21`, `:87-102` | Guard chỉ xét câu **ghi**, chỉ khi SQL là **chuỗi hằng viết thẳng**, chỉ `Exec`/`Raw`/`ExecContext`, allowlist theo **file**. Không bắt: `Raw("SELECT … FROM nodes …")` (đọc xuyên tenant), SQL trong const/biến/ghép chuỗi/`Sprintf`, `INSERT OR REPLACE`, `REPLACE INTO`, `UPDATE ONLY`, `public.nodes`, `MERGE`, `TRUNCATE`, `Query*`; câu ghi mới trong file đã có trong allowlist | đã |
| A5 | MEDIUM | P03b | `schema_postgres.sql:2-3` nhắc `schema_test.go` không tồn tại; `tenantscope_test.go:105-135`, `suite_test.go:54-57`, `.github/workflows/ci.yml` | Không có test tự động: mọi bảng tenant `tailnet_id NOT NULL`, schema SQLite ≡ PostgreSQL (bảng, cột, unique, FK). Test plugin/trigger trên PostgreSQL bỏ qua khi không có DSN; CI không đặt DSN nên có lẽ không chạy. So tay: hai schema tương đương, chỉ khác tên FK `tailnet_moves` | đã (file không có) |
| A6 | MEDIUM | P06/P08b | `hscontrol/state/state.go:672-684` (`if pol != nil`), `db/policy.go:92-94`, `cluster.go:446-456`, `cmd/headscale/cli/cluster.go:206-239` | Operator lưu policy rỗng (thu hồi tenant): reconcile đọc `nil`, **không cài gì**, runtime vẫn áp policy cũ (có thể rộng); revision vẫn ghi là đã áp. Khởi động lại thì thành deny. Vi phạm AGENTS rule 8 trong một tenant. CLI lưu policy không validate: policy hỏng làm tenant không khởi động được ở lần failover/move sau (fail closed nhưng là outage tiềm ẩn) | đã |
| A7 | MEDIUM tiềm ẩn | P03b | `hscontrol/db/tenant.go:311-314` | `ScopedContext` trả lại context của caller khi `WithScope` lỗi (context đã gắn B): câu đọc trên handle A dùng context đó đọc được dòng của B. Chưa có lời gọi ngoài test | hợp lý |
| A8 | MEDIUM | P03b | schema: chỉ `credentials` có trigger tenant theo `user_id` | Không có gì chặn `UPDATE users/nodes/policies SET tailnet_id = …` ở DB (nửa DB của A3) | hợp lý |

## Nhóm B — tính sẵn sàng, lease, tài nguyên

| ID | Mức | Ticket | Vị trí | Vấn đề và kịch bản | Kiểm lại |
|---|---|---|---|---|---|
| B1 | HIGH | P11b | `hscontrol/db/tenantscope/tenantscope.go:385` (`checkLease` … `FOR SHARE`), `internal/lease/lease.go:153` (`Renew` = UPDATE), `manager.go:249` | Mọi câu ghi có fence của mọi tailnet trong cluster lấy `FOR SHARE` trên **một** dòng `cluster_leases`. Chính comment `tenantscope.go:277-283` và benchmark P10c ghi: `FOR SHARE` mới nhập nhóm đang giữ mà không xếp hàng, UPDATE bị bỏ đói (26–136 s với 8 writer). Dòng `tailnets` đã được vá bằng advisory lock công bằng, dòng lease **chưa**. Dưới tải ghi đều, `Renew` có thể chờ quá hạn self-fence → active tự fence, cả cluster sang standby, lặp lại | đã (code và comment); hành vi PG hợp lý, chưa có test dưới tải |
| B2 | HIGH | P11b / lab | `tenantscope.go:337`, `:385`; `internal/lease/lease.go:136-145` (`Acquire` `ON CONFLICT DO UPDATE`); `infra/lab/db/setup-db.sh` (không có timeout) | Active bị SIGSTOP hoặc mất mạng giữa câu có fence và COMMIT giữ `FOR SHARE` trên lease, `FOR SHARE` trên tailnets và advisory lock (SIGSTOP: mãi mãi; mất mạng: tới TCP keepalive ~2 h). `Acquire` của standby chờ khóa dòng, timeout, thử lại; `HandOver`/`TakeOver`/`MoveOwner` gặp `lock_timeout`. Cluster đứng dù lease đã hết hạn. ADR 0006 §4 đòi `query_wait_timeout` < hạn self-fence; lab chưa đặt `idle_transaction_timeout`, `query_wait_timeout`, `idle_in_transaction_session_timeout` | đã (config không có) |
| B3 | HIGH | P11b | `internal/lease/manager.go:150-165` (`onAcquire` chạy xong mới `hold()`); `cluster.go:999-1045` (`onLeaseAcquired`: `waitStopping` ≤ 35 s rồi nạp mọi tailnet) | Hạn self-fence = lúc lấy + TTL − margin (8 s mặc định). Nạp tailnet lâu hơn 8 s (nhiều tailnet, DB chậm, đang drain) → `hold` thấy quá hạn ngay, retire tất cả, chờ lease hết, lấy lại, nạp lại, lặp. Cluster không bao giờ phục vụ | đã |
| B4 | HIGH | P08b | `hscontrol/cluster_apiv2.go:132` (bucket toàn cục), `:174` (bucket key, tailnet) | Bucket toàn cục (200/s) bị trừ **trước** khi xác thực key và trước bucket theo key/tailnet. Tenant A spam bằng key hợp lệ (hoặc token rác, không có giới hạn theo IP) làm cạn bucket toàn cục, request đầu của B nhận `429 global_limit`. Vi phạm I10 | đã |
| B5 | MEDIUM | P05 | `hscontrol/cluster.go:337` (`load` đưa runtime vào `c.runtimes`), `:486` (`start` sau đó), `app.go:1668` (`servable` không kiểm đã start) | Trong khoảng giữa `load` và `start`, request register/map tới runtime có `mapBatcher` nil → panic (500), node đã ghi DB không được báo cho peer; long-poll panic hai lần, bỏ qua `Disconnect`, node kẹt online. Data race trên `h.mapBatcher` | đã |
| B6 | MEDIUM | P05 | `hscontrol/noise.go:231-234`, `session_route.go:57`, `:76` | `netutil.NewOneConnListener`: `Accept` lần hai trả `io.EOF` (đã đọc `tailscale.com/net/netutil/netutil.go:67-78`), nên `Serve` trả về ngay và `unbind()` chạy trước khi phiên được gắn. Phiên gắn sau đó ở lại `h.sessions` tới khi runtime retire: bộ nhớ tăng theo số lần reconnect | đã |
| B7 | MEDIUM | P11 | `internal/bootstrap/http.go:137`; `cmd/mesh-bootstrap/main.go` (`-ip-rate` mặc định 0) | Flood không xác thực > 200 req/s vào `POST /v1/bootstrap` làm mọi tenant nhận 429. Bucket theo tenant chỉ trừ sau xác thực (đúng); điểm yếu là bucket toàn cục. Forwarder TCP che IP client | hợp lý; ghi nhận, xử lý theo P13/P14 (proxy protocol hoặc L7) |

## Nhóm C — chuyển tenant, quota

| ID | Mức | Ticket | Vị trí | Vấn đề và kịch bản | Kiểm lại |
|---|---|---|---|---|---|
| C1 | MEDIUM | P12 | `cluster.go:645` (`handingOver`), `:664-672`; `db/handover.go:83-100`; hủy move không tăng epoch (`placement/tailnets.go:389+`, `moves.go:231-246`) | Move yêu cầu ở epoch E; nguồn retire, goroutine G chờ drain (≤ 30 s). Operator hủy → tick sau nạp lại runtime ở E. Operator yêu cầu lại → tick sau bỏ qua vì `handingOver[id]` còn true. G xong, `HandOver(id, E)` thành công trong khi runtime mới của nguồn vẫn gửi netmap từ bộ nhớ tới reconcile kế (~2 s). Ghi vẫn bị fence; chỉ phá R11 trong bộ nhớ | hợp lý |
| C2 | MEDIUM | P12b | `internal/placement/moves.go:304-308` (`MoveOwner` và `setMoveState` hai transaction), `:231` | Scheduler commit `moving_to = B`; operator hủy move đang SCHEDULED (không xóa `moving_to`) → CANCELLED; `setMoveState` của scheduler lỗi; nguồn vẫn trao tay cho B. Lịch ghi CANCELLED, client bị chuyển, không còn thông báo | hợp lý |
| C3 | MEDIUM | P10 | `placement/tailnets.go:181` (`admits`), `clusters.go:424`, `quota.go:132`; `handover.go` không kiểm lại sức chứa | Kiểm sức chứa và cluster dedicated chỉ đếm tailnet đang sở hữu, bỏ qua move đang tới; enrollment và move khóa khác nhau. Hai move vào cluster dedicated D cùng được nhận → D có hai tenant; nhiều move vào C → vượt `max_devices` sau trao tay | hợp lý |

## LOW / ghi nhận (chuyển P13/P14 hoặc tài liệu)

| ID | Ticket | Vấn đề |
|---|---|---|
| L1 | P08b | `/health` công khai, không xác thực, ping DB mỗi lần gọi (`cluster.go:949`): khuếch đại tải lên pool/PgBouncer dùng chung |
| L2 | P08b | `/ready` công khai trả `lease_epoch` (`cluster.go:973`): lộ số lần và thời điểm failover |
| L3 | P11 | Handle chỉ đọc SQLite: `PRAGMA query_only` chạy một lần trên một kết nối (`readonly.go:61`), mất sau khi kết nối nhàn rỗi bị thay (chỉ SQLite) |
| L4 | P08b | Label metric theo tailnet cấp trước khi kiểm tailnet được phục vụ (`cluster_apiv2.go:170`) và không bao giờ trả lại: tiêu dần trần 2000 |
| L5 | P10 | Không có thu hồi chứng chỉ operator (CRL/OCSP) (`placementapi/auth.go:125`) |
| L6 | P11b | Self-fence dựa trên `CLOCK_MONOTONIC` (đứng khi VM bị pause) và giờ DB (bước NTP) |
| L7 | P12b | Kết quả move ghi sai trong vài thứ tự (`RunDueMoves`); `updated_at` bị đổi làm reset tuổi R10; `CancelScheduledMove` REQUESTED không kiểm epoch, bỏ qua `RowsAffected`; hai process mở DB cùng lúc chạy migration mới thì một bên thoát |
| L8 | P04/P05 | "Đúng một `(machine_key, node_key)`" áp theo tailnet, không theo dòng node (cố ý, ADR 0007); quyền sở hữu kiểm lại mỗi 2 s, không mỗi request; đăng ký web chưa có (fail closed); kiểm SNI phụ thuộc nơi terminate TLS |
| L9 | P06/P08b | Policy reload lỗi vẫn ghi revision đã áp (chỉ có log) |
| L10 | P03b | `hasPrimaryKey` coi slice khác rỗng là có khóa: `Delete(&[]Node{{}})` xóa mọi node của tenant (trong một tenant); `stampOne` với `[]map` panic; `.After` chuỗi chỉ giữ cái cuối; `createDefaultTailnet` tạo tailnet ACTIVE không qua transaction PENDING + `{"grants":[]}` |

**Đã chấp nhận trước đó (không đổi):** P12 P08 một frame sau SIGCONT (R5: không tệ hơn mô tả; khe check-then-act không đóng được ở user space), giới hạn B3, phương án (a), cửa sổ nạp ở cluster đích + retry.

## Invariant reviewer xác nhận giữ đúng (tóm tắt)

- **P03b:** không scope thì không có SQL trên mọi callback GORM của model có `TailnetID`; vị từ có tên bảng; Create đóng dấu/từ chối `tailnet_id`; upsert, Preload bị giới hạn; mọi ghi tenant có fence trong transaction; `tailnet_id NOT NULL` + FK trên mọi bảng tenant, FK kép node→user/credential, trigger credential→user; SQL thô hiện có đều có scope; migration chỉ nối cuối, trong transaction.
- **P04/P05:** register lấy tenant chỉ từ credential; map từ `(mk, nk)` khớp đúng một tailnet; phiên gắn một lần, đổi tenant → 403 + đóng kết nối; host guard 421; không có state toàn process theo node/IP/key; standby không mở phiên Noise.
- **P00d/P06/P07:** MapResponse/delta/event chỉ từ runtime của tailnet; policy rỗng = deny (bật cứng trong cluster mode), lỗi parse không mở; `*`/autogroup/tag chỉ trong tailnet; DNS theo tailnet; routes/exit node theo tailnet; netmap tăng dần mặc định OFF, thu hồi luôn tới đích, đối chiếu định kỳ.
- **P08/P08b/P11:** không debug/metrics/node data trên listener công khai; `/api/v2` scope theo key, ghi policy/DNS bị chặn, OAuth bị từ chối trong cluster mode; `/verify` constant-time, 403 giống nhau, chỉ Allow khi giữ lease; bootstrap từ chối giống từng byte, 503 chỉ sau xác thực; operator API mTLS + allowlist SAN, chặn token tenant; không log secret.
- **P10–P12b:** fence owner/epoch/ACTIVE/lease trong cùng transaction; lease CAS trên giờ DB, epoch tăng ngặt có trigger; hai lần kiểm `servable()` trong `writeMap`; nguồn retire và chờ drain trước khi trao tay; `HandOver`/`TakeOver` là CAS; quota enrollment chính xác; thứ tự khóa không có chu trình; chỉ `SET LOCAL` + `pg_advisory_xact_lock`; reconcile chỉ đọc.

## Theo dõi

Sửa: `tickets/P12c.md`. Sau P12c, từng ticket được ký `Reviewed` theo bảng ở evidence P12c.
