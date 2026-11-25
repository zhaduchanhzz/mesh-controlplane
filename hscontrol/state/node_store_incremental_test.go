package state

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	policyv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/policy/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"pgregory.net/rapid"
	"tailscale.com/tailcfg"
	"tailscale.com/types/views"
)

// P00d M01 at the NodeStore level: a real NodeStore with the incremental
// path on, wired to a real PolicyManager as State does, driven by random
// joins, removals (alone and in concurrent bursts that share a batch),
// payload, tag, route and online writes, and policy swaps (including a
// join between SetPolicy and RebuildPeerMaps, R4). After every step the
// adjacency must equal a full build by a fresh PolicyManager over the same
// nodes and the current policy, and no earlier snapshot's visible adjacency
// may have moved (the patch appends to shared slices).

const (
	incPolicyAllowAll = `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`
	incPolicyHosts    = `{"hosts":{"lan":"100.64.0.0/27"},"acls":[{"action":"accept","src":["lan"],"dst":["lan:*"]}]}`
	incPolicyEmpty    = `{}`
)

var incPolicies = []string{
	incPolicyAllowAll, incPolicyHosts, policyGlobal, policyAutogroupSelf, policyVia, incPolicyEmpty,
}

type incHarness struct {
	store   *NodeStore
	pm      *policyv2.PolicyManager
	users   []types.User
	tenant  bool
	pol     string
	nextID  types.NodeID
	nextIP  byte
	builds  atomic.Int64
	patches atomic.Int64
}

func newIncHarness(t fatalfer, pol string, tenant bool, n int) *incHarness {
	h := &incHarness{
		users:  []types.User{{ID: 1, Name: "u1"}, {ID: 2, Name: "u2"}},
		tenant: tenant,
		pol:    pol,
		nextID: 1,
		nextIP: 1,
	}

	nodes := make(types.Nodes, 0, n)
	for range n {
		nd := h.newNode(int(h.nextID) % 2) //nolint:gosec
		nodes = append(nodes, &nd)
	}

	pm, err := policyv2.NewPolicyManager([]byte(pol), h.users, nodes.ViewSlice(),
		policyv2.WithEmptyPolicyDenies(tenant), policyv2.WithLocalNodeChanges(true))
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	h.pm = pm

	inc := policyIncrementalPeers(pm)
	h.store = NewNodeStore(nodes, policyPeersFunc(pm), TestBatchSize, TestBatchTimeout)
	h.store.SetIncrementalPeers(func(
		all []types.NodeView, prev map[types.NodeID][]types.NodeID, gen uint64, joined, removed []types.NodeID, try bool,
	) (map[types.NodeID][]types.NodeID, uint64, bool) {
		peers, g, ok := inc(all, prev, gen, joined, removed, try)

		h.builds.Add(1)

		if ok {
			h.patches.Add(1)
		}

		return peers, g, ok
	})
	h.store.Start()

	return h
}

func (h *incHarness) newNode(user int) types.Node {
	id := h.nextID
	h.nextID++

	n := createTestNode(id, h.users[user].ID, h.users[user].Name, fmt.Sprintf("n%d", id))
	ip := netip.AddrFrom4([4]byte{100, 64, 0, h.nextIP})
	h.nextIP++
	n.IPv4, n.IPv6 = &ip, nil
	n.User = &h.users[user]

	return n
}

// sync does what State.updatePolicyManagerNodes does after a write.
func (h *incHarness) sync(t fatalfer) {
	changed, err := h.pm.SetNodes(h.store.ListNodes())
	if err != nil {
		t.Fatalf("SetNodes: %v", err)
	}

	if changed {
		h.store.RebuildPeerMaps()
	}
}

