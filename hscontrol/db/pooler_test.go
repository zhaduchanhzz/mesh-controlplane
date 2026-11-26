package db

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// P13b (ADR 0011 §D, ADR 0006 §3.5-3.6): the client side of HA PgBouncer.
// These tests need real servers: tools/pgtest/throwaway.sh starts a writable
// and a read-only PostgreSQL and PgBouncers in front of them, and writes the
// MESH_TEST_PG_* / MESH_TEST_PGBOUNCER* variables. Without them the tests
// skip (CI runs only the plain PostgreSQL ones).

// poolerEnv returns the host:port in env var name, or skips t.
func poolerEnv(t *testing.T, name string) string {
	t.Helper()

	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set (tools/pgtest/throwaway.sh)", name)
	}

	return v
}

func poolerUser(t *testing.T) string {
	t.Helper()

	return poolerEnv(t, "MESH_TEST_PG_USER")
}

// adminOn connects to database postgres on the PostgreSQL server at
// hostport, able to write even on the read-only test server.
func adminOn(t *testing.T, hostport string) *gorm.DB {
	t.Helper()

	host, port, err := net.SplitHostPort(hostport)
	require.NoError(t, err)

	// Key/value form: pgx's URL parser refuses "=" inside a query value.
	dsn := fmt.Sprintf("host=%s port=%s user=%s dbname=postgres sslmode=disable "+
		"options='-c default_transaction_read_only=off'", host, port, poolerUser(t))

	conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)

	t.Cleanup(func() {
		sqlDB, err := conn.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return conn
}

// createDBOn creates an empty database named name on every PostgreSQL
// server in hostports and drops it after t.
func createDBOn(t *testing.T, name string, hostports ...string) {
	t.Helper()

	for _, hp := range hostports {
		admin := adminOn(t, hp)
		require.NoError(t, admin.Exec(`CREATE DATABASE `+name).Error, "create %s on %s", name, hp)

		t.Cleanup(func() {
			a := adminOn(t, hp)
			_ = a.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`).Error
		})
	}
}

func p13bDBName() string { return fmt.Sprintf("mesh_p13b_%d", time.Now().UnixNano()) }

// p13bConfig is a platform config on database name; hosts is written as
// database.postgres.host in the libpq list form "a:1,b:2".
// A single host:port is split into host and port, the only form a single
// server had before P13b.
func p13bConfig(t *testing.T, name, hosts string) *types.Config {
	t.Helper()

	host, port := hosts, 0
	if !strings.Contains(hosts, ",") {
		h, p, err := net.SplitHostPort(hosts)
		require.NoError(t, err)

		host, port = h, atoiOr(p, 0)
	}

	return &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host:                host,
				Port:                port,
				User:                poolerUser(t),
				Name:                name,
				Ssl:                 "disable",
				MaxOpenConnections:  4,
				MaxIdleConnections:  2,
				ConnMaxIdleTimeSecs: 60,
			},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	}
}

// serverPort is the port of the PostgreSQL server behind conn (through a
// pooler too).
func serverPort(t *testing.T, conn *gorm.DB) string {
	t.Helper()

	var port string
	require.NoError(t, conn.Raw(`SELECT current_setting('port')`).Scan(&port).Error)

	return port
}

func portOf(t *testing.T, hostport string) string {
	t.Helper()

	_, port, err := net.SplitHostPort(hostport)
	require.NoError(t, err)

	return port
}

// closedPort is a 127.0.0.1 port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

// TestOpenPlatformMultiHostSkipsReadOnly (P13b, ADR 0011 §D): with a host
// list whose first entry is read-only (a replica, or a PgBouncer in front
// of one), the platform must land on the writable server: every process
// that opens the platform writes (migrations, fenced writes, lease).
func TestOpenPlatformMultiHostSkipsReadOnly(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	ro := poolerEnv(t, "MESH_TEST_PG_RO")

	cases := []struct {
		name   string
		first  string
		second string
	}{
		{"direct", ro, rw},
		{"pgbouncer", poolerEnv(t, "MESH_TEST_PGBOUNCER_RO"), poolerEnv(t, "MESH_TEST_PGBOUNCER")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := p13bDBName()
			createDBOn(t, name, rw, ro)

			p, err := OpenPlatform(p13bConfig(t, name, tc.first+","+tc.second))
			require.NoError(t, err, "a read-only first host must be skipped, not used")

			defer p.Close()

			var readOnly string
			require.NoError(t, p.db.Raw(`SHOW transaction_read_only`).Scan(&readOnly).Error)
			assert.Equal(t, "off", readOnly)
			assert.Equal(t, portOf(t, rw), serverPort(t, p.db), "platform pool on the writable server")
		})
	}
}

// TestOpenPlatformMultiHostSkipsDeadHost: a host that refuses connections
// (PgBouncer killed) is skipped. Regression: pgx already does this.
func TestOpenPlatformMultiHostSkipsDeadHost(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	pgb := poolerEnv(t, "MESH_TEST_PGBOUNCER")

	name := p13bDBName()
	createDBOn(t, name, rw)

	p, err := OpenPlatform(p13bConfig(t, name, closedPort(t)+","+pgb))
	require.NoError(t, err)

	defer p.Close()

	assert.Equal(t, portOf(t, rw), serverPort(t, p.db))
}

// TestOpenPlatformRefusesPoolerWithoutPreparedStatements (P13b, ADR 0006
// §3.5): PgBouncer in transaction mode with max_prepared_statements = 0
// breaks pgx's named prepared statements, but only once two client
// connections share a server connection, i.e. under load and at random.
// Opening the platform must refuse it at once and say why.
func TestOpenPlatformRefusesPoolerWithoutPreparedStatements(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	pgb0 := poolerEnv(t, "MESH_TEST_PGBOUNCER_NOPREP")

	name := p13bDBName()
	createDBOn(t, name, rw)

	p, err := OpenPlatform(p13bConfig(t, name, pgb0))
	if err == nil {
		p.Close()
	}

	require.Error(t, err, "a pooler without prepared statement support must be refused at open")
	assert.Contains(t, err.Error(), "max_prepared_statements", "the error names the pooler setting")
}

// TestLeaseStoreHasOwnPool (P13b, ADR 0006 §3.6): lease operations
// (renew of the active, poll of the standby) must not queue behind data
// traffic. With the data pool exhausted, the lease store still answers.
func TestLeaseStoreHasOwnPool(t *testing.T) {
	pu := newPostgresDBForTest(t)
	pass, _ := pu.User.Password()

	cfg := &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host:               pu.Hostname(),
				Port:               atoiOr(pu.Port(), 0),
				User:               pu.User.Username(),
				Pass:               types.Secret(pass),
				Name:               strings.TrimLeft(pu.Path, "/"),
				Ssl:                "disable",
				MaxOpenConnections: 1,
				MaxIdleConnections: 1,
			},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	}

	p, err := OpenPlatform(cfg)
	require.NoError(t, err)

	defer p.Close()

	// Data traffic holds every connection of the data pool.
	sqlDB, err := p.db.DB()
	require.NoError(t, err)

	tx, err := sqlDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)

	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	start := time.Now()
	l, err := p.LeaseStore().Acquire(ctx, "p13b-cluster", "p13b-holder", 10*time.Second)
	require.NoError(t, err, "lease acquire must not wait for the data pool (took %s)", time.Since(start))
	assert.Equal(t, uint64(1), l.Epoch)

	require.NoError(t, p.LeaseStore().Renew(ctx, l, 10*time.Second))
}

func atoiOr(s string, def int) int {
	var n int

	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil {
		return def
	}

	return n
}
