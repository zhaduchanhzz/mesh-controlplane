//go:build p05cluster

package servertest_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/key"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P09, ADR 0009: an external DERP server of cluster N asks the cluster's
// /verify (through its public URL, so always the lease holder). Only the
// cluster's secret gets an answer; the answer admits a node key iff it
// belongs to a live, unexpired node of a tailnet this cluster owns.

const verifySecret = "p09-verify-secret-0123456789abcdefghij" // >= 32 chars

func nodeID(t *testing.T, tc *servertest.TestCluster, hostname string) types.NodeID {
	t.Helper()

	for _, r := range tc.NodeRows(t) {
		if r.Hostname == hostname {
			return types.NodeID(r.ID)
		}
	}

	t.Fatalf("no node %q", hostname)

	return 0
}

// Without the secret every caller gets the same 403, whatever the node key:
// the endpoint is no oracle of node existence (P08a F2).
func TestClusterVerifyNeedsSecret(t *testing.T) {
	c := servertest.NewTestClusterWith(t, servertest.ClusterOptions{VerifySecret: verifySecret},
		servertest.TailnetSeed{Slug: tenantA})
	c.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, c.Tenant(t, tenantA), "alice", "v-a1")[0]
	known, unknown := selfKey(t, a1), key.NewNode().Public()

	var bodies []string

	for _, s := range []string{"", "wrong-secret-wrong-secret-wrong-secret"} {
		for _, nk := range []key.NodePublic{known, unknown} {
			r := c.Verify(t, s, nk)
			assert.Equal(t, http.StatusForbidden, r.Status, "secret %q", s)
			bodies = append(bodies, r.Body)
		}
	}

	for _, b := range bodies[1:] {
		assert.Equal(t, bodies[0], b, "the refusal does not depend on the node key")
	}

	r := c.Verify(t, verifySecret, known)
	require.Equal(t, http.StatusOK, r.Status, r.Body)
	assert.True(t, r.Allow, "a live node of a tailnet of this cluster")
}

// A cluster without a configured secret refuses every call.
func TestClusterVerifyWithoutSecretRefusesAll(t *testing.T) {
	c := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	c.SetPolicy(t, tenantA, allowAll)
	a1 := enroll(t, c.Tenant(t, tenantA), "alice", "nv-a1")[0]

	assert.Equal(t, http.StatusForbidden, c.Verify(t, verifySecret, selfKey(t, a1)).Status)
}

// Admission: nodes of every tailnet the cluster owns; never an unknown key,
// an expired or deleted node, or a node of a tailnet of another cluster.
func TestClusterVerifyScopedToClusterTailnets(t *testing.T) {
	c1 := servertest.NewTestClusterWith(t, servertest.ClusterOptions{VerifySecret: verifySecret},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	c2 := c1.AddCluster(t)

	require.Equal(t, c2.ClusterID, c1.PlaceTailnet(t, "p09-v-c"), "c1 owns two tailnets: the next goes to c2")
	c2.WaitRuntime(t, "p09-v-c", true, clusterWait)

	for _, s := range []string{tenantA, tenantB} {
		c1.SetPolicy(t, s, allowAll)
	}

	c2.SetPolicyViaDB(t, "p09-v-c", allowAll)

	a := enroll(t, c1.Tenant(t, tenantA), "alice", "v-a1", "v-a2")
	b := enroll(t, c1.Tenant(t, tenantB), "bob", "v-b1")
	cc := enroll(t, c2.Tenant(t, "p09-v-c"), "carol", "v-c1")

	allow := func(tc *servertest.TestCluster, nk key.NodePublic) bool {
		t.Helper()

		r := tc.Verify(t, verifySecret, nk)
		require.Equal(t, http.StatusOK, r.Status, r.Body)

		return r.Allow
	}

	assert.True(t, allow(c1, selfKey(t, a[0])), "a (c1)")
	assert.True(t, allow(c1, selfKey(t, b[0])), "b (c1): every tailnet of the cluster")
	assert.False(t, allow(c1, key.NewNode().Public()), "unknown key")
	assert.False(t, allow(c1, selfKey(t, cc[0])), "a node of c2's tailnet at c1")
	assert.True(t, allow(c2, selfKey(t, cc[0])), "the same node at its own cluster")
	assert.False(t, allow(c2, selfKey(t, a[0])), "a node of c1's tailnet at c2")

	// Expired.
	past := time.Now().Add(-time.Hour)
	_, _, err := c1.Runtime(t, tenantA).GetState().SetNodeExpiry(nodeID(t, c1, "v-a2"), &past)
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return !allow(c1, selfKey(t, a[1])) }, clusterWait, 100*time.Millisecond,
		"an expired node is not admitted")

	// Deleted.
	st := c1.Runtime(t, tenantB).GetState()
	nv, ok := st.GetNodeByID(nodeID(t, c1, "v-b1"))
	require.True(t, ok)
	_, err = st.DeleteNode(nv)
	require.NoError(t, err)
	assert.False(t, allow(c1, selfKey(t, b[0])), "a deleted node is not admitted")
}

// A standby process (no lease, no runtime) admits nobody; after the
// failover the new active admits the same node (ADR 0009: "cả sau failover").
func TestClusterVerifyStandbyRefusesAndFailoverAdmits(t *testing.T) {
	c := servertest.NewTestClusterWith(t, servertest.ClusterOptions{FrontDoor: true, VerifySecret: verifySecret},
		servertest.TailnetSeed{Slug: tenantA})
	standby := c.StartProcess(t)
	c.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, c.Tenant(t, tenantA), "alice", "fv-a1")[0]
	nk := selfKey(t, a1)

	r := c.ProcessVerify(t, standby, verifySecret, nk)
	require.Equal(t, http.StatusOK, r.Status, r.Body)
	assert.False(t, r.Allow, "a standby admits nobody")

	r = c.ProcessVerify(t, c.Primary(), verifySecret, nk)
	require.Equal(t, http.StatusOK, r.Status, r.Body)
	assert.True(t, r.Allow, "the active admits")

	c.StopProcess(t, c.Primary())
	c.WaitReady(t, standby, clusterWait)

	assert.Eventually(t, func() bool {
		r := c.Verify(t, verifySecret, nk)

		return r.Status == http.StatusOK && r.Allow
	}, clusterWait, 100*time.Millisecond, "after failover the new active admits through the public URL")
}

// After a hand-over (ADR 0010) the source cluster no longer admits the
// tailnet's nodes and the target does.
func TestClusterVerifyFollowsHandOver(t *testing.T) {
	c1 := servertest.NewTestClusterWith(t, servertest.ClusterOptions{VerifySecret: verifySecret},
		servertest.TailnetSeed{Slug: tenantA})
	c2 := c1.AddCluster(t)
	c1.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, c1.Tenant(t, tenantA), "alice", "hv-a1")[0]
	nk := selfKey(t, a1)

	require.True(t, c1.Verify(t, verifySecret, nk).Allow)
	require.False(t, c2.Verify(t, verifySecret, nk).Allow)

	c1.MoveOwner(t, tenantA, c1.ClusterID, c2.ClusterID)
	c2.WaitRuntime(t, tenantA, true, clusterWait)

	assert.False(t, c1.Verify(t, verifySecret, nk).Allow, "the source no longer admits the moved tailnet")
	assert.True(t, c2.Verify(t, verifySecret, nk).Allow, "the target admits it")
}
