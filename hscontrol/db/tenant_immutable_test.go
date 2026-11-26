package db

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
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
				assert.Error(t, err, "raw UPDATE %s SET tailnet_id = B must fail", table)

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
