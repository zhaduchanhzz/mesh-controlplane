//go:build p05cluster

// P07: map responses and events stay in their tailnet (I07, S12), with the
// incremental netmap (P00d) off and on, including P00d's M01/M03 across two
// tenants. Every check of the other tenant is "no frame at all" over a
// quiet window plus "nothing of the other tenant in the netmap".

package servertest_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

func p07Modes(t *testing.T, fn func(t *testing.T, tc *servertest.TestCluster)) {
	for _, inc := range []bool{false, true} {
		t.Run(fmt.Sprintf("incremental=%t", inc), func(t *testing.T) {
			tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{Netmap: servertest.NetmapModeOf(inc)},
				servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
			tc.SetPolicy(t, tenantA, allowAll)
			tc.SetPolicy(t, tenantB, allowAll)
			fn(t, tc)
		})
	}
}

func clientByName(cs []*servertest.TestClient, name string) *servertest.TestClient {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}

	return nil
}

// peerIDs is a client's peers as node IDs.
func peerIDs(nm *netmap.NetworkMap) []tailcfg.NodeID {
	var out []tailcfg.NodeID
	for _, p := range nm.Peers {
		out = append(out, p.ID())
	}

	slices.Sort(out)

	return out
}

// serverPeerIDs is what the client's runtime says its peers are.
func serverPeerIDs(srv *servertest.TestServer, c *servertest.TestClient) []tailcfg.NodeID {
	nm := c.Netmap()
	if nm == nil || !nm.SelfNode.Valid() {
		return nil
	}

	var out []tailcfg.NodeID
	for _, p := range srv.State().ListPeers(types.NodeID(nm.SelfNode.ID())).All() { //nolint:gosec
		out = append(out, p.ID().NodeID())
	}

	slices.Sort(out)

	return out
}

// waitConsistent waits until every client of srv holds exactly the peers
// its runtime gives it.
func waitConsistent(t *testing.T, srv *servertest.TestServer, cs []*servertest.TestClient, when string) {
	t.Helper()

	for _, c := range cs {
		c.WaitForCondition(t, when+": peers as the server has them", clusterWait, func(nm *netmap.NetworkMap) bool {
			return slices.Equal(peerIDs(nm), serverPeerIDs(srv, c))
		})
	}
}

func deleteNode(t *testing.T, tc *servertest.TestCluster, slug string, c *servertest.TestClient) {
	t.Helper()

	srv := tc.Tenant(t, slug)
	nv, ok := srv.State().GetNodeByNodeKey(selfKey(t, c))
	require.True(t, ok)

	cs, err := srv.State().DeleteNode(nv)
	require.NoError(t, err)
	tc.Runtime(t, slug).Change(cs...)
}

// S12 / I07: everything a node of A does (map request updates, going
// offline and back, joining, being deleted) reaches A and sends B nothing.
func TestClusterP07NodeActivityInASilentInB(t *testing.T) {
	p07Modes(t, func(t *testing.T, tc *servertest.TestCluster) {
		a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
		aClients := enroll(t, a, "alice", "p07-ta-n1", "p07-ta-n2", "p07-ta-n3")
		bClients := enroll(t, b, "bob", "p07-tb-n1", "p07-tb-n2")

		for _, group := range [][]*servertest.TestClient{aClients, bClients} {
			for _, c := range group {
				waitTenantMesh(t, c, group, false)
			}
		}

		a1, a2, a3 := aClients[0], aClients[1], aClients[2]
		before := settledFrames(t, bClients)

		// Hostinfo by map request.
		route := netip.MustParsePrefix("10.77.0.0/24")
		a1.Direct().SetHostinfo(&tailcfg.Hostinfo{BackendLogID: "servertest-a1", Hostname: "p07-ta-n1", OS: "linux-p07", RoutableIPs: []netip.Prefix{route}})
		require.NoError(t, sendUpdate(a1), "hostinfo map request")
		require.Eventually(t, func() bool {
			nv, ok := a.State().GetNodeByNodeKey(selfKey(t, a1))

			return ok && nv.Hostinfo().Valid() && nv.Hostinfo().OS() == "linux-p07"
		}, clusterWait, 100*time.Millisecond, "A's runtime records a1's new hostinfo")
		assertQuiet(t, bClients, before, "a hostinfo update in A")

		// Endpoints by map request.
		ep := netip.MustParseAddrPort("192.0.2.77:41641")
		a1.Direct().SetEndpoints([]tailcfg.Endpoint{{Addr: ep, Type: tailcfg.EndpointExplicitConf}})
		require.NoError(t, sendUpdate(a1), "endpoint map request")
		a2.WaitForCondition(t, "a2 sees a1's endpoint", clusterWait, func(nm *netmap.NetworkMap) bool {
			for _, p := range nm.Peers {
				if slices.Contains(p.Endpoints().AsSlice(), ep) {
					return true
				}
			}

			return false
		})
		assertQuiet(t, bClients, before, "an endpoint update in A")

		// Offline and back.
		a3.Disconnect(t)
		a2.WaitForCondition(t, "a2 sees a3 offline", clusterWait, func(nm *netmap.NetworkMap) bool {
			p, ok := a2.PeerByName("p07-ta-n3")

			return ok && p.Online().Valid() && !p.Online().Get()
		})
		a3.Reconnect(t)
		a2.WaitForCondition(t, "a2 sees a3 online", clusterWait, func(nm *netmap.NetworkMap) bool {
			p, ok := a2.PeerByName("p07-ta-n3")

			return ok && p.Online().Valid() && p.Online().Get()
		})
		assertQuiet(t, bClients, before, "a node of A going offline and back")

		// Join.
		a4 := enroll(t, a, "alice-a4", "p07-ta-n4")[0]
		a1.WaitForCondition(t, "a1 sees a4", clusterWait, func(nm *netmap.NetworkMap) bool {
			_, ok := a1.PeerByName("p07-ta-n4")

			return ok
		})
		assertQuiet(t, bClients, before, "a join in A")

		// Delete.
		deleteNode(t, tc, tenantA, a4)
		a1.WaitForCondition(t, "a4 gone from a1", clusterWait, func(nm *netmap.NetworkMap) bool {
			_, ok := a1.PeerByName("p07-ta-n4")

			return !ok
		})
		assertQuiet(t, bClients, before, "a delete in A")

		waitConsistent(t, a, []*servertest.TestClient{a1, a2, a3}, "A after its churn")
		waitConsistent(t, b, bClients, "B after A's churn")

		aTokens := tenantTokens(t, a, tenantA, "alice")
		bTokens := tenantTokens(t, b, tenantB, "bob")
		assertTenantView(t, b, bClients, exclusive(aTokens, bTokens))
	})
}

