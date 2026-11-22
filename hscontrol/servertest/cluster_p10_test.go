//go:build p05cluster

package servertest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P10: a cluster serves a tailnet only while it owns it at the epoch it
// loaded it with. Writes are fenced in the database (tenantscope); these
// tests cover what the running cluster does when ownership changes.

const tenantC = "tenant-c"

// Ownership moved to another cluster: the runtime is retired, its tailnet's
// credentials no longer register here, the other tenant is unaffected (I12,
// S17, I10).
func TestClusterOwnershipMoveRetiresRuntime(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	keyA := mintKey(t, a, "alice")
	keyB := mintKey(t, b, "bob")
	servertest.NewClusterClient(t, a, "move-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	b1 := servertest.NewClusterClient(t, b, "move-b1", servertest.ClusterClientOptions{AuthKey: keyB})

	row := tc.TailnetRow(t, tenantA)
	tc.SetTailnetOwner(t, tenantA, "another-cluster", row.OwnerEpoch+1)

	tc.WaitRuntime(t, tenantA, false, clusterWait)

	before := tc.NodeRows(t)
	_, err := servertest.TryNewClusterClient(t, tc.Front(), "move-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	require.Error(t, err, "a key of a tailnet this cluster no longer owns must be refused")
	assert.Equal(t, before, tc.NodeRows(t), "no node created after the move")

	require.True(t, tc.HasRuntime(t, tenantB), "B is still served")
	b2 := servertest.NewClusterClient(t, b, "move-b2", servertest.ClusterClientOptions{AuthKey: keyB})
	waitPeersExactly(t, b1, selfKey(t, b2))
	waitPeersExactly(t, b2, selfKey(t, b1))
}

// The tailnet left ACTIVE (suspended): retired like a move.
func TestClusterSuspendRetiresRuntime(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)

	keyA := mintKey(t, tc.Tenant(t, tenantA), "alice")

	tc.SetTailnetStatus(t, tenantA, types.TailnetSuspended)
	tc.WaitRuntime(t, tenantA, false, clusterWait)

	_, err := servertest.TryNewClusterClient(t, tc.Front(), "susp-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	require.Error(t, err, "a key of a SUSPENDED tailnet must be refused")
	require.True(t, tc.HasRuntime(t, tenantB))
}

// A new epoch for the same cluster (e.g. after a CAS by placement): the old
// runtime is retired and a fresh one loaded at the new epoch; clients
// reconnect without logging in again and keep working.
func TestClusterEpochBumpReloadsRuntime(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	a := tc.Tenant(t, tenantA)
	keyA := mintKey(t, a, "alice")
	a1 := servertest.NewClusterClient(t, a, "epoch-a1", servertest.ClusterClientOptions{AuthKey: keyA})

	old := tc.Runtime(t, tenantA)
	row := tc.TailnetRow(t, tenantA)
	tc.SetTailnetOwner(t, tenantA, tc.ClusterID, row.OwnerEpoch+1)

	require.Eventually(t, func() bool {
		rt, ok := tc.Cluster.Runtime(tc.Tailnet(t, tenantA).ID)

		return ok && rt != old
	}, clusterWait, 50*time.Millisecond, "a new runtime is loaded at the new epoch")

	// A second client joins the reloaded runtime; the first one reconnects
	// on its own (same node, no new login) and sees it.
	a2 := servertest.NewClusterClient(t, tc.Tenant(t, tenantA), "epoch-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, a1, selfKey(t, a2))
	waitPeersExactly(t, a2, selfKey(t, a1))

	rows := tc.NodeRows(t)
	assert.Len(t, rows, 2, "a1 kept its node across the reload: %v", rows)
}

// A tailnet created and activated while the cluster runs is served without
// a restart, starts with a deny-all policy, and stays isolated.
func TestClusterLoadsNewTailnetLive(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, tc.Tenant(t, tenantA), "alice", "live-a1")[0]

	tc.CreateTailnetLive(t, tenantC)
	tc.WaitRuntime(t, tenantC, true, clusterWait)

	c := tc.Tenant(t, tenantC)
	cClients := enroll(t, c, "carol", "live-c1", "live-c2")

	// Deny-all until the owner writes a policy: no peers.
	for _, cl := range cClients {
		require.Empty(t, cl.Netmap().Peers, "%s: a new tailnet starts with a deny-all policy", cl.Name)
	}

	tc.SetPolicy(t, tenantC, allowAll)
	waitPeersExactly(t, cClients[0], selfKey(t, cClients[1]))
	waitPeersExactly(t, cClients[1], selfKey(t, cClients[0]))
	assert.Empty(t, a1.Netmap().Peers, "A sees nothing of C")
}
