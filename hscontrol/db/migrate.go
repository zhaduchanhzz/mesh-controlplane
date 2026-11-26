package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// sqliteSchema is the SQLite schema a database with exactly these
	// migrations has (squibble): schema.sql for the production chain.
	sqliteSchema string
}

func currentChain() chain {
	return chain{migrations: v01Migrations(), compat: migrationCompat, sqliteSchema: dbSchema}
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

	res, err := migrateConn(ctx, conn, cfg, ch)
	if err != nil {
		return res, err
	}

	// SQLite: the migrated schema must match schema.sql (squibble), as
	// OpenPlatform checks; outside the transaction, on its own connection.
	exact, err := verifySchema(conn, ch)
	if err != nil {
		return res, err
	}

	if exact && cfg.Database.IsSQLite() {
		err = validateSQLiteSchema(conn, ch.sqliteSchema) //nolint:contextcheck // bounded by contextTimeout inside, as OpenPlatform's
		if err != nil {
			return res, err
		}
	}

	return res, nil
}

// migrateLockSQL serialises migrate runs on PostgreSQL (L7): a
// transaction-scoped advisory lock, released by COMMIT, ROLLBACK or a lost
// connection, and usable through PgBouncer in transaction mode (ADR 0006
// §3.2). The key is fixed and distinct from the lease and ownership locks.
const migrateLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('mesh/schema-migrate', 0))`

// migrateConn is the migrate step on conn, all in one transaction: on
// PostgreSQL under migrateLockSQL, on SQLite under the write lock its
// IMMEDIATE transaction takes at BEGIN (sqliteconfig). A second run waits
// for the first and then finds nothing to do. A failure rolls everything
// back: the schema, the migrations rows, schema_compat and the version.
func migrateConn(ctx context.Context, conn *gorm.DB, cfg *types.Config, ch chain) (MigrateResult, error) {
	res := MigrateResult{Latest: ch.latest(), Applied: []string{}}

	err := conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Name() == dialectPostgres {
			err := tx.Exec(migrateLockSQL).Error
			if err != nil {
				return fmt.Errorf("waiting for the migrate lock: %w", err)
			}
		}

		fresh, err := checkV01Database(tx)
		if err != nil {
			return fmt.Errorf("version check: %w", err)
		}

		before, err := recordedMigrations(tx)
		if err != nil {
			return err
		}

		// A database a newer binary migrated: this binary runs with it only
		// if every unknown migration allows it, and never migrates it (it
		// cannot know what the newer chain expects).
		if unknown := unknownMigrations(before, ch); len(unknown) > 0 {
			err := checkSchemaCompat(tx, ch, before)
			if errors.Is(err, ErrSchemaNeedsMigration) {
				return fmt.Errorf("%w: the database has migrations this binary does not know (%s) and lacks some of "+
					"its own; migrate with the newest binary", ErrSchemaTooNew, strings.Join(unknown, ", "))
			}

			return err // nil: compatible, nothing to do
		}

		err = checkVersionUpgradePath(tx)
		if err != nil {
			return fmt.Errorf("version check: %w", err)
		}

		migrations := gormigrate.New(tx, gormigrate.DefaultOptions, ch.migrations)
		migrations.InitSchema(func(itx *gorm.DB) error {
			return itx.Transaction(createSchema)
		})

		err = runMigrations(cfg.Database, tx, migrations)
		if err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}

		after, err := recordedMigrations(tx)
		if err != nil {
			return err
		}

		err = recordSchemaCompat(tx, ch, after)
		if err != nil {
			return err
		}

		res.Fresh = fresh

		for _, m := range ch.migrations {
			if after[m.ID] && !before[m.ID] {
				res.Applied = append(res.Applied, m.ID)
			}
		}

		// The version the database was last migrated by (versioncheck):
		// written here only, never by OpenPlatform. Dev builds keep it.
		if v := types.GetVersionInfo().Version; !isDev(v) {
			err = setDatabaseVersion(tx, v)
			if err != nil {
				return fmt.Errorf("storing database version: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return MigrateResult{Latest: ch.latest()}, err
	}

	return res, nil
}

// verifySchema checks, reading only, that db holds a v0.1 schema the binary
// of chain ch can run with (ADR 0013 §4): ErrNotV01Database for a Headscale
// or foreign database, ErrSchemaNeedsMigration for no schema or a missing
// migration, ErrSchemaTooNew for a newer contract migration. exact reports
// that the database has exactly ch's migrations, so the schema files
// describe it.
func verifySchema(db *gorm.DB, ch chain) (bool, error) {
	fresh, err := checkV01Database(db)
	if err != nil {
		return false, fmt.Errorf("version check: %w", err)
	}

	if fresh {
		return false, fmt.Errorf("%w: the database has no schema (this binary's latest migration: %s)",
			ErrSchemaNeedsMigration, ch.latest())
	}

	recorded, err := recordedMigrations(db)
	if err != nil {
		return false, err
	}

	err = checkSchemaCompat(db, ch, recorded)
	if err != nil {
		return false, err
	}

	return len(unknownMigrations(recorded, ch)) == 0, nil
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
