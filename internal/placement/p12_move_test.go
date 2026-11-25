package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR 0010: a move to the owner cluster itself is an epoch bump, done at
// once (one process retires and reloads); a pending move can be cancelled
// at its epoch; a cluster that is the target of a pending move is in use.
func TestMoveRequestCancelAndSameCluster(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 100)
			tn := activeTailnet(t, r, "acme", ClassShared)

			bumped, err := r.MoveOwner(ctx, testOp, tn.ID, "c1", 1, "c1")
			require.NoError(t, err)
			assert.Equal(t, "c1", bumped.OwnerClusterID)
			assert.EqualValues(t, 2, bumped.OwnerEpoch, "same cluster: epoch bump at once")
			assert.Nil(t, bumped.MovingToClusterID)

			_, err = r.CancelMove(ctx, testOp, tn.ID, 2)
			require.ErrorIs(t, err, ErrConflict, "no move pending")

			_, err = r.MoveOwner(ctx, testOp, tn.ID, "c1", 2, "c2")
			require.NoError(t, err)

			err = r.DeleteCluster(ctx, testOp, "c2")
			require.ErrorIs(t, err, ErrClusterInUse, "target of a pending move")

			_, err = r.CancelMove(ctx, testOp, tn.ID, 1)
			require.ErrorIs(t, err, ErrStaleEpoch)

			got, err := r.CancelMove(ctx, testOp, tn.ID, 2)
			require.NoError(t, err)
			assert.Nil(t, got.MovingToClusterID)
			assert.Equal(t, "c1", got.OwnerClusterID)
			assert.EqualValues(t, 2, got.OwnerEpoch, "cancel changes neither owner nor epoch")

			_, err = r.MoveOwner(ctx, testOp, tn.ID, "c1", 2, "c2")
			require.NoError(t, err, "a new request after the cancel")

			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'CancelMove' AND outcome = 'ok'`))
		})
	}
}

// ADR 0010 §7: a move pending longer than MoveMaxAge is R10 (typically the
// owner hangs while holding its lease).
func TestReconcileReportsStuckMove(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := reconcileBase(t, gdb)
			healthyCluster(t, r, "c2", ClassShared, 100)

			var id string
			require.NoError(t, gdb.Raw(`SELECT id FROM tailnets WHERE slug = 'base'`).Scan(&id).Error)

			_, err := r.MoveOwner(ctx, testOp, id, "ok", 1, "c2")
			require.NoError(t, err)
			assert.Empty(t, reconcile(t, r), "a fresh request is not stuck")

			r.now = func() time.Time { return time.Now().UTC().Add(DefaultMoveMaxAge + time.Minute) }

			got := reconcile(t, r)
			require.Len(t, got, 1, "%+v", got)
			assert.Equal(t, FindingMovePendingTooLong, got[0].Code)
			assert.Equal(t, "ok", got[0].ClusterID)
			assert.Equal(t, id, got[0].TailnetID)
			assert.EqualValues(t, DefaultMoveMaxAge/time.Second, got[0].Limit)
			assert.GreaterOrEqual(t, got[0].Count, int64((DefaultMoveMaxAge+time.Minute)/time.Second)-60)
		})
	}
}
