package db

import (
	"errors"
	"fmt"
	"os"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
)

// ErrSchemaNotCurrent reports a database that a read-only consumer cannot
// use: missing, not a v0.1 database, or not migrated to this binary's last
// migration. A read-only consumer never creates or migrates the schema.
var ErrSchemaNotCurrent = errors.New("database schema is not current")

// LatestMigrationID is the ID of the last v0.1 migration this binary knows.
func LatestMigrationID() string {
	m := v01Migrations()

	return m[len(m)-1].ID
}

// OpenReadOnly opens the database of cfg for a consumer that only reads the
// platform tables (the bootstrap service, P11). Unlike [OpenPlatform] it
// never creates the schema, never runs migrations and does not register
// tenantscope: the caller reads with reviewed raw SQL only. It refuses a
// missing SQLite file and a database this binary cannot run with (the rule
// of [OpenPlatform], ADR 0013 §4: every migration it knows present, every
// newer one marked compatible); the version check is not applied, as
// before P13d. On SQLite the connection is put in query_only mode;
// on PostgreSQL callers read in READ ONLY transactions.
func OpenReadOnly(cfg *types.Config) (*gorm.DB, error) {
	return openReadOnly(cfg, currentChain())
}

func openReadOnly(cfg *types.Config, ch chain) (*gorm.DB, error) {
	if cfg.Database.IsSQLite() {
		_, err := os.Stat(cfg.SQLitePath())
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSchemaNotCurrent, err)
		}
	}

	// P13b: the pooler must keep prepared statements (ADR 0006 §3.5); a
	// read-only server is fine for a reader.
	err := checkPooler(cfg.Database, false)
	if err != nil {
		return nil, err
	}

	conn, err := openDB(cfg.Database, cfg.SQLitePath())
	if err != nil {
		return nil, err
	}

	closeConn := func() {
		sqlDB, err := conn.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	}

	// The compatibility rule of the cluster processes (ADR 0013 §4), read
	// only: a newer expand migration is accepted, a newer contract
	// migration or a missing one is not.
	_, err = verifySchema(conn, ch)
	if err != nil {
		closeConn()

		return nil, fmt.Errorf("%w: %w", ErrSchemaNotCurrent, err)
	}

	if cfg.Database.IsSQLite() {
		err = conn.Exec(`PRAGMA query_only = ON`).Error
		if err != nil {
			closeConn()

			return nil, fmt.Errorf("setting query_only: %w", err)
		}
	}

	return conn, nil
}
