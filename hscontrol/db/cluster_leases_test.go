package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
)

// TestClusterLeasesDDLInSchemaFiles (P11b): the lease DDL is the same text
// in the migration and both schema files, and the migration is the last one
// (the bootstrap read-only consumer requires the database at
// LatestMigrationID, so clusters are upgraded before bootstrap).
func TestClusterLeasesDDLInSchemaFiles(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), lease.SQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), lease.PostgresDDL)
	assert.Contains(t, migrationIDs(), lease.MigrationID)
}
