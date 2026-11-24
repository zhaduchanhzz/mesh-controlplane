-- mesh-controlplane schema v0.1, PostgreSQL form: the production store shared
-- by every cluster (ADR 0005). Must stay equivalent to schema.sql (SQLite);
-- see the equivalence test in schema_test.go.

CREATE TABLE migrations(id text, PRIMARY KEY(id));

CREATE TABLE tailnets(
  id text PRIMARY KEY,
  slug text NOT NULL,
  status text NOT NULL DEFAULT 'PENDING' CHECK(status IN ('PENDING','ACTIVE','SUSPENDED','DELETING')),
  owner_cluster_id text NOT NULL,
  owner_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_epoch >= 1),
  policy_revision bigint NOT NULL DEFAULT 1,
  config_revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  deleted_at timestamptz,
  moving_to_cluster_id text
);
CREATE UNIQUE INDEX idx_tailnets_slug ON tailnets(slug);
CREATE INDEX idx_tailnets_owner ON tailnets(owner_cluster_id, status);

CREATE TABLE users(
  id bigserial PRIMARY KEY,
  tailnet_id text NOT NULL,
  name text,
  display_name text,
  email text,
  provider_identifier text,
  provider text,
  profile_pic_url text,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  CONSTRAINT fk_users_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);
CREATE UNIQUE INDEX idx_users_tailnet_id_id ON users(tailnet_id, id);
CREATE INDEX idx_users_deleted_at ON users(deleted_at);
CREATE UNIQUE INDEX idx_provider_identifier ON users(tailnet_id, provider_identifier) WHERE provider_identifier IS NOT NULL;
CREATE UNIQUE INDEX idx_name_provider_identifier ON users(tailnet_id, name, provider_identifier);
CREATE UNIQUE INDEX idx_name_no_provider_identifier ON users(tailnet_id, name) WHERE provider_identifier IS NULL;

CREATE TABLE credentials(
  id bigserial PRIMARY KEY,
  tailnet_id text NOT NULL,
  kind text NOT NULL CHECK(kind IN ('api','authkey','oauth_client','oauth_token')),
  identifier text,
  hash bytea,
  user_id bigint,
  description text,
  scopes text,
  tags text,
  reusable boolean,
  ephemeral boolean DEFAULT false,
  used boolean DEFAULT false,
  last_seen timestamptz,
  client_id text,
  created_at timestamptz,
  expiration timestamptz,
  revoked timestamptz,
  created_by_token boolean DEFAULT false,
  CONSTRAINT fk_credentials_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id),
  CONSTRAINT fk_credentials_user FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE SET NULL,
  CONSTRAINT chk_credentials_hash CHECK(hash IS NOT NULL OR revoked IS NOT NULL),
  CONSTRAINT chk_credentials_hash_format CHECK(hash IS NULL OR substring(hash from 1 for 1) = '\x24'::bytea)
);
CREATE UNIQUE INDEX idx_credentials_identifier ON credentials(kind, identifier);
CREATE INDEX idx_credentials_user_id ON credentials(user_id);
CREATE UNIQUE INDEX idx_credentials_tailnet_id_id ON credentials(tailnet_id, id);

CREATE FUNCTION credentials_user_tailnet_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.user_id IS NOT NULL AND
     (SELECT tailnet_id FROM users WHERE id = NEW.user_id) IS DISTINCT FROM NEW.tailnet_id THEN
    RAISE EXCEPTION 'credential user belongs to another tailnet';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER trg_credentials_user_tailnet BEFORE INSERT OR UPDATE OF user_id, tailnet_id ON credentials
  FOR EACH ROW EXECUTE FUNCTION credentials_user_tailnet_check();

