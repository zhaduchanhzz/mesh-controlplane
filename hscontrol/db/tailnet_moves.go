package db

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
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

// MoveNotices returns, by tailnet ID, the open planned move (SCHEDULED or
// REQUESTED) of each ACTIVE tailnet clusterID owns and would move away (P12b,
// owner decision Q1): the time and the reason the tailnet's clients are told,
// never the target cluster. Explicit SQL: tailnet_moves is a platform table.
func (p *Platform) MoveNotices(clusterID string) (map[string]types.MoveNotice, error) {
	if clusterID == "" {
		return nil, errEmptyClusterID
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	rows, err := p.db.WithContext(ctx).Raw(`SELECT m.tailnet_id, m.scheduled_at, m.reason
		FROM tailnet_moves m JOIN tailnets t ON t.id = m.tailnet_id
		WHERE m.state IN ('SCHEDULED','REQUESTED') AND m.from_cluster_id = ?
		  AND t.owner_cluster_id = ? AND t.status = ? AND t.deleted_at IS NULL`,
		clusterID, clusterID, types.TailnetActive).Rows()
	if err != nil {
		return nil, fmt.Errorf("listing planned moves of cluster %s: %w", clusterID, err)
	}
	defer rows.Close()

	out := make(map[string]types.MoveNotice)

	for rows.Next() {
		var (
			id string
			n  types.MoveNotice
		)

		if err := rows.Scan(&id, &n.At, &n.Reason); err != nil { //nolint:noinlineerr
			return nil, fmt.Errorf("listing planned moves of cluster %s: %w", clusterID, err)
		}

		n.At = n.At.UTC()
		out[id] = n
	}

	if err := rows.Err(); err != nil { //nolint:noinlineerr
		return nil, fmt.Errorf("listing planned moves of cluster %s: %w", clusterID, err)
	}

	return out, nil
}
