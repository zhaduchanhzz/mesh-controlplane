package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"tailscale.com/types/key"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestNodesMachineNodeKeyGloballyUnique: (machine_key, node_key) resolves a
// Noise session to exactly one tailnet (ADR 0007 §2), so the pair is unique
// across tailnets; one machine key with several node keys stays allowed.
func TestNodesMachineNodeKeyGloballyUnique(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			a := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))
			bdb := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))

			mk, nk := key.NewMachine().Public(), key.NewNode().Public()
			require.NoError(t, addNode(t, a, mk, nk))

			require.Error(t, addNode(t, bdb, mk, nk), "the same pair in another tailnet")
			require.Error(t, addNode(t, a, mk, nk), "the same pair twice in one tailnet")
			require.NoError(t, addNode(t, bdb, mk, key.NewNode().Public()), "same machine, another profile")
			require.NoError(t, addNode(t, a, mk, key.NewNode().Public()), "same machine, another node key in A")

			// Nodes created without keys (tests, pre-registration rows) are
			// outside the index; the platform never resolves them.
			var zeroM key.MachinePublic
			var zeroN key.NodePublic
			require.NoError(t, addNode(t, a, zeroM, zeroN))
			require.NoError(t, addNode(t, bdb, zeroM, zeroN))
		})
	}
}

// v01InitState rewinds a current database to how 202611220000-v0.1-init left
// it: without the unique key index and its migration record.
func v01InitState(t *testing.T, raw *gorm.DB) {
	t.Helper()

	require.NoError(t, raw.Exec(`DROP INDEX `+idxNodesMachineNodeKey).Error)
	require.NoError(t, raw.Exec(`DELETE FROM migrations WHERE id = ?`, nodesMachineNodeKeyMigration).Error)
}

// TestMigrationFromV01Init: a database created at the previous migration is
// upgraded and ends up identical to a fresh one (squibble on SQLite, the
// index definition on PostgreSQL); duplicates already present make the
// migration fail without changing the database.
func TestMigrationFromV01Init(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")

			p := openPlatform(t, cfg)
			v01InitState(t, p.db)
			fresh := indexDefinitionOn(t, p.db)
			require.Empty(t, fresh, "index dropped")
			require.NoError(t, p.Close())

			p = openPlatform(t, cfg)
			def := indexDefinitionOn(t, p.db)
			require.NotEmpty(t, def)
			assert.Contains(t, def, "machine_key")
			assert.Contains(t, def, "node_key")

			var ids []string
			require.NoError(t, p.db.Table("migrations").Pluck("id", &ids).Error)
			assert.Contains(t, ids, nodesMachineNodeKeyMigration)

			// Duplicate pairs from before the index: the migration refuses.
			id1 := platformTailnet(t, p, "local", types.TailnetActive, false)
			id2 := platformTailnet(t, p, "local", types.TailnetActive, false)
			v01InitState(t, p.db)

			mk, nk := key.NewMachine().Public(), key.NewNode().Public()
			require.NoError(t, addNode(t, tenantDB(t, p, id1), mk, nk))
			require.NoError(t, addNode(t, tenantDB(t, p, id2), mk, nk))
			require.NoError(t, p.Close())

			_, err := OpenPlatform(cfg)
			require.ErrorIs(t, err, errDuplicateNodeKeys)

			// Nothing changed: no record, no index, the duplicates remain.
			conn, err := openDB(cfg.Database, cfg.SQLitePath())
			require.NoError(t, err)
			t.Cleanup(func() {
				if sqlDB, err := conn.DB(); err == nil {
					_ = sqlDB.Close()
				}
			})

			ids = nil
			require.NoError(t, conn.Table("migrations").Pluck("id", &ids).Error)
			assert.NotContains(t, ids, nodesMachineNodeKeyMigration)
			assert.Empty(t, indexDefinitionOn(t, conn))

			var n int64
			require.NoError(t, conn.Raw(`SELECT count(*) FROM nodes`).Scan(&n).Error)
			assert.EqualValues(t, 2, n)
		})
	}
}

// TestCheckV01DatabaseSurfacesConnectionErrors: a broken connection (e.g.
// PgBouncer with max_prepared_statements=0) must be reported as a database
// error, never as "not a v0.1 database" or as a fresh database.
func TestCheckV01DatabaseSurfacesConnectionErrors(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")
			if cfg.Database.IsSQLite() {
				cfg = sqliteTestConfig(filepath.Join(t.TempDir(), "broken.db"))
			}

			conn, err := openDB(cfg.Database, cfg.SQLitePath())
			require.NoError(t, err)

			sqlDB, err := conn.DB()
			require.NoError(t, err)
			require.NoError(t, sqlDB.Close())

			err = checkV01Database(conn)
			require.Error(t, err, "a closed connection is not a fresh database")
			require.NotErrorIs(t, err, ErrNotV01Database)
			assert.Contains(t, err.Error(), "closed")
		})
	}
}

// indexDefinitionOn returns the stored definition of the unique key index,
// or "" when it does not exist.
func indexDefinitionOn(t *testing.T, conn *gorm.DB) string {
	t.Helper()

	q := `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`
	if conn.Name() == "postgres" {
		q = `SELECT indexdef FROM pg_indexes WHERE indexname = ?`
	}

	var defs []string
	require.NoError(t, conn.Raw(q, idxNodesMachineNodeKey).Scan(&defs).Error)

	if len(defs) == 0 {
		return ""
	}

	return defs[0]
}

// TestNodesMachineNodeKeyIndexInSchemaFiles: the migration's DDL and both
// schema files define the same index, so fresh and migrated databases match.
func TestNodesMachineNodeKeyIndexInSchemaFiles(t *testing.T) {
	want := `CREATE UNIQUE INDEX ` + idxNodesMachineNodeKey + ` ON nodes(machine_key, node_key) WHERE ` + nodesMachineNodeKeyPredicate + `;`
	assert.Contains(t, dbSchema, want)
	assert.Contains(t, dbSchemaPostgres, want)
	assert.Equal(t, "mkey:"+strings.Repeat("0", 64), zeroMachineKey)
	assert.Equal(t, "nodekey:"+strings.Repeat("0", 64), zeroNodeKey)
}
