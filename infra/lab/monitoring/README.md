# Lab monitoring on mc-db (P13f, D10) — NOT APPLIED

Prometheus, Alertmanager, Grafana and the exporters as containers on `mc-db` (VMID 7704, 192.168.1.243), owner decision D10. Nothing here has been deployed; the files are checked statically by `check.sh` (promtool config/rules/unit tests, amtool, compose syntax, dashboard JSON).

| File | What |
|---|---|
| `docker-compose.yml` | pinned images; UIs bound to 127.0.0.1 (SSH tunnel); secrets only from `/opt/mc-mon/secrets/*` (0600, outside Git) |
| `prometheus/prometheus.yml` | scrape jobs: cluster processes, mesh-bootstrap, mesh-placement, node, postgres ×2, platform queries, PgBouncer ×2, Patroni, etcd, blackbox |
| `prometheus/rules/mesh.rules.yml` | alerts (table below) |
| `prometheus/tests/mesh.rules.test.yml` | `promtool test rules`: every alert a drill must fire, firing and not firing |
| `alertmanager/alertmanager.yml` | one receiver (webhook URL from a secret file), inhibitions |
| `grafana/` | datasource + dashboard "Mesh control plane (P13f)" |
| `exporters/blackbox.yml`, `exporters/platform-queries.yaml` | TLS/HTTP probes of the public endpoints; device quota and lease rows from the database |
| `textfile/pgbackrest-metrics.sh` | pgBackRest backup/WAL-archive metrics for node_exporter's textfile collector (P13c repository on mc-db) |
| `check.sh` | static checks (run on mc-ci) |

## What is scraped, and from where (AGENTS rule 9)

Every target is an operator-only listener on the lab LAN, reached from mc-db only. None is public; nothing is proxied (a proxy in front of a metrics listener would make every caller loopback, P08a N7).

| Target | Listener (to set when applying) | Who may read it |
|---|---|---|
| cluster processes c1 (mc-cp .241, mc-c1b .245), c2 (mc-c2 .244, mc-c2b .246) | `metrics_listen_addr: <vm-lan-ip>:9095`, `metrics_operator_cidrs: [192.168.1.243/32]` (today `127.0.0.1:9095`) | loopback + mc-db; VM nftables: tcp/9095 from 192.168.1.243 only |
| mesh-bootstrap (mc-cp) | `-metrics-listen 192.168.1.241:9097 -metrics-operator-cidrs 192.168.1.243/32` | same |
| mesh-placement (mc-cp) | `-metrics-listen 192.168.1.241:9098 -metrics-operator-cidrs 192.168.1.243/32` | same |
| Patroni REST `/metrics` (:8008), etcd metrics URL (:2381), node_exporter (:9100) on mc-pg-01/02 | P13a | mc-db per the P13a nftables (8008 "chỉ từ localhost và mc-db") |
| PostgreSQL, PgBouncer | through exporters on mc-db with a monitoring role (`pg_monitor`) and a PgBouncer `stats_users` user | — |
| public endpoints c1 :8443, c2 :8445, bootstrap :8444 | blackbox `https_health` from mc-db through the VPS | — |

The cluster listener already refuses everyone but loopback and the configured CIDRs, and never the Tailscale range (`hscontrol/operator_access.go`, `internal/opsmetrics`). Per-tailnet series (tailnet IDs) and the platform quota query stay on this stack.

## Alerts (tickets/P13.md §P13f "Cảnh báo tối thiểu")

