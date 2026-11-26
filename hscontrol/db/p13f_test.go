package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P13f L3 (review P11): the read-only handle of the bootstrap service put
// its SQLite connection in query_only once, with a PRAGMA on whichever
// connection ran it. A connection opened later (the idle one closed and
// replaced) wrote again. query_only must hold on every connection.
func TestOpenReadOnlySQLiteEveryConnectionQueryOnly(t *testing.T) {
	cfg := sqliteTestConfig(filepath.Join(t.TempDir(), "ro.db"))

	// The schema is created by the explicit migrate step (ADR 0013).
	p, err := MigrateAndOpenPlatform(cfg)
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

// P13f L10 (review P03b, AGENTS rule 8): the platform opener of a cluster
// process (and of mesh-placement) never creates a tailnet: on a fresh
// database it finds none, and a handle for an unknown tailnet fails. Only
// the single-tenant opener creates the default tailnet, ACTIVE with no
// policy row (upstream "unset"; see createDefaultTailnet).
func TestDefaultTailnetOnlyFromSingleTenantOpener(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "c-p13f")

			// The operator migrate step (ADR 0013) creates the schema; it
			// creates no tailnet either.
			_, err := Migrate(context.Background(), cfg)
			require.NoError(t, err)

			p, err := OpenPlatform(cfg)
			require.NoError(t, err)

			count := func() int64 {
				var n int64

				require.NoError(t, p.RegistryDB().Raw(`SELECT count(*) FROM tailnets`).Scan(&n).Error)

				return n
			}

			require.Zero(t, count(), "OpenPlatform created a tailnet")

			owned, err := p.OwnedTailnets("c-p13f")
			require.NoError(t, err)
			assert.Empty(t, owned)

			_, err = p.TenantDB(tailnet.MustParseID("33333333-3333-4333-8333-333333333333"))
			require.Error(t, err)
			assert.Zero(t, count(), "TenantDB of an unknown tailnet created one")
			require.NoError(t, p.Close())

			hsdb := open(t, b.cfg(t, "", ""))
			assert.Equal(t, types.TailnetActive, hsdb.Tailnet().Status)

			_, err = hsdb.GetPolicy()
			require.ErrorIs(t, err, types.ErrPolicyNotFound, "single-tenant mode keeps the upstream unset policy")
		})
	}
}
