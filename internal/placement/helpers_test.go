package placement

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
	"zombiezen.com/go/postgrestest"
)

type backend struct {
	name string
	open func(t *testing.T) *gorm.DB
}

// backends returns SQLite and PostgreSQL. The PostgreSQL backend skips the
// test when pg_ctl/initdb are unavailable (as hscontrol/db tests do); on the
// CI VM mc-ci they are installed.
func backends() []backend {
	return []backend{
		{name: "sqlite", open: func(t *testing.T) *gorm.DB {
			t.Helper()

			return openPlatformDB(t, &types.Config{
				Database: types.DatabaseConfig{
					Type:   types.DatabaseSqlite,
					Sqlite: types.SqliteConfig{Path: filepath.Join(t.TempDir(), "registry.db")},
				},
				Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
			})
		}},
		{name: "postgres", open: func(t *testing.T) *gorm.DB {
			t.Helper()

			return openPlatformDB(t, postgresTestConfig(t))
		}},
	}
}

// postgresTestConfig starts a throwaway PostgreSQL database for t.
func postgresTestConfig(t *testing.T) *types.Config {
	t.Helper()

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

	return &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host: pu.Hostname(), User: pu.User.Username(), Name: strings.TrimLeft(pu.Path, "/"),
				Pass: types.Secret(pass), Port: port, Ssl: "disable",
				MaxOpenConnections: 32, MaxIdleConnections: 8,
			},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	}
}

func openPlatformDB(t *testing.T, cfg *types.Config) *gorm.DB {
	t.Helper()

	p, err := db.OpenPlatform(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	return p.RegistryDB()
}

func newRegistry(t *testing.T, gdb *gorm.DB, cfg Config) *Registry {
	t.Helper()

	r, err := New(gdb, cfg)
	require.NoError(t, err)

	return r
}

var testOp = Op{Actor: "spiffe://mesh/operator/test"}

func addCluster(t *testing.T, r *Registry, id string, class Class, state ClusterState, maxDevices int64) Cluster {
	t.Helper()

	c, err := r.CreateCluster(context.Background(), testOp, ClusterSpec{
		ID: id, EndpointURL: "https://" + id + ".cp.example.test:8443", Class: class,
		State: state, MaxDevices: maxDevices,
	})
	require.NoError(t, err)

	// A process of the cluster holds its lease (P12: placement skips
	// clusters without one).
	setLease(t, r.db, id, true)

	return c
}

// addNodes inserts n live device rows (tailnet_id only) into tailnet id with
// one statement.
func addNodes(t *testing.T, gdb *gorm.DB, id string, n int) {
	t.Helper()

	if n == 0 {
		return
	}

	now := time.Now().UTC()
	q := `INSERT INTO nodes (tailnet_id, hostname, created_at, updated_at)
		WITH RECURSIVE s(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM s WHERE x < ?)
		SELECT ?, 'dev-' || x, ?, ? FROM s`
	args := []any{n, id, now, now}

	if gdb.Name() == "postgres" {
		q = `INSERT INTO nodes (tailnet_id, hostname, created_at, updated_at)
			SELECT ?, 'dev-' || x, ?, ? FROM generate_series(1, ?) AS x`
		args = []any{id, now, now, n}
	}

	require.NoError(t, gdb.Exec(q, args...).Error)
}

// rawTailnet inserts a tailnet row directly (e.g. owned by an unregistered
// cluster, as databases from before P10 have).
func rawTailnet(t *testing.T, gdb *gorm.DB, owner string, status types.TailnetStatus) string {
	t.Helper()

	id := uuid.NewV4().String()
	now := time.Now().UTC()
	require.NoError(t, gdb.Create(&types.Tailnet{
		ID: id, Slug: "raw-" + id[:8], Status: status, OwnerClusterID: owner,
		OwnerEpoch: 1, PolicyRevision: 1, ConfigRevision: 1, CreatedAt: now, UpdatedAt: now,
	}).Error)

	return id
}

func count(t *testing.T, gdb *gorm.DB, q string, args ...any) int64 {
	t.Helper()

	var n int64
	require.NoError(t, gdb.Raw(q, args...).Scan(&n).Error)

	return n
}

func activeTailnet(t *testing.T, r *Registry, slug string, class Class) types.Tailnet {
	t.Helper()

	tn, err := r.CreateTailnet(context.Background(), testOp, slug, class)
	require.NoError(t, err)

	tn, err = r.ActivateTailnet(context.Background(), testOp, tn.ID, tn.OwnerEpoch)
	require.NoError(t, err)

	return tn
}

func keyOp(key, hash string) Op {
	return Op{Actor: testOp.Actor, IdempotencyKey: key, RequestHash: hash}
}

func slugN(i int) string { return fmt.Sprintf("tn-%03d", i) }
