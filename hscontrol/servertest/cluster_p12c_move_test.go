//go:build p05cluster

package servertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// P12c C1 (review 2026-11-26): a move is requested at epoch E, the source
// retires its runtime and waits for the drain (held here by an admitted
// request); the operator cancels (the epoch stays E), the source loads the
// tailnet again, the operator requests the move again, still at E, while
// the first drain is under way; that drain ends before the source's next
// reconcile tick. The source must stop serving the reloaded runtime before
// the owner flips (ADR 0010, invariant 1): at no sample may
// the database name c2 the owner while c1 still holds a runtime, and never
// may both clusters hold one (R11).
func TestTwoClustersCancelReloadRerequestKeepsR11(t *testing.T) {
	c1 := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "p12c-c1"})
	c2 := c1.AddCluster(t)

	const slug = "p12c-c1"

	c1.WaitRuntime(t, slug, true, clusterWait)

	ctx := context.Background()
	id := c1.Tailnet(t, slug).ID.String()

	// An admitted request keeps the first retire in its drain.
	release := c1.Runtime(t, slug).HoldRequestForTest(t)
	t.Cleanup(release)

	epoch := c1.MoveOwner(t, slug, c1.ClusterID, c2.ClusterID)
	c1.WaitRuntime(t, slug, false, clusterWait)

	_, err := c1.Registry(t).CancelMove(ctx, servertest.TestOperator, id, epoch)
	require.NoError(t, err)

	c1.WaitRuntime(t, slug, true, clusterWait)
	require.Equal(t, epoch, c1.TailnetRow(t, slug).OwnerEpoch, "a cancel does not bump the epoch")

	require.Equal(t, epoch, c1.MoveOwner(t, slug, c1.ClusterID, c2.ClusterID), "requested again at the same epoch")

	// The first drain ends now, before the source's next reconcile tick
	// has seen the new request: the reloaded runtime still serves.
	release()

	var both, flippedWhileServing int

	deadline := time.Now().Add(clusterWait)

	for time.Now().Before(deadline) {
		owner := c1.TailnetRow(t, slug).OwnerClusterID
		srcHas, dstHas := c1.HasRuntime(t, slug), c2.HasRuntime(t, slug)

		if srcHas && dstHas {
			both++
		}

		if owner == c2.ClusterID && srcHas {
			flippedWhileServing++
		}

		if dstHas && !srcHas {
			break
		}

		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // sampling both clusters
	}

	t.Logf("C1 samples: both loaded %d; owner c2 while c1 served %d", both, flippedWhileServing)

	assert.Zero(t, both, "R11: both clusters held a runtime")
	assert.Zero(t, flippedWhileServing, "the owner flipped while the source still served the tailnet")

	c2.WaitRuntime(t, slug, true, clusterWait)

	row := c1.TailnetRow(t, slug)
	assert.Equal(t, c2.ClusterID, row.OwnerClusterID)
	assert.Equal(t, epoch+1, row.OwnerEpoch, "one hand-over, epoch + 1")
	assert.Nil(t, row.MovingToClusterID)

	notServedFor(t, c1, slug, 3*time.Second)
}
