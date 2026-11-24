package v2

import (
	"flag"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"pgregory.net/rapid"
)

// localPolicies are policies under which some joins and removals are local
// (P00d): rules over * and host prefixes resolve to no node, rules over a
// user or tag do for the nodes of that user or tag.
var localPolicies = []string{
	`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`,
	`{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`,
	`{"hosts":{"lan":"100.64.0.0/26"},"acls":[{"action":"accept","src":["lan"],"dst":["lan:*"]}]}`,
	`{"tagOwners":{"tag:srv":["u1@"],"tag:db":["u1@"]},"acls":[{"action":"accept","src":["tag:srv"],"dst":["tag:db:*"]}]}`,
	`{"acls":[{"action":"accept","src":["u1@"],"dst":["*:22"]}]}`,
	`{"acls":[{"action":"accept","src":["autogroup:member"],"dst":["*:*"]}]}`,
	// Allow-all with rules that resolve per node outside the filter hash:
	// never local.
	`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}],
	  "ssh":[{"action":"accept","src":["autogroup:member"],"dst":["autogroup:self"],"users":["root"]}]}`,
	`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}],
	  "nodeAttrs":[{"target":["autogroup:member"],"attr":["funnel"]}]}`,
}

func clonePeerMap(m map[types.NodeID][]types.NodeID) map[types.NodeID][]types.NodeID {
	out := make(map[types.NodeID][]types.NodeID, len(m))
	for id, ps := range m {
		out[id] = slices.Clone(ps)
	}

	return out
}

// peerSets normalises a peer map for comparison: sorted lists, no empty
// entries.
func peerSets(m map[types.NodeID][]types.NodeID) map[types.NodeID][]types.NodeID {
	out := make(map[types.NodeID][]types.NodeID, len(m))

	for id, ps := range m {
		if len(ps) == 0 {
			continue
		}

		s := slices.Clone(ps)
		slices.Sort(s)
		out[id] = s
	}

	return out
}

// TestIncrementalPeerMapMatchesFullBuild is the P00d gate at the policy
// level (M01, I02, I05, I07). Over random tailnets and policies, a node set
// change by joins and removals is applied with SetNodes; then:
//   - SetNodes reports the change as local exactly when IncrementalPeerMap
//     accepts it;
//   - an accepted patch equals the full build on the new node set, as sets
//     per node (a peer the full build does not list is reported first, as
//     an isolation failure);
//   - for a local change, every node present before and after keeps its
//     packet filter and SSH policy, so a peer delta is all it needs.
func TestIncrementalPeerMapMatchesFullBuild(t *testing.T) {
	if f := flag.Lookup("rapid.checks"); f != nil && f.Value.String() == "100" {
		require.NoError(t, f.Value.Set("2000"))

		defer func() { _ = f.Value.Set("100") }()
	}

	var cov struct{ runs, local, refused, joins, removals, linkedJoins int }

	rapid.Check(t, func(rt *rapid.T) {
		nodes := drawPeerMapNodes(rt)

		polB := drawPeerMapPolicy(rt)
		if rapid.Bool().Draw(rt, "localPolicy") {
			polB = []byte(rapid.SampledFrom(localPolicies).Draw(rt, "policy"))
		}

		tenant := rapid.Bool().Draw(rt, "emptyDenies")

		order := rapid.Permutation(nodes).Draw(rt, "order")
		cut := rapid.IntRange(0, len(order)).Draw(rt, "before")
		before := slices.Clone(order[:cut])
		pool := order[cut:]

		// Joins: some of the nodes not there before.
		joining := pool[:rapid.IntRange(0, len(pool)).Draw(rt, "joins")]

		// Removals: some of the nodes there before.
		leaving := map[types.NodeID]bool{}

		for _, n := range before {
			if rapid.IntRange(0, 4).Draw(rt, "leave") == 0 {
				leaving[n.ID] = true
			}
		}

		if len(joining) == 0 && len(leaving) == 0 {
			rt.Skip("no node set change")
		}

		var after types.Nodes

		for _, n := range before {
			if !leaving[n.ID] {
				after = append(after, n)
			}
		}

		// Sometimes a node that stays also changes owner or tags in the
		// same SetNodes: never local (its relations can move without the
		// filter moving, e.g. same-user adjacency).
		if len(after) > 0 && rapid.IntRange(0, 3).Draw(rt, "alsoRetag") == 0 {
			i := rapid.IntRange(0, len(after)-1).Draw(rt, "retag")
			moved := *after[i]

			if moved.IsTagged() {
				u := rapid.SampledFrom(scanUsers).Draw(rt, "newOwner")
				moved.Tags, moved.User, moved.UserID = nil, &u, &u.ID
			} else {
				moved.Tags, moved.User, moved.UserID = []string{rapid.SampledFrom(scanTags).Draw(rt, "newTag")}, nil, nil
			}

			after[i] = &moved
		}

		after = append(after, joining...)
		after = rapid.Permutation(after).Draw(rt, "afterOrder")

		pm, err := NewPolicyManager(polB, scanUsers, before.ViewSlice(),
			WithEmptyPolicyDenies(tenant), WithLocalNodeChanges(true))
		if err != nil {
			rt.Skipf("policy rejected: %v", err)
		}

		// A second manager stays on the old node set: what nodes held.
		old, err := NewPolicyManager(polB, scanUsers, before.ViewSlice(), WithEmptyPolicyDenies(tenant))
		require.NoError(rt, err)

		prev, gen := pm.BuildPeerMapGen(before.ViewSlice())

		changed, err := pm.SetNodes(after.ViewSlice())
		require.NoError(rt, err)

		joined := make([]types.NodeID, 0, len(joining))
		for _, n := range joining {
			joined = append(joined, n.ID)
		}

		removed := make([]types.NodeID, 0, len(leaving))
		for id := range leaving {
			removed = append(removed, id)
		}

		got, gen2, ok := pm.IncrementalPeerMap(after.ViewSlice(), clonePeerMap(prev), gen, joined, removed)

		cov.runs++

		require.Equalf(rt, !changed, ok,
			"SetNodes local=%t but IncrementalPeerMap ok=%t\npolicy %s", !changed, ok, polB)

		if !ok {
			cov.refused++

			require.NotEqual(rt, gen, pm.RulesGeneration(), "a non-local change moves the rules generation")

			return
		}

		cov.local++
		cov.joins += len(joined)
		cov.removals += len(removed)

		require.Equal(rt, gen, gen2, "a local change keeps the rules generation")

		want := peerSets(pm.BuildPeerMap(after.ViewSlice()))
		have := peerSets(got)

		for id, peers := range have {
			for _, p := range peers {
				if !slices.Contains(want[id], p) {
					rt.Fatalf("ISOLATION: incremental lists %d as a peer of %d; the full build does not\npolicy %s", p, id, polB)
				}
			}
		}

		require.Equalf(rt, want, have, "policy %s", polB)

		for _, j := range joined {
			if len(have[j]) > 0 {
				cov.linkedJoins++
			}
		}

		// Nothing but the peer list moved for the nodes that stayed.
		for _, n := range after {
			if leaving[n.ID] || slices.Contains(joined, n.ID) {
				continue
			}

			fNew, err := pm.FilterForNode(n.View())
			require.NoError(rt, err)

			fOld, err := old.FilterForNode(n.View())
			require.NoError(rt, err)
			require.Equalf(rt, fOld, fNew, "local change moved the filter of %d\npolicy %s", n.ID, polB)

			sNew, err := pm.SSHPolicy("https://h", n.View())
			require.NoError(rt, err)

			sOld, err := old.SSHPolicy("https://h", n.View())
			require.NoError(rt, err)
			require.Equalf(rt, sOld, sNew, "local change moved the SSH policy of %d\npolicy %s", n.ID, polB)

			require.Equalf(rt, old.NodeCapMap(n.ID), pm.NodeCapMap(n.ID),
				"local change moved the capabilities of %d\npolicy %s", n.ID, polB)
		}
	})

	t.Logf("coverage: %+v", cov)

	if cov.runs < 200 {
		return
	}

	require.Positive(t, cov.local, "local changes")
	require.Positive(t, cov.refused, "refused changes")
	require.Positive(t, cov.joins, "joins")
	require.Positive(t, cov.removals, "removals")
	require.Positive(t, cov.linkedJoins, "joined nodes with peers")
}

