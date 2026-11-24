# ADR 0008 — DNS theo tailnet trong process nhiều tailnet (P06)

- **Trạng thái:** Accepted (chủ dự án duyệt 2026-11-24, gồm Q-a–Q-c ở cuối). Viết theo quyết định cùng ngày trong `evidence/source-inventory-P06.md` §4a: base domain theo tailnet, DNS theo tailnet làm ngay trong P06.
- **Liên quan:** ADR 0005 (một cluster nhiều tenant, một hostname), ADR 0007 (runtime theo tailnet), P10 (`policy_revision`, reconcile), P03b (`tenantscope`).

## Bối cảnh

Ở cluster mode, mọi runtime dùng chung **một** `*types.Config` (`Cluster.cfg`). Phần DNS gửi cho client (base domain, MagicDNS, nameservers, search domains, split DNS, extra_records) đều đọc từ đối tượng dùng chung đó (`mapper/mapper.go:103`, `types/config.go:1323`). Inventory P06 tìm ra ba hậu quả:

1. **Tên chứng chỉ trùng giữa các tenant.** `CertDomain` = `given_name + base_domain dùng chung` (`types/node.go:939`). Given name chỉ unique trong một tailnet, nên node `phone` của B có thể đặt ACME challenge và lấy chứng chỉ cho đúng tên `phone.<base>` của A (I02, I15).
2. **Data race.** `newHeadscale` gọi `cfg.StartDNS` cho mỗi runtime (`app.go:236`). Nó gán lại con trỏ `cfg.dns` mà không khóa, trong khi mapper của các runtime khác đang đọc; runtime nạp nóng bởi `reconcile` cũng làm vậy. Ngoài ra nó đặt lại extra_records về giá trị trong file.
3. **Tenant không có DNS riêng.** Không tenant nào đặt được nameserver, search domain hay record cho mạng của mình. N runtime cùng theo dõi một file `extra_records_path`.

## Quyết định

### 1. Base domain theo tailnet, do hệ thống đặt
- Ở cluster mode, base domain của tailnet có slug `s` là **`s.<dns.base_domain của cluster>`**. Tên MagicDNS của node là `<given_name>.<s>.<base>`.
- Tenant **không** đặt được base domain. Một tên do tenant tự chọn có thể trùng hoặc che tên của tenant khác, và cần một bước xác minh quyền sở hữu mà hệ thống chưa có.
- Slug phải là một nhãn DNS hợp lệ: chữ thường, chữ số và `-`, dài 1–63 ký tự, không bắt đầu hay kết thúc bằng `-`. **Hiện `CreateTailnet` (`db/platform_admin.go:28`) chỉ kiểm slug khác rỗng**; P06 thêm kiểm tra này khi tạo tailnet. Migration P06 kiểm cả các slug đã có và báo lỗi nếu có slug sai, không âm thầm sửa.
- **Slug bất biến** sau khi tạo. Hiện không có API nào đổi slug; P06 thêm một test để giữ điều đó. Nếu sau này cho đổi slug, phải có ADR mới, vì đổi slug là đổi tên MagicDNS và tên chứng chỉ.
- `CertDomain`, `ValidateGivenName`, các tên giữ chỗ cho extra_records, `ExitNodeFilteredSet` và API v2 (`devices.go`) đều đọc base domain của **runtime**, không đọc của cluster. `dns.certificates.zone` của cluster phải bao base domain của cluster; khi đó nó tự bao mọi `s.<base>`.
- **Một tailnet (`headscale serve`)** giữ nguyên hành vi upstream: base domain là giá trị trong file config.

### 2. Cấu hình DNS theo tailnet, lưu trong DB
- **Tenant đặt được:** `magic_dns` (bật/tắt), `override_local_dns`, `nameservers.global`, `nameservers.split`, `search_domains`, `extra_records`. Đây là cùng các khóa và cùng kiểu như phần `dns:` của file config, để tái dùng bộ validate có sẵn.
- **Chỉ cluster đặt** (từ file config, áp chung): `base_domain` (gốc của §1), `certificates`, `challenge` (provider ACME), `extra_records_path`.
- **Lưu trữ, theo mô hình dữ liệu đã dự kiến trong PLAN §4:**
  - **Bảng tenant mới `tailnet_settings`:** `tailnet_id` là PK và FK tới `tailnets`, cột `dns_config` text (JSON đã validate, NULL = mặc định). Bảng có `tailnet_id NOT NULL`, nên đi qua plugin `tenantscope` như mọi bảng tenant. Bảng có thể nhận thêm thiết lập khác sau này.
  - **Revision:** dùng cột **`tailnets.config_revision` đã có sẵn** (`schema.sql:25`). Hiện cột luôn bằng 1 và chưa có code nào đọc. Mỗi lần ghi `tailnet_settings` thì tăng cột này trong cùng transaction, giống `policy_revision`.
  
  Migration được thêm vào **cuối** chuỗi, cập nhật cả `schema.sql` và `schema_postgres.sql`. Ghi đi qua đường admin platform có `LockOwnership`, cùng mô hình fence như ghi policy (P10c).
