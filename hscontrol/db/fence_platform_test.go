package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/key"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P10 on the real v0.1 schema (SQLite and PostgreSQL): a runtime's handle is
// fenced on the owner and epoch it was loaded with (I12, S17).
func TestTenantDBWritesFencedAfterOwnershipChange(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)

			a := tenantDB(t, p, idA)
			bdb := tenantDB(t, p, idB)

			u, err := a.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err, "write with the current fence")
			require.NoError(t, addNode(t, a, key.NewMachine().Public(), key.NewNode().Public()))

			move := func(owner string, epoch uint64) {
				t.Helper()
				require.NoError(t, p.db.Model(&types.Tailnet{}).Where("id = ?", idA.String()).
					Updates(map[string]any{"owner_cluster_id": owner, "owner_epoch": epoch, "updated_at": time.Now().UTC()}).Error)
			}

			usersBefore, err := a.ListUsers(nil)
			require.NoError(t, err)

			for _, c := range []struct {
				name  string
				owner string
				epoch uint64
			}{
				{"newer_epoch_same_cluster", "local", 2},
				{"other_cluster", "cluster-2", 3},
			} {
				t.Run(c.name, func(t *testing.T) {
					move(c.owner, c.epoch)

					_, err := a.CreateUser(types.User{Name: "late-" + c.name})
					require.ErrorIs(t, err, tenantscope.ErrFenced, "create after the move")

					err = a.DB.Model(&types.User{}).Where("id = ?", u.ID).Update("display_name", "x").Error
					require.ErrorIs(t, err, tenantscope.ErrFenced, "update after the move")

					err = addNode(t, bdb, key.NewMachine().Public(), key.NewNode().Public())
					require.NoError(t, err, "B is not affected by A's move")
				})
			}

			usersAfter, err := a.ListUsers(nil)
			require.NoError(t, err)
			assert.Len(t, usersAfter, len(usersBefore), "no write of A committed after the move")

			// A fresh handle at the new owner/epoch is refused here (other cluster).
			_, err = p.TenantDB(idA)
			require.ErrorIs(t, err, ErrTailnetNotOwned)

			// Back to this cluster at a newer epoch: a new handle writes again,
			// the old one stays fenced.
			move("local", 4)
			a2 := tenantDB(t, p, idA)
			_, err = a2.CreateUser(types.User{Name: "after-reload"})
			require.NoError(t, err)
			_, err = a.CreateUser(types.User{Name: "stale-handle"})
			require.ErrorIs(t, err, tenantscope.ErrFenced)
		})
	}
}
