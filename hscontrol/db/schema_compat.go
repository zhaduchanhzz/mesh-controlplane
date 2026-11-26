package db

import (
	"fmt"
	"slices"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// SchemaCompatMigrationID adds schema_compat (P13d, ADR 0013 §3): for each
// migration from this one on, the oldest migration a binary must know to
// run on a database that has it (min_reader_id):
//
//   - expand (new table, nullable column, index, trigger an older binary
//     does not trip over): min_reader_id = the migration before it, or an
//     earlier one, so binaries of the previous release keep starting;
//   - contract (changes or drops something older binaries use):
//     min_reader_id = the migration itself.
//
// A binary accepts a database whose every migration it does not know has a
// row here naming a migration it knows. The rows are written in the migrate
// transaction (recordSchemaCompat). The nine migrations committed before
// this one have no row: binaries that predate schema_compat refuse every
// migration they do not know, so this migration is itself contract.
const SchemaCompatMigrationID = "202611271000-schema-compat"

// SchemaCompatSQLiteDDL and SchemaCompatPostgresDDL are appended verbatim to
// schema.sql / schema_postgres.sql.
const (
	SchemaCompatSQLiteDDL = `CREATE TABLE schema_compat(
  migration_id text PRIMARY KEY,
  min_reader_id text NOT NULL
);
`
	SchemaCompatPostgresDDL = `CREATE TABLE schema_compat(
  migration_id text PRIMARY KEY,
  min_reader_id text NOT NULL
);
`
)

// migrationCompat declares the min_reader_id of every migration from
// SchemaCompatMigrationID on (TestSchemaCompatDeclarations makes it
// mandatory). Review question for each new entry: does a binary of the
// previous release still run correctly on the migrated database?
var migrationCompat = map[string]string{
	// contract: binaries before P13d refuse every unknown migration.
	SchemaCompatMigrationID: SchemaCompatMigrationID,
}

func schemaCompatMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: SchemaCompatMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := SchemaCompatSQLiteDDL
				if tx.Name() == dialectPostgres {
					ddl = SchemaCompatPostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`DROP TABLE IF EXISTS schema_compat`).Error
			})
		},
	}
}

// recordSchemaCompat writes the schema_compat row of every migration of ch
// that declares one and is recorded, unless a row exists (rows written by an
// earlier binary are kept). It runs in the migrate transaction.
func recordSchemaCompat(tx *gorm.DB, ch chain, recorded map[string]bool) error {
	for _, m := range ch.migrations {
		reader, ok := ch.compat[m.ID]
		if !ok || !recorded[m.ID] {
			continue
		}

		err := tx.Exec(`INSERT INTO schema_compat (migration_id, min_reader_id) VALUES (?, ?)
			ON CONFLICT (migration_id) DO NOTHING`, m.ID, reader).Error
		if err != nil {
			return fmt.Errorf("recording schema_compat of %s: %w", m.ID, err)
		}
	}

	return nil
}

// schemaCompatRows reads schema_compat (none when the table does not exist).
func schemaCompatRows(db *gorm.DB) (map[string]string, error) {
	present, err := tableExists(db, "schema_compat")
	if err != nil || !present {
		return map[string]string{}, err
	}

	var rows []struct{ MigrationID, MinReaderID string }

	err = db.Raw(`SELECT migration_id, min_reader_id FROM schema_compat`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("%w: reading schema_compat: %w", ErrDatabaseUnavailable, err)
	}

	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.MigrationID] = r.MinReaderID
	}

	return out, nil
}

// unknownMigrations lists, sorted, the recorded migrations ch does not know.
func unknownMigrations(recorded map[string]bool, ch chain) []string {
	known := ch.known()

	var out []string

	for id := range recorded {
		if !known[id] {
			out = append(out, id)
		}
	}

	slices.Sort(out)

	return out
}

// missingMigrations lists, in chain order, the migrations of ch not recorded.
func missingMigrations(recorded map[string]bool, ch chain) []string {
	var out []string

	for _, m := range ch.migrations {
		if !recorded[m.ID] {
			out = append(out, m.ID)
		}
	}

	return out
}

// checkSchemaCompat is the rule of ADR 0013 §4 for a binary of chain ch on a
// database with the recorded migrations: every migration the binary does
// not know must have a schema_compat row naming one it knows (else
// ErrSchemaTooNew), and every migration it knows must be there (else
// ErrSchemaNeedsMigration). It only reads.
func checkSchemaCompat(db *gorm.DB, ch chain, recorded map[string]bool) error {
	unknown := unknownMigrations(recorded, ch)
	if len(unknown) > 0 {
		readers, err := schemaCompatRows(db)
		if err != nil {
			return err
		}

		known := ch.known()

		for _, id := range unknown {
			reader, ok := readers[id]

			switch {
			case !ok:
				return fmt.Errorf("%w: migration %s is unknown to this binary (latest %s) and has no schema_compat row "+
					"(treated as contract)", ErrSchemaTooNew, id, ch.latest())
			case !known[reader]:
				return fmt.Errorf("%w: the database has contract migration %s, which needs a binary that knows %s "+
					"(this binary's latest: %s)", ErrSchemaTooNew, id, reader, ch.latest())
			}
		}
	}

	if missing := missingMigrations(recorded, ch); len(missing) > 0 {
		return fmt.Errorf("%w: the database lacks %s (this binary's latest: %s)",
			ErrSchemaNeedsMigration, strings.Join(missing, ", "), ch.latest())
	}

	return nil
}
