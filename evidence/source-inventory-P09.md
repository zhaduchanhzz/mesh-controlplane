# Source Inventory — P09 (Gate N1: tailnet isolation với client thật)

- **Commit:** `master` `225fe1a`, ngày 2026-11-25.
- **Nhánh:** `feat/P09-inventory`.
- **Trạng thái:** `Planned`. Chưa có code. Đây chỉ là inventory và hợp đồng; các câu hỏi cho chủ dự án ở §6.

## 1. Đã đọc

| Nguồn | Nội dung liên quan |
|---|---|
| `PLAN.md:46` | N1 = 2 tailnet trên 1 process, S01–S18 PASS với client Tailscale thật |
| `PLAN.md:279` | P09 = Gate N1; công tắc netmap tăng dần OFF **và** ON; S01–S18, M06, nhiều phiên bản client |
| `PLAN.md:295` | P10–P11 làm trước N1, chỉ tính là xong khi N1 đạt |
| `PLAN.md:301` | Công tắc netmap tăng dần chỉ được bật mặc định ON sau P09 và review bảo mật |
| `PLAN.md:305, 356–383` (§9) | Lab giai đoạn 2 phải xong **trước P09** |
| `PLAN.md:313–321` (§7) | Danh sách điều kiện của N1 |
| `06-tests-benchmarks.md:9–26, 49–54` | Định nghĩa S01–S18 và M01–M06 |
| `evidence/*.md` | P02, P03b, P05, P06, P07, P08, P08b, P09a, P10*, P11, P11b, P12, `E2E-two-cluster.md`, `E2E-postgres-pgbouncer.md`, `LAB-*.md` |
| `infra/lab/e2e/{p00d,p05,p08b,p11b,p12,twocluster}/run.sh` | Mã ca test trong header; công tắc netmap |
| `hscontrol/servertest/cluster_p0{5,6,7}_test.go`, `cluster_p10/p11b/p12*_test.go` | Servertest cách ly |
| `hscontrol/db/tenantscope/*_test.go`, `hscontrol/db/raw_write_guard_test.go` | Plugin fail-closed, lint chặn ghi SQL thô |
| `infra/lab/mc-cp/cluster/config.yaml` (`derp:`) | Cluster đang dùng DERP công cộng của Tailscale |
| `infra/lab/stage2-*.tf` | IaC giai đoạn 2, mặc định tắt, vẫn theo topology bản 1.1 |

Ma trận ở §3 được lập bằng cách đọc toàn bộ các nguồn trên. Hai kết luận đã kiểm lại trực tiếp:
- chỉ `p00d/run.sh` bật `incremental_netmap`;
- chưa evidence nào có chấp thuận của reviewer người thật.

## 2. Hiện trạng tổng quát

- **E2E đang có:** mọi E2E ở cluster mode đều chạy với công tắc netmap tăng dần **OFF**, là mặc định. M06 (ON) mới chạy trên `headscale serve` single-tailnet (P00d).
- **Phiên bản client:**
  - Harness p05 (chứa phần lớn S-case trên một process) mới có evidence với v1.104.7.
  - p08b, p11b và p12 đã chạy với cả v1.104.7 và v1.84.3.
- **Phạm vi phủ:** S-case nằm rải rác ở 6 harness, mỗi harness dùng hệ mã riêng (R0x, D0x, N0x, P0x, F0x, A0x…). Chưa có lượt chạy nào đối chiếu một-một với danh mục S01–S18.
- **Review:** không ticket nào `Reviewed`. Chưa có reviewer bảo mật người thật chấp nhận mô hình một Noise key mỗi cluster (N1 `PLAN.md:321`).

## 3. Ma trận phủ S01–S18, M01–M06

Phân loại:
- **E2E:** client thật trên lab.
- **Một phần:** thiếu biến thể hoặc phiên bản client.
- **Unit/servertest:** chỉ có test trong process.
- **Thiếu:** chưa có kiểm chứng nào.

