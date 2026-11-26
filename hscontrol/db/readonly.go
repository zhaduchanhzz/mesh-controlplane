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
// missing SQLite file and a database whose migrations do not include
// [LatestMigrationID]. On SQLite every connection of the handle is in
// query_only mode (set in the DSN, P13f L3); on PostgreSQL callers read in
// READ ONLY transactions.
func OpenReadOnly(cfg *types.Config) (*gorm.DB, error) {
	if cfg.Database.IsSQLite() {
		_, err := os.Stat(cfg.SQLitePath())
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSchemaNotCurrent, err)
		}
	}

	conn, err := openDB(cfg.Database, cfg.SQLitePath(), true)
	if err != nil {
		return nil, err
	}

	closeConn := func() {
		sqlDB, err := conn.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	}

	var n int64

	err = conn.Raw(`SELECT count(*) FROM migrations WHERE id = ?`, LatestMigrationID()).Scan(&n).Error
	if err != nil || n != 1 {
		closeConn()

		return nil, fmt.Errorf("%w: want migration %s (err=%w)", ErrSchemaNotCurrent, LatestMigrationID(), err)
	}

	return conn, nil
}
