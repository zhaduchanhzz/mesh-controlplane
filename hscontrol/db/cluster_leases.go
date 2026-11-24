package db

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"gorm.io/gorm"
)

// clusterLeasesMigration creates cluster_leases (P11b): one row per cluster
// naming the process allowed to serve it, with an epoch that only grows and
// an expiry set by the database clock. See internal/lease. The DDL lives in
// internal/lease and is appended to schema.sql / schema_postgres.sql, so a
// fresh database gets it from the schema files.
func clusterLeasesMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: lease.MigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := lease.SQLiteDDL
				if tx.Name() == "postgres" {
					ddl = lease.PostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				err := tx.Exec(`DROP TABLE IF EXISTS cluster_leases`).Error
				if err == nil && tx.Name() == "postgres" {
					err = tx.Exec(`DROP FUNCTION IF EXISTS cluster_leases_epoch_monotonic()`).Error
				}

				return err
			})
		},
	}
}