CREATE TABLE nodes(
  id bigserial PRIMARY KEY,
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
  user_id bigint,
  register_method text,
  tags text,
  auth_key_id bigint,
  ephemeral boolean DEFAULT false,
  last_seen timestamptz,
  expiry timestamptz,
  key_expiry_disabled boolean DEFAULT false,
  approved_routes text,
  auto_approved_routes text,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  CONSTRAINT fk_nodes_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id),
  CONSTRAINT fk_nodes_user FOREIGN KEY(tailnet_id, user_id) REFERENCES users(tailnet_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_nodes_auth_key FOREIGN KEY(tailnet_id, auth_key_id) REFERENCES credentials(tailnet_id, id)
);
CREATE INDEX idx_nodes_auth_key_id ON nodes(auth_key_id);
CREATE INDEX idx_nodes_tailnet_machine_key ON nodes(tailnet_id, machine_key);
CREATE INDEX idx_nodes_tailnet_node_key ON nodes(tailnet_id, node_key);
-- Overlay address unique within a tailnet (P06); see schema.sql.
CREATE UNIQUE INDEX idx_nodes_tailnet_ipv4 ON nodes(tailnet_id, ipv4) WHERE ipv4 IS NOT NULL;
CREATE UNIQUE INDEX idx_nodes_tailnet_ipv6 ON nodes(tailnet_id, ipv6) WHERE ipv6 IS NOT NULL;
-- One tailnet per (machine_key, node_key) pair (ADR 0007 §2); see schema.sql.
CREATE INDEX idx_nodes_machine_key_node_key ON nodes(machine_key, node_key);
CREATE FUNCTION nodes_key_pair_tailnet_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.machine_key <> 'mkey:0000000000000000000000000000000000000000000000000000000000000000' AND NEW.node_key <> 'nodekey:0000000000000000000000000000000000000000000000000000000000000000' THEN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.machine_key || '/' || NEW.node_key, 0));
    IF EXISTS (SELECT 1 FROM nodes WHERE machine_key = NEW.machine_key AND node_key = NEW.node_key
               AND tailnet_id <> NEW.tailnet_id AND id <> NEW.id) THEN
      RAISE EXCEPTION 'node key pair belongs to another tailnet' USING ERRCODE = 'unique_violation';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER trg_nodes_key_pair_tailnet BEFORE INSERT OR UPDATE OF machine_key, node_key, tailnet_id ON nodes
  FOR EACH ROW EXECUTE FUNCTION nodes_key_pair_tailnet_check();

CREATE TABLE policies(
  id bigserial PRIMARY KEY,
  tailnet_id text NOT NULL,
  data text,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  CONSTRAINT fk_policies_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);
CREATE INDEX idx_policies_deleted_at ON policies(deleted_at);
CREATE INDEX idx_policies_tailnet_id ON policies(tailnet_id);

CREATE TABLE database_versions(
  id integer PRIMARY KEY,
  version text NOT NULL,
  updated_at timestamptz
);

-- Registry (P10): platform tables, see hscontrol/db/registry.go.
CREATE TABLE clusters(
  id text PRIMARY KEY,
  endpoint_url text NOT NULL,
  class text NOT NULL CHECK(class IN ('shared','dedicated')),
  state text NOT NULL DEFAULT 'DISABLED' CHECK(state IN ('ACTIVE','DRAINING','DISABLED')),
  noise_public_key text,
  max_devices bigint NOT NULL CHECK(max_devices > 0),
  revision bigint NOT NULL DEFAULT 1 CHECK(revision >= 1),
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX idx_clusters_endpoint_url ON clusters(endpoint_url);
CREATE FUNCTION clusters_revision_monotonic() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.revision < OLD.revision THEN
    RAISE EXCEPTION 'cluster revision must not decrease' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER trg_clusters_revision_monotonic BEFORE UPDATE OF revision ON clusters
  FOR EACH ROW EXECUTE FUNCTION clusters_revision_monotonic();
CREATE FUNCTION tailnets_owner_epoch_monotonic() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.owner_epoch < OLD.owner_epoch OR
     (NEW.owner_cluster_id <> OLD.owner_cluster_id AND NEW.owner_epoch <= OLD.owner_epoch) THEN
    RAISE EXCEPTION 'owner_epoch must grow with an owner change and never decrease' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER trg_tailnets_owner_epoch_monotonic BEFORE UPDATE OF owner_epoch, owner_cluster_id ON tailnets
  FOR EACH ROW EXECUTE FUNCTION tailnets_owner_epoch_monotonic();
CREATE TABLE platform_audit(
  id bigserial PRIMARY KEY,
  at timestamptz NOT NULL,
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
  created_at timestamptz NOT NULL,
  PRIMARY KEY(actor, idempotency_key)
);
