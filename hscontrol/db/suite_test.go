package db

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"zombiezen.com/go/postgrestest"
)

func newSQLiteTestDB() (*HSDatabase, error) {
	tmpDir, err := os.MkdirTemp("", "headscale-db-test-*")
	if err != nil {
		return nil, err
	}

	log.Printf("database path: %s", tmpDir+"/headscale_test.db")

	db, err := NewHeadscaleDatabase(
		&types.Config{
			Database: types.DatabaseConfig{
				Type: types.DatabaseSqlite,
				Sqlite: types.SqliteConfig{
					Path: tmpDir + "/headscale_test.db",
				},
			},
			Policy: types.PolicyConfig{
				Mode: types.PolicyModeDB,
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func newPostgresTestDB(t *testing.T) *HSDatabase {
	t.Helper()

	return newHeadscaleDBFromPostgresURL(t, newPostgresDBForTest(t))
}

// newPostgresDBForTest returns a fresh, empty PostgreSQL database for t:
// a database created on the server MESH_TEST_POSTGRES_DSN names when it is
// set (CI, mc-ci), otherwise one on a throwaway server postgrestest starts.
// Without either the test is skipped, unless MESH_TEST_REQUIRE_POSTGRES is
// set (CI): then it fails, so PostgreSQL coverage cannot vanish silently
// (P12c A5).
func newPostgresDBForTest(t *testing.T) *url.URL {
	t.Helper()

	if dsn := os.Getenv("MESH_TEST_POSTGRES_DSN"); dsn != "" {
		return newPostgresDBFromDSN(t, dsn)
	}

	ctx := t.Context()

	srv, err := postgrestest.Start(ctx)
	if err != nil {
		if os.Getenv("MESH_TEST_REQUIRE_POSTGRES") != "" {
			t.Fatalf("MESH_TEST_REQUIRE_POSTGRES is set but PostgreSQL did not start: %s", err)
		}

		t.Skipf("start postgres: %s", err)
	}

	t.Cleanup(srv.Cleanup)

	u, err := srv.CreateDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("created local postgres: %s", u)
	pu, _ := url.Parse(u)

	return pu
}

// newPostgresDBFromDSN creates a database for t on the server of a
// key=value DSN (host, port, user, password, dbname) and drops it after t.
func newPostgresDBFromDSN(t *testing.T, dsn string) *url.URL {
	t.Helper()

	kv := map[string]string{}

	for _, f := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			t.Fatalf("MESH_TEST_POSTGRES_DSN: want key=value fields, got %q", k)
		}

		kv[k] = v
	}

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("connecting to MESH_TEST_POSTGRES_DSN: %v", err)
	}

	name := fmt.Sprintf("mesh_db_test_%d", time.Now().UnixNano())

	err = admin.Exec(`CREATE DATABASE ` + name).Error
	if err != nil {
		t.Fatalf("creating test database: %v", err)
	}

	t.Cleanup(func() {
		_ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`).Error

		sqlDB, err := admin.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	port := kv["port"]
	if port == "" {
		port = "5432"
	}

	u := &url.URL{Scheme: "postgres", Host: kv["host"] + ":" + port, Path: "/" + name}
	if pass, ok := kv["password"]; ok {
		u.User = url.UserPassword(kv["user"], pass)
	} else {
		u.User = url.User(kv["user"])
	}

	return u
}

func newHeadscaleDBFromPostgresURL(t *testing.T, pu *url.URL) *HSDatabase {
	t.Helper()

	pass, _ := pu.User.Password()
	port, _ := strconv.Atoi(pu.Port())

	db, err := NewHeadscaleDatabase(
		&types.Config{
			Database: types.DatabaseConfig{
				Type: types.DatabasePostgres,
				Postgres: types.PostgresConfig{
					Host: pu.Hostname(),
					User: pu.User.Username(),
					Name: strings.TrimLeft(pu.Path, "/"),
					Pass: types.Secret(pass),
					Port: port,
					Ssl:  "disable",
				},
			},
			Policy: types.PolicyConfig{
				Mode: types.PolicyModeDB,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	return db
}
