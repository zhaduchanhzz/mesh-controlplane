package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
	"tailscale.com/types/key"
)

// TestNodeKeyPairOneTailnet: (machine_key, node_key) resolves a Noise
// session to exactly one tailnet (ADR 0007 §2), so the pair may not appear in
// two tailnets, by insert or by update. Upstream semantics inside one tailnet
// stay: the same pair for another user (re-login) and one machine key with
// several node keys are allowed.
func TestNodeKeyPairOneTailnet(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			a := tenantDB(t, p, idA)
			bdb := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))

			mk, nk := key.NewMachine().Public(), key.NewNode().Public()
			require.NoError(t, addNode(t, a, mk, nk))

			require.Error(t, addNode(t, bdb, mk, nk), "the same pair in another tailnet")
			require.NoError(t, addNode(t, a, mk, nk), "the same pair for another user of the same tailnet (upstream re-login)")
			require.NoError(t, addNode(t, bdb, mk, key.NewNode().Public()), "same machine, another profile")
			require.NoError(t, addNode(t, a, mk, key.NewNode().Public()), "same machine, another node key in A")

			// B cannot move one of its nodes onto A's pair.
			nodes, err := bdb.ListNodes()
			require.NoError(t, err)
			require.NotEmpty(t, nodes)
			err = bdb.DB.Model(&types.Node{}).Where("id = ?", nodes[0].ID).
				Update("node_key", nk.String()).Error
			require.Error(t, err, "update onto another tailnet's pair")

			got, err := p.TailnetForNode(mk, nk)
			require.NoError(t, err)
			assert.Equal(t, idA, got)

			// Nodes created without keys are exempt; the platform never
			// resolves them.
			var (
				zeroM key.MachinePublic
				zeroN key.NodePublic
			)
			require.NoError(t, addNode(t, a, zeroM, zeroN))
			require.NoError(t, addNode(t, bdb, zeroM, zeroN))
		})
	}
}

// v01InitState rewinds a current database to how 202611220000-v0.1-init left
// it, with the migration's own Rollback.
func v01InitState(t *testing.T, raw *gorm.DB) {
	t.Helper()

	require.NoError(t, nodeKeyPairMigration().Rollback(raw))
	require.NoError(t, raw.Exec(`DELETE FROM migrations WHERE id = ?`, nodeKeyPairMigrationID).Error)
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
			assert.Contains(t, ids, nodeKeyPairMigrationID)

			id1 := platformTailnet(t, p, "local", types.TailnetActive, false)
			id2 := platformTailnet(t, p, "local", types.TailnetActive, false)

			// The migrated trigger is active.
			mk0, nk0 := key.NewMachine().Public(), key.NewNode().Public()
			require.NoError(t, addNode(t, tenantDB(t, p, id1), mk0, nk0))
			require.Error(t, addNode(t, tenantDB(t, p, id2), mk0, nk0))

			// Pairs already in several tailnets before the migration: it
			// refuses.
			v01InitState(t, p.db)

			mk, nk := key.NewMachine().Public(), key.NewNode().Public()
			require.NoError(t, addNode(t, tenantDB(t, p, id1), mk, nk))
			require.NoError(t, addNode(t, tenantDB(t, p, id2), mk, nk))
			require.NoError(t, p.Close())

			_, err := MigrateAndOpenPlatform(cfg)
			require.ErrorIs(t, err, errDuplicateNodeKeys)

			// Nothing changed: no record, no index, the duplicates remain.
			conn, err := openDB(cfg.Database, cfg.SQLitePath())
			require.NoError(t, err)
			t.Cleanup(func() {
				sqlDB, err := conn.DB()
				if err == nil {
					_ = sqlDB.Close()
				}
			})

			ids = nil
			require.NoError(t, conn.Table("migrations").Pluck("id", &ids).Error)
			assert.NotContains(t, ids, nodeKeyPairMigrationID)
			assert.Empty(t, indexDefinitionOn(t, conn))

			var n int64
			require.NoError(t, conn.Raw(`SELECT count(*) FROM nodes`).Scan(&n).Error)
			assert.EqualValues(t, 3, n)
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

			err = checkV01Database(conn, currentChain())
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

// TestNodeKeyPairDDLInSchemaFiles: the migration's DDL and the schema files
// define the same index and trigger, so fresh and migrated databases match.
func TestNodeKeyPairDDLInSchemaFiles(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), nodeKeyPairSQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), nodeKeyPairPostgresDDL)
	assert.Contains(t, nodeKeyPairSQLiteDDL, "'"+zeroMachineKey+"'")
	assert.Contains(t, nodeKeyPairSQLiteDDL, "'"+zeroNodeKey+"'")
	assert.Contains(t, nodeKeyPairPostgresDDL, "'"+zeroMachineKey+"'")
	assert.Contains(t, nodeKeyPairPostgresDDL, "'"+zeroNodeKey+"'")
}
