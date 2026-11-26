//go:build p05cluster

package servertest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// P13d (ADR 0013 §1): a cluster process never creates or migrates the
// schema at start. On a database without the schema it refuses to start
// with the cause and the migrate command to run, and leaves the database
// as it found it.
func TestP13dClusterDoesNotMigrateAtStart(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "a"})

	path := filepath.Join(t.TempDir(), "unmigrated.db")

	err := tc.NewClusterOn(t, path)
	require.ErrorIs(t, err, db.ErrSchemaNeedsMigration, "cluster started on a database without the schema")
	assert.Contains(t, err.Error(), "mesh-placement migrate")

	// Nothing was created: the file is absent or holds no table.
	_, statErr := os.Stat(path)
	if statErr == nil {
		conn, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)

		var n int64
		require.NoError(t, conn.Raw(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&n).Error)
		assert.Zero(t, n, "cluster created tables at start")

		sqlDB, err := conn.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	}
}
