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
  deleted_at datetime
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
-- Resolves a Noise session to exactly one tailnet (ADR 0007 §2): a (machine,
-- node) key pair is unique across tailnets. One machine key with several node
-- keys (one profile per tailnet) stays allowed. Zero keys (nodes created
-- without keys) are never resolved and stay outside the index.
CREATE UNIQUE INDEX idx_nodes_machine_key_node_key ON nodes(machine_key, node_key) WHERE machine_key <> 'mkey:0000000000000000000000000000000000000000000000000000000000000000' AND node_key <> 'nodekey:0000000000000000000000000000000000000000000000000000000000000000';

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
