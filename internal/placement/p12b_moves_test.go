package placement

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var schedOp = Op{Actor: "spiffe://mesh/placement/scheduler"}

// moveEnv: clusters c1 (owner) and c2 (target), both ACTIVE with a live
// lease; tailnet acme ACTIVE on c1 at epoch 1.
func moveEnv(t *testing.T, gdb *gorm.DB) (*Registry, string) {
	t.Helper()

	r := newRegistry(t, gdb, Config{})
	addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
	addCluster(t, r, "c2", ClassShared, ClusterActive, 100)

	tn, err := r.CreateTailnetOn(context.Background(), testOp, "acme", "c1")
	require.NoError(t, err)
	tn, err = r.ActivateTailnet(context.Background(), testOp, tn.ID, tn.OwnerEpoch)
	require.NoError(t, err)

	return r, tn.ID
}

func at(r *Registry, d time.Duration) time.Time { return r.now().Add(d) }

// handOver plays the owner cluster's hand-over (ADR 0010) on the row.
func handOver(t *testing.T, gdb *gorm.DB, id string) {
	t.Helper()
	require.NoError(t, gdb.Exec(`UPDATE tailnets SET owner_cluster_id = moving_to_cluster_id, owner_epoch = owner_epoch + 1,
		moving_to_cluster_id = NULL, updated_at = ? WHERE id = ? AND moving_to_cluster_id IS NOT NULL`, time.Now().UTC(), id).Error)
}

func movingTo(t *testing.T, gdb *gorm.DB, id string) string {
	t.Helper()

	var v []string
	require.NoError(t, gdb.Raw(`SELECT COALESCE(moving_to_cluster_id, '') FROM tailnets WHERE id = ?`, id).Scan(&v).Error)
	require.Len(t, v, 1)

	return v[0]
}

// Q2: scheduling validates the tailnet, the target and the time, keeps one
// open move per tailnet, is idempotent and audited.
func TestScheduleMoveValidates(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)
			addCluster(t, r, "off", ClassShared, ClusterDisabled, 100)

			when := at(r, time.Hour)

			_, err := r.ScheduleMove(ctx, testOp, "00000000-0000-4000-8000-000000000000", "c2", when, "")
			require.ErrorIs(t, err, ErrNotFound, "unknown tailnet")
			_, err = r.ScheduleMove(ctx, testOp, id, "c1", when, "")
			require.ErrorIs(t, err, ErrInvalid, "to its own owner")
			_, err = r.ScheduleMove(ctx, testOp, id, "ghost", when, "")
			require.ErrorIs(t, err, ErrNotFound, "unregistered target")
			_, err = r.ScheduleMove(ctx, testOp, id, "off", when, "")
			require.ErrorIs(t, err, ErrNoCapacity, "DISABLED target")
			_, err = r.ScheduleMove(ctx, testOp, id, "c2", at(r, -time.Minute), "")
			require.ErrorIs(t, err, ErrInvalid, "in the past")
			_, err = r.ScheduleMove(ctx, testOp, id, "c2", when, strings.Repeat("x", MaxMoveReasonLen+1))
			require.ErrorIs(t, err, ErrInvalid, "reason too long")

			op := keyOp("m1", "h1")
			m, err := r.ScheduleMove(ctx, op, id, "c2", when, "rebalance")
			require.NoError(t, err)
			assert.Equal(t, MoveScheduled, m.State)
			assert.Equal(t, "c1", m.FromClusterID)
			assert.Equal(t, "c2", m.ToClusterID)
			assert.EqualValues(t, 1, m.FromEpoch)
			assert.Equal(t, "rebalance", m.Reason)
			assert.WithinDuration(t, when, m.ScheduledAt, time.Second)
			assert.Empty(t, movingTo(t, gdb, id), "scheduling changes nothing on the tailnet")

			again, err := r.ScheduleMove(ctx, op, id, "c2", when, "rebalance")
			require.NoError(t, err)
			assert.Equal(t, m.ID, again.ID, "idempotent replay")

			_, err = r.ScheduleMove(ctx, testOp, id, "c2", when.Add(time.Hour), "")
			require.ErrorIs(t, err, ErrConflict, "one open move per tailnet")

			got, err := r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, m.ID, got.ID)

			_, err = r.GetMove(ctx, "no-such-move")
			require.ErrorIs(t, err, ErrNotFound)

			list, err := r.ListMoves(ctx, id)
			require.NoError(t, err)
			require.Len(t, list, 1)

			pm, ok, err := r.PendingMove(ctx, id)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, m.ID, pm.ID)

			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'ScheduleMove' AND outcome = 'ok'`))
		})
	}
}

// The scheduler requests a due move (ADR 0010: moving_to set, epoch kept),
// then records the hand-over the owner cluster makes.
func TestRunDueMovesRequestsThenRecordsHandOver(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)

			m, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Hour), "")
			require.NoError(t, err)

			rep, err := r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Empty(t, rep.Requested, "not due yet")
			assert.Empty(t, movingTo(t, gdb, id))

			base := r.now
			r.now = func() time.Time { return base().Add(2 * time.Hour) }

			rep, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Equal(t, []string{m.ID}, rep.Requested)
			assert.Equal(t, "c2", movingTo(t, gdb, id), "the move is requested")

			got, err := r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveRequested, got.State)
			require.NotNil(t, got.RequestedAt)

			rep, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Empty(t, rep.Requested, "requested once")
			assert.Empty(t, rep.HandedOver, "not handed over yet")

			handOver(t, gdb, id)

			rep, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Equal(t, []string{m.ID}, rep.HandedOver)

			got, err = r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveHandedOver, got.State)
			require.NotNil(t, got.FinishedAt)

			_, ok, err := r.PendingMove(ctx, id)
			require.NoError(t, err)
			assert.False(t, ok, "no open move after the hand-over")

			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'MoveOwner' AND outcome = 'ok'`))
		})
	}
}

