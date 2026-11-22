package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"gorm.io/gorm"
	"tailscale.com/types/key"
)

// legacyPlaintextKey is the pre-0.28 plaintext pre-auth key (id 5) seeded in
// both 0.29.3 fixtures.
//
// TODO(kradalby): remove in 0.31 with the credentials migration.
const legacyPlaintextKey = "plaintextlegacykey0000000000000000000000000000"

func TestSQLiteSameQueryInAndOutsideTransaction(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
server_url: https://example.com
prefixes:
  v4: 100.64.0.0/10
noise:
  private_key_path: noise_private.key
dns:
  magic_dns: false
  override_local_dns: false
database:
  type: sqlite3
  sqlite:
    path: headscale.db
`), 0o600))
	l, err := config.Load(configPath, true, os.Environ())
	require.NoError(t, err)
	require.NoError(t, config.Validate(l.Config))
	hsdb, err := NewHeadscaleDatabase(l.Config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, hsdb.Close()) })

	user, err := hsdb.CreateUser(types.User{Name: "test-user"})
	require.NoError(t, err)
	sqlDB, err := hsdb.DB.DB()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Replacing the context must keep the tailnet scope (fail closed otherwise).
	hsdb.DB = hsdb.DB.WithContext(hsdb.ScopedContext(ctx))
	poolResult := make(chan error, 1)

	err = hsdb.Read(func(rx *gorm.DB) error {
		waitCount := sqlDB.Stats().WaitCount

		go func() {
			_, err := hsdb.GetUserByID(types.UserID(user.ID))
			poolResult <- err
		}()

		require.Eventually(t, func() bool {
			return sqlDB.Stats().WaitCount > waitCount
		}, time.Second, time.Millisecond)

		_, err := GetUserByID(rx, types.UserID(user.ID))

		return err
	})
	require.NoError(t, err)

	select {
	case err := <-poolResult:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(
			"pool query did not finish after the transaction released its connection",
		)
	}
}

func createSQLiteFromSQLFile(sqlFilePath, dbPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	schemaContent, err := os.ReadFile(sqlFilePath)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(context.Background(), string(schemaContent))

	return err
}

// requireConstraintFailed checks that a unique index refused the write.
func requireConstraintFailed(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, gorm.ErrDuplicatedKey)
}

func TestConstraints(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *gorm.DB)
	}{
		{
			name: "no-duplicate-username-if-no-oidc",
			run: func(t *testing.T, db *gorm.DB) { //nolint:thelper
				_, err := CreateUser(db, types.User{Name: "user1"})
				require.NoError(t, err)
				_, err = CreateUser(db, types.User{Name: "user1"})
				requireConstraintFailed(t, err)
			},
		},
		{
			name: "no-oidc-duplicate-username-and-id",
			run: func(t *testing.T, db *gorm.DB) { //nolint:thelper
				user := types.User{
					ID:   1,
					Name: "user1",
				}
				user.ProviderIdentifier = sql.NullString{String: "http://test.com/user1", Valid: true}

				err := db.Save(&user).Error
				require.NoError(t, err)

				user = types.User{
					ID:   2,
					Name: "user1",
				}
				user.ProviderIdentifier = sql.NullString{String: "http://test.com/user1", Valid: true}

				err = db.Save(&user).Error
				requireConstraintFailed(t, err)
			},
		},
		{
			name: "no-oidc-duplicate-id",
			run: func(t *testing.T, db *gorm.DB) { //nolint:thelper
				user := types.User{
					ID:   1,
					Name: "user1",
				}
				user.ProviderIdentifier = sql.NullString{String: "http://test.com/user1", Valid: true}

				err := db.Save(&user).Error
				require.NoError(t, err)

				user = types.User{
					ID:   2,
					Name: "user1.1",
				}
				user.ProviderIdentifier = sql.NullString{String: "http://test.com/user1", Valid: true}

				err = db.Save(&user).Error
				requireConstraintFailed(t, err)
			},
		},
		{
			name: "allow-duplicate-username-cli-then-oidc",
			run: func(t *testing.T, db *gorm.DB) { //nolint:thelper
				_, err := CreateUser(db, types.User{Name: "user1"}) // Create CLI username
				require.NoError(t, err)

				user := types.User{
					Name:               "user1",
					ProviderIdentifier: sql.NullString{String: "http://test.com/user1", Valid: true},
				}

				err = db.Save(&user).Error
				require.NoError(t, err)
			},
		},
		{
			name: "allow-duplicate-username-oidc-then-cli",
			run: func(t *testing.T, db *gorm.DB) { //nolint:thelper
				user := types.User{
					Name:               "user1",
					ProviderIdentifier: sql.NullString{String: "http://test.com/user1", Valid: true},
				}

				err := db.Save(&user).Error
				require.NoError(t, err)

				_, err = CreateUser(db, types.User{Name: "user1"}) // Create CLI username
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"-postgres", func(t *testing.T) {
			db := newPostgresTestDB(t)
			tt.run(t, db.DB.Debug())
		})
		t.Run(tt.name+"-sqlite", func(t *testing.T) {
			db, err := newSQLiteTestDB()
			if err != nil {
				t.Fatalf("creating database: %s", err)
			}

			tt.run(t, db.DB.Debug())
		})
	}
}

func dbForTest(t *testing.T) *HSDatabase {
	t.Helper()
	return dbForTestWithPath(t, "")
}

func dbForTestWithPath(t *testing.T, sqlFilePath string) *HSDatabase {
	t.Helper()

	dbPath := t.TempDir() + "/headscale_test.db"

	// If SQL file path provided, validate and create database from it
	if sqlFilePath != "" {
		// Validate that the file is a SQL text file
		if !strings.HasSuffix(sqlFilePath, ".sql") {
			t.Fatalf("dbForTestWithPath only accepts .sql files, got: %s", sqlFilePath)
		}

		err := createSQLiteFromSQLFile(sqlFilePath, dbPath)
		if err != nil {
			t.Fatalf("setting up database from SQL file %s: %s", sqlFilePath, err)
		}
	}

	db, err := NewHeadscaleDatabase(
		&types.Config{
			Database: types.DatabaseConfig{
				Type: "sqlite3",
				Sqlite: types.SqliteConfig{
					Path: dbPath,
				},
			},
			Policy: types.PolicyConfig{
				Mode: types.PolicyModeDB,
			},
		},
	)
	if err != nil {
		t.Fatalf("setting up database: %s", err)
	}

	if sqlFilePath != "" {
		t.Logf("database set up from %s at: %s", sqlFilePath, dbPath)
	} else {
		t.Logf("database set up at: %s", dbPath)
	}

	return db
}

// TestCredentialTableRoundTrip confirms the unified credentials table is created
// by migration (newSQLiteTestDB validates the schema with squibble) and stores
// and reads back a credential of each kind.
func TestCredentialTableRoundTrip(t *testing.T) {
	db, err := newSQLiteTestDB()
	require.NoError(t, err)

	now := time.Now().UTC()
	creds := []types.Credential{
		{Kind: types.CredentialAPIKey, Identifier: "apikey000001", Hash: []byte("$h1"), CreatedAt: &now},
		{Kind: types.CredentialPreAuthKey, Identifier: "authkey00001", Hash: []byte("$h2"), Reusable: true, Tags: []string{"tag:a"}, CreatedAt: &now},
		{Kind: types.CredentialOAuthClient, Identifier: "client000001", Hash: []byte("$h3"), Scopes: []string{"devices:read"}, CreatedAt: &now},
		{Kind: types.CredentialOAuthToken, Identifier: "oauthtok0001", Hash: []byte("$h4"), ClientID: "client000001", CreatedAt: &now},
	}

	for i := range creds {
		require.NoError(t, db.DB.Save(&creds[i]).Error)
	}

	var got []types.Credential
	require.NoError(t, db.DB.Order("id").Find(&got).Error)
	require.Len(t, got, 4)
	assert.Equal(t, types.CredentialPreAuthKey, got[1].Kind)
	assert.Equal(t, []string{"tag:a"}, got[1].Tags)
	assert.Equal(t, "client000001", got[3].ClientID)

	// The composite (kind, identifier) index permits the same identifier under a
	// different kind but rejects a duplicate within a kind.
	require.NoError(t, db.DB.Save(&types.Credential{
		Kind: types.CredentialAPIKey, Identifier: "client000001", Hash: []byte("$h5"), CreatedAt: &now,
	}).Error)

	err = db.DB.Save(&types.Credential{
		Kind: types.CredentialAPIKey, Identifier: "apikey000001", Hash: []byte("$dup"), CreatedAt: &now,
	}).Error
	require.Error(t, err, "duplicate (kind, identifier) must be rejected")
}

// requireCredentialConstraints asserts the credentials table rejects rows the
// application must never write: an unknown kind, a hash in no known format,
// and a missing hash on a usable (unrevoked) row.
func requireCredentialConstraints(t *testing.T, db *gorm.DB) {
	t.Helper()

	now := time.Now().UTC()

	bad := map[string]types.Credential{
		"unknown kind":        {Kind: "bogus", Identifier: "chk000000001", Hash: []byte("$sha256$00")},
		"hash format":         {Kind: types.CredentialAPIKey, Identifier: "chk000000002", Hash: []byte("plain")},
		"missing hash usable": {Kind: types.CredentialAPIKey, Identifier: "chk000000003"},
	}
	for name, cred := range bad {
		require.Error(t, db.Create(&cred).Error, name)
	}

	// Only the migration writes hashless rows, as NULL.
	require.NoError(t, db.Exec(
		`INSERT INTO credentials (tailnet_id, kind, identifier, hash, revoked) VALUES ((SELECT id FROM tailnets LIMIT 1), ?, ?, NULL, ?)`,
		types.CredentialPreAuthKey, "chk000000004", now,
	).Error, "a revoked row may lack a hash")
}

func TestCredentialConstraints(t *testing.T) {
	t.Run("fresh sqlite", func(t *testing.T) {
		db, err := newSQLiteTestDB()
		require.NoError(t, err)
		requireCredentialConstraints(t, db.DB)
	})

	t.Run("fresh postgres", func(t *testing.T) {
		requireCredentialConstraints(t, newPostgresTestDB(t).DB)
	})
}

// TestNodeAuthKeyOnlyResolvesPreAuthKeys points a node at a credential of
// another kind, which the shared table's foreign key permits, and asserts it
// is not loaded as the node's pre-auth key.
func TestNodeAuthKeyOnlyResolvesPreAuthKeys(t *testing.T) {
	db, err := newSQLiteTestDB()
	require.NoError(t, err)

	user := db.CreateUserForTest("kind-filter")
	node := db.CreateNodeForTest(user, "kind-filter")

	_, apiKey, err := db.CreateAPIKey(nil)
	require.NoError(t, err)

	require.NoError(t, db.DB.Model(&types.Node{}).Where("id = ?", node.ID).
		Update("auth_key_id", apiKey.ID).Error)

	got, err := db.GetNodeByID(node.ID)
	require.NoError(t, err)
	assert.Nil(t, got.AuthKey, "an API key must not load as a node's pre-auth key")
}

func sqliteTestConfig(path string) *types.Config {
	return &types.Config{
		Database: types.DatabaseConfig{
			Type:   "sqlite3",
			Sqlite: types.SqliteConfig{Path: path},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	}
}

// TestRevokedKeysBackingNodesSurviveCollection revokes the keys behind the
// migrated ephemeral nodes, runs the collector past the retention window and
// reloads: keys still backing a node must survive, so the nodes stay
// ephemeral, while unreferenced revoked keys are reaped.
//
// TODO(kradalby): seed without the 0.29.3 fixture in 0.31, when the
// credentials migration is dropped.
func TestRevokedKeysBackingNodesSurviveCollection(t *testing.T) {
	// v0.1 rewrite: the original used a Headscale 0.29.3 dump, which v0.1
	// refuses. The behaviour under test is unchanged: revoked pre-auth keys
	// still referenced by nodes are not reaped; unreferenced ones are.
	dbPath := t.TempDir() + "/headscale_test.db"

	hsdb, err := NewHeadscaleDatabase(sqliteTestConfig(dbPath))
	require.NoError(t, err)

	user, err := hsdb.CreateUser(types.User{Name: "backing"})
	require.NoError(t, err)

	var (
		revoked []uint64
		nodeIDs []types.NodeID
	)

	for i := range 2 {
		pak, err := hsdb.CreatePreAuthKey(user.TypedID(), false, true, nil, nil)
		require.NoError(t, err)

		pakID := pak.ID
		node := &types.Node{
			MachineKey:     key.NewMachine().Public(),
			NodeKey:        key.NewNode().Public(),
			Hostname:       fmt.Sprintf("ephemeral-%d", i),
			UserID:         &user.ID,
			RegisterMethod: util.RegisterMethodAuthKey,
			AuthKeyID:      &pakID,
			Ephemeral:      true,
		}
		require.NoError(t, hsdb.DB.Create(node).Error)

		revoked = append(revoked, pak.ID)
		nodeIDs = append(nodeIDs, node.ID)
	}

	unused, err := hsdb.CreatePreAuthKey(nil, false, false, nil, []string{"tag:x"})
	require.NoError(t, err)

	for _, id := range append(revoked, unused.ID) {
		require.NoError(t, hsdb.RevokePreAuthKey(id))
	}

	reaped, err := hsdb.DestroyRevokedPreAuthKeysBefore(time.Now().Add(24 * time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, reaped, "only the unreferenced revoked key is reaped")

	require.NoError(t, hsdb.Close())

	hsdb, err = NewHeadscaleDatabase(sqliteTestConfig(dbPath))
	require.NoError(t, err)

	ephemeral, err := hsdb.ListEphemeralNodes()
	require.NoError(t, err)

	ids := make([]types.NodeID, 0, len(ephemeral))
	for _, n := range ephemeral {
		ids = append(ids, n.ID)
	}

	assert.ElementsMatch(t, nodeIDs, ids)
}

// TestInterruptedInitSchemaRecovers simulates a first start that created the
// schema but died before gormigrate recorded the migrations, and asserts the
// next start completes instead of failing on the existing credentials table.
func TestInterruptedInitSchemaRecovers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dbPath := t.TempDir() + "/headscale_test.db"

		hsdb, err := NewHeadscaleDatabase(sqliteTestConfig(dbPath))
		require.NoError(t, err)
		require.NoError(t, hsdb.DB.Exec(`DELETE FROM migrations`).Error)
		require.NoError(t, hsdb.Close())

		hsdb, err = NewHeadscaleDatabase(sqliteTestConfig(dbPath))
		require.NoError(t, err)

		_, _, err = hsdb.CreateAPIKey(nil)
		require.NoError(t, err)
	})

	t.Run("postgres", func(t *testing.T) {
		u := newPostgresDBForTest(t)

		hsdb := newHeadscaleDBFromPostgresURL(t, u)
		require.NoError(t, hsdb.DB.Exec(`DELETE FROM migrations`).Error)
		require.NoError(t, hsdb.Close())

		hsdb = newHeadscaleDBFromPostgresURL(t, u)

		_, _, err := hsdb.CreateAPIKey(nil)
		require.NoError(t, err)
	})
}

func TestOpenDBReturnsOpenError(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(t *testing.T) types.DatabaseConfig
	}{
		{
			name: "sqlite-path-is-a-directory",
			cfg: func(t *testing.T) types.DatabaseConfig {
				t.Helper()

				return types.DatabaseConfig{
					Type:   types.DatabaseSqlite,
					Sqlite: types.SqliteConfig{Path: t.TempDir()},
				}
			},
		},
		{
			name: "postgres-unreachable-host",
			cfg: func(t *testing.T) types.DatabaseConfig {
				t.Helper()

				return types.DatabaseConfig{
					Type: types.DatabasePostgres,
					Postgres: types.PostgresConfig{
						Host: "127.0.0.1",
						Port: 1,
						Name: "headscale",
						User: "headscale",
						Ssl:  "false",
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				db  *gorm.DB
				err error
			)

			require.NotPanics(t, func() {
				cfg := tt.cfg(t)
				db, err = openDB(cfg, cfg.Sqlite.Path)
			})
			require.Error(t, err)
			assert.Nil(t, db)
		})
	}
}

// TestSQLiteWriteAheadLog switches one database between modes, because SQLite
// stores the journal mode in the file and turning WAL off has to undo it.
func TestSQLiteWriteAheadLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headscale.db")

	open := func(wal bool) (string, int) {
		t.Helper()

		cfg := sqliteTestConfig(path)
		cfg.Database.Sqlite.WriteAheadLog = wal

		hsdb, err := NewHeadscaleDatabase(cfg)
		require.NoError(t, err)

		var (
			mode        string
			synchronous int
		)

		require.NoError(t, hsdb.DB.Raw("PRAGMA journal_mode").Scan(&mode).Error)
		require.NoError(t, hsdb.DB.Raw("PRAGMA synchronous").Scan(&synchronous).Error)
		require.NoError(t, hsdb.Close())

		return mode, synchronous
	}

	const normal, full = 1, 2

	mode, synchronous := open(true)
	assert.Equal(t, "wal", mode)
	assert.Equal(t, normal, synchronous)

	mode, synchronous = open(false)
	assert.Equal(t, "delete", mode)
	assert.Equal(t, full, synchronous)

	mode, _ = open(true)
	assert.Equal(t, "wal", mode)
}
