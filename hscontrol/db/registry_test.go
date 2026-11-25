package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestRegistryDDLInSchemaFiles: the migration's DDL and the schema files
// define the same registry objects, so fresh and migrated databases match.
func TestRegistryDDLInSchemaFiles(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), registrySQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), registryPostgresDDL)
}

// TestRegistryMigrationFromPreviousChain: a database created before the
// registry migration gets it on open, ends up like a fresh one (squibble on
// SQLite), keeps its tailnets (unregistered owner ids included: no foreign
// key to clusters), and enforces the epoch trigger from then on.
func TestRegistryMigrationFromPreviousChain(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")

			p := openPlatform(t, cfg)
			id := platformTailnet(t, p, "local", types.TailnetActive, false)

			require.NoError(t, registryMigration().Rollback(p.db))
			require.NoError(t, p.db.Exec(`DELETE FROM migrations WHERE id = ?`, registryMigrationID).Error)

			present, err := tableExists(p.db, "clusters")
			require.NoError(t, err)
			require.False(t, present)

			// Before the migration an owner change without epoch passes.
			require.NoError(t, p.db.Exec(`UPDATE tailnets SET owner_cluster_id = 'elsewhere' WHERE id = ?`, id.String()).Error)
			require.NoError(t, p.db.Exec(`UPDATE tailnets SET owner_cluster_id = 'local' WHERE id = ?`, id.String()).Error)
			require.NoError(t, p.Close())

			p = openPlatform(t, cfg) // migrates; SQLite schema validated by squibble

			var ids []string
			require.NoError(t, p.db.Table("migrations").Pluck("id", &ids).Error)
			assert.Contains(t, ids, registryMigrationID)

			for _, table := range []string{"clusters", "platform_audit", "platform_idempotency"} {
				present, err := tableExists(p.db, table)
				require.NoError(t, err)
				assert.True(t, present, table)
			}

			tn, err := p.TailnetBySlug("tn-" + id.String()[:8])
			require.NoError(t, err)
			assert.Equal(t, "local", tn.OwnerClusterID)

			require.Error(t, p.db.Exec(`UPDATE tailnets SET owner_cluster_id = 'elsewhere' WHERE id = ?`, id.String()).Error)
			require.Error(t, p.db.Exec(`UPDATE tailnets SET owner_epoch = 0 WHERE id = ?`, id.String()).Error)
			require.NoError(t, p.db.Exec(`UPDATE tailnets SET owner_cluster_id = 'elsewhere', owner_epoch = owner_epoch + 1 WHERE id = ?`, id.String()).Error)
		})
	}
}
