package db

// SchemaCompatMigrationID adds schema_compat (P13d, ADR 0013 §3). RED stub:
// not in the chain yet.
const SchemaCompatMigrationID = "202611271000-schema-compat"

// SchemaCompatSQLiteDDL and SchemaCompatPostgresDDL: RED stub.
const (
	SchemaCompatSQLiteDDL   = "CREATE TABLE schema_compat("
	SchemaCompatPostgresDDL = "CREATE TABLE schema_compat("
)

// migrationCompat declares, for every migration from SchemaCompatMigrationID
// on, its min_reader_id. RED stub: empty.
var migrationCompat = map[string]string{}
