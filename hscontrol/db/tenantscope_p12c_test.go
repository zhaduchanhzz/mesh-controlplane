package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"tailscale.com/types/key"
)

// P12c A2: statements that name a tenant table without the tenant schema
// (Table, TableExpr, string Joins, a subquery in a condition, a destination
// without TailnetID) are refused, never run unscoped, on the real schema,
// SQLite and PostgreSQL.

// nodeDTO has no TailnetID: the plugin cannot scope it.
type nodeDTO struct {
	ID       uint64
	Hostname string
}

// nodeRow maps onto nodes without a TailnetID field.
type nodeRow struct {
	ID       uint64
	Hostname string
}

func (nodeRow) TableName() string { return "nodes" }

type twoTenants struct {
	p      *Platform
	idA    tailnet.ID
	idB    tailnet.ID
	a, b   *HSDatabase
	nodesB int64
}

func seedTwoTenants(t *testing.T, cfg *types.Config) twoTenants {
	t.Helper()

	p := openPlatform(t, cfg)
	tt := twoTenants{
		p:   p,
		idA: platformTailnet(t, p, "local", types.TailnetActive, false),
		idB: platformTailnet(t, p, "local", types.TailnetActive, false),
	}
	tt.a, tt.b = tenantDB(t, p, tt.idA), tenantDB(t, p, tt.idB)

	for _, h := range []*HSDatabase{tt.a, tt.b} {
		require.NoError(t, addNode(t, h, key.NewMachine().Public(), key.NewNode().Public()))
	}

	tt.nodesB = tt.count(t, tt.b, &types.Node{}, "hostname = ?", "laptop")
	require.EqualValues(t, 1, tt.nodesB)

	return tt
}

// count reads under h's own scope.
func (tt twoTenants) count(t *testing.T, h *HSDatabase, model any, where string, args ...any) int64 {
	t.Helper()

	var n int64
	require.NoError(t, h.DB.Model(model).Where(where, args...).Count(&n).Error)

	return n
}

// rawTailnetOf reads a row's tailnet_id with reviewed test SQL, unscoped.
func (tt twoTenants) rawTailnetOf(t *testing.T, table string, id uint64) string {
	t.Helper()

	var got string
	require.NoError(t, tt.p.db.Raw(`SELECT tailnet_id FROM `+table+` WHERE id = ?`, id).Scan(&got).Error)

	return got
}

func TestTenantTableWithoutSchemaIsRefused(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			tt := seedTwoTenants(t, bk.cfg(t, "", ""))
			a := tt.a.DB

			qualified := "main.nodes"
			if tt.p.db.Name() == dialectPostgres {
				qualified = "public.nodes"
			}

			reads := []struct {
				name string
				run  func() (int, error)
			}{
				{"Table(nodes).Find(dto)", func() (int, error) {
					var out []nodeDTO

					err := a.Table("nodes").Find(&out).Error

					return len(out), err
				}},
				{"Table(nodes AS n).Find(dto)", func() (int, error) {
					var out []nodeDTO

					err := a.Table("nodes AS n").Find(&out).Error

					return len(out), err
				}},
				{"Table(schema.nodes).Find(dto)", func() (int, error) {
					var out []nodeDTO

					err := a.Table(qualified).Find(&out).Error

					return len(out), err
				}},
				{"Find(model without TailnetID, TableName nodes)", func() (int, error) {
					var out []nodeRow

					err := a.Find(&out).Error

					return len(out), err
				}},
				{"Table(users).Pluck", func() (int, error) {
					var out []string

					err := a.Table("users").Pluck("name", &out).Error

					return len(out), err
				}},
				{"Model(Tailnet).Joins(JOIN nodes).Scan", func() (int, error) {
					var out []string

					err := a.Model(&types.Tailnet{}).Joins("JOIN nodes ON nodes.tailnet_id = tailnets.id").
						Select("nodes.hostname").Scan(&out).Error

					return len(out), err
				}},
				{"Model(Tailnet).Where(subquery on nodes)", func() (int, error) {
					var out []types.Tailnet

					err := a.Model(&types.Tailnet{}).
						Where("id IN (SELECT tailnet_id FROM nodes WHERE hostname = ?)", "laptop").Find(&out).Error

					return len(out), err
				}},
				{"Model(Node).Select(subquery on credentials)", func() (int, error) {
					var out []int64

					err := a.Model(&types.Node{}).Select("(SELECT count(*) FROM credentials)").Scan(&out).Error

					return len(out), err
				}},
			}

			for _, r := range reads {
				n, err := r.run()
				require.Error(t, err, "%s under A must be refused, read %d rows", r.name, n)
				assert.Zero(t, n, "%s returned rows", r.name)
			}

			err := a.Table("credentials").Create(map[string]any{
				"tailnet_id": tt.idB.String(), "kind": "api", "identifier": "smuggled", "hash": []byte("$x"),
			}).Error
			require.Error(t, err, "Table(credentials).Create(map) under A must be refused")
			assert.Zero(t, tt.count(t, tt.b, &types.Credential{}, "identifier = ?", "smuggled"), "nothing inserted into B")

			err = a.Table("nodes").Where("hostname = ?", "laptop").Updates(map[string]any{"hostname": "pwned"}).Error
			require.Error(t, err, "Table(nodes).Updates(map) under A must be refused")

			err = a.Table("nodes").Where("hostname = ?", "laptop").Delete(&nodeDTO{}).Error
			require.Error(t, err, "Table(nodes).Delete(dto) under A must be refused")

			assert.Equal(t, tt.nodesB, tt.count(t, tt.b, &types.Node{}, "hostname = ?", "laptop"), "B's nodes unchanged")
			assert.EqualValues(t, 1, tt.count(t, tt.a, &types.Node{}, "hostname = ?", "laptop"), "A's nodes unchanged")

			// Legitimate uses keep working.
			var ids []string
			require.NoError(t, a.Table("migrations").Pluck("id", &ids).Error)
			assert.Contains(t, ids, v01InitMigration)

			_, err = tt.a.ListEphemeralNodes() // Joins("AuthKey"): an association join
			require.NoError(t, err)

			var nodes []types.Node
			require.NoError(t, a.Where("nodes.hostname = ?", "laptop").Find(&nodes).Error, "a column qualified with the table")
			assert.Len(t, nodes, 1)
		})
	}
}