// TestIncrementalPeerMapOffByDefault: without WithLocalNodeChanges a node
// set change is reported as before and the patch is refused.
func TestIncrementalPeerMapOffByDefault(t *testing.T) {
	nodes := scanManyRulesNodes(6)
	pm, err := NewPolicyManager([]byte(localPolicies[0]), scanUsers, nodes[:4].ViewSlice())
	require.NoError(t, err)

	prev, gen := pm.BuildPeerMapGen(nodes[:4].ViewSlice())

	changed, err := pm.SetNodes(nodes.ViewSlice())
	require.NoError(t, err)
	require.True(t, changed, "off: a join is a policy change, as upstream")

	_, _, ok := pm.IncrementalPeerMap(nodes.ViewSlice(), prev, gen, []types.NodeID{nodes[4].ID, nodes[5].ID}, nil)
	require.False(t, ok)
}

// TestIncrementalPeerMapRefusesStaleGeneration: a policy change between the
// build and the patch (R4: SetPolicy before RebuildPeerMaps) refuses it.
func TestIncrementalPeerMapRefusesStaleGeneration(t *testing.T) {
	nodes := scanManyRulesNodes(6)
	joined := []types.NodeID{nodes[4].ID, nodes[5].ID}

	// Control: the same join without a policy change in between is local.
	ctl, err := NewPolicyManager([]byte(localPolicies[0]), scanUsers, nodes[:4].ViewSlice(), WithLocalNodeChanges(true))
	require.NoError(t, err)

	ctlPrev, ctlGen := ctl.BuildPeerMapGen(nodes[:4].ViewSlice())

	changed, err := ctl.SetNodes(nodes.ViewSlice())
	require.NoError(t, err)
	require.False(t, changed, "a join under allow-all is local")

	_, _, ok := ctl.IncrementalPeerMap(nodes.ViewSlice(), ctlPrev, ctlGen, joined, nil)
	require.True(t, ok, "control")

	pm, err := NewPolicyManager([]byte(localPolicies[0]), scanUsers, nodes[:4].ViewSlice(), WithLocalNodeChanges(true))
	require.NoError(t, err)

	prev, gen := pm.BuildPeerMapGen(nodes[:4].ViewSlice())

	_, err = pm.SetPolicy([]byte(localPolicies[2]))
	require.NoError(t, err)

	_, err = pm.SetNodes(nodes.ViewSlice())
	require.NoError(t, err)

	_, _, ok = pm.IncrementalPeerMap(nodes.ViewSlice(), prev, gen, joined, nil)
	require.False(t, ok, "the peer map was built under another policy")
}
