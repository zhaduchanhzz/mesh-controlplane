package servertest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
)

// TestReviewR2PolicyOverPeerBoundRefused (P14f review R2): the tailnet's
// peer bound (tuning.max_peer_entries) holds for policy writes too. Nodes
// registered under a denying policy, then a policy that would link them all
// past the bound: refused, nothing stored, the old policy stays in force.
func TestReviewR2PolicyOverPeerBoundRefused(t *testing.T) {
	t.Parallel()

	srv := servertest.NewServer(t, servertest.WithPolicyEmptyDenies(), servertest.WithMaxPeerEntries(6))
	// One user per node: same-user nodes are always peers.
	for _, n := range []string{"u1", "u2", "u3", "u4"} {
		servertest.NewClient(t, srv, n+"-node", servertest.WithUser(srv.CreateUser(t, n)))
	}

	_, _, err := srv.State().UpdatePolicy([]byte(`{"grants":[]}`))
	require.NoError(t, err)

	// 4 nodes, full mesh: 12 peer entries > 6.
	_, _, err = srv.State().UpdatePolicy([]byte(`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`))
	require.ErrorIs(t, err, state.ErrInvalidPolicy, "a policy past the peer bound is refused")
	require.ErrorContains(t, err, "peer")

	p, err := srv.State().GetPolicy()
	require.NoError(t, err)
	assert.JSONEq(t, `{"grants":[]}`, p.Data, "nothing stored")
	assert.Zero(t, srv.State().ListPeers(srv.State().ListNodes().At(0).ID()).Len(), "the old policy stays in force")

	// Within the bound (validation only): one pair linked, 2 entries.
	err = srv.State().ValidatePolicy([]byte(`{"acls":[{"action":"accept","src":["u1@"],"dst":["u2@:*"]}]}`))
	require.NoError(t, err, "a policy within the bound passes")
}
