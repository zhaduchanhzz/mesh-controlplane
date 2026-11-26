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

// migrateP13b runs the operator migrate step (ADR 0013) with cfg: since
// P13d OpenPlatform never creates the schema of a fresh database.
func migrateP13b(t *testing.T, cfg *types.Config) {
	t.Helper()

	_, err := Migrate(t.Context(), cfg)
	require.NoError(t, err, "migrate step")
}

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

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
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

			cfg := p13bConfig(t, name, tc.first+","+tc.second)

			// The migrate step writes too: it must skip the read-only
			// first host as well.
			migrateP13b(t, cfg)

			p, err := OpenPlatform(cfg)
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
// (PgBouncer killed) is skipped and the next one used.
func TestOpenPlatformMultiHostSkipsDeadHost(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	pgb := poolerEnv(t, "MESH_TEST_PGBOUNCER")

	name := p13bDBName()
	createDBOn(t, name, rw)

	cfg := p13bConfig(t, name, closedPort(t)+","+pgb)
	migrateP13b(t, cfg)

	p, err := OpenPlatform(cfg)
	require.NoError(t, err)

	defer p.Close()

	assert.Equal(t, portOf(t, rw), serverPort(t, p.db))
}

// TestOpenPlatformHostsKey: the same through database.postgres.hosts, with
// a bracketed IPv6 entry first (nothing listens on that port of ::1), which
// pgx must parse and skip.
func TestOpenPlatformHostsKey(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	ro := poolerEnv(t, "MESH_TEST_PG_RO")
	pgb := poolerEnv(t, "MESH_TEST_PGBOUNCER")

	name := p13bDBName()
	createDBOn(t, name, rw, ro)

	cfg := p13bConfig(t, name, pgb)
	cfg.Database.Postgres.Host, cfg.Database.Postgres.Port = "", 0
	cfg.Database.Postgres.Hosts = []string{"[::1]:" + portOf(t, closedPort(t)), ro, pgb}
	migrateP13b(t, cfg)

	p, err := OpenPlatform(cfg)
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
	migrateP13b(t, p13bConfig(t, name, rw)) // the operator step, direct (ADR 0006 §4)

	p, err := OpenPlatform(p13bConfig(t, name, pgb0))
	if err == nil {
		p.Close()
	}

	require.Error(t, err, "a pooler without prepared statement support must be refused at open")
	assert.Contains(t, err.Error(), "max_prepared_statements", "the error names the pooler setting")
	require.ErrorIs(t, err, ErrPoolerMisconfigured)
	t.Logf("refused: %v", err)

	// The same pooler with the simple protocol (no prepared statements):
	// the documented way out, accepted.
	cfg := p13bConfig(t, name, pgb0)
	cfg.Database.Postgres.QueryExecMode = types.PostgresQueryExecSimpleProtocol

	p, err = OpenPlatform(cfg)
	require.NoError(t, err)
	p.Close()
}

// TestOpenReadOnlyRefusesPoolerWithoutPreparedStatements: mesh-bootstrap's
// read-only handle gets the same check (but no read-only one).
func TestOpenReadOnlyRefusesPoolerWithoutPreparedStatements(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	ro := poolerEnv(t, "MESH_TEST_PG_RO")
	pgb0 := poolerEnv(t, "MESH_TEST_PGBOUNCER_NOPREP")

	name := p13bDBName()
	createDBOn(t, name, rw, ro)

	_, err := OpenReadOnly(p13bConfig(t, name, pgb0))
	require.ErrorIs(t, err, ErrPoolerMisconfigured)

	// A read-only server passes the self-check (no v0.1 schema there).
	_, err = OpenReadOnly(p13bConfig(t, name, ro))
	require.ErrorIs(t, err, ErrSchemaNotCurrent)
	assert.NotErrorIs(t, err, ErrReadOnlyDatabase)
}

