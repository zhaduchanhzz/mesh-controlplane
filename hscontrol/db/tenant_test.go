package db

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"tailscale.com/types/key"
)

// backend opens runtimes on one shared database: a SQLite file, or one
// PostgreSQL database (ADR 0005: every cluster shares one PostgreSQL).
type backend struct {
	name string
	cfg  func(t *testing.T, tailnetID, clusterID string) *types.Config
}

func backends(t *testing.T) []backend {
	t.Helper()

	sqlitePath := filepath.Join(t.TempDir(), "shared.db")

	return []backend{{
		name: "sqlite",
		cfg: func(t *testing.T, tn, cl string) *types.Config {
			t.Helper()

			c := sqliteTestConfig(sqlitePath)
			c.TailnetID, c.ClusterID = tn, cl

			return c
		},
	}, {
		name: "postgres",
		cfg: func(t *testing.T, tn, cl string) *types.Config {
			t.Helper()

			c := postgresConfig(t)
			c.TailnetID, c.ClusterID = tn, cl

			return c
		},
	}}
}

// postgresConfig lazily starts one PostgreSQL database per test (skips the
// test when pg_ctl/initdb are unavailable, as the other PostgreSQL tests do).
func postgresConfig(t *testing.T) *types.Config {
	t.Helper()

	pu := pgURLs[t.Name()]
	if pu == nil {
		pu = newPostgresDBForTest(t)
		pgURLs[t.Name()] = pu
	}

	pass, _ := pu.User.Password()
	port, _ := strconv.Atoi(pu.Port())

	return &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host: pu.Hostname(), User: pu.User.Username(), Name: strings.TrimLeft(pu.Path, "/"),
				Pass: types.Secret(pass), Port: port, Ssl: "disable",
			},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	}
}

var pgURLs = map[string]*url.URL{}

func open(t *testing.T, cfg *types.Config) *HSDatabase {
	t.Helper()

	hsdb, err := NewHeadscaleDatabase(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hsdb.Close() })

	return hsdb
}

// addTailnet registers a second tailnet through the unscoped (platform)
// handle, as the future CreateTailnet API will.
func addTailnet(t *testing.T, hsdb *HSDatabase, cluster string) string {
	t.Helper()

	id := uuid.NewV4().String()
	now := time.Now().UTC()
	require.NoError(t, hsdb.raw.Create(&types.Tailnet{
		ID: id, Slug: "tn-" + id[:8], Status: types.TailnetActive,
		OwnerClusterID: cluster, OwnerEpoch: 1, PolicyRevision: 1, ConfigRevision: 1,
		CreatedAt: now, UpdatedAt: now,
	}).Error)

	return id
}

func TestFreshDatabaseHasOneDefaultTailnet(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			hsdb := open(t, b.cfg(t, "", ""))
			tn := hsdb.Tailnet()
			assert.Equal(t, types.TailnetActive, tn.Status)
			assert.Equal(t, "local", tn.OwnerClusterID)
			assert.EqualValues(t, 1, tn.OwnerEpoch)

			u, err := hsdb.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err)
			assert.Equal(t, tn.ID, u.TailnetID, "rows are stamped with the runtime's tailnet")
		})
	}
}

func TestTwoTailnetsShareOneDatabaseInIsolation(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			a := open(t, b.cfg(t, "", ""))
			idB := addTailnet(t, a, "local")
			bdb := open(t, b.cfg(t, idB, ""))
			require.NotEqual(t, a.Tailnet().ID, bdb.Tailnet().ID)

			// The same user name in both tailnets is allowed (per-tailnet unique).
			ua, err := a.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err)
			ub, err := bdb.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err)
			// ...but not twice in one tailnet.
			_, err = a.CreateUser(types.User{Name: "alice"})
			require.Error(t, err)

			mk := key.NewMachine().Public() // one device, two tailnets
			for _, x := range []struct {
				db *HSDatabase
				u  *types.User
			}{{a, ua}, {bdb, ub}} {
				n := &types.Node{
					MachineKey: mk, NodeKey: key.NewNode().Public(), Hostname: "laptop",
					UserID: &x.u.ID, RegisterMethod: util.RegisterMethodAuthKey,
				}
				require.NoError(t, x.db.DB.Create(n).Error)
			}

			for _, x := range []*HSDatabase{a, bdb} {
				users, err := x.ListUsers(nil)
				require.NoError(t, err)
				require.Len(t, users, 1)
				assert.Equal(t, x.Tailnet().ID, users[0].TailnetID)

				nodes, err := x.ListNodes()
				require.NoError(t, err)
				require.Len(t, nodes, 1, "each tailnet sees only its own node for the shared machine key")
				assert.Equal(t, x.Tailnet().ID, nodes[0].TailnetID)
			}

			// A cannot read B's user by id.
			_, err = a.GetUserByID(types.UserID(ub.ID))
			require.Error(t, err)

			// A cannot delete B's user.
			_ = a.DestroyUser(types.UserID(ub.ID))
			users, err := bdb.ListUsers(nil)
			require.NoError(t, err)
			require.Len(t, users, 1)
		})
	}
}

