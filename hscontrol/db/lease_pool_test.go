package db

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// The cluster lease on a pool of its own (P13b, ADR 0006 §3.6), ported in
// P14f F5: a join burst in one large tailnet held every data connection and
// the lease renewal queued behind them until the process self-fenced.

// TestLeaseStoreHasOwnPool: with the data pool exhausted, the lease store
// still acquires and renews.
func TestLeaseStoreHasOwnPool(t *testing.T) {
	pu := newPostgresDBForTest(t)
	pass, _ := pu.User.Password()
	port, _ := strconv.Atoi(pu.Port())

	cfg := &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host:               pu.Hostname(),
				Port:               port,
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
	l, err := p.LeaseStore().Acquire(ctx, "p14f-cluster", "p14f-holder", 10*time.Second)
	require.NoError(t, err, "lease acquire must not wait for the data pool (took %s)", time.Since(start))
	assert.Equal(t, uint64(1), l.Epoch)

	require.NoError(t, p.LeaseStore().Renew(ctx, l, 10*time.Second))
}

// TestLeaseDatabase: the lease pool uses lease_name (a PgBouncer alias) and
// lease_max_open_conns, the rest of the data pool's settings, and never more
// than its own cap.
func TestLeaseDatabase(t *testing.T) {
	base := types.DatabaseConfig{Type: types.DatabasePostgres, Postgres: types.PostgresConfig{
		Host: "db", Port: 6432, Name: "meshlab", User: "headscale", Ssl: "disable",
		MaxOpenConnections: 100, MaxIdleConnections: 10,
	}}

	got := leaseDatabase(base).Postgres
	assert.Equal(t, "meshlab", got.Name, "no alias: the same database")
	assert.Equal(t, 2, got.MaxOpenConnections, "unset cap: 2")
	assert.Equal(t, 2, got.MaxIdleConnections)
	assert.Equal(t, "db", got.Host)
	assert.Equal(t, 6432, got.Port)

	base.Postgres.LeaseName, base.Postgres.LeaseMaxOpenConnections = "mesh_lease", 4
	got = leaseDatabase(base).Postgres
	assert.Equal(t, "mesh_lease", got.Name)
	assert.Equal(t, 4, got.MaxOpenConnections)
	assert.Equal(t, "meshlab", base.Postgres.Name, "the data pool's config is not changed")
}
