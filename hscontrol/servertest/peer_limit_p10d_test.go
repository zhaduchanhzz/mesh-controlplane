package servertest_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestPeerEntriesLimitRefusesNewNode (P10d): under a full mesh (no policy
// here allows all), three nodes make 3x2 = 6 peer entries; with
// tuning.max_peer_entries 6 the fourth node is refused with a message that
// tells the tenant why, and leaves no node. Nodes already in keep working.
func TestPeerEntriesLimitRefusesNewNode(t *testing.T) {
	srv := servertest.NewServer(t, servertest.WithMaxPeerEntries(6))
	user := srv.CreateUser(t, "mesh")
	key := srv.CreatePreAuthKey(t, types.UserID(user.ID))

	clients := make([]*servertest.TestClient, 0, 3)
	for i := range 3 {
		clients = append(clients, servertest.NewClient(t, srv, fmt.Sprintf("m%d", i), servertest.WithAuthKey(key)))
	}

	for _, c := range clients {
		c.WaitForPeers(t, 2, 10*time.Second)
	}

	refused, err := servertest.TryNewClient(t, srv, "m3", servertest.WithAuthKey(key))
	require.Error(t, err, "a fourth node would take the mesh to 12 entries")
	assert.Contains(t, err.Error(), "connects too many devices")
	assert.Empty(t, srv.State().GetNodesByMachineKeyAllUsers(refused.MachineKey()), "a refused join leaves no node")

	for _, c := range clients {
		assert.Len(t, c.Peers(), 2, "nodes already in keep their peers")
	}
}

// TestPeerEntriesLimitIgnoresUnconnectedNodes (P10d): the bound counts peer
// relationships, not devices. A policy that connects no one (deny-all) lets
// any number of nodes join under the same small bound.
func TestPeerEntriesLimitIgnoresUnconnectedNodes(t *testing.T) {
	srv := servertest.NewServer(t, servertest.WithMaxPeerEntries(6), servertest.WithPolicyEmptyDenies())
	user := srv.CreateUser(t, "isolated")
	key := srv.CreatePreAuthKey(t, types.UserID(user.ID))

	for i := range 6 {
		_, err := servertest.TryNewClient(t, srv, fmt.Sprintf("i%d", i), servertest.WithAuthKey(key))
		require.NoError(t, err, "node %d: no peer relationships, no bound reached", i)
	}
}
