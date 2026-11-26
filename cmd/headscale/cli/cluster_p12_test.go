package cli

import (
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"zombiezen.com/go/postgrestest"
)

// p12Platform opens a fresh platform database as `cluster --cluster-id
// cluster` would, on SQLite or PostgreSQL.
func p12Platform(t *testing.T, backend, cluster string) *db.Platform {
	t.Helper()

	cfg := &types.Config{
		ClusterID:         cluster,
		PolicyEmptyDenies: true,
		Policy:            types.PolicyConfig{Mode: types.PolicyModeDB},
		Database: types.DatabaseConfig{
			Type:   types.DatabaseSqlite,
			Sqlite: types.SqliteConfig{Path: filepath.Join(t.TempDir(), "cli.db")},
		},
	}

	if backend == "postgres" {
		srv, err := postgrestest.Start(t.Context())
		if err != nil {
			t.Skipf("start postgres: %s", err)
		}

		t.Cleanup(srv.Cleanup)

		u, err := srv.CreateDatabase(t.Context())
		require.NoError(t, err)

		pu, err := url.Parse(u)
		require.NoError(t, err)

		pass, _ := pu.User.Password()
		port, _ := strconv.Atoi(pu.Port())
		cfg.Database = types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host: pu.Hostname(), User: pu.User.Username(), Name: strings.TrimLeft(pu.Path, "/"),
				Pass: types.Secret(pass), Port: port, Ssl: "disable",
				MaxOpenConnections: 8, MaxIdleConnections: 2,
			},
		}
	}

	p, err := db.MigrateAndOpenPlatform(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	return p
}

func p12Register(t *testing.T, p *db.Platform, id string, class placement.Class, state placement.ClusterState) {
	t.Helper()

	reg, err := placement.New(p.RegistryDB(), placement.Config{})
	require.NoError(t, err)

	_, err = reg.CreateCluster(context.Background(), placement.Op{Actor: "test"}, placement.ClusterSpec{
		ID: id, EndpointURL: "https://" + id + ".cp.example.test:8443", Class: class, State: state, MaxDevices: 100,
	})
	require.NoError(t, err)
}

// p12Live takes cluster id's lease as its running process would.
func p12Live(t *testing.T, p *db.Platform, id string) {
	t.Helper()

	_, err := lease.NewStore(p.RegistryDB()).Acquire(context.Background(), id, "test-"+id, time.Hour)
	require.NoError(t, err)
}

func p12Count(t *testing.T, p *db.Platform, q string, args ...any) int64 {
	t.Helper()

	var n int64
	require.NoError(t, p.RegistryDB().Raw(q, args...).Scan(&n).Error)

	return n
}

// P12, owner decision Q5: `cluster --cluster-id X tailnet create` goes
// through the placement registry: X must be registered and ACTIVE, with
// room (dedicated: no other live tailnet); it is audited as the CLI of X.
// X's process must hold X's lease (owner decision 2026-11-25).
func TestCLITailnetCreateFollowsRegistry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			p := p12Platform(t, backend, "c1")

			_, err := createTailnetCLI(ctx, p, "c1", "a")
			require.ErrorIs(t, err, placement.ErrNotFound, "c1 not registered: refused (no R1 by the CLI)")
			assert.Zero(t, p12Count(t, p, `SELECT count(*) FROM tailnets WHERE slug = 'a'`))

			p12Register(t, p, "c1", placement.ClassDedicated, placement.ClusterActive)

			_, err = createTailnetCLI(ctx, p, "c1", "a")
			require.ErrorIs(t, err, placement.ErrNoLiveLease, "registered, but no process of c1 runs")

			p12Live(t, p, "c1")

			tn, err := createTailnetCLI(ctx, p, "c1", "a")
			require.NoError(t, err)
			assert.Equal(t, "c1", tn.OwnerClusterID)
			assert.Equal(t, types.TailnetPending, tn.Status)

			_, err = createTailnetCLI(ctx, p, "c1", "b")
			require.ErrorIs(t, err, placement.ErrNoCapacity, "dedicated c1 already owns a (no R8 by the CLI)")

			assert.Equal(t, int64(1), p12Count(t, p,
				`SELECT count(*) FROM platform_audit WHERE action = 'CreateTailnet' AND actor = 'cli:c1' AND outcome = 'ok'`))
		})
	}
}

// P12, Q5: `tailnet reassign --to Y` is the registry's MoveOwner from this
// cluster: Y must be registered and ACTIVE with room; the same cluster
// again is a new epoch.
func TestCLITailnetReassignFollowsRegistry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			p := p12Platform(t, backend, "c1")

			p12Register(t, p, "c1", placement.ClassShared, placement.ClusterActive)
			p12Register(t, p, "c3", placement.ClassShared, placement.ClusterDisabled)
			p12Register(t, p, "c2", placement.ClassShared, placement.ClusterActive)
			p12Live(t, p, "c1")

			tn, err := createTailnetCLI(ctx, p, "c1", "a")
			require.NoError(t, err)

			_, err = reassignTailnetCLI(ctx, p, "c1", "a", "ghost")
			require.ErrorIs(t, err, placement.ErrNotFound, "unregistered target refused (no R1 by the CLI)")

			_, err = reassignTailnetCLI(ctx, p, "c1", "a", "c3")
			require.ErrorIs(t, err, placement.ErrNoCapacity, "DISABLED target refused")

			assert.Equal(t, int64(1), p12Count(t, p, `SELECT owner_epoch FROM tailnets WHERE id = ?`, tn.ID), "refusals change nothing")

			got, err := reassignTailnetCLI(ctx, p, "c1", "a", "c1")
			require.NoError(t, err)
			assert.Equal(t, uint64(2), got.OwnerEpoch, "same cluster: new epoch")

			_, err = reassignTailnetCLI(ctx, p, "c1", "a", "c2")
			require.ErrorIs(t, err, placement.ErrNoLiveLease, "c2 registered but no process of c2 runs")

			p12Live(t, p, "c2")

			// ADR 0010: to another cluster it is a request; c1 hands the
			// tailnet over once it stopped serving it.
			got, err = reassignTailnetCLI(ctx, p, "c1", "a", "c2")
			require.NoError(t, err)
			assert.Equal(t, "c1", got.OwnerClusterID, "owner changes at the hand-over")
			assert.Equal(t, uint64(2), got.OwnerEpoch)
			require.NotNil(t, got.MovingToClusterID)
			assert.Equal(t, "c2", *got.MovingToClusterID)

			_, err = reassignTailnetCLI(ctx, p, "c1", "a", "c1")
			require.ErrorIs(t, err, placement.ErrConflict, "a move is pending")

			assert.Equal(t, int64(2), p12Count(t, p,
				`SELECT count(*) FROM platform_audit WHERE action = 'MoveOwner' AND actor = 'cli:c1' AND outcome = 'ok'`))
		})
	}
}
