package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

func hasMovingColumn(t *testing.T, conn *gorm.DB) bool {
	t.Helper()

	return conn.Migrator().HasColumn(&types.Tailnet{}, "moving_to_cluster_id")
}

// TestMigrationTailnetsMovingTo (P11): a fresh database has
// tailnets.moving_to_cluster_id; a database from before the migration is
// upgraded on open, keeps its rows, and then matches the schema files
// (OpenPlatform validates SQLite against schema.sql with squibble, so a
// mismatch fails the reopen). The column is nil until a move sets it.
func TestMigrationTailnetsMovingTo(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")

			p := openPlatform(t, cfg)
			require.True(t, hasMovingColumn(t, p.db), "a fresh database has the column")

			id := platformTailnet(t, p, "local", types.TailnetActive, false)

			// Rewind to before the migration.
			require.NoError(t, movingMigration().Rollback(p.db))
			require.NoError(t, p.db.Exec(`DELETE FROM migrations WHERE id = ?`, movingMigrationID).Error)
			require.False(t, hasMovingColumn(t, p.db))
			require.NoError(t, p.Close())

			p = openPlatform(t, cfg)
			require.True(t, hasMovingColumn(t, p.db), "upgraded on open")

			var ids []string
			require.NoError(t, p.db.Table("migrations").Pluck("id", &ids).Error)
			assert.Contains(t, ids, movingMigrationID)

			var tn types.Tailnet
			require.NoError(t, p.db.Where("id = ?", id.String()).Take(&tn).Error, "rows survive")
			assert.Nil(t, tn.MovingToClusterID)

			target := "c2"
			require.NoError(t, p.db.Model(&types.Tailnet{}).Where("id = ?", id.String()).
				Update("moving_to_cluster_id", target).Error)
			require.NoError(t, p.db.Where("id = ?", id.String()).Take(&tn).Error)
			require.NotNil(t, tn.MovingToClusterID)
			assert.Equal(t, target, *tn.MovingToClusterID)
		})
	}
}
