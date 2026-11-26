package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
)

// P12c A3: an UPDATE, Updates or Save under tailnet A that assigns another
// tailnet_id is refused and the row stays in A (SQLite and PostgreSQL).
func TestUpdateCannotMoveRowToAnotherTailnet(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			tt := seedTwoTenants(t, bk.cfg(t, "", ""))
			a := tt.a.DB
			idA, idB := tt.idA.String(), tt.idB.String()

			// Fresh rows of A for each case, without references a foreign
			// key would already stop from moving: a tagged key (no user), a
			// policy, a user without nodes.
			taggedKey := func() uint64 {
				exp := time.Now().Add(time.Hour)
				k, err := tt.a.CreatePreAuthKey(nil, false, false, &exp, []string{"tag:srv"})
				require.NoError(t, err)

				return k.ID
			}
			policy := func() uint64 {
				pol, err := tt.a.SetPolicy(`{"grants":[]}`)
				require.NoError(t, err)

				return uint64(pol.ID)
			}
			user := func() uint64 {
				u, err := tt.a.CreateUser(types.User{Name: "loner-" + time.Now().Format("150405.000000000")})
				require.NoError(t, err)

				return uint64(u.ID)
			}

			moves := []struct {
				name  string
				table string
				row   func() uint64
				run   func(id uint64) error
			}{
				{"Update(tailnet_id, B)", "credentials", taggedKey, func(id uint64) error {
					return a.Model(&types.Credential{ID: id}).Update("tailnet_id", idB).Error
				}},
				{"Updates(map tailnet_id: B)", "policies", policy, func(id uint64) error {
					return a.Model(&types.Policy{}).Where("id = ?", id).Updates(map[string]any{"tailnet_id": idB}).Error
				}},
				{"Updates(map TailnetID: B)", "users", user, func(id uint64) error {
					return a.Model(&types.User{}).Where("id = ?", id).Updates(map[string]any{"TailnetID": idB}).Error
				}},
				{"Updates(struct TailnetID: B)", "policies", policy, func(id uint64) error {
					return a.Model(&types.Policy{}).Where("id = ?", id).
						Updates(types.Policy{TailnetID: idB, Data: `{"grants":[]}`}).Error
				}},
				{"Update(tailnet_id, Expr)", "users", user, func(id uint64) error {
					return a.Model(&types.User{}).Where("id = ?", id).Update("tailnet_id", gorm.Expr("?", idB)).Error
				}},
				{"UpdateColumn(tailnet_id, B)", "users", user, func(id uint64) error {
					return a.Model(&types.User{}).Where("id = ?", id).UpdateColumn("tailnet_id", idB).Error
				}},
				{"Save(row with TailnetID B)", "credentials", taggedKey, func(id uint64) error {
					var c types.Credential
					if err := a.First(&c, id).Error; err != nil { //nolint:noinlineerr
						return err
					}

					c.TailnetID = idB

					return a.Save(&c).Error
				}},
			}

			for _, m := range moves {
				id := m.row()
				assert.Error(t, m.run(id), "%s on %s under A must be refused", m.name, m.table)
				assert.Equal(t, idA, tt.rawTailnetOf(t, m.table, id), "%s on %s: the row stays in A", m.name, m.table)
			}

			// Assigning the scope's own tailnet is a no-op and allowed.
			id := user()
			require.NoError(t, a.Model(&types.User{}).Where("id = ?", id).Update("tailnet_id", idA).Error)
			assert.Equal(t, idA, tt.rawTailnetOf(t, "users", id))
		})
	}
}