// TestOpenPlatformStatementTimeout: statement_timeout is a startup
// parameter. PostgreSQL applies it; PgBouncer refuses it unless it tracks
// it (track_extra_parameters), and the self-check says so.
func TestOpenPlatformStatementTimeout(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")

	for _, tc := range []struct {
		name    string
		via     string
		wantErr string
	}{
		{"direct", rw, ""},
		{"pgbouncer tracking it", poolerEnv(t, "MESH_TEST_PGBOUNCER_TRACK"), ""},
		{"pgbouncer not tracking it", poolerEnv(t, "MESH_TEST_PGBOUNCER"), "track_extra_parameters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := p13bDBName()
			createDBOn(t, name, rw)
			migrateP13b(t, p13bConfig(t, name, rw))

			cfg := p13bConfig(t, name, tc.via)
			cfg.Database.Postgres.StatementTimeout = 1500 * time.Millisecond

			p, err := OpenPlatform(cfg)
			if tc.wantErr != "" {
				require.ErrorIs(t, err, ErrPoolerMisconfigured)
				assert.Contains(t, err.Error(), tc.wantErr)
				t.Logf("refused: %v", err)

				return
			}

			require.NoError(t, err)

			defer p.Close()

			var st string
			require.NoError(t, p.db.Raw(`SHOW statement_timeout`).Scan(&st).Error)
			assert.Equal(t, "1500ms", st)
		})
	}
}

// TestOpenPlatformRefusesReadOnlyServer: a writer that reaches only a
// read-only server is refused at open with ErrReadOnlyDatabase, with the
// default target (read-write: pgx refuses the server) and with any (the
// self-check's SHOW transaction_read_only).
func TestOpenPlatformRefusesReadOnlyServer(t *testing.T) {
	rw := poolerEnv(t, "MESH_TEST_PG_RW")
	ro := poolerEnv(t, "MESH_TEST_PG_RO")

	for _, attrs := range []types.PostgresTargetSessionAttrs{"", types.PostgresTargetAny} {
		t.Run("target="+string(attrs), func(t *testing.T) {
			name := p13bDBName()
			createDBOn(t, name, rw, ro)

			cfg := p13bConfig(t, name, poolerEnv(t, "MESH_TEST_PGBOUNCER_RO"))
			cfg.Database.Postgres.TargetSessionAttrs = attrs

			_, err := OpenPlatform(cfg)
			require.ErrorIs(t, err, ErrReadOnlyDatabase)
			t.Logf("refused: %v", err)
		})
	}
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

	migrateP13b(t, cfg)

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

// TestLeaseDatabase: the lease pool uses lease_name (a PgBouncer alias) and
// lease_max_open_conns, the rest of the data pool's settings, and never more
// than its own cap.
func TestLeaseDatabase(t *testing.T) {
	base := types.DatabaseConfig{Type: types.DatabasePostgres, Postgres: types.PostgresConfig{
		Hosts: []string{"a:6432", "b:6432"}, Name: "meshlab", User: "headscale",
		MaxOpenConnections: 10, MaxIdleConnections: 10, TargetSessionAttrs: types.PostgresTargetReadWrite,
	}}

	got := leaseDatabase(base).Postgres
	assert.Equal(t, "meshlab", got.Name, "no alias: the same database")
	assert.Equal(t, 2, got.MaxOpenConnections, "unset cap: 2")
	assert.Equal(t, 2, got.MaxIdleConnections)
	assert.Equal(t, base.Postgres.Hosts, got.Hosts)
	assert.Equal(t, types.PostgresTargetReadWrite, got.TargetSessionAttrs)

	base.Postgres.LeaseName, base.Postgres.LeaseMaxOpenConnections = "mesh_lease", 4
	got = leaseDatabase(base).Postgres
	assert.Equal(t, "mesh_lease", got.Name)
	assert.Equal(t, 4, got.MaxOpenConnections)
	assert.Equal(t, "meshlab", base.Postgres.Name, "the data pool's config is not changed")
}

func atoiOr(s string, def int) int {
	var n int

	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil {
		return def
	}

	return n
}
