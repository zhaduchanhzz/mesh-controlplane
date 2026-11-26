package db

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
)

// P13d (ADR 0013): migrations are an operator step. Cluster processes,
// mesh-placement and the admin CLI open the database with [OpenPlatform],
// which only checks that the schema is one this binary can run with; the
// schema is created and migrated by [Migrate] (`mesh-placement migrate`,
// `headscale cluster migrate`) only.

var (
	// ErrSchemaNeedsMigration reports a database that lacks a migration this
	// binary needs (or has no schema at all): run the migrate step first.
	ErrSchemaNeedsMigration = errors.New(
		"database schema is behind this binary; run `mesh-placement migrate` (or `headscale cluster migrate`) first")
	// ErrSchemaTooNew reports a database with a migration this binary does
	// not know and may not run with (a contract migration, or one not marked
	// compatible in schema_compat): the binary is too old for the database.
	ErrSchemaTooNew = errors.New("database schema is newer than this binary allows; upgrade the binary")
)

// MigrateResult says what [Migrate] did.
type MigrateResult struct {
	// Fresh is true when the database had no schema and got the whole v0.1
	// schema from the schema files.
	Fresh bool `json:"fresh"`
	// Applied lists the migrations this run recorded, in chain order; empty
	// when the database was already current (e.g. another run did it).
	Applied []string `json:"applied"`
	// Latest is the last migration this binary knows.
	Latest string `json:"latest"`
}

// chain is a migration chain with its compatibility declarations
// (schema_compat, ADR 0013 §3): the production chain is [currentChain];
// tests build longer chains to play a newer binary.
type chain struct {
	migrations []*gormigrate.Migration
	// compat maps each migration from SchemaCompatMigrationID on to its
	// min_reader_id.
	compat map[string]string
}

func currentChain() chain {
	return chain{migrations: v01Migrations(), compat: migrationCompat}
}

func (ch chain) latest() string { return ch.migrations[len(ch.migrations)-1].ID }

func (ch chain) known() map[string]bool {
	k := map[string]bool{schemaInitMigration: true}
	for _, m := range ch.migrations {
		k[m.ID] = true
	}

	return k
}

// Migrate brings the database of cfg to this binary's schema: on a fresh
// database it creates the v0.1 schema, otherwise it runs the pending
// migrations. It is the only path that changes the schema (ADR 0013).
func Migrate(ctx context.Context, cfg *types.Config) (MigrateResult, error) {
	return migrateChain(ctx, cfg, currentChain())
}

// MigrateAndOpenPlatform migrates (see [Migrate]) and then opens the
// platform on the same connection pool. It is the explicit development and
// single-tenant option of ADR 0013 §Hệ quả: never used by a cluster process,
// mesh-placement serve or the cluster admin CLI.
func MigrateAndOpenPlatform(cfg *types.Config) (*Platform, error) {
	return openPlatformChain(cfg, currentChain(), true)
}

func migrateChain(ctx context.Context, cfg *types.Config, ch chain) (MigrateResult, error) {
	conn, err := openDB(cfg.Database, cfg.SQLitePath())
	if err != nil {
		return MigrateResult{}, err
	}

	defer func() {
		sqlDB, err := conn.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	}()

	return migrateConn(ctx, conn, cfg, ch)
}

// migrateConn is the migrate step on conn. RED stub (P13d): today's
// behaviour, no lock.
func migrateConn(ctx context.Context, conn *gorm.DB, cfg *types.Config, ch chain) (MigrateResult, error) {
	res := MigrateResult{Latest: ch.latest()}

	err := checkV01Database(conn, ch)
	if err != nil {
		return res, fmt.Errorf("version check: %w", err)
	}

	err = checkVersionUpgradePath(conn)
	if err != nil {
		return res, fmt.Errorf("version check: %w", err)
	}

	before, err := recordedMigrations(conn.WithContext(ctx))
	if err != nil {
		return res, err
	}

	migrations := gormigrate.New(conn.WithContext(ctx), gormigrate.DefaultOptions, ch.migrations)
	migrations.InitSchema(func(tx *gorm.DB) error {
		return tx.Transaction(createSchema)
	})

	err = runMigrations(cfg.Database, conn, migrations)
	if err != nil {
		return res, fmt.Errorf("migration failed: %w", err)
	}

	after, err := recordedMigrations(conn.WithContext(ctx))
	if err != nil {
		return res, err
	}

	res.Fresh = len(before) == 0

	for _, m := range ch.migrations {
		if after[m.ID] && !before[m.ID] {
			res.Applied = append(res.Applied, m.ID)
		}
	}

	return res, nil
}

// recordedMigrations returns the IDs in the migrations table; none when the
// table does not exist.
func recordedMigrations(db *gorm.DB) (map[string]bool, error) {
	present, err := tableExists(db, "migrations")
	if err != nil || !present {
		return map[string]bool{}, err
	}

	var ids []string

	err = db.Table("migrations").Pluck("id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("%w: reading migrations: %w", ErrDatabaseUnavailable, err)
	}

	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}

	return out, nil
}

// sortedKeys is for messages.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	slices.Sort(out)

	return out
}
