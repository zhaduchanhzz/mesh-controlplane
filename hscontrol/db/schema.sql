-- mesh-controlplane schema v0.1 (SQLite form, used for development and unit
-- tests). Production uses schema_postgres.sql on the shared PostgreSQL
-- (ADR 0005); the two must stay equivalent.
--
-- This file is executed verbatim to create a fresh database and is also the
-- squibble source of truth the database is validated against.
--
-- Tenant isolation (ADR 0005, application layer only):
--   - every tenant table carries tailnet_id NOT NULL;
--   - the tenantscope GORM plugin confines statements to one tailnet;
--   - composite foreign keys / triggers stop cross-tailnet references even
--     when application code is wrong.

CREATE TABLE migrations(id text,PRIMARY KEY(id));

-- One row per tailnet. owner_cluster_id/owner_epoch fence the cluster that
-- may serve and mutate the tailnet (I12): every mutation checks both.
CREATE TABLE tailnets(
  id text PRIMARY KEY,
  slug text NOT NULL,
  status text NOT NULL DEFAULT 'PENDING' CHECK(status IN ('PENDING','ACTIVE','SUSPENDED','DELETING')),
  owner_cluster_id text NOT NULL,
  owner_epoch integer NOT NULL DEFAULT 1 CHECK(owner_epoch >= 1),
  policy_revision integer NOT NULL DEFAULT 1,
  config_revision integer NOT NULL DEFAULT 1,
  created_at datetime NOT NULL,
  updated_at datetime NOT NULL,
  deleted_at datetime,
  moving_to_cluster_id text
);
CREATE UNIQUE INDEX idx_tailnets_slug ON tailnets(slug);
CREATE INDEX idx_tailnets_owner ON tailnets(owner_cluster_id, status);

CREATE TABLE users(
  id integer PRIMARY KEY AUTOINCREMENT,
  tailnet_id text NOT NULL,
  name text,
  display_name text,
  email text,
  provider_identifier text,
  provider text,
  profile_pic_url text,

  created_at datetime,
  updated_at datetime,
  deleted_at datetime,

  CONSTRAINT fk_users_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);
-- Target of composite foreign keys (tailnet_id, user_id).
CREATE UNIQUE INDEX idx_users_tailnet_id_id ON users(tailnet_id, id);
CREATE INDEX idx_users_deleted_at ON users(deleted_at);

-- The user identity model, now per tailnet:
-- 1. Users are local (provider_identifier NULL) or external.
-- 2. An external provider identifier is unique within a tailnet.
-- 3. A local username is unique among the tailnet's local users.
-- 4. The same name may exist in different tailnets and across providers.
CREATE UNIQUE INDEX idx_provider_identifier ON users(tailnet_id, provider_identifier) WHERE provider_identifier IS NOT NULL;
CREATE UNIQUE INDEX idx_name_provider_identifier ON users(tailnet_id, name, provider_identifier);
CREATE UNIQUE INDEX idx_name_no_provider_identifier ON users(tailnet_id, name) WHERE provider_identifier IS NULL;