// A node of A reconnecting over and over while B streams: B gets nothing,
// A converges.
func TestClusterP07ReconnectStormInASilentInB(t *testing.T) {
	p07Modes(t, func(t *testing.T, tc *servertest.TestCluster) {
		a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
		aClients := enroll(t, a, "alice", "p07-ta-n1", "p07-ta-n2")
		bClients := enroll(t, b, "bob", "p07-tb-n1", "p07-tb-n2")

		for _, group := range [][]*servertest.TestClient{aClients, bClients} {
			for _, c := range group {
				waitTenantMesh(t, c, group, false)
			}
		}

		before := settledFrames(t, bClients)

		for range 20 {
			aClients[0].Disconnect(t)
			aClients[0].Reconnect(t)
		}

		aClients[1].WaitForCondition(t, "a2 sees a1 online after the storm", clusterWait, func(nm *netmap.NetworkMap) bool {
			p, ok := aClients[1].PeerByName("p07-ta-n1")

			return ok && p.Online().Valid() && p.Online().Get()
		})
		assertQuiet(t, bClients, before, "20 reconnects of a node of A")
		waitConsistent(t, a, aClients, "A after the storm")
	})
}

// P00d M01 across tenants: a seeded random sequence of joins, deletes and
// hostinfo updates in A and B with the incremental netmap on; after every
// step each client holds exactly its runtime's peers, nothing of the other
// tenant, and the tenant not acted on received nothing.
func TestClusterP07IncrementalAcrossTenants(t *testing.T) {
	seed := uint64(time.Now().UnixNano()) //nolint:gosec
	if v := os.Getenv("P07_SEED"); v != "" {
		var err error
		seed, err = strconv.ParseUint(v, 10, 64)
		require.NoError(t, err)
	}
	t.Logf("seed %d", seed)

	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15)) //nolint:gosec // test

	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{Netmap: servertest.NetmapIncremental},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	srv := map[string]*servertest.TestServer{tenantA: tc.Tenant(t, tenantA), tenantB: tc.Tenant(t, tenantB)}
	clients := map[string][]*servertest.TestClient{
		tenantA: enroll(t, srv[tenantA], "alice", "p07-tenant-a-node-0", "p07-tenant-a-node-1"),
		tenantB: enroll(t, srv[tenantB], "bob", "p07-tenant-b-node-0", "p07-tenant-b-node-1"),
	}
	other := map[string]string{tenantA: tenantB, tenantB: tenantA}
	next := 2

	for slug := range clients {
		waitConsistent(t, srv[slug], clients[slug], "start")
	}

	for step := range 12 {
		slug := []string{tenantA, tenantB}[rng.IntN(2)]
		quiet := clients[other[slug]]
		before := settledFrames(t, quiet)
		hist := histories(quiet)

		var what string

		switch op := rng.IntN(3); {
		case op == 0 || len(clients[slug]) < 2: // join
			// Long names: the token check looks for substrings.
			name := fmt.Sprintf("p07-%s-node-%d", slug, next)
			next++
			clients[slug] = append(clients[slug], enroll(t, srv[slug], "user-"+name, name)...)
			what = "join " + name
		case op == 1: // delete
			i := rng.IntN(len(clients[slug]))
			c := clients[slug][i]
			deleteNode(t, tc, slug, c)
			c.Disconnect(t)
			clients[slug] = slices.Delete(clients[slug], i, i+1)
			what = "delete " + c.Name
		default: // hostinfo
			c := clients[slug][rng.IntN(len(clients[slug]))]
			c.Direct().SetHostinfo(&tailcfg.Hostinfo{BackendLogID: "servertest-" + c.Name, Hostname: c.Name, OS: fmt.Sprintf("os-%d", step)})
			require.NoError(t, sendUpdate(c), "hostinfo map request")
			what = "hostinfo " + c.Name
		}

		when := fmt.Sprintf("step %d (%s in %s)", step, what, slug)
		t.Log(when)
		waitConsistent(t, srv[slug], clients[slug], when)
		assertQuiet(t, quiet, before, when)
		explainFrames(t, quiet, hist, when)
		waitConsistent(t, srv[other[slug]], quiet, when+", other tenant")
	}

	aTokens := tenantTokens(t, srv[tenantA], tenantA, "alice")
	bTokens := tenantTokens(t, srv[tenantB], tenantB, "bob")
	assertTenantView(t, srv[tenantA], clients[tenantA], exclusive(bTokens, aTokens))
	assertTenantView(t, srv[tenantB], clients[tenantB], exclusive(aTokens, bTokens))
}