// A due move whose target has no live lease, or whose tailnet changed owner
// or epoch since it was scheduled, fails loudly; the tailnet is untouched.
func TestRunDueMovesFailsLoudly(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)

			m, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)

			setLease(t, gdb, "c2", false)

			base := r.now
			r.now = func() time.Time { return base().Add(time.Hour) }

			rep, err := r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Equal(t, []string{m.ID}, rep.Failed)
			assert.Empty(t, movingTo(t, gdb, id))

			got, err := r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveFailed, got.State)
			assert.Contains(t, got.Error, "lease")

			// A new schedule after the failure; then the epoch moves on.
			setLease(t, gdb, "c2", true)

			r.now = base
			m2, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)
			require.NoError(t, gdb.Exec(`UPDATE tailnets SET owner_epoch = owner_epoch + 1 WHERE id = ?`, id).Error)

			r.now = func() time.Time { return base().Add(time.Hour) }
			rep, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Equal(t, []string{m2.ID}, rep.Failed)

			got, err = r.GetMove(ctx, m2.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveFailed, got.State)
			assert.Contains(t, got.Error, "epoch")
			assert.Empty(t, movingTo(t, gdb, id))
		})
	}
}

// Cancel: a SCHEDULED move is just closed; a REQUESTED one also withdraws
// the request on the tailnet (CancelMove); after the hand-over: conflict.
// A request already made by hand before the scheduler ran is adopted, not
// failed (crash between the two steps).
func TestCancelScheduledMoveAndAdoption(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)

			m, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)
			c, err := r.CancelScheduledMove(ctx, testOp, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveCancelled, c.State)

			_, err = r.CancelScheduledMove(ctx, testOp, m.ID)
			require.ErrorIs(t, err, ErrConflict, "already closed")

			m, err = r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)

			base := r.now
			r.now = func() time.Time { return base().Add(time.Hour) }
			_, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			require.Equal(t, "c2", movingTo(t, gdb, id))

			c, err = r.CancelScheduledMove(ctx, testOp, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveCancelled, c.State)
			assert.Empty(t, movingTo(t, gdb, id), "the request is withdrawn")

			// Adoption: the request exists before the scheduler records it.
			r.now = base
			m3, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)
			_, err = r.MoveOwner(ctx, testOp, id, "c1", 1, "c2")
			require.NoError(t, err)

			r.now = func() time.Time { return base().Add(time.Hour) }
			rep, err := r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Equal(t, []string{m3.ID}, rep.Requested)
			assert.Empty(t, rep.Failed)

			handOver(t, gdb, id)

			_, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)

			_, err = r.CancelScheduledMove(ctx, testOp, m3.ID)
			require.ErrorIs(t, err, ErrConflict, "after the hand-over only forward")
		})
	}
}
