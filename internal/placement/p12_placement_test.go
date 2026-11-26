package placement

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setLease writes cluster id's cluster_leases row: live (expires an hour
// from now by the database clock) or expired (an hour ago). Rows are never
// deleted (P13c trigger): an existing row keeps its holder and epoch.
func setLease(t *testing.T, gdb *gorm.DB, id string, live bool) {
	t.Helper()

	sign := "+"
	if !live {
		sign = "-"
	}

	exp := `strftime('%Y-%m-%d %H:%M:%f','now','` + sign + `1 hour')`
	now := `strftime('%Y-%m-%d %H:%M:%f','now')`

	if gdb.Name() == "postgres" {
		exp = `clock_timestamp() ` + sign + ` interval '1 hour'`
		now = `clock_timestamp()`
	}

	require.NoError(t, gdb.Exec(`INSERT INTO cluster_leases (cluster_id, holder, epoch, expires_at, updated_at)
		VALUES (?, 'test-holder', 1, `+exp+`, `+now+`)
		ON CONFLICT (cluster_id) DO UPDATE SET expires_at = excluded.expires_at, updated_at = excluded.updated_at`, id).Error)
}

// P12, owner decision Q1: equal device load is broken by the number of live
// tailnets of the cluster, then by id, so new (empty) tailnets spread.
func TestPlacementSpreadsNewTailnets(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			for _, id := range []string{"s1", "s2"} {
				addCluster(t, r, id, ClassShared, ClusterActive, 100)
			}

			owners := map[string]int{}

			for _, slug := range []string{"t1", "t2", "t3"} {
				tn, err := r.CreateTailnet(ctx, testOp, slug, ClassShared)
				require.NoError(t, err)

				owners[tn.OwnerClusterID]++
			}

			assert.Equal(t, map[string]int{"s1": 2, "s2": 1}, owners, "3 empty tailnets on 2 empty clusters spread 2/1")

			// Device load still comes first: devices on s2 send the next
			// tailnet to s1 although s1 owns more tailnets.
			var t2 string
			require.NoError(t, gdb.Raw(`SELECT id FROM tailnets WHERE owner_cluster_id = 's2'`).Scan(&t2).Error)
			addNodes(t, gdb, t2, 5)

			c, err := r.PlaceTailnet(ctx, ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s1", c.ID, "least loaded by devices first")
		})
	}
}

// P12, owner decision Q2: a cluster with no process holding a live lease
// is never chosen; with none left, placement has no capacity.
func TestPlacementSkipsClusterWithoutLiveLease(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			addClusterNoLease(t, r, "s1", ClassShared, ClusterActive, 100) // never ran: no lease row
			addCluster(t, r, "s2", ClassShared, ClusterActive, 100)
			addCluster(t, r, "s3", ClassShared, ClusterActive, 100)
			setLease(t, gdb, "s2", false) // its process stopped renewing
			setLease(t, gdb, "s3", true)

			for _, slug := range []string{"t1", "t2"} {
				tn, err := r.CreateTailnet(ctx, testOp, slug, ClassShared)
				require.NoError(t, err)
				assert.Equal(t, "s3", tn.OwnerClusterID, "only s3 has a live lease")
			}

			c, err := r.PlaceTailnet(ctx, ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s3", c.ID)

			setLease(t, gdb, "s3", false)

			_, err = r.PlaceTailnet(ctx, ClassShared)
			require.ErrorIs(t, err, ErrNoCapacity, "no cluster with a live lease")

			_, err = r.CreateTailnet(ctx, testOp, "t3", ClassShared)
			require.ErrorIs(t, err, ErrNoCapacity)

			setLease(t, gdb, "s1", true)

			c, err = r.PlaceTailnet(ctx, ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s1", c.ID, "a cluster is placeable again once a process holds its lease")
		})
	}
}