-- Unified store for every authenticatable secret (API keys, pre-auth keys,
-- OAuth clients and access tokens), discriminated by kind. Only a hash of the
-- secret is stored; identifier is the public lookup value, unique within a
-- kind (random, so global uniqueness is harmless; lookups are scoped).
CREATE TABLE credentials(
  id integer PRIMARY KEY AUTOINCREMENT,
  tailnet_id text NOT NULL,
  kind text NOT NULL CHECK(kind IN ('api','authkey','oauth_client','oauth_token')),
  identifier text,
  hash blob,
  user_id integer,
  description text,
  scopes text,
  tags text,
  reusable numeric,
  ephemeral numeric DEFAULT false,
  used numeric DEFAULT false,
  last_seen datetime,
  client_id text,
  created_at datetime,
  expiration datetime,
  revoked datetime,
  created_by_token numeric DEFAULT false,

  CONSTRAINT fk_credentials_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id),
  -- ON DELETE SET NULL cannot be a composite key: it would null tailnet_id.
  -- The tailnet match is enforced by the triggers below instead.
  CONSTRAINT fk_credentials_user FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE SET NULL,
  CONSTRAINT chk_credentials_hash CHECK(hash IS NOT NULL OR revoked IS NOT NULL),
  CONSTRAINT chk_credentials_hash_format CHECK(hash IS NULL OR substr(CAST(hash AS TEXT), 1, 1) = '$')
);
CREATE UNIQUE INDEX idx_credentials_identifier ON credentials(kind, identifier);
CREATE INDEX idx_credentials_user_id ON credentials(user_id);
CREATE UNIQUE INDEX idx_credentials_tailnet_id_id ON credentials(tailnet_id, id);

CREATE TRIGGER trg_credentials_user_tailnet_insert BEFORE INSERT ON credentials
WHEN NEW.user_id IS NOT NULL AND (SELECT tailnet_id FROM users WHERE id = NEW.user_id) IS NOT NEW.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'credential user belongs to another tailnet');
END;
CREATE TRIGGER trg_credentials_user_tailnet_update BEFORE UPDATE OF user_id, tailnet_id ON credentials
WHEN NEW.user_id IS NOT NULL AND (SELECT tailnet_id FROM users WHERE id = NEW.user_id) IS NOT NEW.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'credential user belongs to another tailnet');
END;

CREATE TABLE nodes(
  id integer PRIMARY KEY AUTOINCREMENT,
  tailnet_id text NOT NULL,
  machine_key text,
  node_key text,
  disco_key text,

  endpoints text,
  host_info text,
  ipv4 text,
  ipv6 text,
  last_control_address text,
  hostname text,
  given_name varchar(63),
  -- user_id is NULL for tagged nodes (owned by tags, not a user).
  -- Only set for user-owned nodes (no tags).
  user_id integer,
  register_method text,
  tags text,
  auth_key_id integer,
  ephemeral numeric DEFAULT false,
  last_seen datetime,
  expiry datetime,
  key_expiry_disabled numeric DEFAULT false,
  approved_routes text,
  auto_approved_routes text,

  created_at datetime,
  updated_at datetime,
  deleted_at datetime,

  CONSTRAINT fk_nodes_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id),
  CONSTRAINT fk_nodes_user FOREIGN KEY(tailnet_id, user_id) REFERENCES users(tailnet_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_nodes_auth_key FOREIGN KEY(tailnet_id, auth_key_id) REFERENCES credentials(tailnet_id, id)
);
CREATE INDEX idx_nodes_auth_key_id ON nodes(auth_key_id);
-- Not unique: upstream lets one machine key register as different users of
-- the same tailnet (NodeStore nodesByMachineKey[mk][userID]).
CREATE INDEX idx_nodes_tailnet_machine_key ON nodes(tailnet_id, machine_key);
CREATE INDEX idx_nodes_tailnet_node_key ON nodes(tailnet_id, node_key);
-- An overlay address belongs to one node of a tailnet; two tailnets are two
-- overlays and may reuse it. NULL (no address of that family) may repeat
-- (P06, migration 202611241200-nodes-ip-unique-per-tailnet).
CREATE UNIQUE INDEX idx_nodes_tailnet_ipv4 ON nodes(tailnet_id, ipv4) WHERE ipv4 IS NOT NULL;
CREATE UNIQUE INDEX idx_nodes_tailnet_ipv6 ON nodes(tailnet_id, ipv6) WHERE ipv6 IS NOT NULL;
-- A (machine_key, node_key) pair resolves a Noise session to exactly one
-- tailnet (ADR 0007 §2): it may not appear in two tailnets. Within one tailnet
-- upstream keeps several rows for it (re-login as another user), so this is a
-- trigger, not a unique index. Zero keys (nodes created without keys) are
-- never resolved and are exempt.
CREATE INDEX idx_nodes_machine_key_node_key ON nodes(machine_key, node_key);
CREATE TRIGGER trg_nodes_key_pair_tailnet_insert BEFORE INSERT ON nodes
WHEN NEW.machine_key <> 'mkey:0000000000000000000000000000000000000000000000000000000000000000' AND NEW.node_key <> 'nodekey:0000000000000000000000000000000000000000000000000000000000000000'
  AND EXISTS (SELECT 1 FROM nodes WHERE machine_key = NEW.machine_key AND node_key = NEW.node_key AND tailnet_id <> NEW.tailnet_id)