- **Mặc định khi `dns_config` là NULL:** MagicDNS bật, không có nameserver, search domain hay record. Phần DNS tenant-đặt-được của file config cluster **không** được dùng làm mặc định cho tenant. Một giá trị operator đặt cho cả cluster sẽ trở thành giá trị chung, mà đó lại đúng là thứ ADR này bỏ.
- **Từ chối khởi động (fail closed)** khi cluster config có `dns.extra_records`, `dns.extra_records_path`, `dns.nameservers` hoặc `dns.search_domains` khác rỗng. Giống OIDC và embedded DERP: một giá trị bị bỏ qua lặng lẽ còn nguy hiểm hơn một lỗi khi khởi động.

### 3. Validate: lỗi thì từ chối, không bao giờ rơi về mặc định
- Dùng lại bộ validate của `hscontrol/config` cho nameserver (IP hoặc URL DoH), split domain, search domain và record.
- **Ràng buộc thêm cho extra_records:**
  - Tên record phải nằm dưới base domain của **chính** tailnet đó (`*.<s>.<base>`), hoặc nằm hoàn toàn ngoài base domain của cluster.
  - Không được đặt record dưới `<khác>.<base>`, kể cả khi record đó chỉ tới được client của chính tenant. Lý do: tránh nhầm lẫn và tránh tạo đường tới tên chứng chỉ của tenant khác.
  - Tên record trùng tên MagicDNS của một node trong tailnet vẫn bị chặn như hiện nay: `reservedLabel`, và `CertDomain` từ chối tên mà một extra record đang giữ.
- **Giới hạn kích thước (chống DoS, I10), chỉnh được trong config của cluster:** mặc định tối đa 1 000 record, 64 nameserver (global + split), 64 search domain, JSON tối đa 256 KiB. Khóa: `tuning.tenant_dns.max_extra_records`, `max_nameservers`, `max_search_domains`, `max_config_bytes`; mỗi khóa phải dương, có mặc định, có doc sinh tự động và nằm trong `TestConfigFields`. Chỉ operator đổi được (file config), tenant thì không. Hạ giới hạn khi đã có tailnet vượt mức: DNS đang dùng của tailnet đó vẫn nạp (không cắt dữ liệu đã lưu) nhưng có cảnh báo, và lần ghi kế tiếp phải nằm trong giới hạn mới.
- **Ghi lỗi:** JSON hỏng hoặc vi phạm ràng buộc thì lệnh ghi bị từ chối, giá trị cũ giữ nguyên.
- **Đọc lỗi:** khi nạp, nếu `dns_config` trong DB không validate được (ví dụ do bị sửa tay), runtime của tailnet đó **không được nạp** và có log lỗi chỉ ghi tailnet ID. Không có trường hợp chạy tiếp với DNS mặc định.

### 4. Ai ghi được
- **P06:** chỉ operator, qua CLI cluster `cluster tailnet dns --slug S --file dns.json`. Có thêm `--clear` để về mặc định và `--show` để xem. CLI đi qua platform DB handle, giống `tailnet policy`.
- **API v2 cho tenant** (khóa admin scoped theo tailnet, P08a): **chưa** làm trong P06. Ghi DNS từ phía tenant cần review authZ và giới hạn tần suất riêng; sẽ làm ở ticket API sau (P08b hoặc P12). Endpoint `GET /api/v2/tailnet/-/settings` hiện đọc `cfg.DNSConfig` dùng chung (`api/v2/settings.go:82`); P06 sửa nó để đọc DNS của runtime.

### 5. Mỗi runtime có `Config` riêng cho DNS
- `Cluster.load` dựng một bản `Config` cho runtime: **sao chép nông** `Cluster.cfg`, rồi gán `DNSConfig` mới (sao chép sâu các slice) từ §1–§2. Sau đó gọi `StartDNS` trên **bản sao** trước khi `newHeadscale` dùng nó. `newHeadscale` không còn gọi `StartDNS` trên một config dùng chung.
  - **Audit bắt buộc khi code:** liệt kê mọi trường của `Config` bị ghi sau khi khởi động. Hiện đã biết: `dns` (liveDNS), `SetExtraRecords`. Không trường nào trong số đó được trỏ chung giữa bản sao và bản gốc. Phần không thuộc DNS (Tuning, DERP, Policy mode, ...) chỉ đọc và vẫn dùng chung.
