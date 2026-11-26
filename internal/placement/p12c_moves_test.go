package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P12c C2 (review 2026-11-26): the operator cancels a SCHEDULED move after
// the scheduler took it as due and before the scheduler recorded the
// outcome. The cancel is acknowledged, so the move must end CANCELLED with
// no request left on the tailnet: nothing would tell the owner cluster not
// to hand the tailnet over, and the schedule would say CANCELLED for a move
// that happened.
func TestRunDueMovesCancelBetweenRequestAndRecord(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)

			m, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)

			base := r.now
			r.now = func() time.Time { return base().Add(time.Hour) }

			var (
				fired     bool
				cancelErr error
			)

			r.failAt = func(step string) error {
				if step != "move-due" || fired {
					return nil
				}

				fired = true
				_, cancelErr = r.CancelScheduledMove(ctx, testOp, m.ID)

				return nil
			}

			rep, err := r.RunDueMoves(ctx, schedOp)
			r.failAt = nil

			require.True(t, fired, "the cancel ran between the two steps")
			require.NoError(t, cancelErr, "the cancel is acknowledged to the operator")
			assert.NoError(t, err, "a move cancelled under the scheduler is not a scheduler error")
			assert.Empty(t, rep.Requested, "a cancelled move is not reported as requested")

			got, err := r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveCancelled, got.State)
			assert.Empty(t, movingTo(t, gdb, id), "no request is left for the owner cluster to hand over")

			// Later passes change nothing.
			rep, err = r.RunDueMoves(ctx, schedOp)
			require.NoError(t, err)
			assert.Empty(t, rep.Requested)
			assert.Empty(t, rep.HandedOver)
			assert.Empty(t, movingTo(t, gdb, id))

			got, err = r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveCancelled, got.State)
		})
	}
}

// due makes every scheduled move due (the registry's clock one hour ahead)
// and runs the scheduler once.
func due(t *testing.T, r *Registry, base func() time.Time) RunReport {
	t.Helper()

	r.now = func() time.Time { return base().Add(time.Hour) }
	defer func() { r.now = base }()

	rep, err := r.RunDueMoves(context.Background(), schedOp)
	require.NoError(t, err)

	return rep
}

// P12c L7 (review 2026-11-26): when the scheduler, not the flip, records a
// requested move's outcome (a flip that did not record it), it decides by
// the epoch, not by the owner alone, and never takes the time from
// tailnets.updated_at.
func TestRunDueMovesOutcomeByEpoch(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)
			base := r.now

			// Handed over (epoch 2 at c2), then a new move back requested
			// before the scheduler looked: this move was HANDED_OVER.
			m1, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)
			require.Equal(t, []string{m1.ID}, due(t, r, base).Requested)
			handOver(t, gdb, id)
			_, err = r.MoveOwner(ctx, testOp, id, "c2", 2, "c1")
			require.NoError(t, err)
			require.NoError(t, gdb.Exec(`UPDATE tailnets SET updated_at = ? WHERE id = ?`,
				time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), id).Error)

			rep := due(t, r, base)
			assert.Equal(t, []string{m1.ID}, rep.HandedOver, "handed over, then re-requested: %+v", rep)

			got, err := r.GetMove(ctx, m1.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveHandedOver, got.State)
			require.NotNil(t, got.FinishedAt)
			require.NotNil(t, got.RequestedAt)
			assert.False(t, got.FinishedAt.Before(*got.RequestedAt), "finished %s before requested %s (tailnets.updated_at)",
				got.FinishedAt, got.RequestedAt)

			_, err = r.CancelMove(ctx, testOp, id, 2)
			require.NoError(t, err)

			// c2 -> c1 -> c2 between two passes: owner is the move's source
			// again, at epoch 4; the move was handed over, not cancelled.
			m2, err := r.ScheduleMove(ctx, testOp, id, "c1", at(r, time.Minute), "")
			require.NoError(t, err)
			require.Equal(t, []string{m2.ID}, due(t, r, base).Requested)
			handOver(t, gdb, id)
			_, err = r.MoveOwner(ctx, testOp, id, "c1", 3, "c2")
			require.NoError(t, err)
			handOver(t, gdb, id)

			rep = due(t, r, base)
			assert.Empty(t, rep.Cancelled, "owner back at the source at another epoch is not a cancel")

			got, err = r.GetMove(ctx, m2.ID)
			require.NoError(t, err)
			assert.NotEqual(t, MoveCancelled, got.State)
			assert.Equal(t, MoveFailed, got.State, "outcome not recorded by the flip: reported, not guessed")
			assert.Contains(t, got.Error, "epoch")

			// Withdrawn at its own epoch: CANCELLED.
			m3, err := r.ScheduleMove(ctx, testOp, id, "c1", at(r, time.Minute), "")
			require.NoError(t, err)
			require.Equal(t, []string{m3.ID}, due(t, r, base).Requested)
			require.NoError(t, gdb.Exec(`UPDATE tailnets SET moving_to_cluster_id = NULL WHERE id = ?`, id).Error)

			rep = due(t, r, base)
			assert.Equal(t, []string{m3.ID}, rep.Cancelled)
		})
	}
}

// P12c L7: CancelScheduledMove of a REQUESTED move checks the epoch, not
// only the owner: a tailnet handed over and moved back is at the source
// again, at a later epoch, and its move cannot be cancelled any more.
func TestCancelScheduledMoveRequestedChecksEpoch(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)
			base := r.now

			m, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)
			require.Equal(t, []string{m.ID}, due(t, r, base).Requested)

			handOver(t, gdb, id) // c2 at epoch 2
			_, err = r.MoveOwner(ctx, testOp, id, "c2", 2, "c1")
			require.NoError(t, err)
			handOver(t, gdb, id) // back at c1, epoch 3

			_, err = r.CancelScheduledMove(ctx, testOp, m.ID)
			require.ErrorIs(t, err, ErrConflict, "the move was handed over (epoch 1 -> 2)")

			got, err := r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.NotEqual(t, MoveCancelled, got.State)
		})
	}
}

// P12c L7: CancelMove (the request on the tailnet) also closes the planned
// move that made that request, in the same transaction.
func TestCancelMoveClosesPlannedMove(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)

			m, err := r.ScheduleMove(ctx, testOp, id, "c2", at(r, time.Minute), "")
			require.NoError(t, err)
			require.Equal(t, []string{m.ID}, due(t, r, r.now).Requested)

			_, err = r.CancelMove(ctx, testOp, id, 1)
			require.NoError(t, err)

			got, err := r.GetMove(ctx, m.ID)
			require.NoError(t, err)
			assert.Equal(t, MoveCancelled, got.State)
			require.NotNil(t, got.FinishedAt)

			_, ok, err := r.PendingMove(ctx, id)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}