// TestDatabaseRejectsCrossTailnetReferences bypasses the ORM with raw SQL:
// the schema itself must refuse a node or credential of tailnet B pointing
// at a user of tailnet A (composite FK / trigger).
func TestDatabaseRejectsCrossTailnetReferences(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			a := open(t, b.cfg(t, "", ""))
			idB := addTailnet(t, a, "local")

			ua, err := a.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err)

			err = a.raw.Exec(
				`INSERT INTO nodes (tailnet_id, hostname, user_id) VALUES (?, ?, ?)`,
				idB, "smuggled", ua.ID,
			).Error
			require.Error(t, err, "node of B referencing A's user must violate fk_nodes_user")

			hash := []byte("$sha256$x")
			err = a.raw.Exec(
				`INSERT INTO credentials (tailnet_id, kind, identifier, hash, user_id) VALUES (?, ?, ?, ?, ?)`,
				idB, types.CredentialPreAuthKey, "smuggled", hash, ua.ID,
			).Error
			require.Error(t, err, "credential of B owned by A's user must be refused by the trigger")
		})
	}
}

func TestRuntimeRefusesTailnetOfAnotherCluster(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			a := open(t, b.cfg(t, "", "")) // default tailnet owned by "local"
			_, err := NewHeadscaleDatabase(b.cfg(t, a.Tailnet().ID, "cluster-2"))
			require.ErrorIs(t, err, ErrTailnetNotOwned)
		})
	}
}

func TestAmbiguousTailnetsRefused(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			a := open(t, b.cfg(t, "", ""))
			addTailnet(t, a, "local")
			_, err := NewHeadscaleDatabase(b.cfg(t, "", ""))
			require.ErrorIs(t, err, ErrAmbiguousTailnet)
		})
	}
}

func TestRejectsHeadscaleDatabases(t *testing.T) {
	var dumps []string

	for _, dir := range []string{"testdata/sqlite", "testdata/sqlite_too_old"} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)

		for _, e := range entries {
			dumps = append(dumps, filepath.Join(dir, e.Name()))
		}
	}

	require.NotEmpty(t, dumps)

	for _, dump := range dumps {
		t.Run(filepath.Base(dump), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "old.db")
			require.NoError(t, createSQLiteFromSQLFile(dump, dbPath))

			_, err := NewHeadscaleDatabase(sqliteTestConfig(dbPath))
			require.ErrorIs(t, err, ErrNotV01Database)
		})
	}
}

func TestRejectsUnknownMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "future.db")
	hsdb, err := NewHeadscaleDatabase(sqliteTestConfig(dbPath))
	require.NoError(t, err)
	require.NoError(t, hsdb.raw.Exec(`INSERT INTO migrations (id) VALUES ('209901010000-from-the-future')`).Error)
	require.NoError(t, hsdb.Close())

	_, err = NewHeadscaleDatabase(sqliteTestConfig(dbPath))
	require.ErrorIs(t, err, ErrNotV01Database)
}

func TestScopedContextKeepsTailnet(t *testing.T) {
	hsdb := open(t, sqliteTestConfig(filepath.Join(t.TempDir(), "ctx.db")))
	_, err := hsdb.CreateUser(types.User{Name: "alice"})
	require.NoError(t, err)

	var n int64

	err = hsdb.DB.WithContext(t.Context()).Model(&types.User{}).Count(&n).Error
	require.Error(t, err, "a scope-less context must fail closed")

	require.NoError(t, hsdb.DB.WithContext(hsdb.ScopedContext(t.Context())).Model(&types.User{}).Count(&n).Error)
	assert.EqualValues(t, 1, n)
}
