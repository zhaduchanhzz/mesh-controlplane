package db

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"tailscale.com/types/key"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
)

// addNodeIPs inserts a node with the given overlay addresses (nil = NULL)
// through the tenant handle, so tailnet_id is set by tenantscope.
func addNodeIPs(t *testing.T, hsdb *HSDatabase, v4, v6 *netip.Addr) error {
	t.Helper()

	u, err := hsdb.CreateUser(types.User{Name: "u-" + uuid.NewV4().String()[:8]})
	require.NoError(t, err)

	return hsdb.DB.Create(&types.Node{
		MachineKey: key.NewMachine().Public(), NodeKey: key.NewNode().Public(),
		Hostname: "laptop", UserID: &u.ID, RegisterMethod: util.RegisterMethodAuthKey,
		IPv4: v4, IPv6: v6,
	}).Error
}

func requireUniqueViolation(t *testing.T, err error, msg string) {
	t.Helper()

	require.Error(t, err, msg)

	// SQLite: gorm translates the constraint error; PostgreSQL keeps
	// "duplicate key value violates unique constraint".
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		assert.Contains(t, strings.ToLower(err.Error()), "unique constraint", msg)
	}
}

// TestNodeIPUniquePerTailnet (P06, owner decision 2026-11-24): two nodes of
// one tailnet never hold the same overlay address, enforced by the database
// and not only by the in-memory allocator (AGENTS 7). Two tailnets are two
// overlays and may reuse addresses; NULL (no address of that family) may
// repeat.
func TestNodeIPUniquePerTailnet(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			a := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))
			bdb := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))

			v4, v6 := nap("100.64.0.1"), nap("fd7a:115c:a1e0::1")
			require.NoError(t, addNodeIPs(t, a, v4, v6))

			// Same tailnet: refused per family.
			requireUniqueViolation(t, addNodeIPs(t, a, v4, nap("fd7a:115c:a1e0::2")), "same IPv4 in one tailnet")
			requireUniqueViolation(t, addNodeIPs(t, a, nap("100.64.0.2"), v6), "same IPv6 in one tailnet")
			requireUniqueViolation(t, addNodeIPs(t, a, v4, nil), "same IPv4, no IPv6")
			requireUniqueViolation(t, addNodeIPs(t, a, nil, v6), "same IPv6, no IPv4")

			// Another tailnet is another overlay: the same addresses pass.
			require.NoError(t, addNodeIPs(t, bdb, v4, v6), "same addresses in another tailnet")

			// NULL repeats in one tailnet.
			require.NoError(t, addNodeIPs(t, a, nil, nil))
			require.NoError(t, addNodeIPs(t, a, nil, nil))
			require.NoError(t, addNodeIPs(t, a, nap("100.64.0.3"), nil))
			require.NoError(t, addNodeIPs(t, a, nap("100.64.0.4"), nil))
			require.NoError(t, addNodeIPs(t, a, nil, nap("fd7a:115c:a1e0::3")))
			require.NoError(t, addNodeIPs(t, a, nil, nap("fd7a:115c:a1e0::4")))

			// An update cannot move a node onto an address in use either.
			var n types.Node
			require.NoError(t, a.DB.Where("ipv4 = ?", "100.64.0.3").Take(&n).Error)
			requireUniqueViolation(t, a.DB.Model(&types.Node{}).Where("id = ?", n.ID).
				Update("ipv4", "100.64.0.1").Error, "update onto an IPv4 in use")
			requireUniqueViolation(t, a.DB.Model(&types.Node{}).Where("id = ?", n.ID).
				Update("ipv6", "fd7a:115c:a1e0::1").Error, "update onto an IPv6 in use")

			var cnt int64
			require.NoError(t, a.DB.Model(&types.Node{}).Count(&cnt).Error)
			assert.EqualValues(t, 7, cnt, "refused inserts left no rows")
		})
	}
}

// TestNodeIPDDLInSchemaFiles: the migration's DDL and the schema files define
// the same indexes, so fresh and migrated databases match.
func TestNodeIPDDLInSchemaFiles(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), nodeIPDDL)
	assert.Contains(t, norm(dbSchemaPostgres), nodeIPDDL)
	assert.Contains(t, migrationIDs(), nodeIPMigrationID)
}

func migrationIDs() []string {
	var ids []string
	for _, m := range v01Migrations() {
		ids = append(ids, m.ID)
	}

	return ids
}

