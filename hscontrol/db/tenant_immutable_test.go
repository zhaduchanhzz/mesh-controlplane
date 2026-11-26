package db

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"gorm.io/gorm"
	"tailscale.com/types/key"
)

// tenantRows creates, in tailnet A, one row of every tenant table that no
// other foreign key would stop from moving to B: a user without nodes, a
// tagged node, a tagged pre-auth key, a policy and DNS settings. It returns
// table -> "WHERE" condition selecting that row.
func tenantRows(t *testing.T, tt twoTenants) map[string]string {
	t.Helper()

	u, err := tt.a.CreateUser(types.User{Name: "mover"})
	require.NoError(t, err)

	node := types.Node{
		MachineKey: key.NewMachine().Public(), NodeKey: key.NewNode().Public(), Hostname: "tagged",
		Tags: []string{"tag:srv"}, RegisterMethod: util.RegisterMethodAuthKey,
	}
	require.NoError(t, tt.a.DB.Create(&node).Error)

	exp := time.Now().Add(time.Hour)
	k, err := tt.a.CreatePreAuthKey(nil, false, false, &exp, []string{"tag:srv"})
	require.NoError(t, err)

	pol, err := tt.a.SetPolicy(`{"grants":[]}`)
	require.NoError(t, err)

	require.NoError(t, tt.p.db.Exec(`INSERT INTO tailnet_settings (tailnet_id, dns_config) VALUES (?, ?)`,
		tt.idA.String(), `{}`).Error)

	return map[string]string{
		"users":            "id = " + strconv.FormatUint(uint64(u.ID), 10),
		"nodes":            "id = " + strconv.FormatUint(uint64(node.ID), 10),
		"credentials":      "id = " + strconv.FormatUint(k.ID, 10),
		"policies":         "id = " + strconv.FormatUint(uint64(pol.ID), 10),
		"tailnet_settings": "tailnet_id = '" + tt.idA.String() + "'",
	}
}

// P12c A8: the database itself refuses to change the tailnet_id of a tenant
// row, whatever SQL does it (the application-layer half is A3).
func TestTailnetIDImmutableInDatabase(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			tt := seedTwoTenants(t, bk.cfg(t, "", ""))
			rows := tenantRows(t, tt)

			for table, where := range rows {
				err := tt.p.db.Exec(`UPDATE `+table+` SET tailnet_id = ? WHERE `+where, tt.idB.String()).Error
				require.Error(t, err, "raw UPDATE %s SET tailnet_id = B must fail", table)

				var got string
				require.NoError(t, tt.p.db.Raw(`SELECT tailnet_id FROM `+table+` WHERE `+where).Scan(&got).Error)
				assert.Equal(t, tt.idA.String(), got, "%s row stays in A", table)

				// Rewriting the same value is not a change.
				require.NoError(t, tt.p.db.Exec(`UPDATE `+table+` SET tailnet_id = tailnet_id WHERE `+where).Error,
					"%s: setting tailnet_id to itself", table)
			}
		})
	}
}

// TestTenantIDImmutableDDLInSchemaFiles: a fresh database gets the triggers
// from the schema files (InitSchema records every migration), so they must
// carry the migration's DDL verbatim; it is the last migration of v0.1.
func TestTenantIDImmutableDDLInSchemaFiles(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), TenantIDImmutableSQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), TenantIDImmutablePostgresDDL)
	assert.Equal(t, TenantIDImmutableMigrationID, LatestMigrationID(), "appended at the end of the chain")

	for _, table := range tenantscope.TenantTables() {
		trigger := "trg_" + table + "_tailnet_id_immutable"
		assert.Contains(t, TenantIDImmutableSQLiteDDL, trigger+" BEFORE UPDATE OF tailnet_id ON "+table+"\n")
		assert.Contains(t, TenantIDImmutablePostgresDDL, trigger+" BEFORE UPDATE OF tailnet_id ON "+table+"\n")
	}
}

// TestTenantIDImmutableMigrationUpgrades: a database from before the
// migration (rewound with its own Rollback) keeps its data when the
// migration runs, ends up with the triggers active, and (SQLite) matches
// schema.sql (OpenPlatform validates it with squibble).
func TestTenantIDImmutableMigrationUpgrades(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			cfg := bk.cfg(t, "", "")
			tt := seedTwoTenants(t, cfg)
			rows := tenantRows(t, tt)

			counts := func(conn *gorm.DB) map[string]int64 {
				out := map[string]int64{}

				for table := range rows {
					var n int64
					require.NoError(t, conn.Raw(`SELECT count(*) FROM `+table).Scan(&n).Error)
					out[table] = n
				}

				return out
			}

			before := counts(tt.p.db)

			// Rewind to the previous migration: no trigger, no record.
			require.NoError(t, tenantIDImmutableMigration().Rollback(tt.p.db))
			require.NoError(t, tt.p.db.Exec(`DELETE FROM migrations WHERE id = ?`, TenantIDImmutableMigrationID).Error)
			assert.Zero(t, immutableTriggers(t, tt.p.db), "rolled back: no trigger left")
			require.NoError(t, tt.p.Close())

			p := openPlatform(t, cfg)

			var ids []string
			require.NoError(t, p.db.Table("migrations").Pluck("id", &ids).Error)
			assert.Contains(t, ids, TenantIDImmutableMigrationID)
			assert.Equal(t, len(rows), immutableTriggers(t, p.db), "one trigger per tenant table")
			assert.Equal(t, before, counts(p.db), "data kept")

			for table, where := range rows {
				err := p.db.Exec(`UPDATE `+table+` SET tailnet_id = ? WHERE `+where, tt.idB.String()).Error
				require.Error(t, err, "migrated trigger on %s is active", table)
			}
		})
	}
}

// immutableTriggers counts the A8 triggers present in the database.
func immutableTriggers(t *testing.T, conn *gorm.DB) int {
	t.Helper()

	q := `SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'trg\_%\_tailnet\_id\_immutable' ESCAPE '\'`
	if conn.Name() == dialectPostgres {
		q = `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname LIKE 'trg\_%\_tailnet\_id\_immutable' ESCAPE '\'`
	}

	var n int
	require.NoError(t, conn.Raw(q).Scan(&n).Error)

	return n
}
