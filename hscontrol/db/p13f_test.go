package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P13f L3 (review P11): the read-only handle of the bootstrap service put
// its SQLite connection in query_only once, with a PRAGMA on whichever
// connection ran it. A connection opened later (the idle one closed and
// replaced) wrote again. query_only must hold on every connection.
func TestOpenReadOnlySQLiteEveryConnectionQueryOnly(t *testing.T) {
	cfg := sqliteTestConfig(filepath.Join(t.TempDir(), "ro.db"))

	p, err := OpenPlatform(cfg)
	require.NoError(t, err)
	require.NoError(t, p.Close())

	ro, err := OpenReadOnly(cfg)
	require.NoError(t, err)

	sqlDB, err := ro.DB()
	require.NoError(t, err)

	t.Cleanup(func() { _ = sqlDB.Close() })

	// No idle connection is kept: every statement runs on a new one.
	sqlDB.SetMaxIdleConns(0)

	for i := range 3 {
		err = ro.Exec(`INSERT INTO migrations (id) VALUES (?)`, "p13f-l3-probe").Error
		require.Error(t, err, "write %d through the read-only handle succeeded", i)
	}

	var n int64

	rw, err := OpenPlatform(cfg)
	require.NoError(t, err)

	t.Cleanup(func() { _ = rw.Close() })

	require.NoError(t, rw.RegistryDB().Raw(`SELECT count(*) FROM migrations WHERE id = ?`, "p13f-l3-probe").Scan(&n).Error)
	assert.Zero(t, n, "the read-only handle wrote a row")
}

// P13f L10 (review P03b, AGENTS rule 8): the default tailnet of a fresh
// single-tenant database was inserted ACTIVE with no policy, outside a
// transaction. Like every tailnet it is created PENDING with the deny-all
// policy {"grants":[]} in one transaction, then made ACTIVE.
func TestDefaultTailnetCreatedWithDenyAllPolicy(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			hsdb := open(t, b.cfg(t, "", ""))
			tn := hsdb.Tailnet()
			assert.Equal(t, types.TailnetActive, tn.Status)

			pol, err := hsdb.GetPolicy()
			require.NoError(t, err, "the default tailnet has a policy row")
			assert.JSONEq(t, `{"grants":[]}`, pol.Data)

			var rows []struct {
				TailnetID string
				Data      string
			}

			require.NoError(t, hsdb.raw.Raw(`SELECT tailnet_id, data FROM policies`).Scan(&rows).Error)
			require.Len(t, rows, 1)
			assert.Equal(t, tn.ID, rows[0].TailnetID)
		})
	}
}
