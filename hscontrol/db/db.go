package db

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/rs/zerolog/log"
	"github.com/tailscale/squibble"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/sqliteconfig"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util/zlog/zf"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

//go:embed schema.sql
var dbSchema string

//go:embed schema_postgres.sql
var dbSchemaPostgres string

func init() {
	schema.RegisterSerializer("text", TextSerialiser{})
}

var errDatabaseNotSupported = errors.New("database type not supported")

var errForeignKeyConstraintsViolated = errors.New("foreign key constraints violated")

const contextTimeout = 10 * time.Second

type HSDatabase struct {
	// DB is confined to the runtime's tailnet by the tenantscope plugin:
	// every statement on a tenant table is filtered and stamped with it.
	DB *gorm.DB
	// raw is the unscoped connection, for the platform-level tailnets table.
	raw     *gorm.DB
	tailnet types.Tailnet
	cfg     *types.Config
}

// Tailnet returns the tailnet this handle is bound to.
func (hsdb *HSDatabase) Tailnet() types.Tailnet { return hsdb.tailnet }

// v01InitMigration marks a database created from the v0.1 schema files. The
// upstream Headscale migration chain is not carried: v0.1 does not upgrade
// Headscale databases (tickets/P03.md).
const v01InitMigration = "202611220000-v0.1-init"

// NewHeadscaleDatabase opens the database, creates the v0.1 schema on a fresh
// one, refuses databases from Headscale or unknown versions, validates the
// schema and binds the handle to the runtime's tailnet (tenantscope).
func NewHeadscaleDatabase(cfg *types.Config) (_ *HSDatabase, retErr error) {
	dbConn, err := openDB(cfg.Database, cfg.SQLitePath())
	if err != nil {
		return nil, err
	}

	// Release the connection on every failure path (e.g. a refused Headscale
	// database); otherwise it leaks and, on Windows, keeps the file locked.
	defer func() {
		if retErr != nil {
			if sqlDB, err := dbConn.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
	}()

	err = checkV01Database(dbConn)
	if err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}

	err = checkVersionUpgradePath(dbConn)
	if err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}

	migrations := gormigrate.New(
		dbConn,
		gormigrate.DefaultOptions,
		[]*gormigrate.Migration{
			{
				// The schema files create everything; this ID only marks a
				// v0.1 database.
				ID:       v01InitMigration,
				Migrate:  func(*gorm.DB) error { return nil },
				Rollback: func(*gorm.DB) error { return nil },
			},
			// New migrations are appended here, each in tx.Transaction, with
			// explicit DDL for both SQLite and PostgreSQL (hscontrol/db/AGENTS.md).
		},
	)

	migrations.InitSchema(func(tx *gorm.DB) error {
		return tx.Transaction(createSchema)
	})

	err = runMigrations(cfg.Database, dbConn, migrations)
	if err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	// Store the current version in the database after migrations succeed.
	// Dev builds skip this to preserve the stored version for the next
	// real versioned binary.
	currentVersion := types.GetVersionInfo().Version
	if !isDev(currentVersion) {
		err = setDatabaseVersion(dbConn, currentVersion)
		if err != nil {
			return nil, fmt.Errorf(
				"storing database version: %w",
				err,
			)
		}
	}

	// Validate that the schema ends up in the expected state (SQLite only;
	// squibble does not support PostgreSQL). Done in a helper so its
	// dedicated connection is released before the runtime binds its tailnet:
	// SQLite allows a single open connection.
	if cfg.Database.IsSQLite() {
		err = validateSQLiteSchema(dbConn)
		if err != nil {
			return nil, err
		}
	}

	scoped, tn, err := bindRuntimeTailnet(dbConn, cfg)
	if err != nil {
		return nil, fmt.Errorf("binding runtime tailnet: %w", err)
	}

	db := HSDatabase{
		DB:      scoped,
		raw:     dbConn,
		tailnet: tn,
		cfg:     cfg,
	}

	return &db, nil
}

