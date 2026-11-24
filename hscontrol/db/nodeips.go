package db

import (
	"errors"
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// nodeIPMigrationID makes an overlay address unique within a tailnet (P06,
// owner decision 2026-11-24). Before it only the in-memory allocator of the
// tailnet's runtime kept two nodes off one address (AGENTS 7: a database
// constraint is the second line). Each tailnet is its own overlay, so two
// tailnets may hold the same address (TestClusterOverlapAndConfigIndependence
// names_and_ipam_independent): the key is (tailnet_id, address). A node
// without an address of a family stores NULL there, which may repeat.
const (
	nodeIPMigrationID   = "202611241200-nodes-ip-unique-per-tailnet"
	idxNodesTailnetIPv4 = "idx_nodes_tailnet_ipv4"
	idxNodesTailnetIPv6 = "idx_nodes_tailnet_ipv6"
)

var errDuplicateNodeIPs = errors.New("an overlay address is held by several nodes of one tailnet")

// nodeIPDDL is byte-identical in schema.sql and schema_postgres.sql
// (squibble compares the SQL stored by SQLite; TestNodeIPDDLInSchemaFiles
// checks both files). Partial unique indexes have the same syntax and
// meaning on both dialects.
const nodeIPDDL = `CREATE UNIQUE INDEX idx_nodes_tailnet_ipv4 ON nodes(tailnet_id, ipv4) WHERE ipv4 IS NOT NULL;
CREATE UNIQUE INDEX idx_nodes_tailnet_ipv6 ON nodes(tailnet_id, ipv6) WHERE ipv6 IS NOT NULL;`

func nodeIPMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: nodeIPMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				// Refuse with a clear error and leave the database
				// unchanged: which node keeps a shared address is an
				// operator decision, never made by dropping rows here.
				var dups int64

				err := tx.Raw(`SELECT
					(SELECT count(*) FROM (SELECT 1 FROM nodes WHERE ipv4 IS NOT NULL
						GROUP BY tailnet_id, ipv4 HAVING count(*) > 1) d4) +
					(SELECT count(*) FROM (SELECT 1 FROM nodes WHERE ipv6 IS NOT NULL
						GROUP BY tailnet_id, ipv6 HAVING count(*) > 1) d6)`).Scan(&dups).Error
				if err != nil {
					return fmt.Errorf("checking node addresses: %w", err)
				}

				if dups > 0 {
					return fmt.Errorf("%w: %d addresses; readdress or remove those nodes before upgrading",
						errDuplicateNodeIPs, dups)
				}

				return tx.Exec(nodeIPDDL).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				for _, idx := range []string{idxNodesTailnetIPv4, idxNodesTailnetIPv6} {
					if err := tx.Exec(`DROP INDEX IF EXISTS ` + idx).Error; err != nil { //nolint:noinlineerr
						return err
					}
				}

				return nil
			})
		},
	}
}