| Ticket item | Alert | Source | Drill that fires it |
|---|---|---|---|
| cluster without holder > TTL | `MeshClusterNoLeaseHolder` (30 s) | `headscale_cluster_lease_held` | F05 (PG down: no renewal), P13d switchover gap |
| takeover rate (flapping) | `MeshClusterLeaseFlapping`, `MeshClusterLeaseLost` | `headscale_cluster_lease_events_total` | F01/F02, P13b PgBouncer kill |
| renewals failing | `MeshClusterLeaseDeadlineLow`, `MeshClusterLeaseRenewSlow`, `MeshClusterLeaseRowExpired` | lease deadline/renew histogram, `cluster_leases` via the platform exporter | F05.4 (partition), P13b |
| fence error rate | `MeshFenceRefusalsSpike` | `headscale_tenant_fence_refusals_total{reason}` | F08 (holder before restore), P12b move |
| epoch regression | `MeshClusterLeaseEpochRegression` | `headscale_cluster_lease_epoch` | F08 restore before fence |
| PgBouncer `cl_waiting` > 0 for 30 s, `maxwait` | `PgBouncerClientsWaiting`, `PgBouncerMaxWaitHigh`, `PgBouncerDown` | pgbouncer_exporter | P13b load drill, D9b alias saturation |
| DB pool saturation | `MeshDBPoolSaturated`, `MeshDBPoolWaiting` | `headscale_db_pool_*` | bench-db B2 load |
| Patroni without leader / sync standby | `PatroniNoLeader`, `PatroniNoSyncStandby`, `EtcdNoLeader` | Patroni, etcd | F05.1–F05.4 |
| replica lag | `PostgresReplicationLagBytes` (16 MiB, 2 min) | Patroni xlog locations | F05 under write load |
| WAL archive failing | `PostgresWALArchiveFailing` | textfile (`pgbackrest check`, newest WAL) | P13c (archive_command broken on purpose) |
| backup older than 26 h | `PostgresBackupTooOld`, `BackupMetricsMissing` | textfile | P13c schedule stopped |
| PG clock skew > 100 ms | `HostClockSkew` | node_exporter timex | P13a (chrony stopped, offset injected) |
| disk > 80 % | `HostDiskAlmostFull` | node_exporter | fill test file |
| TLS < 14 days | `MeshTLSCertExpiresSoon`, `MeshPublicEndpointDown` | blackbox | short-lived test cert |
| tenant ≥ 80 % quota | `MeshTailnetQuota80` | platform query (`mesh_tailnet_quota_*`) | P10 quota test tailnet |
| tailnet labels near cap | `MeshTailnetLabelsNearCap` | `headscale_tailnet_metric_labels`, overflow | `tuning.tailnet_metric_label_limit` lowered |
| L9 reload failing | `MeshTailnetReloadPending` | `headscale_cluster_tailnet_runtimes{state="reload_pending"}` | invalid policy stored |
| B7 | `MeshBootstrapGlobalLimit`, `MeshBootstrapProxyHeaderRefused` | mesh-bootstrap | flood through the VPS HAProxy |
| L5 | `MeshPlacementRevocationListUnavailable` | mesh-placement | revocation file broken |

## Deploy (owner-approved only; lab VMs, inside the pool `meshcontrol-lab`)

1. **Roles (PostgreSQL primary, through the direct port):** `CREATE ROLE mc_monitor LOGIN PASSWORD :'gen' IN ROLE pg_monitor;` and, for the platform queries, `CREATE ROLE mc_platform_ro LOGIN PASSWORD :'gen'; GRANT CONNECT ON DATABASE meshlab TO mc_platform_ro; GRANT SELECT (id, status, deleted_at, owner_cluster_id) ON tailnets TO mc_platform_ro; GRANT SELECT (id, class, max_devices) ON clusters TO mc_platform_ro; GRANT SELECT (id, tailnet_id, deleted_at) ON nodes TO mc_platform_ro; GRANT SELECT ON cluster_leases TO mc_platform_ro;` (column grants: no keys, no node data). PgBouncer: a `stats_users` entry for the exporter. Passwords generated on the VM, stored in `%USERPROFILE%\.mesh-lab\mc-mon.env` and `/opt/mc-mon/secrets/*.env` (0600), never on a command line or in a log (AGENTS rule 2).
2. **mc-db:** copy this directory to `/opt/mc-mon/`; create `/opt/mc-mon/secrets/{grafana,pg-exporter-01,pg-exporter-02,pg-exporter-platform,pgbouncer-exporter-01,pgbouncer-exporter-02}.env` (`DATA_SOURCE_NAME=…`, `PGBOUNCER_EXPORTER_CONNECTION_STRING=…`, `GF_SECURITY_ADMIN_PASSWORD=…`) and `alertmanager-webhook-url`; `install -d /var/lib/mc-mon/textfile`; `docker compose -f /opt/mc-mon/docker-compose.yml up -d`.
3. **Cluster VMs:** `metrics_listen_addr` to the LAN address and `metrics_operator_cidrs: [192.168.1.243/32]`; VM nftables `tcp dport 9095 ip saddr 192.168.1.243 accept`; restart the standby first, then switch over (P13d procedure). mesh-bootstrap/mesh-placement flags as in the table above.
4. **Backups (with P13c):** systemd timer on mc-db every 10 min: `textfile/pgbackrest-metrics.sh <stanza> --check` as the repository user.
5. **Check:** Prometheus targets all `up`; `promtool test rules` already green (`check.sh`); fire one alert per drill as listed.

Resources: Prometheus 30 days / 10 GB cap at ~2 000 series per cluster process; about 1 GiB RAM for the whole stack. mc-db has 8 GiB and 54 GB free (`evidence/source-inventory-P13.md` §3.2).

## Pinning

Images are pinned by tag in `docker-compose.yml` (prom/prometheus v3.5.0, prom/alertmanager v0.28.1, grafana/grafana 12.0.2, prom/node-exporter v1.9.1, postgres-exporter v0.17.1, pgbouncer-exporter v0.10.2, blackbox-exporter v0.27.0). On the first pull on mc-db, record each `RepoDigest` (`docker image inspect -f '{{index .RepoDigests 0}}'`) in the deploy evidence and switch the compose file to `image: name@sha256:…`.
