package db

// rawSQLAllowlist holds the reviewed raw SQL statements that name a tenant
// table or that the guard cannot resolve (see raw_sql_guard_test.go): one
// entry per statement, keyed by file and the hash of its normalised SQL (or
// "expr:" source). Changing a statement changes its hash, so it must be
// reviewed again. P12c A4 moved the former per-file write allowlist
// (platform_admin.go, placement/tailnets.go, bench-db/b3.go,
// tailnet_settings.go, placement/apikeys.go) here statement by statement;
// the reads and unresolved statements were not checked before and are
// listed for the first time.
//
//nolint:gosec // G101: file names, hashes and reasons; "credentials" is a table name
var rawSQLAllowlist = []rawSQLEntry{
	// Benchmarks (build tag benchdb): never compiled into a server binary.
	{"cmd/bench-db/b2.go", "b9fe4cb5ae358fc4", "benchmark B2: row count of each benchmark table for the report; tbl ranges over a fixed list"},
	{"cmd/bench-db/b2.go", "2384e1eef1084994", "benchmark B2: PgBouncer admin SHOW command chosen by the benchmark, not tenant data"},
	{"cmd/bench-db/b3.go", "966d5c8ebf20e852", "benchmark B3: rewrites cluster_id of the blogsim copy (own schema) to measure the per-row move v0.1 avoids"},
	{"cmd/bench-db/b3.go", "0c161ece4c48c0a1", "benchmark B3 bulk rewrite: write load on nodes of one tailnet (former file allowlist entry)"},
	{"cmd/bench-db/b3.go", "1e218c3e7dafa171", "benchmark B3: reads cluster_id of the blogsim copy"},
	{"cmd/bench-db/b3.go", "4e58ed8fb33d1087", "benchmark B3: pg_stat_user_tables of the tenant tables (statistics, no rows)"},
	{"cmd/bench-db/b3.go", "6798167db73a83c4", "benchmark B3: write load on the blogsim copy"},
	{"cmd/bench-db/b3.go", "fc9635c9b12493ae", "benchmark B3: node ids of one tailnet for the load; table from a fixed list"},
	{"cmd/bench-db/b3.go", "87656b5695bbaff6", "benchmark B3: creates the blogsim copy of the tenant tables from a fixed table list"},
	{"cmd/bench-db/b3.go", "cbdcc676b2af392b", "benchmark B3: node count of one tailnet for the report"},

	// Migrations and schema creation: DDL and prechecks that return counts.
	{"hscontrol/db/nodeips.go", "810782358eda0013", "migration nodes-ip-unique-per-tailnet precheck: counts duplicate (tailnet_id, ip) pairs, no row data"},
	{"hscontrol/db/nodeips.go", "b8a9ff90a24909a2", "migration nodes-ip-unique-per-tailnet DDL: per-tailnet unique address indexes"},
	{"hscontrol/db/nodeips.go", "76905e59605d7cdf", "migration nodes-ip-unique-per-tailnet Rollback: drops its indexes (idx ranges over fixed names)"},
	{"hscontrol/db/nodekeys.go", "40bd4ab6a9f4320b", "migration node-key-pair DDL (PostgreSQL): index and trigger keeping a key pair in one tailnet"},
	{"hscontrol/db/nodekeys.go", "9b716da5af245fcc", "migration node-key-pair DDL (SQLite): index and triggers keeping a key pair in one tailnet"},
	{"hscontrol/db/nodekeys.go", "0353ed4b60626ef8", "migration node-key-pair Rollback: fixed DROP statements"},
	{"hscontrol/db/nodekeys.go", "deacf32a6591f271", "migration node-key-pair precheck: counts key pairs found in several tailnets, no row data"},
	{"hscontrol/db/registry.go", "f78df47f42cbb22c", "registry migration Rollback: fixed DROP statements of platform tables"},
	{"hscontrol/db/tailnet_settings.go", "0a57c82dc7ae47b9", "migration tailnet-settings DDL"},
	{"hscontrol/db/tailnet_settings.go", "6a639fbf832d89eb", "migration tailnet-settings Rollback"},
	{"hscontrol/db/tenant.go", "09b0dbeded5110cc", "createSchema: runs the embedded schema.sql / schema_postgres.sql, reviewed with every migration"},
	{"hscontrol/db/tenant_immutable.go", "2e4cfb1ab6925355", "migration tenant-id-immutable DDL (SQLite): tailnet_id immutability triggers (P12c A8)"},
	{"hscontrol/db/tenant_immutable.go", "88188931aec68389", "migration tenant-id-immutable DDL (PostgreSQL): tailnet_id immutability triggers (P12c A8)"},
	{"hscontrol/db/tenant_immutable.go", "6c336818c89bd567", "migration tenant-id-immutable Rollback: drops the triggers"},

	// Platform lookups (ADR 0007 §1): resolve which tailnet a credential or a
	// Noise session belongs to; they read tailnet_id and what verifies it.
	{"hscontrol/db/platform.go", "499a53492be436cf", "TailnetForAuthKey: tailnet_id and hash of the pre-auth key named by kind and identifier, to verify it"},
	{"hscontrol/db/platform.go", "a3b7f34fa0824469", "TailnetForNode: tailnet_id of the node with this (machine_key, node_key), at most two rows to detect a conflict"},
	{"hscontrol/db/platform_apikey.go", "0e2b2e116a4b3bb2", "TailnetForAPIKey: tailnet_id, hash and expiry of the tenant API key named by identifier, to verify it"},
	{"internal/bootstrap/store.go", "545a0bf1e965384e", "bootstrap (P11): tailnet and state of the enrollment credential on the read-only handle, to verify it"},

	// Platform operator paths: name their tailnet_id explicitly.
	{"hscontrol/db/platform_admin.go", "b391983d0abb59c9", "CreateTailnet: deny-all policy of the PENDING tailnet in the transaction that creates it (AGENTS rule 8); former file allowlist entry"},
	{"hscontrol/db/tailnet_settings.go", "1e0438ebccc5922d", "TailnetDNS: DNS settings of the tailnet the caller names (cluster load, operator show)"},
	{"hscontrol/db/tailnet_settings.go", "5011acfac974074d", "SetTailnetDNS under LockOwnership with the config_revision bump (P06, ADR 0008); former file allowlist entry"},
	{"internal/placement/apikeys.go", "26a4189c8d04ad34", "RevokeAPIKey under LockOwnership, by tailnet_id and id (P08b, Q3); former file allowlist entry"},
	{"internal/placement/apikeys.go", "56e3158651c22985", "CreateAPIKey: user of the key by tailnet_id and name"},
	{"internal/placement/apikeys.go", "7250f0603e961f4a", "RevokeAPIKey: reads back the key the UPDATE before it matched by tailnet_id and id, same transaction"},
	{"internal/placement/apikeys.go", "9e3ad017c5239f6e", "CreateAPIKey under LockOwnership, names its tailnet_id (P08b, Q3); former file allowlist entry"},
	{"internal/placement/apikeys.go", "eac1ae4f126bc8b9", "ListAPIKeys: metadata of one tailnet's API keys (no hashes), user joined within the same tailnet"},
	{"internal/placement/tailnets.go", "b391983d0abb59c9", "placement CreateTailnet: deny-all policy in the transaction that creates the PENDING tailnet (AGENTS rule 8); former file allowlist entry"},

	// Placement counts (P10 quota, admission, moves): numbers only.
	{"internal/placement/clusters.go", "c546dae73e9f8bdd", "clusterUsage: tailnets and devices per owning cluster, counts only"},
	{"internal/placement/clusters.go", "f51361bbeedab5f9", "clusterUsage with incoming moves (P12c C3): tailnets and devices per owning or moving-to cluster, counts only"},
	{"internal/placement/quota.go", "d73f7920873331f1", "device budget of a cluster incl. tailnets moving in (P12c C3): a count"},
	{"internal/placement/tailnets.go", "672991bdea3b832f", "admission incl. tailnets moving in, excluding the moving one (P12c C3): counts only"},
	{"internal/placement/tailnets.go", "ff223c04e2408d54", "deviceCount: live devices of one tailnet, a count"},

	// Unresolved SQL on platform tables: the dynamic part is a clock
	// expression or a fixed fragment, never input.
	{"hscontrol/db/handover.go", "4e234e783ab44271", "TakeOver CAS on tailnets (platform table), dedicated-target guard (P12c C3); dbNow is the dialect's now(); EXISTS reads cluster_leases"},
	{"hscontrol/db/handover.go", "5c79111541ae4436", "HandOver CAS on tailnets (platform table), dedicated-target guard (P12c C3); dbNow is the dialect's now(); EXISTS reads cluster_leases"},
	{"hscontrol/db/noisekey.go", "fb8701509d049b66", "cluster Noise key registration on clusters (platform table) under the lease; dbNow is the dialect's now()"},
	{"hscontrol/db/tenantscope/tenantscope.go", "6d4127be02fd74d7", "LockOwnership: SET LOCAL lock_timeout from an integer duration, no table"},
	{"internal/lease/lease.go", "1908e1bbb6a4294c", "lease Get on cluster_leases (platform table); now() is the dialect's clock"},
	{"internal/lease/lease.go", "560e0bc57dc77763", "lease Renew on cluster_leases; now()/nowPlus() are the dialect's clock"},
	{"internal/lease/lease.go", "8f109ce712d23d27", "lease Release on cluster_leases; now() is the dialect's clock"},
	{"internal/lease/lease.go", "5f6903d7977cd4c8", "lease acquire upsert on cluster_leases; q embeds the dialect's clock (P12c B1)"},
	{"internal/lease/lease.go", "2c879f711d09f405", "lease Acquire: lock-free liveness read on cluster_leases (P12c B1); now() is the dialect's clock"},
	{"internal/lease/lease.go", "5e553bace6cb0f8a", "lease Acquire: SET LOCAL lock_timeout from an integer millisecond count, no table (P12c B1)"},
	{"internal/placement/moves.go", "40a70a65947699db", "queryMoves on tailnet_moves (platform table); where is a fixed condition of the callers in moves.go"},
}
