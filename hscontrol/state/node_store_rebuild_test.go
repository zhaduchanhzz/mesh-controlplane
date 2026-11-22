package state

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestRebuildPeerMapsAppliesQueuedWritesFirst pins that RebuildPeerMaps
// does not wait for the batch timeout, and that writes queued before it
// are applied in the same batch, before it, in order: a caller that
// returns from RebuildPeerMaps sees every earlier write in the snapshot.
func TestRebuildPeerMapsAppliesQueuedWritesFirst(t *testing.T) {
	const batchTimeout = time.Hour // would hang the test if waited for

	var builds [][]types.NodeID

	peersFunc := func(nodes []types.NodeView) map[types.NodeID][]types.NodeID {
		ids := make([]types.NodeID, 0, len(nodes))
		for _, n := range nodes {
			ids = append(ids, n.ID())
		}

		builds = append(builds, ids)

		return allowAllPeersFunc(nodes)
	}

	node1 := createTestNode(1, 1, "user1", "node1")
	store := NewNodeStore(types.Nodes{&node1}, peersFunc, 100, batchTimeout)
	store.Start()

	defer store.Stop()

	// Queue two writes; the queue is unbuffered, so each send returns once
	// the writer holds it in its pending batch.
	node2 := createTestNode(2, 1, "user1", "node2")
	node3 := createTestNode(3, 1, "user1", "node3")
	results := make([]chan struct{}, 0, 2)

	for _, n := range []types.Node{node2, node3} {
		w := work{op: put, nodeID: n.ID, node: n, result: make(chan struct{})}
		store.writeQueue <- w

		results = append(results, w.result)
	}

	done := make(chan struct{})

	go func() {
		store.RebuildPeerMaps()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RebuildPeerMaps waited for the batch timeout")
	}

	// When RebuildPeerMaps returns, the snapshot already holds the
	// earlier writes ...
	snap := store.data.Load()
	require.Len(t, snap.nodesByID, 3)
	require.ElementsMatch(t, []types.NodeID{2, 3}, snap.peersByNode[1])
	// One build for the initial snapshot, one for the batch: the two puts
	// and the rebuild were applied together.
	require.Len(t, builds, 2)
	require.Len(t, builds[1], 3)

	// ... and their writers are released by the same batch, not the
	// batch timeout.
	for _, r := range results {
		select {
		case <-r:
		case <-time.After(10 * time.Second):
			t.Fatal("a write queued before the rebuild was not applied with it")
		}
	}
}