| ID | Đã có | Mức | Thiếu cho N1 |
|---|---|---|---|
| S01 | p05 S01, R01/R03; twocluster N02; p12 P02 | E2E | p05 chạy với v1.84.3; công tắc ON; policy dạng `grants` (hiện dùng `acls`) |
| S02 | R01, p11b I01, N02; `TestClusterTwoTailnetsPeersIsolated` | E2E | Quét key/IP đầy đủ phía client; công tắc ON |
| S03 | p05 S03; `TestClusterP06WildcardStaysInTailnet` | E2E | v1.84.3; công tắc ON |
| S04 | `TestCreateTailnetPendingDenyOwner`, `TestCreateTailnetFailureLeavesNothing`, `TestTenantDefaultDeny` | Unit/servertest | Race "tạo tailnet đồng thời với enroll"; E2E |
| S05 | p05 R04 (cùng cluster); `TestClusterSameMachineKeyTwoTailnets` | Một phần | **Biến thể hai cluster**; v1.84.3 |
| S06 | R02 (trùng IP); `TestClusterP06SameIPHostnameAndRoute` | Một phần | E2E trùng hostname và trùng subnet route |
| S07 | p05 D02/D03; `TestClusterP06DNS*` | E2E | v1.84.3; công tắc ON |
| S08 | `TestClusterP06AutoApproversBoundedByTailnet` | Unit/servertest | E2E route / exit node trùng CIDR |
| S09 | `TestClusterP06TagOwnersBoundedByTailnet` | Unit/servertest | E2E tag |
| S10 | R05–R07, p08b A02/A05, bootstrap B02/N05 | E2E | Không áp dụng cookie/web-approval (cluster mode tắt web auth) |
| S11 | (a)(b) `TestClusterSessionCannotSwitchTenant` (raw Noise); (c) C03 421; (e) N03/N04, p12 P03 | Một phần | (a)(b)(d): client chính thức không gửi được request kiểu này. Cần quyết cách chứng minh (Q3) |
| S12 | p05 S12; p08b metric; `TestClusterP07*` (công tắc OFF và ON) | E2E (OFF) | E2E với công tắc ON |
| S13 | `TestBootstrapRateLimit`, `TestP08bTenantAPIRateLimits`, quota; p08b A06 | Một phần | Flood enroll / MapStream / bootstrap theo SLO (đã hẹn P14) |
| S14 | C02/C04, B04, p08b A07/X01, p11b H01, N01; unit endpoint inventory | E2E | Thăm dò gRPC admin theo tên |
| S15 | `TestP08aVerify*`, `TestP08aDERPVerifyTwoTailnetsOneProcess` | Unit | **E2E DERP verify** (cần relay riêng, ADR 0009 chưa viết). Lab đang dùng DERP công cộng |
| S16 | p05 S16, p12 P04/P05, N06, p11b F01, p12 P06/P07 | E2E | — |
| S17 | p11b F02/F03, p12 P08/P09/P10, N08; fence tests | E2E | **P08 ở lần v1.84.3: một frame sau SIGCONT, chưa giải thích**; chủ dự án đã chấp nhận cho P12 |
| S18 | p05 S18; `TestPlatformDeletingIsTerminal` | E2E | v1.84.3 |
| M01–M05 | Property / golden / unit (P00d, P07) | Unit (đúng như đặc tả) | — |
| M06 | `p00d/run.sh`, công tắc ON và OFF | Một phần | **Chạy trên cluster mode nhiều tenant**; v1.84.3 |

### Các điều kiện khác của N1 (`PLAN.md:314–321`)

| Điều kiện | Hiện trạng |
|---|---|
| 2 tailnet / 1 process / 1 Noise key cluster | E2E C01, R01/R04 |
| Không lộ chéo registration / map / policy / DNS / routes / API / metrics | E2E cho mọi mục **trừ routes** (chỉ servertest) |
| Phiên gắn đúng một tenant | Chỉ servertest (raw Noise) |
| `grants=[]` atomic; policy nil/rỗng = deny | Unit/servertest; E2E dùng `grants:[]` tường minh; chưa có E2E với policy nil |
| Machine key trùng ở tailnet khác hợp lệ | E2E R04, chỉ cùng cluster |
| Plugin fail-closed, lint SQL thô | Có test; lint mới chặn **ghi**, phần **đọc** SQL thô chỉ được review tay |
| M01–M06 nếu bật công tắc | Xem dòng M |
| Reviewer bảo mật chấp nhận một key mỗi cluster | **Thiếu** |

