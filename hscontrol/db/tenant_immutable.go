package db

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// TenantIDImmutableMigrationID adds a BEFORE UPDATE OF tailnet_id trigger
// on every tenant table (P12c A8): the tailnet of a user, node, credential,
// policy or DNS settings row never changes, whatever SQL tries. The
// application-layer check is tenantscope's (P12c A3); this is the backstop
// when SQL bypasses it. A tailnet moves between clusters by its owner
// (tailnets.owner_cluster_id/owner_epoch), never by rewriting its rows.
const TenantIDImmutableMigrationID = "202611261200-tenant-id-immutable"

// TenantIDImmutableSQLiteDDL and TenantIDImmutablePostgresDDL are appended
// verbatim to schema.sql / schema_postgres.sql.
const TenantIDImmutableSQLiteDDL = `CREATE TRIGGER trg_users_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON users
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
`

const TenantIDImmutablePostgresDDL = `CREATE FUNCTION tenant_tailnet_id_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.tailnet_id IS DISTINCT FROM OLD.tailnet_id THEN
    RAISE EXCEPTION 'tailnet_id of a tenant row is immutable' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER trg_users_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON users
  FOR EACH ROW EXECUTE FUNCTION tenant_tailnet_id_immutable();
CREATE TRIGGER trg_credentials_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON credentials
  FOR EACH ROW EXECUTE FUNCTION tenant_tailnet_id_immutable();
CREATE TRIGGER trg_nodes_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON nodes
  FOR EACH ROW EXECUTE FUNCTION tenant_tailnet_id_immutable();
CREATE TRIGGER trg_policies_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON policies
  FOR EACH ROW EXECUTE FUNCTION tenant_tailnet_id_immutable();
CREATE TRIGGER trg_tailnet_settings_tailnet_id_immutable BEFORE UPDATE OF tailnet_id ON tailnet_settings
  FOR EACH ROW EXECUTE FUNCTION tenant_tailnet_id_immutable();
`

// tenantIDImmutableSQLiteRollback and tenantIDImmutablePostgresRollback undo
// the migration.
const tenantIDImmutableSQLiteRollback = `DROP TRIGGER IF EXISTS trg_users_tailnet_id_immutable;
DROP TRIGGER IF EXISTS trg_credentials_tailnet_id_immutable;
DROP TRIGGER IF EXISTS trg_nodes_tailnet_id_immutable;
DROP TRIGGER IF EXISTS trg_policies_tailnet_id_immutable;
DROP TRIGGER IF EXISTS trg_tailnet_settings_tailnet_id_immutable;
`

const tenantIDImmutablePostgresRollback = `DROP TRIGGER IF EXISTS trg_users_tailnet_id_immutable ON users;
DROP TRIGGER IF EXISTS trg_credentials_tailnet_id_immutable ON credentials;
DROP TRIGGER IF EXISTS trg_nodes_tailnet_id_immutable ON nodes;
DROP TRIGGER IF EXISTS trg_policies_tailnet_id_immutable ON policies;
DROP TRIGGER IF EXISTS trg_tailnet_settings_tailnet_id_immutable ON tailnet_settings;
DROP FUNCTION IF EXISTS tenant_tailnet_id_immutable();
`

func tenantIDImmutableMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: TenantIDImmutableMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := TenantIDImmutableSQLiteDDL
				if tx.Name() == dialectPostgres {
					ddl = TenantIDImmutablePostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := tenantIDImmutableSQLiteRollback
				if tx.Name() == dialectPostgres {
					ddl = tenantIDImmutablePostgresRollback
				}

				return tx.Exec(ddl).Error
			})
		},
	}
}