// openDB opens the database cfg names; sqlitePath is database.sqlite.path
// resolved.
func openDB(cfg types.DatabaseConfig, sqlitePath string) (*gorm.DB, error) {
	// TODO(kradalby): Integrate this with zerolog
	var dbLogger logger.Interface
	if cfg.Debug {
		dbLogger = util.NewDBLogWrapper(&log.Logger, time.Duration(cfg.Gorm.SlowThreshold)*time.Millisecond, cfg.Gorm.SkipErrRecordNotFound, cfg.Gorm.ParameterizedQueries)
	} else {
		dbLogger = logger.Default.LogMode(logger.Silent)
	}

	switch {
	case cfg.IsSQLite():
		dir := filepath.Dir(sqlitePath)

		err := util.EnsureDir(dir)
		if err != nil {
			return nil, fmt.Errorf("creating directory for sqlite: %w", err)
		}

		log.Info().
			Str("database", string(types.DatabaseSqlite)).
			Str("path", sqlitePath).
			Msg("Opening database")

		// Build SQLite configuration with pragmas set at connection time
		sqliteConfig := sqliteconfig.Default(sqlitePath)
		if cfg.Sqlite.WriteAheadLog {
			sqliteConfig.WALAutocheckpoint = cfg.Sqlite.WALAutoCheckPoint
		} else {
			// The journal mode persists in the file, so leaving it unset keeps
			// a database an earlier run put in WAL mode there. FULL is SQLite's
			// default for a rollback journal; NORMAL can corrupt it on power loss.
			sqliteConfig.JournalMode = sqliteconfig.JournalModeDelete
			sqliteConfig.Synchronous = sqliteconfig.SynchronousFull
		}

		connectionURL, err := sqliteConfig.ToURL()
		if err != nil {
			return nil, fmt.Errorf("building sqlite connection URL: %w", err)
		}

		// No PrepareStmt: its cache can wait on the one connection a transaction holds.
		// An outside query can publish a pending cached statement while waiting
		// for that connection. If the transaction requests the same statement,
		// it waits for preparation, deadlocking both callers. The regression
		// test TestSQLiteSameQueryInAndOutsideTransaction covers this ordering.
		db, err := gorm.Open(
			sqlite.Open(connectionURL),
			&gorm.Config{
				Logger: dbLogger,
				// Callers match gorm.ErrDuplicatedKey, not driver text.
				TranslateError: true,
			},
		)
		if err != nil {
			return nil, err
		}

		// The pure Go SQLite library does not handle locking in
		// the same way as the C based one and we can't use the gorm
		// connection pool as of 2022/02/23.
		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}

		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetConnMaxIdleTime(time.Hour)

		return db, nil

	case cfg.Type == types.DatabasePostgres:
		// Log the DSN without the password.
		pg := cfg.Postgres
		pg.Pass = ""

		log.Info().
			Str(zf.Database, string(types.DatabasePostgres)).
			Str(zf.Path, pg.DSN()).
			Msg("Opening database")

		pass, err := types.ResolveSecret("database.postgres.pass", string(cfg.Postgres.Pass))
		if err != nil {
			return nil, err
		}

		pg.Pass = types.Secret(pass)

		db, err := gorm.Open(postgres.Open(pg.DSN()), &gorm.Config{
			Logger:         dbLogger,
			TranslateError: true,
		})
		if err != nil {
			return nil, err
		}

		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}

		sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConnections)
		sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConnections)
		sqlDB.SetConnMaxIdleTime(
			time.Duration(cfg.Postgres.ConnMaxIdleTimeSecs) * time.Second,
		)

		return db, nil
	}

	return nil, fmt.Errorf(
		"database of type %s is not supported: %w",
		cfg.Type,
		errDatabaseNotSupported,
	)
}

