package db

import (
	"errors"
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"tailscale.com/types/key"
)

// A (machine_key, node_key) pair resolves a Noise session to exactly one
// tailnet (ADR 0007 §2), so the pair may not appear in two tailnets. A plain
// unique index on the pair would also forbid what upstream does inside one
// tailnet: a node that re-registers as another user with the same keys gets
// a new row and the old one is kept (TestAuthenticationFlows
// reauth_existing_node_different_user_*, TestPreAuthKeyLogoutAndReloginDifferentUser).
// The rule "one tailnet per pair" is therefore a trigger, next to a lookup
// index. Zero keys (nodes created without keys) are never resolved and are
// exempt.
const (
	nodeKeyPairMigrationID = "202611230000-nodes-key-pair-one-tailnet"
	idxNodesMachineNodeKey  = "idx_nodes_machine_key_node_key"
)

var errDuplicateNodeKeys = errors.New("a (machine_key, node_key) pair is used in several tailnets")

// zeroMachineKey and zeroNodeKey are how the text serialiser stores the zero
// keys; the DDL below spells them out.
var (
	zeroMachineKey = key.MachinePublic{}.String()
	zeroNodeKey    = key.NodePublic{}.String()
)

// The DDL is byte-identical to schema.sql / schema_postgres.sql (squibble
// compares the SQL stored by SQLite; TestNodeKeyPairDDLInSchemaFiles checks
// both files).
const (
	nodeKeyPairIndexDDL = `CREATE INDEX idx_nodes_machine_key_node_key ON nodes(machine_key, node_key);`

	nodeKeyPairSQLiteDDL = nodeKeyPairIndexDDL + `
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
END;`

	// The advisory lock serialises concurrent writers of one pair, so two
	// transactions cannot both pass the check under READ COMMITTED (each
	// statement of a volatile plpgsql function takes a fresh snapshot).
	nodeKeyPairPostgresDDL = nodeKeyPairIndexDDL + `
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
  FOR EACH ROW EXECUTE FUNCTION nodes_key_pair_tailnet_check();`
)

func nodeKeyPairMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: nodeKeyPairMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				// Refuse with a clear error and leave the database
				// unchanged: an operator must resolve pairs already used in
				// several tailnets (they make a Noise session ambiguous).
				var dups int64

				err := tx.Raw(`SELECT count(*) FROM (SELECT machine_key, node_key FROM nodes
					WHERE machine_key <> ? AND node_key <> ?
					GROUP BY machine_key, node_key HAVING count(DISTINCT tailnet_id) > 1) d`,
					zeroMachineKey, zeroNodeKey).Scan(&dups).Error
				if err != nil {
					return fmt.Errorf("checking node key pairs: %w", err)
				}

				if dups > 0 {
					return fmt.Errorf("%w: %d pairs; remove them before upgrading", errDuplicateNodeKeys, dups)
				}

				ddl := nodeKeyPairSQLiteDDL
				if tx.Name() == "postgres" {
					ddl = nodeKeyPairPostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				stmts := []string{
					`DROP TRIGGER IF EXISTS trg_nodes_key_pair_tailnet_insert`,
					`DROP TRIGGER IF EXISTS trg_nodes_key_pair_tailnet_update`,
					`DROP INDEX IF EXISTS ` + idxNodesMachineNodeKey,
				}
				if tx.Name() == "postgres" {
					stmts = []string{
						`DROP TRIGGER IF EXISTS trg_nodes_key_pair_tailnet ON nodes`,
						`DROP FUNCTION IF EXISTS nodes_key_pair_tailnet_check()`,
						`DROP INDEX IF EXISTS ` + idxNodesMachineNodeKey,
					}
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