// TestTailnetSettingsDDLInSchemaFiles (P06, ADR 0008): the table DDL is
// the same text in the migration and both schema files, and the migration
// is the last one (the bootstrap read-only consumer requires it).
func TestTailnetSettingsDDLInSchemaFiles(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), tailnetSettingsDDL)
	assert.Contains(t, norm(dbSchemaPostgres), tailnetSettingsDDL)
	assert.Equal(t, tailnetSettingsMigrationID, LatestMigrationID())
}

// nodeIPIndexes returns the stored definitions of the two address indexes
// that exist on conn.
func nodeIPIndexes(t *testing.T, conn *gorm.DB) []string {
	t.Helper()

	q := `SELECT sql FROM sqlite_master WHERE type = 'index' AND name IN (?, ?) ORDER BY name`
	if conn.Name() == "postgres" {
		q = `SELECT indexdef FROM pg_indexes WHERE indexname IN (?, ?) ORDER BY indexname`
	}

	var defs []string
	require.NoError(t, conn.Raw(q, idxNodesTailnetIPv4, idxNodesTailnetIPv6).Scan(&defs).Error)

	return defs
}

func rewindNodeIPMigration(t *testing.T, raw *gorm.DB) {
	t.Helper()

	require.NoError(t, nodeIPMigration().Rollback(raw))
	require.NoError(t, raw.Exec(`DELETE FROM migrations WHERE id = ?`, nodeIPMigrationID).Error)
}

// TestMigrationNodeIPUnique: the rollback drops both indexes; a database from
// before the migration is upgraded on open (squibble validates SQLite against
// schema.sql) when its addresses repeat only across tailnets; an address
// repeated inside one tailnet makes the migration fail with a clear error and
// leaves the database as it was (no record, no index, every row kept).
func TestMigrationNodeIPUnique(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")

			p := openPlatform(t, cfg)
			require.Len(t, nodeIPIndexes(t, p.db), 2, "a fresh database has both indexes")
			for _, def := range nodeIPIndexes(t, p.db) {
				assert.Contains(t, strings.ToUpper(def), "UNIQUE")
				assert.Contains(t, strings.ToUpper(def), "IS NOT NULL")
			}

			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)
			v4, v6 := nap("100.64.0.1"), nap("fd7a:115c:a1e0::1")

			// Down migration: both indexes gone, and the database no
			// longer refuses a duplicate.
			rewindNodeIPMigration(t, p.db)
			require.Empty(t, nodeIPIndexes(t, p.db), "rollback drops the indexes")

			require.NoError(t, addNodeIPs(t, tenantDB(t, p, idA), v4, v6))
			require.NoError(t, addNodeIPs(t, tenantDB(t, p, idB), v4, v6), "same addresses in another tailnet")
			require.NoError(t, p.Close())

			// Addresses shared only across tailnets: the upgrade passes.
			p = openPlatform(t, cfg)
			require.Len(t, nodeIPIndexes(t, p.db), 2, "upgraded on open")

			var ids []string
			require.NoError(t, p.db.Table("migrations").Pluck("id", &ids).Error)
			assert.Contains(t, ids, nodeIPMigrationID)
			requireUniqueViolation(t, addNodeIPs(t, tenantDB(t, p, idA), v4, nil), "the migrated index is active")

			// An address held twice inside tailnet A: the upgrade refuses.
			rewindNodeIPMigration(t, p.db)
			require.NoError(t, addNodeIPs(t, tenantDB(t, p, idA), nil, v6))
			require.NoError(t, p.Close())

			_, err := OpenPlatform(cfg)
			require.ErrorIs(t, err, errDuplicateNodeIPs)
			assert.Contains(t, err.Error(), "1 addresses")

			conn, err := openDB(cfg.Database, cfg.SQLitePath())
			require.NoError(t, err)
			t.Cleanup(func() {
				if sqlDB, err := conn.DB(); err == nil {
					_ = sqlDB.Close()
				}
			})

			ids = nil
			require.NoError(t, conn.Table("migrations").Pluck("id", &ids).Error)
			assert.NotContains(t, ids, nodeIPMigrationID, "not recorded")
			assert.Empty(t, nodeIPIndexes(t, conn), "no index created")

			var n int64
			require.NoError(t, conn.Raw(`SELECT count(*) FROM nodes`).Scan(&n).Error)
			assert.EqualValues(t, 3, n, "no row dropped")

			// Once the operator resolves the duplicate the upgrade passes.
			require.NoError(t, conn.Exec(`UPDATE nodes SET ipv6 = NULL WHERE tailnet_id = ? AND ipv4 IS NULL`,
				idA.String()).Error)

			p = openPlatform(t, cfg)
			require.Len(t, nodeIPIndexes(t, p.db), 2)
		})
	}
}
