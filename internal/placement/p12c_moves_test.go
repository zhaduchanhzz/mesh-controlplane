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
