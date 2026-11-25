package db

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// TailnetMovesMigrationID adds tailnet_moves (P12b, owner decision Q2,
// 2026-11-26): the schedule of planned tailnet moves between clusters. A row
// is SCHEDULED until mesh-placement requests the move at scheduled_at
// (placement.Registry.MoveOwner, ADR 0010), REQUESTED until the owner cluster
// hands the tailnet over, then HANDED_OVER; or CANCELLED / FAILED. At most
// one open (SCHEDULED or REQUESTED) move per tailnet. A platform table: it is
// read and written with explicit SQL only (its tailnet_id names the tailnet
// being moved, not a tenant scope).
const TailnetMovesMigrationID = "202611261000-tailnet-moves"

// TailnetMovesSQLiteDDL and TailnetMovesPostgresDDL are appended verbatim to
// schema.sql / schema_postgres.sql.
const TailnetMovesSQLiteDDL = `CREATE TABLE tailnet_moves(
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
`

const TailnetMovesPostgresDDL = `CREATE TABLE tailnet_moves(
  id text PRIMARY KEY,
  tailnet_id text NOT NULL REFERENCES tailnets(id),
  from_cluster_id text NOT NULL,
  to_cluster_id text NOT NULL,
  from_epoch bigint NOT NULL CHECK(from_epoch >= 1),
  scheduled_at timestamptz NOT NULL,
  reason text NOT NULL DEFAULT '',
  state text NOT NULL CHECK(state IN ('SCHEDULED','REQUESTED','HANDED_OVER','CANCELLED','FAILED')),
  error text NOT NULL DEFAULT '',
  requested_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX idx_tailnet_moves_open ON tailnet_moves(tailnet_id) WHERE state IN ('SCHEDULED','REQUESTED');
CREATE INDEX idx_tailnet_moves_due ON tailnet_moves(state, scheduled_at);
`

func tailnetMovesMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: TailnetMovesMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := TailnetMovesSQLiteDDL
				if tx.Name() == "postgres" {
					ddl = TailnetMovesPostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`DROP TABLE IF EXISTS tailnet_moves`).Error
			})
		},
	}
}