// P00d M03 across tenants: a divergence injected into A's adjacency is
// found by A's reconciliation, repaired, and sent to A's nodes only.
func TestClusterP07ReconcileStaysInTailnet(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{Netmap: servertest.NetmapIncremental},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	aClients := enroll(t, a, "alice", "p07-ta-n1", "p07-ta-n2", "p07-ta-n3")
	bClients := enroll(t, b, "bob", "p07-tb-n1", "p07-tb-n2")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, false)
		}
	}

	id := func(c *servertest.TestClient) types.NodeID { return types.NodeID(c.Netmap().SelfNode.ID()) } //nolint:gosec
	a1, a2 := id(aClients[0]), id(aClients[1])

	// Both runtimes equal before any fault.
	require.Empty(t, tc.Runtime(t, tenantA).ReconcileNetmap())
	require.Empty(t, tc.Runtime(t, tenantB).ReconcileNetmap())

	before := settledFrames(t, bClients)

	a.State().MutatePeerMapForTest(func(m map[types.NodeID][]types.NodeID) {
		m[a1] = slices.DeleteFunc(slices.Clone(m[a1]), func(p types.NodeID) bool { return p == a2 })
		m[a2] = slices.DeleteFunc(slices.Clone(m[a2]), func(p types.NodeID) bool { return p == a1 })
	})

	require.ElementsMatch(t, []types.NodeID{a1, a2}, tc.Runtime(t, tenantA).ReconcileNetmap())
	require.Empty(t, tc.Runtime(t, tenantB).ReconcileNetmap(), "B's runtime has nothing to repair")

	waitConsistent(t, a, aClients, "A after the repair")
	assertQuiet(t, bClients, before, "a reconciliation in A")
}

func histories(cs []*servertest.TestClient) []int {
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = len(c.History())
	}

	return out
}

// explainFrames logs the netmaps quiet clients got since hist: peers by
// name and key, so a failed quiet check says whose data arrived.
func explainFrames(t *testing.T, cs []*servertest.TestClient, hist []int, when string) {
	t.Helper()

	for i, c := range cs {
		h := c.History()
		start := min(hist[i], len(h))
		for j, nm := range h[start:] {
			var prev *netmap.NetworkMap
			if start+j > 0 {
				prev = h[start+j-1]
			}

			t.Logf("%s: %s got a netmap (self=%s): %s", when, c.Name, nm.SelfNode.Hostinfo().Hostname(), netmapDelta(prev, nm))
		}
	}
}

func sendUpdate(c *servertest.TestClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return c.Direct().SendUpdate(ctx)
}

// netmapDelta says what changed in nm from prev: peers added, removed, or
// with a different online state, last seen, OS or endpoints; or "no change".
func netmapDelta(prev, nm *netmap.NetworkMap) string {
	desc := func(p tailcfg.NodeView) string {
		on := "?"
		if p.Online().Valid() {
			on = fmt.Sprint(p.Online().Get())
		}

		ls := ""
		if p.LastSeen().Valid() {
			ls = p.LastSeen().Get().Format("15:04:05.000")
		}

		return fmt.Sprintf("online=%s lastSeen=%s os=%s eps=%d", on, ls, p.Hostinfo().OS(), p.Endpoints().Len())
	}

	before := map[string]string{}
	if prev != nil {
		for _, p := range prev.Peers {
			before[p.Hostinfo().Hostname()] = desc(p)
		}
	}

	var out []string

	seen := map[string]bool{}

	for _, p := range nm.Peers {
		name := p.Hostinfo().Hostname()
		seen[name] = true

		switch d, ok := before[name]; {
		case !ok:
			out = append(out, "+"+name+" "+desc(p))
		case d != desc(p):
			out = append(out, "~"+name+" "+d+" -> "+desc(p))
		}
	}

	for name := range before {
		if !seen[name] {
			out = append(out, "-"+name)
		}
	}

	if len(out) == 0 {
		return "no peer change"
	}

	return fmt.Sprint(out)
}
