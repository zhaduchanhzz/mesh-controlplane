//go:build p05cluster

package servertest_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P13c (ADR 0012 §3): a cluster never serves a tailnet again at an
// owner_epoch below one it served it at (database restored to an earlier
// point): the runtime is retired and not loaded again until the epoch fence
// raised owner_epoch above it. Other tailnets are not affected.
func TestP13cReconcileRefusesOwnerEpochRegression(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "a"}, servertest.TailnetSeed{Slug: "b"})
	tc.WaitRuntime(t, "a", true, 10*time.Second)

	idA := tc.Tailnet(t, "a").ID

	var loadsA atomic.Int32

	tc.Cluster.SetLoadHook(func(id tailnet.ID) {
		if id == idA {
			loadsA.Add(1)
		}
	})
	t.Cleanup(func() { tc.Cluster.SetLoadHook(nil) })

	// A new epoch (re-assignment to the same cluster): reloaded at 3.
	tc.SetTailnetOwner(t, "a", tc.ClusterID, 3)

	require.Eventually(t, func() bool { return loadsA.Load() >= 1 && tc.HasRuntime(t, "a") },
		10*time.Second, 50*time.Millisecond, "runtime of a reloaded at epoch 3")

	// Restore: owner_epoch back to 2.
	loadsA.Store(0)
	tc.RestoreOwnerEpochForTest(t, "a", 2)

	tc.WaitRuntime(t, "a", false, 10*time.Second)

	// Three reconcile intervals: never loaded again at the older epoch.
	time.Sleep(3 * 2 * time.Second) //nolint:forbidigo // reconcile runs every 2 s

	assert.Zero(t, loadsA.Load(), "tailnet a loaded again at owner_epoch 2 after serving it at 3")
	assert.False(t, tc.HasRuntime(t, "a"))
	assert.True(t, tc.HasRuntime(t, "b"), "another tailnet is not affected")

	// The epoch fence raises owner_epoch above anything served: loaded.
	tc.SetTailnetOwner(t, "a", tc.ClusterID, 1<<40)
	tc.WaitRuntime(t, "a", true, 10*time.Second)
}
