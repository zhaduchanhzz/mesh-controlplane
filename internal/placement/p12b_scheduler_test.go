package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The scheduler loop requests a move once it is due, then stops with ctx.
func TestRunSchedulerRequestsDueMove(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r, id := moveEnv(t, gdb)

			m, err := r.ScheduleMove(context.Background(), testOp, id, "c2", time.Now().UTC().Add(300*time.Millisecond), "")
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})

			go func() {
				defer close(done)
				RunScheduler(ctx, r, 100*time.Millisecond, t.Logf)
			}()

			assert.Eventually(t, func() bool {
				got, err := r.GetMove(context.Background(), m.ID)

				return err == nil && got.State == MoveRequested
			}, 10*time.Second, 50*time.Millisecond)
			assert.Equal(t, "c2", movingTo(t, gdb, id))

			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("scheduler did not stop")
			}
		})
	}
}