func (h *incHarness) check(t fatalfer, when string) {
	snap := h.store.data.Load()
	nodes := views.SliceOf(snap.allNodes)

	fresh, err := policyv2.NewPolicyManager([]byte(h.pol), h.users, nodes, policyv2.WithEmptyPolicyDenies(h.tenant))
	if err != nil {
		t.Fatalf("fresh policy: %v", err)
	}

	want := fresh.BuildPeerMap(nodes)

	for id := range snap.nodesByID {
		got := slices.Sorted(slices.Values(snap.peersByNode[id]))
		exp := slices.Sorted(slices.Values(want[id]))

		for _, p := range got {
			if !slices.Contains(exp, p) {
				t.Fatalf("ISOLATION %s: node %d lists %d; full build %v\npolicy %s", when, id, p, exp, h.pol)
			}
		}

		if !slices.Equal(got, exp) {
			t.Fatalf("%s: node %d: adjacency %v, full build %v\npolicy %s", when, id, got, exp, h.pol)
		}
	}

	for id, ps := range snap.peersByNode {
		if _, ok := snap.nodesByID[id]; !ok && len(ps) > 0 {
			t.Fatalf("%s: adjacency for removed node %d", when, id)
		}
	}
}

func deepPeers(m map[types.NodeID][]types.NodeID) map[types.NodeID][]types.NodeID {
	out := make(map[types.NodeID][]types.NodeID, len(m))
	for id, ps := range m {
		out[id] = slices.Clone(ps)
	}

	return out
}

func TestNodeStoreIncrementalAdjacencyMatchesFullBuild(t *testing.T) {
	subnet := netip.MustParsePrefix("10.33.0.0/24")

	var builds, patches atomic.Int64

	for _, start := range incPolicies {
		for _, tenant := range []bool{false, true} {
			t.Run(fmt.Sprintf("%.24s/tenant=%t", start, tenant), func(t *testing.T) {
				rapid.Check(t, func(rt *rapid.T) {
					h := newIncHarness(rt, start, tenant, rapid.IntRange(0, 6).Draw(rt, "initial"))
					defer h.store.Stop()

					ids := func() []types.NodeID {
						return slices.Sorted(maps.Keys(h.store.data.Load().nodesByID))
					}

					pick := func(label string) (types.NodeID, bool) {
						cur := ids()
						if len(cur) == 0 {
							return 0, false
						}

						return rapid.SampledFrom(cur).Draw(rt, label), true
					}

					for step := range rapid.IntRange(1, 25).Draw(rt, "steps") {
						old := h.store.data.Load()
						oldPeers := deepPeers(old.peersByNode)

						op := rapid.IntRange(0, 9).Draw(rt, "op")
						switch op {
						case 0, 1: // join
							n := h.newNode(rapid.IntRange(0, 1).Draw(rt, "user"))
							if rapid.IntRange(0, 4).Draw(rt, "joinTagged") == 0 {
								n.Tags, n.UserID, n.User = []string{"tag:srv"}, nil, nil
							}

							if rapid.IntRange(0, 5).Draw(rt, "joinRouter") == 0 {
								n.Hostinfo = &tailcfg.Hostinfo{RoutableIPs: []netip.Prefix{subnet}}
								n.ApprovedRoutes = []netip.Prefix{subnet}
							}

							h.store.PutNode(n)
						case 2: // delete
							if id, ok := pick("del"); ok {
								h.store.DeleteNode(id)
							}
						case 3: // burst: joins and deletes queued together
							var wg sync.WaitGroup

							for range rapid.IntRange(1, 4).Draw(rt, "burstJoins") {
								n := h.newNode(rapid.IntRange(0, 1).Draw(rt, "burstUser"))

								wg.Go(func() { h.store.PutNode(n) })
							}

							if id, ok := pick("burstDel"); ok && rapid.Bool().Draw(rt, "burstDelete") {
								wg.Go(func() { h.store.DeleteNode(id) })
							}

							// A relation write in the same batch: never local.
							if id, ok := pick("burstTag"); ok && rapid.Bool().Draw(rt, "burstRelation") {
								wg.Go(func() {
									h.store.UpdateNode(id, func(n *types.Node) {
										n.Tags, n.UserID, n.User = []string{"tag:srv"}, nil, nil
									})
								})
							}

							wg.Wait()
						case 4: // payload only
							if id, ok := pick("payload"); ok {
								h.store.UpdateNode(id, func(n *types.Node) { n.LastSeen = new(time.Now()) })
							}
						case 5: // tag
							if id, ok := pick("tag"); ok {
								h.store.UpdateNode(id, func(n *types.Node) {
									n.Tags, n.UserID, n.User = []string{"tag:srv"}, nil, nil
								})
							}
						case 6: // routes
							if id, ok := pick("route"); ok {
								h.store.UpdateNode(id, func(n *types.Node) {
									n.Hostinfo = &tailcfg.Hostinfo{RoutableIPs: []netip.Prefix{subnet}}
									n.ApprovedRoutes = []netip.Prefix{subnet}
								})
							}
						case 7: // online flip
							if id, ok := pick("online"); ok {
								h.store.UpdateNode(id, func(n *types.Node) { n.IsOnline = new(!n.Online()) })
							}
						case 8: // policy swap as State.SetPolicy does
							h.pol = rapid.SampledFrom(incPolicies).Draw(rt, "policy")
							if _, err := h.pm.SetPolicy([]byte(h.pol)); err != nil {
								rt.Fatalf("SetPolicy: %v", err)
							}

							h.store.RebuildPeerMaps()
						case 9: // R4: a join lands between SetPolicy and the rebuild
							h.pol = rapid.SampledFrom(incPolicies).Draw(rt, "policyR4")
							if _, err := h.pm.SetPolicy([]byte(h.pol)); err != nil {
								rt.Fatalf("SetPolicy: %v", err)
							}

							h.store.PutNode(h.newNode(rapid.IntRange(0, 1).Draw(rt, "r4User")))
							h.check(rt, fmt.Sprintf("step %d R4 before rebuild", step))
							h.store.RebuildPeerMaps()
						}

						// The write's own snapshot first, then after the
						// caller's policy sync, as in the existing gate.
						h.check(rt, fmt.Sprintf("step %d op %d", step, op))
						h.sync(rt)
						h.check(rt, fmt.Sprintf("step %d op %d after sync", step, op))

						for id, ps := range oldPeers {
							if !slices.Equal(old.peersByNode[id], ps) {
								rt.Fatalf("step %d: an earlier snapshot's adjacency of %d moved: %v -> %v",
									step, id, ps, old.peersByNode[id])
							}
						}
					}

					builds.Add(h.builds.Load())
					patches.Add(h.patches.Load())
				})
			})
		}
	}

	t.Logf("relation builds %d, incremental %d", builds.Load(), patches.Load())
	require.Positive(t, patches.Load(), "the incremental path must have run")
	require.Less(t, patches.Load(), builds.Load(), "and the full build too")
}

