package db

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// movingMigrationID adds tailnets.moving_to_cluster_id (P11, owner decision
// 2026-11-23). A tailnet being moved to another cluster (P12b) names its
// target there; bootstrap refuses such a tailnet (ADR 0005 Bổ sung 2 §1,
// "MOVING"). It is a column rather than a status value because SQLite
// cannot change the CHECK of tailnets without rebuilding the table with
// foreign keys off, which hscontrol/db/AGENTS.md forbids.
const movingMigrationID = "202611232000-tailnets-moving-to"

func movingMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: movingMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`ALTER TABLE tailnets ADD COLUMN moving_to_cluster_id text`).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`ALTER TABLE tailnets DROP COLUMN moving_to_cluster_id`).Error
			})
		},
	}
}
