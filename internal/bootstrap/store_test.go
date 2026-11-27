package bootstrap

import (
	"context"
	"database/sql"
	"net/url"
	"os"
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
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
	"zombiezen.com/go/postgrestest"
)

var testOp = placement.Op{Actor: "spiffe://mesh/operator/test"}

type backend struct {
	name string
	cfg  func(t *testing.T) *types.Config
}

func backends() []backend {
	return []backend{
		{"sqlite", func(t *testing.T) *types.Config {
			t.Helper()

			return &types.Config{
				ClusterID: "c1",
				Database: types.DatabaseConfig{
					Type:   types.DatabaseSqlite,
					Sqlite: types.SqliteConfig{Path: filepath.Join(t.TempDir(), "boot.db")},
				},
				Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
			}
		}},
		{"postgres", func(t *testing.T) *types.Config {
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
				ClusterID: "c1",
				Database: types.DatabaseConfig{
					Type: types.DatabasePostgres,
					Postgres: types.PostgresConfig{
						Host: pu.Hostname(), User: pu.User.Username(), Name: strings.TrimLeft(pu.Path, "/"),
						Pass: types.Secret(pass), Port: port, Ssl: "disable",
						MaxOpenConnections: 8, MaxIdleConnections: 2,
					},
				},
				Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
			}
		}},
	}
}

func splitKey(t *testing.T, key string) (string, string) {
	t.Helper()

	rest, ok := strings.CutPrefix(key, "hskey-auth-")
	require.True(t, ok, "pre-auth key format")

	id, secret, ok := strings.Cut(rest, "-")
	require.True(t, ok)

	return id, secret
}

func closeGorm(t *testing.T, g *gorm.DB) {
	t.Helper()
	t.Cleanup(func() {
		s, err := g.DB()
		if err == nil {
			_ = s.Close()
		}
	})
}

// TestBootstrapStoreFollowsOwner (F06, I12) on SQLite and PostgreSQL: a
// key of tailnet acme on c1 resolves to c1's URL through the read-only
// store; after MoveOwner to c2 it resolves to c2's URL, never c1's; while
// moving_to_cluster_id is set it is MOVING; a SUSPENDED tailnet is refused.
func TestBootstrapStoreFollowsOwner(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			cfg := b.cfg(t)

			p, err := db.MigrateAndOpenPlatform(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { _ = p.Close() })

			r, err := placement.New(p.RegistryDB(), placement.Config{})
			require.NoError(t, err)

			for _, c := range []struct{ id, url string }{{"c1", urlA}, {"c2", urlB}} {
				_, err = r.CreateCluster(ctx, testOp, placement.ClusterSpec{
					ID: c.id, EndpointURL: c.url, Class: placement.ClassShared,
					State: placement.ClusterActive, MaxDevices: 100,
				})
				require.NoError(t, err)

				// Placement chooses only clusters with a live lease (P12 Q2).
				_, err = lease.NewStore(p.RegistryDB()).Acquire(ctx, c.id, "test-"+c.id, time.Hour)
				require.NoError(t, err)
			}

			tn, err := r.CreateTailnet(ctx, testOp, "acme", placement.ClassShared)
			require.NoError(t, err)
			require.Equal(t, "c1", tn.OwnerClusterID)
			tn, err = r.ActivateTailnet(ctx, testOp, tn.ID, tn.OwnerEpoch)
			require.NoError(t, err)

			tdb, err := p.TenantDB(tailnet.MustParseID(tn.ID))
			require.NoError(t, err)
			u, err := tdb.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err)

			uid := types.UserID(u.ID)
			pak, err := tdb.CreatePreAuthKey(&uid, true, false, nil, nil)
			require.NoError(t, err)
			keyID, secret := splitKey(t, pak.Key)

			ro, err := db.OpenReadOnly(cfg)
			require.NoError(t, err)
			closeGorm(t, ro)

			svc := NewService(NewGormStore(ro))
			resolve := func(tenant string) Result {
				t.Helper()

				res, err := svc.Resolve(ctx, Request{
					Credential: Credential{Kind: CredentialKindAuthKeyProof, KeyID: keyID, Proof: ProofOf(secret)},
					Tenant:     tenant,
				})
				require.NoError(t, err)

				return res
			}

			got := resolve("acme")
			require.Equal(t, OutcomeOK, got.Outcome)
			assert.Equal(t, urlA, got.ControlURL)

			bad, err := svc.Resolve(ctx, Request{Credential: Credential{
				Kind: CredentialKindAuthKeyProof, KeyID: keyID, Proof: ProofOf(secret + "x"),
			}})
			require.NoError(t, err)
			assert.Equal(t, OutcomeRefused, bad.Outcome, "wrong proof")

			_, err = r.MoveOwner(ctx, testOp, tn.ID, "c1", tn.OwnerEpoch, "c2")
			require.NoError(t, err)
			assert.Equal(t, OutcomeMoving, resolve("").Outcome, "requested, not handed over yet (ADR 0010)")

			// c1's serving process hands the tailnet over (p is c1's platform).
			p.SetLease("test-c1", 1)
			to, err := p.HandOver(tailnet.MustParseID(tn.ID), tn.OwnerEpoch)
			require.NoError(t, err)
			require.Equal(t, "c2", to)

			got = resolve("")
			require.Equal(t, OutcomeOK, got.Outcome)
			assert.Equal(t, urlB, got.ControlURL, "the new owner, never the old one")

			require.NoError(t, p.RegistryDB().Exec(`UPDATE tailnets SET moving_to_cluster_id = ? WHERE id = ?`, "c1", tn.ID).Error)
			assert.Equal(t, OutcomeMoving, resolve("").Outcome)

			require.NoError(t, p.RegistryDB().Exec(`UPDATE tailnets SET moving_to_cluster_id = NULL, status = ? WHERE id = ?`,
				types.TailnetSuspended, tn.ID).Error)
			assert.Equal(t, OutcomeRefused, resolve("").Outcome, "suspended")

			// The handle only reads.
			if b.name == "postgres" {
				err = ro.Transaction(func(tx *gorm.DB) error {
					return tx.Exec(`UPDATE tailnets SET slug = 'x'`).Error
				}, &sql.TxOptions{ReadOnly: true})
			} else {
				err = ro.Exec(`UPDATE tailnets SET slug = 'x'`).Error
			}

			require.Error(t, err, "a write through the read-only handle")
		})
	}
}

// TestOpenReadOnlyRefusesUnusableDatabase: OpenReadOnly never creates a
// database and refuses one without this binary's last migration.
func TestOpenReadOnlyRefusesUnusableDatabase(t *testing.T) {
	cfg := backends()[0].cfg(t)

	_, err := db.OpenReadOnly(cfg)
	require.ErrorIs(t, err, db.ErrSchemaNotCurrent)

	_, statErr := os.Stat(cfg.SQLitePath())
	require.True(t, os.IsNotExist(statErr), "no database file was created")

	p, err := db.MigrateAndOpenPlatform(cfg)
	require.NoError(t, err)
	require.NoError(t, p.RegistryDB().Exec(`DELETE FROM migrations WHERE id = ?`, db.LatestMigrationID()).Error)
	require.NoError(t, p.Close())

	_, err = db.OpenReadOnly(cfg)
	require.ErrorIs(t, err, db.ErrSchemaNotCurrent)
}
