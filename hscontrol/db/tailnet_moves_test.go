package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

func TestTailnetMovesDDLInSchemaFilesAndLast(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), TailnetMovesSQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), TailnetMovesPostgresDDL)
	assert.Equal(t, TailnetMovesMigrationID, LatestMigrationID(), "appended at the end of the v0.1 chain")
}

func insertMove(tx *gorm.DB, id, tailnetID, state string) error {
	now := time.Now().UTC()

	return tx.Exec(`INSERT INTO tailnet_moves (id, tailnet_id, from_cluster_id, to_cluster_id, from_epoch,
		scheduled_at, state, created_at, updated_at) VALUES (?, ?, 'c1', 'c2', 1, ?, ?, ?, ?)`,
		id, tailnetID, now, state, now, now).Error
}

// P12b Q2: a fresh database has tailnet_moves; one from before the migration
// is upgraded on open and keeps its rows; at most one open move per tailnet;
// a move names an existing tailnet.
func TestMigrationTailnetMoves(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")

			p := openPlatform(t, cfg)
			require.True(t, p.db.Migrator().HasTable("tailnet_moves"), "fresh database")

			id := platformTailnet(t, p, "local", types.TailnetActive, false)

			require.NoError(t, tailnetMovesMigration().Rollback(p.db))
			require.NoError(t, p.db.Exec(`DELETE FROM migrations WHERE id = ?`, TailnetMovesMigrationID).Error)
			require.False(t, p.db.Migrator().HasTable("tailnet_moves"))
			require.NoError(t, p.Close())

			p = openPlatform(t, cfg)
			require.True(t, p.db.Migrator().HasTable("tailnet_moves"), "upgraded on open")

			var n int64
			require.NoError(t, p.db.Raw(`SELECT count(*) FROM tailnets WHERE id = ?`, id.String()).Scan(&n).Error)
			assert.EqualValues(t, 1, n, "rows survive")

			require.NoError(t, insertMove(p.db, "m1", id.String(), "SCHEDULED"))
			require.Error(t, insertMove(p.db, "m2", id.String(), "REQUESTED"), "a second open move")
			require.NoError(t, insertMove(p.db, "m3", id.String(), "CANCELLED"), "closed moves do not count")
			require.Error(t, insertMove(p.db, "m4", id.String(), "LOST"), "unknown state")

			if b.name == "postgres" {
				require.Error(t, insertMove(p.db, "m5", "00000000-0000-4000-8000-00000000dead", "CANCELLED"), "unknown tailnet")
			}
		})
	}
}
