package mapper

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
	"tailscale.com/tailcfg"
)

// P00d batcher-side safeguards, run through the golden harness with the
// switch on. Every step checks each node's client model against the
// server's adjacency (netmapGolden.checkModel), so a revoked peer a client
// keeps fails the step as ISOLATION.

func connectAll(g *netmapGolden) {
	for _, h := range []string{"a1", "a2", "b1", "b2", "c1"} {
		g.connect(g.node(h).ID())
	}
}

func fullFrame(rs []*tailcfg.MapResponse) bool {
	return slices.ContainsFunc(rs, func(r *tailcfg.MapResponse) bool { return r.Peers != nil })
}

func removes(rs []*tailcfg.MapResponse, id types.NodeID) bool {
	return slices.ContainsFunc(rs, func(r *tailcfg.MapResponse) bool {
		return slices.Contains(r.PeersRemoved, id.NodeID())
	})
}

func without(ps []types.NodeID, id types.NodeID) []types.NodeID {
	return slices.DeleteFunc(slices.Clone(ps), func(p types.NodeID) bool { return p == id })
}

// TestReconcileReplacesDivergedAdjacency is M03 (I02): an extra peer
// injected into the adjacency reaches a client; reconciliation finds it,
// restores the full build, and sends a full map to the two nodes involved
// and no other, which removes the peer from the client.
func TestReconcileReplacesDivergedAdjacency(t *testing.T) {
	g := newNetmapGolden(t, true)
	connectAll(g)
	g.step("policy groups", g.setPolicy("groups", goldenPolicyGroups))

	a1, c1 := g.node("a1").ID(), g.node("c1").ID()
	require.NotContains(t, g.model[a1], c1.NodeID(), "precondition: groups keep alice and carol apart")

	// Equal before any fault.
	require.Empty(t, g.st.ReconcilePeerMaps())

	g.st.MutatePeerMapForTest(func(m map[types.NodeID][]types.NodeID) {
		m[a1] = append(m[a1], c1)
		m[c1] = append(m[c1], a1)
	})

	// A peer delta naming c1, rendered through the corrupt adjacency (a
	// state write such as a rename would rebuild it first).
	g.step("deltas over the corrupt adjacency", []change.Change{change.NodeAdded(c1), change.NodeAdded(a1)})
	require.True(t, g.model[a1][c1.NodeID()], "the fault reached a1's client")

	diverged := g.st.ReconcilePeerMaps()
	require.ElementsMatch(t, []types.NodeID{a1, c1}, diverged)

	g.b.SendFullMaps(diverged)
	g.step("reconcile", nil)

	require.ElementsMatch(t, []types.NodeID{a1, c1}, slices.Collect(func(yield func(types.NodeID) bool) {
		for id := range g.last {
			if !yield(id) {
				return
			}
		}
	}), "only the diverged nodes get a frame")
	require.True(t, fullFrame(g.last[a1]), "a1 gets a full map")
	require.True(t, removes(g.last[a1], c1), "which removes c1")
	require.False(t, g.model[a1][c1.NodeID()])

	require.Empty(t, g.st.ReconcilePeerMaps(), "equal again after the repair")
}

// TestRemovalReachesNodesTheAdjacencyMissed is M04 / R5: the adjacency
// misses a node that was sent the deleted peer; with the policy refresh
// gone from local removals, the removal still reaches it through what it
// was sent.
func TestRemovalReachesNodesTheAdjacencyMissed(t *testing.T) {
	g := newNetmapGolden(t, true)
	connectAll(g)

	a1, b2 := g.node("a1").ID(), g.node("b2").ID()
	require.True(t, g.model[a1][b2.NodeID()])

	g.st.MutatePeerMapForTest(func(m map[types.NodeID][]types.NodeID) {
		m[a1] = without(m[a1], b2)
		m[b2] = without(m[b2], a1)
	})

	cs, err := g.st.DeleteNode(g.node("b2"))
	require.NoError(t, err)

	for _, c := range cs {
		require.NotEqual(t, "policy", c.Type(), "a local removal has no policy refresh")
		require.NotEqual(t, a1, c.TargetNode, "the adjacency does not name a1")
	}

	g.step("delete b2", cs)
	require.True(t, removes(g.last[a1], b2), "a1 still gets the removal")
}

// TestRemovalSurvivesFullPendingQueue is M04 (I11, I13): a node whose
// pending queue overflowed collapses to a full map; a removal queued
// behind it still takes the peer out.
func TestRemovalSurvivesFullPendingQueue(t *testing.T) {
	g := newNetmapGolden(t, true)
	connectAll(g)

	a1, b2 := g.node("a1").ID(), g.node("b2").ID()

	// Changes that do not fold, past the cap.
	for range maxPendingSize + 8 {
		g.b.addToBatch(change.Change{Reason: "flood", TargetNode: a1, IncludeDNS: true})
	}

	cs, err := g.st.DeleteNode(g.node("b2"))
	require.NoError(t, err)
	g.step("delete b2 behind a full queue", cs)

	require.True(t, fullFrame(g.last[a1]), "the overflowed queue collapsed to a full map")
	require.True(t, removes(g.last[a1], b2), "with b2 removed")
}

// TestFullMapRefreshCoversEveryNode is the periodic full map (measure 4):
// the refresher hands out every connected node once per interval in equal
// shares, and a share gets full maps and nothing else does.
func TestFullMapRefreshCoversEveryNode(t *testing.T) {
	g := newNetmapGolden(t, true)
	connectAll(g)

	const ticks = 4

	var r fullMapRefresher

	seen := make([]types.NodeID, 0, 2*ticks)

	for range ticks {
		share := r.next(g.b, ticks)
		require.Len(t, share, 2, "ceil(5/4) per tick")

		g.b.SendFullMaps(share)
		g.step("refresh share", nil)

		for id, rs := range g.last {
			require.Contains(t, share, id, "only the share gets frames")
			require.True(t, fullFrame(rs), "and they are full maps")
		}

		seen = append(seen, share...)
	}

	hosts := []string{"a1", "a2", "b1", "b2", "c1"}

	all := make([]types.NodeID, 0, len(hosts))
	for _, h := range hosts {
		all = append(all, g.node(h).ID())
	}

	require.Subset(t, seen, all, "every node within one interval")

	// The running loop: a 2s interval refreshes everyone within it.
	done := make(chan struct{})
	go g.b.RunFullMapRefresh(2*time.Second, done)

	time.Sleep(2500 * time.Millisecond) //nolint:forbidigo // runs the 2 s refresh loop for one full interval
	close(done)
	g.step("refresh loop", nil)

	for _, id := range all {
		require.True(t, fullFrame(g.last[id]), "node %d refreshed", id)
	}
}