- **Không còn `ExtraRecordsManager` theo file** ở cluster mode; extra_records của tenant đến từ DB.
- **Nạp lại:** `Cluster.reconcile` so `config_revision` với giá trị runtime đang giữ, giống `policy_revision`. Khi khác:
  1. validate;
  2. thay DNS của runtime một cách nguyên tử (`Config.ReplaceDNS`, có khóa);
  3. phát `change.DNSConfig` cho mọi node của runtime đó, và chỉ runtime đó.
  
  Nếu validate lỗi thì giữ DNS cũ, log lỗi, và runtime giữ `config_revision` cũ để lần reconcile sau thử lại.
- **I07:** đổi DNS của A không phát bất kỳ change nào sang runtime B. Có test kiểm.

### 6. Ảnh hưởng tới client đang chạy
- v0.1 chưa chạy production; mọi tên MagicDNS cũ dạng `node.<base>` chỉ có trong lab. Đổi sang `node.<s>.<base>` là thay đổi phá vỡ được chấp nhận. Harness E2E P05, P09a và P11 phải sửa các chỗ kiểm tên.
- Client nhận base domain mới qua MapResponse sau khi kết nối lại hoặc sau một lần đổi DNS, không cần đăng nhập lại.

## Hệ quả
- **Bảo mật:** hết lỗi tên chứng chỉ trùng giữa tenant; hết race `StartDNS`; DNS của A không bao giờ tới client của B.
- **Chi phí:** thêm một bảng và một migration; thêm một nhánh trong reconcile; mỗi runtime giữ thêm một bản `Config` nhỏ.
- **Giới hạn còn lại:** tenant chưa tự ghi được DNS (chỉ operator), slug không đổi được, và chỉ có một provider ACME cho cả cluster.

## Phương án đã loại
| Phương án | Lý do loại |
|---|---|
| Giữ DNS dùng chung, từ chối `dns.certificates` ở cluster mode | Chủ dự án chọn DNS theo tailnet (2026-11-24) |
| Tenant tự đặt base domain | Không xác minh được quyền sở hữu; tên trùng hoặc che tên tenant khác |
| Mỗi tenant một file config DNS | Không nằm trong DB, không đi qua fence và owner/epoch, không nạp lại theo revision |
| Thêm cột `dns_config`/`dns_revision` trên `tailnets` | `tailnets` là bảng platform không scope; thiết lập của tenant thuộc bảng tenant `tailnet_settings` mà PLAN đã dự kiến, và `config_revision` đã có sẵn |

## Test bắt buộc (RED trước)
1. **Tên khác nhau:** node `phone` ở A và B có tên MagicDNS và `CertDomain` khác nhau. `set-dns` của B cho tên của A bị từ chối.
2. **DNS tách biệt:** A và B có nameserver, search domain và extra_records khác nhau. Mỗi client chỉ nhận DNS của tailnet mình. Đổi DNS của A thì client B không nhận frame nào (I07).
3. **Từ chối cấu hình sai:**
   - record dưới `<b>.<base>` được ghi vào A thì bị từ chối;
   - JSON hỏng, hoặc vượt giới hạn, thì bị từ chối và giá trị cũ còn nguyên;
   - `dns_config` hỏng trong DB làm runtime không được nạp.
4. **Từ chối khởi động:** cluster config có DNS tenant-đặt-được thì không khởi động.
5. **Race:** nạp nóng runtime trong lúc mapper của runtime khác đọc DNS, chạy với `-race`; trước khi sửa phải thấy race.
6. **Migration:** bảng `tailnet_settings` đúng FK và scope, `config_revision` tăng khi ghi; slug không hợp lệ làm migration fail; down migration hoạt động; schema SQLite và PostgreSQL vẫn bằng nhau.
7. **E2E client thật:** `tailscale status` và `tailscale dns status` cho thấy tên `node.<s>.<base>` và DNS riêng của từng tailnet.
8. **Một tailnet:** `headscale serve` vẫn dùng base domain trong file; golden và test upstream không đổi.

## Quyết định của chủ dự án (2026-11-24)
- **Q-a:** giữ giới hạn như đề xuất (1 000 record, 64 nameserver, 64 search domain, 256 KiB); mọi thông số về số lượng là tham số config của cluster, dễ chỉnh (§3).
- **Q-b:** API v2 cho tenant ghi DNS để ticket sau; P06 chỉ có CLI của operator (§4).
- **Q-c:** cluster config có DNS kiểu tenant (extra_records, extra_records_path, nameservers, search_domains) thì từ chối khởi động (§2).
