package servertest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"tailscale.com/types/netmap"
)

// TestTenantDefaultDeny (P03a) runs a server as serve-tailnets runs each
// tailnet runtime, with PolicyEmptyDenies on and no policy stored, and real
// control clients. Nobody, not even two devices of one user, may see a peer
// or receive a packet filter rule until an explicit policy allows it.
func TestTenantDefaultDeny(t *testing.T) {
	t.Parallel()

	const timeout = 15 * time.Second

	srv := servertest.NewServer(t, servertest.WithPolicyEmptyDenies())
	alice := srv.CreateUser(t, "alice")
	bob := srv.CreateUser(t, "bob")

	a1 := servertest.NewClient(t, srv, "a1", servertest.WithUser(alice))
	a2 := servertest.NewClient(t, srv, "a2", servertest.WithUser(alice))
	b1 := servertest.NewClient(t, srv, "b1", servertest.WithUser(bob))
	clients := []*servertest.TestClient{a1, a2, b1}

	_, err := srv.State().GetPolicy()
	require.Error(t, err, "precondition: no policy is stored")

	denied := func(nm *netmap.NetworkMap) bool {
		return len(nm.Peers) == 0 && len(nm.PacketFilter) == 0 && nm.PacketFilterRules.Len() == 0 &&
			(nm.SSHPolicy == nil || len(nm.SSHPolicy.Rules) == 0)
	}

	assertDenied := func(t *testing.T, phase string) {
		t.Helper()

		for _, c := range clients {
			nm := c.Netmap()
			require.NotNil(t, nm, "%s: %s has a netmap", phase, c.Name)
			assert.Empty(t, c.PeerNames(), "%s: %s must see no peers", phase, c.Name)
			assert.Empty(t, nm.PacketFilter, "%s: %s packet filter must be empty", phase, c.Name)
			assert.Zero(t, nm.PacketFilterRules.Len(), "%s: %s packet filter rules must be empty", phase, c.Name)

			if nm.SSHPolicy != nil {
				assert.Empty(t, nm.SSHPolicy.Rules, "%s: %s must get no SSH rules", phase, c.Name)
			}

			nv, ok := srv.State().GetNodeByNodeKey(c.Netmap().NodeKey)
			require.True(t, ok)
			assert.Zero(t, srv.State().ListPeers(nv.ID()).Len(), "%s: server lists no peers for %s", phase, c.Name)
		}
	}

	// updatePolicy writes pol through the policy API path (validate, store,
	// reload) and returns every client's update count from before the write.
	updatePolicy := func(t *testing.T, pol string) []int {
		t.Helper()

		counts := make([]int, len(clients))
		for i, c := range clients {
			counts[i] = c.UpdateCount()
		}

		_, changes, err := srv.State().UpdatePolicy([]byte(pol))
		require.NoError(t, err)
		require.NotEmpty(t, changes, "the policy changed, so clients must be told")
		srv.App.Change(changes...)

		return counts
	}

	// Phase 1: no policy at all. Every client got its first map at
	// registration; a1 and a2 registered before b1 joined.
	assertDenied(t, "no policy")

	// Barrier: an explicit empty grant list changes the policy (nil to
	// {"grants":[]}) after all three nodes exist. It must still deny
	// everything. Deny to deny changes nothing a client holds, so with delta-first
	// (ADR 0015 D2) no client gets a frame: wait for the server to have
	// handed out the change instead, then for every client to be denied.
	updatePolicy(t, `{"grants":[]}`)
	srv.WaitMapsDelivered(t, timeout)

	for _, c := range clients {
		c.WaitForCondition(t, "denied after {\"grants\":[]}", timeout, denied)
	}

	assertDenied(t, `{"grants":[]}`)

	// Phase 2: an explicit allow policy; everyone sees everyone.
	counts := updatePolicy(t, `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`)
	for i, c := range clients {
		c.WaitForCondition(t, "peers and filter after allow", timeout, func(nm *netmap.NetworkMap) bool {
			return c.UpdateCount() > counts[i] && len(nm.Peers) == 2 && len(nm.PacketFilter) > 0
		})
	}

	servertest.AssertMeshComplete(t, clients)
	servertest.AssertSymmetricVisibility(t, clients)

	// Phase 3: back to "{}", which upstream reads as allow-all. In a
	// tenant tailnet it revokes everything again.
	counts = updatePolicy(t, `{}`)
	for i, c := range clients {
		c.WaitForCondition(t, "revoked after {}", timeout, func(nm *netmap.NetworkMap) bool {
			return c.UpdateCount() > counts[i] && denied(nm)
		})
	}

	assertDenied(t, "{}")
}

// TestTenantDefaultDenyOffKeepsUpstream: the same start without the switch
// is upstream headscale, where no policy allows all.
func TestTenantDefaultDenyOffKeepsUpstream(t *testing.T) {
	t.Parallel()

	srv := servertest.NewServer(t)
	alice := srv.CreateUser(t, "alice")
	bob := srv.CreateUser(t, "bob")

	a1 := servertest.NewClient(t, srv, "a1", servertest.WithUser(alice))
	b1 := servertest.NewClient(t, srv, "b1", servertest.WithUser(bob))

	a1.WaitForPeers(t, 1, 15*time.Second)
	b1.WaitForPeers(t, 1, 15*time.Second)
	assert.NotEmpty(t, a1.Netmap().PacketFilter)
}
