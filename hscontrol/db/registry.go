package db

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// The registry (P10, PLAN §4 "Registry"): platform tables in the shared
// database, read and written only by the placement service
// (internal/placement, cmd/mesh-placement) and never by a tenant request.
//
//   - clusters: one row per cluster (endpoint, shard class, state, Noise
//     public key for cross-checking, device budget). revision only grows.
//   - platform_audit: one row per operator mutation (and per refused or
//     replayed one).
//   - platform_idempotency: the stored result of each mutation made with an
//     idempotency key, written in the mutation's own transaction.
//   - trg_tailnets_owner_epoch_monotonic: owner_epoch never decreases, and
//     owner_cluster_id never changes without owner_epoch growing (no A→B→A
//     with the same epoch), whatever code path issues the UPDATE (I12, I14).
//
// tailnets.owner_cluster_id has no foreign key to clusters: databases created
// before P10 carry cluster ids (e.g. "local") that are not registered.
const registryMigrationID = "202611231200-registry-clusters-audit"

// The DDL is byte-identical to the tail of schema.sql / schema_postgres.sql
// (squibble compares the SQL stored by SQLite; TestRegistryDDLInSchemaFiles
// checks both files).
const (
	registrySQLiteDDL = `CREATE TABLE clusters(
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
);`

	registryPostgresDDL = `CREATE TABLE clusters(
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
);`
)

func registryMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: registryMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := registrySQLiteDDL
				if tx.Name() == "postgres" {
					ddl = registryPostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				stmts := []string{
					`DROP TRIGGER IF EXISTS trg_tailnets_owner_epoch_monotonic`,
					`DROP TRIGGER IF EXISTS trg_clusters_revision_monotonic`,
				}
				if tx.Name() == "postgres" {
					stmts = []string{
						`DROP TRIGGER IF EXISTS trg_tailnets_owner_epoch_monotonic ON tailnets`,
						`DROP FUNCTION IF EXISTS tailnets_owner_epoch_monotonic()`,
					}
				}

				stmts = append(stmts,
					`DROP TABLE IF EXISTS platform_idempotency`,
					`DROP TABLE IF EXISTS platform_audit`,
					`DROP TABLE IF EXISTS clusters`,
				)
				if tx.Name() == "postgres" {
					stmts = append(stmts, `DROP FUNCTION IF EXISTS clusters_revision_monotonic()`)
				}

				for _, s := range stmts {
					if err := tx.Exec(s).Error; err != nil { //nolint:noinlineerr
						return err
					}
				}

				return nil
			})
		},
	}
}

// RegistryDB returns the platform's pool for the placement service
// (internal/placement). It is unscoped: statements on tenant tables need a
// tenantscope scope, so placement reaches them only through reviewed raw
// statements that name their tailnet (the deny-all policy of a new tailnet)
// or return counts only (devices per tailnet / per cluster). Never hand it to
// code serving a tenant request.
func (p *Platform) RegistryDB() *gorm.DB { return p.db }