// TestNodeStoreIncrementalOffKeepsFullBuild: without SetIncrementalPeers the
// NodeStore never patches, whatever the policy manager allows (M05 at the
// NodeStore level).
func TestNodeStoreIncrementalOffKeepsFullBuild(t *testing.T) {
	store, pm := nodeStoreWithPolicy(t, incPolicyAllowAll, []types.User{{ID: 1, Name: "u1"}}, nil)
	defer store.Stop()

	before := testutilCounter(t, "incremental")

	for i := 1; i <= 5; i++ {
		n := createTestNode(types.NodeID(i), 1, "u1", fmt.Sprintf("n%d", i)) //nolint:gosec
		store.PutNode(n)
		syncPolicy(t, store, pm)
	}

	require.Equal(t, before, testutilCounter(t, "incremental"), "off: no incremental build")
	require.Zero(t, store.data.Load().rulesGen, "off: snapshots carry no rules generation")
}

func testutilCounter(t *testing.T, label string) float64 {
	t.Helper()

	var m dto.Metric
	require.NoError(t, nodeStoreSnapshotBuilds.WithLabelValues(label).Write(&m))

	return m.GetCounter().GetValue()
}

// TestNodeStoreIncrementalPolicyChangeAlwaysFull is M02 (I05): a policy
// swap, a user list change and an explicit rebuild always build in full,
// and a join right after one patches only once the rules are settled.
func TestNodeStoreIncrementalPolicyChangeAlwaysFull(t *testing.T) {
	h := newIncHarness(t, incPolicyAllowAll, true, 3)
	defer h.store.Stop()

	type counts struct{ builds, patches int64 }

	now := func() counts { return counts{h.builds.Load(), h.patches.Load()} }
	delta := func(before counts) counts {
		after := now()

		return counts{after.builds - before.builds, after.patches - before.patches}
	}

	// Warm up: the first relation build after construction is full (the
	// initial snapshot carries no rules generation).
	h.store.PutNode(h.newNode(0))
	h.sync(t)

	before := now()

	h.store.PutNode(h.newNode(1))
	h.sync(t)
	require.Equal(t, counts{1, 1}, delta(before), "a join under allow-all patches")
	h.check(t, "join")

	before = now()
	_, err := h.pm.SetPolicy([]byte(incPolicyHosts))
	require.NoError(t, err)
	h.store.RebuildPeerMaps()
	h.pol = incPolicyHosts

	require.Equal(t, counts{1, 0}, delta(before), "a policy swap builds in full")
	h.check(t, "policy")

	before = now()

	h.store.PutNode(h.newNode(0))
	h.sync(t)
	require.Equal(t, counts{1, 1}, delta(before), "a join under the new, local policy patches")
	h.check(t, "join after policy")

	before = now()

	users := append(slices.Clone(h.users), types.User{ID: 3, Name: "u3"})
	_, _, err = h.pm.SetUsers(users)
	require.NoError(t, err)

	h.users = users
	h.store.RebuildPeerMaps()
	require.Equal(t, counts{1, 0}, delta(before), "a user change builds in full")

	before = now()

	h.store.RebuildPeerMaps()
	require.Equal(t, counts{1, 0}, delta(before), "an explicit rebuild builds in full")
	h.check(t, "rebuild")
}