## 4. Lab giai đoạn 2 (PLAN §9)

| Thành phần | Yêu cầu | Hiện có | Cần cho N1? |
|---|---|---|---|
| c1, c2 active/standby | `mc-c{1,2}-a/b` | mc-cp + mc-c1b; mc-c2 + mc-c2b | Đã có |
| PostgreSQL | primary + replica, PgBouncer trên mỗi VM | mc-db: một PostgreSQL + một PgBouncer | Không (thuộc N3 / P13) |
| bootstrap, placement | `mc-bootstrap` | Chạy tạm trên mc-cp | Không bắt buộc |
| DERP + STUN + verify | `mc-relay` | **Không có**; dùng DERP công cộng | **Có** (S15) |
| Client | VM `mc-client-a1/a2/b1/b2` | Container docker trên mc-ci | Có, để kiểm "VPN isolation" trên underlay tách |
| Mạng underlay | 10.77.{10,20,30,40}.0/24, không cho A↔B đi thẳng | **Không có** | Có (yêu cầu của S01–S03 về data plane) |
| CA lab | TLS CA lab hoặc DNS-01 | Hostname công khai qua VPS | Không bắt buộc |
| IaC | `stage2-*.tf` | Mặc định tắt, topology bản 1.1 | Phải cập nhật trong ticket LAB riêng (`PLAN.md:376`) |

## 5. Giả định (chưa kiểm chứng)

- DERP riêng trên một VM trong pool lab, với `verify_clients` trỏ tới endpoint verify của cluster, có thể kiểm S15 mà không đụng hạ tầng ngoài lab. Phải viết ADR 0009 trước.
- Client nằm trên VNet SDN riêng cho A và B. Proxmox SDN cần người thao tác (`infra/lab/README.md:75–113`); agent không tự tạo zone/VNet.
- Chạy cluster mode với công tắc ON không cần đổi code; đó chỉ là cấu hình `tuning.incremental_netmap` (`p00d/run.sh:35,134`).

## 6. Câu hỏi cho chủ dự án

- **Q1 — Phạm vi lab giai đoạn 2 trước N1.** **[Đề xuất]**
  - Làm trước N1, bằng một ticket LAB riêng có diff trình duyệt:
    - (a) VM `mc-relay` với DERP + STUN + verify;
    - (b) client VM trên hai VNet underlay A/B tách biệt.
  - Để sang P13: replica PostgreSQL và PgBouncer thứ hai (gate N3).
- **Q2 — Công tắc netmap tăng dần.** **[Đề xuất]** Chạy toàn bộ bộ N1 hai lần, công tắc OFF và ON, với cả v1.104.7 và v1.84.3 (4 tổ hợp). Công tắc vẫn **mặc định OFF** cho tới khi review bảo mật xong.
- **Q3 — S11 (a)(b)(d):** client chính thức không gửi được request đổi tenant giữa phiên. **[Đề xuất]** Chấp nhận servertest dùng raw Noise (`TestClusterSessionCannotSwitchTenant`) làm bằng chứng, cộng một tool nhỏ chạy trên lab gửi đúng chuỗi frame đó tới cluster thật (E2E protocol-level, không dùng client chính thức).
- **Q4 — S13 (flood):** **[Đề xuất]** Giữ ở P14. N1 chỉ cần chứng minh tenant B không bị chặn khi A chạm quota hoặc rate limit; phần này đã có E2E A06 cùng test quota.
- **Q5 — S17 / P08 (một frame sau SIGCONT):** **[Đề xuất]** Đo chính xác trong P09 bằng metric thời điểm ghi frame cuối và vòng lặp F02 nhiều lần, vì S17 nằm trong N1.
- **Q6 — Reviewer bảo mật:** N1 bắt buộc có người duyệt chấp nhận mô hình một Noise key mỗi cluster. Ai duyệt, và khi nào?
- **Q7 — Harness:** **[Đề xuất]** Viết một harness N1 duy nhất `infra/lab/e2e/n1/run.sh`, mã ca đúng S01–S18 và M06, tái sử dụng các khối của p05/p12. Không chạy lại 6 harness cũ.
