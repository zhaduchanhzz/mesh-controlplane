package placement

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P12, owner decision Q5: a tailnet created on a cluster the operator names
// (CLI) follows the registry rules: the cluster is registered and ACTIVE,
// a dedicated cluster takes one live tailnet, a shared one must have room.
// Owner decision 2026-11-25 (overrides the first Q5 design): the named
// cluster must also have a process holding its lease, like automatic
// placement (Q2): no tailnet is ever created where nothing serves it.
func TestCreateTailnetOnNamedCluster(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			addCluster(t, r, "off", ClassShared, ClusterDisabled, 100)
			addCluster(t, r, "drain", ClassShared, ClusterDraining, 100)
			addCluster(t, r, "full", ClassShared, ClusterActive, 2)
			addCluster(t, r, "ded", ClassDedicated, ClusterActive, 100)
			addCluster(t, r, "cold", ClassShared, ClusterActive, 100)
			require.NoError(t, gdb.Exec(`DELETE FROM cluster_leases WHERE cluster_id = 'cold'`).Error)

			addNodes(t, gdb, rawTailnet(t, gdb, "full", types.TailnetActive), 2)

			for _, tc := range []struct {
				cluster string
				want    error
			}{
				{"ghost", ErrNotFound}, // not registered: would be R1
				{"off", ErrNoCapacity},
				{"drain", ErrNoCapacity},
				{"full", ErrNoCapacity}, // 2/2 devices
			} {
				before := count(t, gdb, `SELECT count(*) FROM tailnets`)
				_, err := r.CreateTailnetOn(ctx, testOp, "t-"+tc.cluster, tc.cluster)
				require.ErrorIs(t, err, tc.want, tc.cluster)
				assert.Equal(t, before, count(t, gdb, `SELECT count(*) FROM tailnets`), "%s: nothing created", tc.cluster)
			}

			// Named cluster without a live lease: refused, nothing created,
			// both for a cluster that never ran and one whose lease expired.
			for _, live := range []bool{false, true} {
				if live {
					setLease(t, gdb, "cold", false) // ran, then stopped renewing
				}

				before := count(t, gdb, `SELECT count(*) FROM tailnets`)
				_, err := r.CreateTailnetOn(ctx, testOp, "cold-1", "cold")
				require.ErrorIs(t, err, ErrNoLiveLease)
				require.ErrorIs(t, err, ErrNoCapacity, "still a capacity refusal for API callers (503)")
				assert.Equal(t, before, count(t, gdb, `SELECT count(*) FROM tailnets`))
			}

			// Once its process holds the lease: created.
			setLease(t, gdb, "cold", true)

			tn, err := r.CreateTailnetOn(ctx, testOp, "cold-1", "cold")
			require.NoError(t, err)
			assert.Equal(t, "cold", tn.OwnerClusterID)
			assert.Equal(t, types.TailnetPending, tn.Status)
			assert.Equal(t, uint64(1), tn.OwnerEpoch)
			assert.Equal(t, int64(1), count(t, gdb, `SELECT count(*) FROM policies WHERE tailnet_id = ?`, tn.ID), "deny-all policy in the same transaction")

			// Dedicated: the first tailnet fits, a second does not (R8).
			_, err = r.CreateTailnetOn(ctx, testOp, "ded-1", "ded")
			require.NoError(t, err)
			_, err = r.CreateTailnetOn(ctx, testOp, "ded-2", "ded")
			require.ErrorIs(t, err, ErrNoCapacity)

			// Slug rules and uniqueness are the registry's.
			_, err = r.CreateTailnetOn(ctx, testOp, "Bad_Slug", "cold")
			require.ErrorIs(t, err, ErrInvalid)
			_, err = r.CreateTailnetOn(ctx, testOp, "cold-1", "cold")
			require.ErrorIs(t, err, ErrConflict)

			entries, err := r.AuditLog(ctx, 100)
			require.NoError(t, err)

			n := 0

			for _, e := range entries {
				if e.Action == "CreateTailnet" && e.Actor == testOp.Actor && e.Outcome == outcomeOK {
					n++
				}
			}

			assert.Equal(t, 2, n, "each created tailnet is audited")
		})
	}
}

// P12, Q5 (and R7): a move to another shared cluster must fit its device
// budget with the moving tailnet's devices; a move to the same cluster (a
// new epoch) is not a capacity change.
func TestMoveOwnerChecksTargetCapacity(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			addCluster(t, r, "src", ClassShared, ClusterActive, 100)

			moving := activeTailnet(t, r, "moving", ClassShared)
			require.Equal(t, "src", moving.OwnerClusterID)

			addCluster(t, r, "dst", ClassShared, ClusterActive, 3)
			addNodes(t, gdb, moving.ID, 2)
			addNodes(t, gdb, rawTailnet(t, gdb, "dst", types.TailnetActive), 2)

			_, err := r.MoveOwner(ctx, testOp, moving.ID, "src", moving.OwnerEpoch, "dst")
			require.ErrorIs(t, err, ErrNoCapacity, "2 + 2 devices > 3")

			row := count(t, gdb, `SELECT owner_epoch FROM tailnets WHERE id = ?`, moving.ID)
			assert.Equal(t, int64(moving.OwnerEpoch), row, "nothing changed")

			// Same cluster again: a new epoch, whatever the load and even
			// without a live lease (owner decision 2026-11-25).
			setLease(t, gdb, "src", false)

			tn, err := r.MoveOwner(ctx, testOp, moving.ID, "src", moving.OwnerEpoch, "src")
			require.NoError(t, err)
			assert.Equal(t, moving.OwnerEpoch+1, tn.OwnerEpoch)

			// Room made at dst: the move fits (2 + 1 <= 3), but not while no
			// process of dst holds its lease (owner decision 2026-11-25).
			require.NoError(t, gdb.Exec(`DELETE FROM nodes WHERE id IN (SELECT id FROM nodes WHERE tailnet_id <> ? LIMIT 1)`, moving.ID).Error)
			setLease(t, gdb, "dst", false)

			_, err = r.MoveOwner(ctx, testOp, moving.ID, "src", tn.OwnerEpoch, "dst")
			require.ErrorIs(t, err, ErrNoLiveLease)
			require.ErrorIs(t, err, ErrNoCapacity)

			setLease(t, gdb, "dst", true)

			tn, err = r.MoveOwner(ctx, testOp, moving.ID, "src", tn.OwnerEpoch, "dst")
			require.NoError(t, err)
			assert.Equal(t, "src", tn.OwnerClusterID, "a request (ADR 0010)")
			require.NotNil(t, tn.MovingToClusterID)
			assert.Equal(t, "dst", *tn.MovingToClusterID)
		})
	}
}
