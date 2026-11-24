# Source inventory: E2E two clusters (lab stage 2a)

- **Nhánh:** `feat/E2E-two-cluster`, từ `master` `15dd9f3` (sau merge `feat/LAB-stage2a`).
- **Mục tiêu:** chạy client thật trên hai cluster thật dùng chung một PostgreSQL (c1 trên mc-cp, c2 trên mc-c2), kiểm những gì đã có code: owner/epoch và reconcile (P10), bootstrap trả URL theo owner (P11, F06), cluster không sở hữu tailnet từ chối (F07), chuyển tailnet giữa hai cluster bằng `tailnet reassign` với client đăng nhập lại (ADR 0005). Không phải P12 (placement mới, reconcile giữa cluster) hay P12b (thủ tục move có kế hoạch): hai ticket đó chưa bắt đầu.
- **Code server:** không đổi kể từ `d56a931` (`git diff d56a931..15dd9f3 -- hscontrol cmd internal go.mod` rỗng). Binary build trên mc-ci từ `15dd9f3`: cluster sha256 `c6270a4f168e8cac…`, `mesh-bootstrap` `15a68e78540c47ed…`.

## Đã đọc

- `hscontrol/cluster.go`:
  - `reconcileInterval` 2 s;
  - `reconcile()` nạp hoặc retire runtime theo owner/status, log `loaded tailnet runtime` (slug, epoch) và `retiring tailnet runtime`;
  - enrollment từ chối khi tailnet không thuộc cluster.
- `hscontrol/db/platform_admin.go`: `CreateTailnet` đặt `OwnerClusterID` = `--cluster-id` của CLI, epoch 1.
- `cmd/headscale/cli/cluster.go`:
  - `register --endpoint/--class/--max-devices`;
  - `tailnet create|status|policy|authkey|nodes|reassign`;
  - `withTenant`/`withPlatform` đọc ghi toàn DB, không gắn với cluster của CLI.
- `internal/bootstrap/{bootstrap.go,store.go,http.go}`: `control_url` = `endpoint` của cluster sở hữu trong registry, 503 khi owner chưa đăng ký/không ACTIVE, 404 đồng nhất cho các trường hợp từ chối.
- `infra/lab/e2e/p05/run.sh`: setup cluster trên PostgreSQL, các helper client, `boot_req`, `redact`, cleanup.
- `infra/lab/c2/setup-c2.sh`, `infra/lab/mc-cp/cluster/config.yaml`, `infra/lab/mc-cp/headscale-cluster.service`.

## Ràng buộc

- AGENTS rule 2: auth key chỉ nằm trong thư mục tạm 0700 trên máy chạy; proof bootstrap tính trong một bước python; state client (có private key) chỉ được sửa trên mc-ci trong file 0600 và xóa ngay.
- Rule 6, 13: tenant isolation giữa cluster là điều kiện BLOCK MERGE.
- DB: mỗi lần chạy dùng database riêng `e2e_n2c<run>` trên mc-db, xóa khi cleanup (regex `^e2e_[0-9a-z]+$`); `meshlab` không bị đụng; mc-c2 tạm chuyển sang DB của lần chạy rồi khôi phục về `meshlab`.

## Giả định còn lại

- Client của tailnet bị chuyển phải đăng nhập lại ở cluster mới (ADR 0005); harness ghi lại node ID và IP trước và sau để biết server giữ hay tạo node mới, nhưng không coi đó là tiêu chí pass.
- "Không owner kép" ở đây được kiểm qua hành vi: sau reassign, cluster cũ từ chối và client còn trỏ cluster cũ không nhận cập nhật. Chưa đo cửa sổ chồng lấn giữa retire ở c1 và load ở c2 (P12b).