// TestNodeStoreReconcileReplacesDivergence is M03 at the NodeStore level:
// an injected extra and missing peer are found, counted, and replaced by
// the full build; a policy change since the build skips the comparison.
func TestNodeStoreReconcileReplacesDivergence(t *testing.T) {
	h := newIncHarness(t, incPolicyHosts, true, 6)
	defer h.store.Stop()

	h.store.fullPeers = func(all []types.NodeView) (map[types.NodeID][]types.NodeID, uint64) {
		return h.pm.BuildPeerMapGen(views.SliceOf(all))
	}

	// A first relation build records the rules generation.
	h.store.PutNode(h.newNode(0))
	h.sync(t)

	res := h.store.ReconcilePeerMaps()
	require.True(t, res.Checked)
	require.Empty(t, res.Diverged)

	snap := *h.store.data.Load()
	peers := deepPeers(snap.peersByNode)

	var a, b types.NodeID

	for id, ps := range peers {
		if len(ps) >= 2 {
			a, b = id, ps[0]

			break
		}
	}

	require.NotZero(t, a, "precondition: a node with peers")

	outsider := h.newNode(0)
	outsider.ID = 999
	peers[a] = append(without(peers[a], b), outsider.ID) // b missing, 999 extra
	snap.peersByNode = peers
	h.store.data.Store(&snap)

	res = h.store.ReconcilePeerMaps()
	require.True(t, res.Checked)
	require.Contains(t, res.Diverged, a)
	require.Equal(t, 1, res.Extra)
	require.Equal(t, 1, res.Missing)
	h.check(t, "after reconcile")

	// Rules moved since the build: nothing to compare against.
	_, err := h.pm.SetPolicy([]byte(incPolicyAllowAll))
	require.NoError(t, err)

	res = h.store.ReconcilePeerMaps()
	require.False(t, res.Checked)
}

func without(ps []types.NodeID, id types.NodeID) []types.NodeID {
	return slices.DeleteFunc(slices.Clone(ps), func(p types.NodeID) bool { return p == id })
}