func runMigrations(cfg types.DatabaseConfig, dbConn *gorm.DB, migrations *gormigrate.Gormigrate) error {
	if cfg.IsSQLite() {
		if err := migrations.Migrate(); err != nil { //nolint:noinlineerr
			return err
		}

		// Check for constraint violations at the end
		type constraintViolation struct {
			Table           string
			RowID           int
			Parent          string
			ConstraintIndex int
		}

		var violatedConstraints []constraintViolation

		rows, err := dbConn.Raw("PRAGMA foreign_key_check").Rows()
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var violation constraintViolation

			err := rows.Scan(&violation.Table, &violation.RowID, &violation.Parent, &violation.ConstraintIndex)
			if err != nil {
				return err
			}

			violatedConstraints = append(violatedConstraints, violation)
		}

		if err := rows.Err(); err != nil { //nolint:noinlineerr
			return err
		}

		if len(violatedConstraints) > 0 {
			for _, violation := range violatedConstraints {
				log.Error().
					Str("table", violation.Table).
					Int("row_id", violation.RowID).
					Str("parent", violation.Parent).
					Msg("Foreign key constraint violated")
			}

			return errForeignKeyConstraintsViolated
		}
	} else {
		// PostgreSQL can run all migrations in one block - no foreign key issues
		err := migrations.Migrate()
		if err != nil {
			return err
		}
	}

	return nil
}

func (hsdb *HSDatabase) PingDB(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	sqlDB, err := hsdb.DB.DB()
	if err != nil {
		return err
	}

	return sqlDB.PingContext(ctx)
}

func (hsdb *HSDatabase) Close() error {
	db, err := hsdb.DB.DB()
	if err != nil {
		return err
	}

	if hsdb.cfg.Database.IsSQLite() && hsdb.cfg.Database.Sqlite.WriteAheadLog {
		db.Exec("VACUUM") //nolint:errcheck,noctx
	}

	return db.Close()
}

func (hsdb *HSDatabase) Read(fn func(rx *gorm.DB) error) error {
	rx := hsdb.DB.Begin()
	defer rx.Rollback()

	return fn(rx)
}

func Read[T any](db *gorm.DB, fn func(rx *gorm.DB) (T, error)) (T, error) {
	rx := db.Begin()
	defer rx.Rollback()

	ret, err := fn(rx)
	if err != nil {
		var no T
		return no, err
	}

	return ret, nil
}

func (hsdb *HSDatabase) Write(fn func(tx *gorm.DB) error) error {
	tx := hsdb.DB.Begin()
	defer tx.Rollback()

	err := fn(tx)
	if err != nil {
		return err
	}

	return tx.Commit().Error
}

func Write[T any](db *gorm.DB, fn func(tx *gorm.DB) (T, error)) (T, error) {
	tx := db.Begin()
	defer tx.Rollback()

	ret, err := fn(tx)
	if err != nil {
		var no T
		return no, err
	}

	return ret, tx.Commit().Error
}

// validateSQLiteSchema checks the database against schema.sql with squibble.
func validateSQLiteSchema(dbConn *gorm.DB) error {
	sqlConn, err := dbConn.DB()
	if err != nil {
		return fmt.Errorf("getting DB from gorm: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	// squibble nests queries. Without WAL a second connection would
	// hang: its opening pragmas write, and the write waits for the
	// first connection's read.
	conn, err := sqlConn.Conn(ctx)
	if err != nil {
		return fmt.Errorf("getting connection for schema validation: %w", err)
	}
	defer conn.Close()

	opts := squibble.DigestOptions{
		IgnoreTables: []string{
			// Litestream tables, these are inserted by
			// litestream and not part of our schema
			// https://litestream.io/how-it-works
			"_litestream_lock",
			"_litestream_seq",
		},
	}

	if err := squibble.Validate(ctx, conn, dbSchema, &opts); err != nil { //nolint:noinlineerr
		return fmt.Errorf("validating schema: %w", err)
	}

	return nil
}