BEGIN
  SELECT RAISE(ABORT, 'node key pair belongs to another tailnet');
END;
CREATE TRIGGER trg_nodes_key_pair_tailnet_update BEFORE UPDATE OF machine_key, node_key, tailnet_id ON nodes
WHEN NEW.machine_key <> 'mkey:0000000000000000000000000000000000000000000000000000000000000000' AND NEW.node_key <> 'nodekey:0000000000000000000000000000000000000000000000000000000000000000'
  AND EXISTS (SELECT 1 FROM nodes WHERE machine_key = NEW.machine_key AND node_key = NEW.node_key AND tailnet_id <> NEW.tailnet_id AND id <> NEW.id)
BEGIN
  SELECT RAISE(ABORT, 'node key pair belongs to another tailnet');
END;

CREATE TABLE policies(
  id integer PRIMARY KEY AUTOINCREMENT,
  tailnet_id text NOT NULL,
  data text,

  created_at datetime,
  updated_at datetime,
  deleted_at datetime,

  CONSTRAINT fk_policies_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);
CREATE INDEX idx_policies_deleted_at ON policies(deleted_at);
CREATE INDEX idx_policies_tailnet_id ON policies(tailnet_id);

CREATE TABLE database_versions(
  id integer PRIMARY KEY,
  version text NOT NULL,
  updated_at datetime
);

-- Registry (P10): platform tables, see hscontrol/db/registry.go.
CREATE TABLE clusters(
  id text PRIMARY KEY,
  endpoint_url text NOT NULL,
  class text NOT NULL CHECK(class IN ('shared','dedicated')),
  state text NOT NULL DEFAULT 'DISABLED' CHECK(state IN ('ACTIVE','DRAINING','DISABLED')),
  noise_public_key text,
  max_devices integer NOT NULL CHECK(max_devices > 0),
  revision integer NOT NULL DEFAULT 1 CHECK(revision >= 1),
  created_at datetime NOT NULL,
  updated_at datetime NOT NULL
);
CREATE UNIQUE INDEX idx_clusters_endpoint_url ON clusters(endpoint_url);
CREATE TRIGGER trg_clusters_revision_monotonic BEFORE UPDATE OF revision ON clusters
WHEN NEW.revision < OLD.revision
BEGIN
  SELECT RAISE(ABORT, 'cluster revision must not decrease');
END;
CREATE TRIGGER trg_tailnets_owner_epoch_monotonic BEFORE UPDATE OF owner_epoch, owner_cluster_id ON tailnets
WHEN NEW.owner_epoch < OLD.owner_epoch OR (NEW.owner_cluster_id <> OLD.owner_cluster_id AND NEW.owner_epoch <= OLD.owner_epoch)
BEGIN
  SELECT RAISE(ABORT, 'owner_epoch must grow with an owner change and never decrease');
END;
CREATE TABLE platform_audit(
  id integer PRIMARY KEY AUTOINCREMENT,
  at datetime NOT NULL,
  actor text NOT NULL,
  action text NOT NULL,
  target text NOT NULL,
  outcome text NOT NULL CHECK(outcome IN ('ok','replay','error')),
  detail text,
  idempotency_key text
);
CREATE INDEX idx_platform_audit_at ON platform_audit(at);
CREATE TABLE platform_idempotency(
  actor text NOT NULL,
  idempotency_key text NOT NULL,
  operation text NOT NULL,
  request_hash text NOT NULL,
  response text NOT NULL,
  created_at datetime NOT NULL,
  PRIMARY KEY(actor, idempotency_key)
);

-- Per-tailnet settings (P06, ADR 0008): DNS settings of a tailnet as
-- validated JSON; see hscontrol/db/tailnet_settings.go.
CREATE TABLE tailnet_settings(
  tailnet_id text PRIMARY KEY,
  dns_config text,
  CONSTRAINT fk_tailnet_settings_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);

-- Cluster lease (P11b): the process allowed to serve each cluster;
-- see internal/lease.
CREATE TABLE cluster_leases(
  cluster_id text PRIMARY KEY,
  holder text NOT NULL,
  epoch integer NOT NULL CHECK(epoch >= 1),
  expires_at datetime NOT NULL,
  updated_at datetime NOT NULL
);
CREATE TRIGGER trg_cluster_leases_epoch_monotonic BEFORE UPDATE OF epoch, holder ON cluster_leases
WHEN NEW.epoch < OLD.epoch OR (NEW.holder <> OLD.holder AND NEW.epoch <= OLD.epoch)
BEGIN
  SELECT RAISE(ABORT, 'lease epoch must grow with a holder change and never decrease');
END;

-- Planned tailnet moves (P12b): see hscontrol/db/tailnet_moves.go.
CREATE TABLE tailnet_moves(
  id text PRIMARY KEY,
  tailnet_id text NOT NULL,
  from_cluster_id text NOT NULL,
  to_cluster_id text NOT NULL,
  from_epoch integer NOT NULL CHECK(from_epoch >= 1),
  scheduled_at datetime NOT NULL,
  reason text NOT NULL DEFAULT '',
  state text NOT NULL CHECK(state IN ('SCHEDULED','REQUESTED','HANDED_OVER','CANCELLED','FAILED')),
  error text NOT NULL DEFAULT '',
  requested_at datetime,
  finished_at datetime,
  created_at datetime NOT NULL,
  updated_at datetime NOT NULL,
  CONSTRAINT fk_tailnet_moves_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);
CREATE UNIQUE INDEX idx_tailnet_moves_open ON tailnet_moves(tailnet_id) WHERE state IN ('SCHEDULED','REQUESTED');
CREATE INDEX idx_tailnet_moves_due ON tailnet_moves(state, scheduled_at);

-- tailnet_id of a tenant row never changes (P12c A8): see
-- hscontrol/db/tenant_immutable.go.
CREATE TRIGGER trg_users_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON users
WHEN NEW.tailnet_id IS NOT OLD.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'tailnet_id of a tenant row is immutable');
END;
CREATE TRIGGER trg_credentials_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON credentials
WHEN NEW.tailnet_id IS NOT OLD.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'tailnet_id of a tenant row is immutable');
END;
CREATE TRIGGER trg_nodes_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON nodes
WHEN NEW.tailnet_id IS NOT OLD.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'tailnet_id of a tenant row is immutable');
END;
CREATE TRIGGER trg_policies_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON policies
WHEN NEW.tailnet_id IS NOT OLD.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'tailnet_id of a tenant row is immutable');
END;
CREATE TRIGGER trg_tailnet_settings_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON tailnet_settings
WHEN NEW.tailnet_id IS NOT OLD.tailnet_id
BEGIN
  SELECT RAISE(ABORT, 'tailnet_id of a tenant row is immutable');
END;

-- Schema compatibility (P13d, ADR 0013): min_reader_id of each migration
-- from 202611271000-schema-compat on; see hscontrol/db/schema_compat.go.
CREATE TABLE schema_compat(
  migration_id text PRIMARY KEY,
  min_reader_id text NOT NULL
);
