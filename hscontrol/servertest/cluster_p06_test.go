//go:build p05cluster

// P06 cross-tailnet isolation suite (S06, S08, S09, I05, policy derpMap):
// two tailnets A and B served by ONE cluster process, real in-process
// Tailscale control clients. See evidence/source-inventory-P06.md §4 for the
// RED test plan. Same rule as cluster_p05_test.go: assert the security
// property (no other-tenant node, key, address, route or region in a view;
// no approval or tag granted by another tenant's policy), never an exact
// error string.

package servertest_test

import (
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
)

// --- TestClusterP06SameIPHostnameAndRoute (S06) ---

// A and B each have a "router" and a "laptop" of a user "alice": same
// hostnames, same overlay addresses (per-tailnet IPAM, same sequence), and
// both routers advertise and get approved the same subnet 10.33.0.0/24. B
// has a third node, so B holds an address and a hostname A does not.
// Every client sees only its own tailnet: peers, the router behind
// 10.33.0.0/24 (AllowedIPs and PrimaryRoutes), MagicDNS names, packet
// filter.
func TestClusterP06SameIPHostnameAndRoute(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	subnet := netip.MustParsePrefix("10.33.0.0/24")

	aClients := enroll(t, a, "alice", "router", "laptop")
	bClients := enroll(t, b, "alice", "router", "laptop", "b-extra")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, false)
		}
	}

	aRouter, aLaptop := aClients[0], aClients[1]
	bRouter, bLaptop := bClients[0], bClients[1]

	require.Equal(t, aRouter.Netmap().SelfNode.Addresses().AsSlice(), bRouter.Netmap().SelfNode.Addresses().AsSlice(),
		"precondition: A's and B's routers share their overlay addresses")
	require.Equal(t, aLaptop.Netmap().SelfNode.Addresses().AsSlice(), bLaptop.Netmap().SelfNode.Addresses().AsSlice(),
		"precondition: A's and B's laptops share their overlay addresses")

	for _, side := range []struct {
		srv    *servertest.TestServer
		slug   string
		router *servertest.TestClient
	}{{a, tenantA, aRouter}, {b, tenantB, bRouter}} {
		advertiseRoutes(t, side.router, []netip.Prefix{subnet})

		_, c, err := side.srv.State().SetApprovedRoutes(findNodeID(t, side.srv, "router"), []netip.Prefix{subnet})
		require.NoError(t, err)
		tc.Runtime(t, side.slug).Change(c)
	}

	aRouterKey, bRouterKey := selfKey(t, aRouter), selfKey(t, bRouter)

	for _, cs := range []struct {
		clients []*servertest.TestClient
		router  key.NodePublic
	}{{aClients[1:], aRouterKey}, {bClients[1:], bRouterKey}} {
		for _, c := range cs.clients {
			c.WaitForCondition(t, "own router serves 10.33.0.0/24", clusterWait, func(nm *netmap.NetworkMap) bool {
				p, ok := p06PeerByKey(nm, cs.router)

				return ok && slices.Contains(p.AllowedIPs().AsSlice(), subnet) &&
					slices.Contains(p.PrimaryRoutes().AsSlice(), subnet)
			})
		}
	}

	bOnly := p06ForeignTokens(t, b, a)
	require.NotEmpty(t, bOnly.ips, "B must hold an address A does not, or the address check on A is vacuous")

	aOnly := p06ForeignTokens(t, a, b)

	for _, side := range []struct {
		own     *servertest.TestServer
		clients []*servertest.TestClient
		router  key.NodePublic
		foreign tokens
		label   string
	}{{a, aClients, aRouterKey, bOnly, "A"}, {b, bClients, bRouterKey, aOnly, "B"}} {
		assertTenantView(t, side.own, side.clients, side.foreign)

		for _, c := range side.clients {
			nm := c.Netmap()

			for _, p := range nm.Peers {
				if slices.Contains(p.AllowedIPs().AsSlice(), subnet) || slices.Contains(p.PrimaryRoutes().AsSlice(), subnet) {
					assert.Equal(t, side.router, p.Key(), "%s %s: 10.33.0.0/24 must route to the same-tailnet router only",
						side.label, c.Name)
				}
			}

			if c.Name == "router" {
				continue
			}

			// MagicDNS: "router" resolves to the same-tailnet router.
			p, ok := c.PeerByName("router")
			require.True(t, ok, "%s %s resolves 'router'", side.label, c.Name)
			assert.Equal(t, side.router, p.Key(), "%s %s: 'router' is its own tailnet's router", side.label, c.Name)
		}
	}

	_, ok := aLaptop.PeerByName("b-extra")
	assert.False(t, ok, "A's laptop must not resolve B's b-extra")
}

// --- TestClusterP06AutoApproversBoundedByTailnet (S08) ---

// A's policy auto-approves alice's exit node and 10.40.0.0/16; B's policy
// has no autoApprovers. Both tailnets have a user "alice" with nodes
// "exit" and "obs" (same names, same addresses). B's exit node advertising
// the same routes gets nothing approved, B's clients never see A's exit
// node, default route or subnet, and A's clients never see B's routes,
// including the exit node B's admin approves.
func TestClusterP06AutoApproversBoundedByTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)

	subnet := netip.MustParsePrefix("10.40.0.0/16")
	advertised := append(slices.Clone(tsaddr.ExitRoutes()), subnet)

	keyA := mintKey(t, a, "alice")
	keyB := mintKey(t, b, "alice")

	tc.SetPolicy(t, tenantA, `{
		"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
		"autoApprovers": {"routes": {"10.40.0.0/16": ["alice@"]}, "exitNode": ["alice@"]}
	}`)
	tc.SetPolicy(t, tenantB, allowAll)

	newPair := func(s *servertest.TestServer, k string) []*servertest.TestClient {
		return []*servertest.TestClient{
			servertest.NewClusterClient(t, s, "exit", servertest.ClusterClientOptions{AuthKey: k}),
			servertest.NewClusterClient(t, s, "obs", servertest.ClusterClientOptions{AuthKey: k}),
		}
	}

	aClients, bClients := newPair(a, keyA), newPair(b, keyB)

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, false)
		}
	}

	aExit, aObs := aClients[0], aClients[1]
	bExit, bObs := bClients[0], bClients[1]
	aExitKey, bExitKey := selfKey(t, aExit), selfKey(t, bExit)

	require.Equal(t, aExit.Netmap().SelfNode.Addresses().AsSlice(), bExit.Netmap().SelfNode.Addresses().AsSlice(),
		"precondition: both exit nodes share their overlay addresses")

	approvedIn := func(s *servertest.TestServer, nk key.NodePublic) []netip.Prefix {
		nv, ok := s.State().GetNodeByNodeKey(nk)
		require.True(t, ok)

		return nv.ApprovedRoutes().AsSlice()
	}

	// Positive control: A's own policy approves A's announcement.
	advertiseRoutes(t, aExit, advertised)
	require.Eventually(t, func() bool {
		return len(approvedIn(a, aExitKey)) == len(advertised)
	}, clusterWait, 50*time.Millisecond, "positive control: A's autoApprovers approve A's exit node and subnet")
	aObs.WaitForCondition(t, "A's observer sees A's exit node and subnet", clusterWait, func(nm *netmap.NetworkMap) bool {
		p, ok := p06PeerByKey(nm, aExitKey)

		return ok && tsaddr.ContainsExitRoutes(p.AllowedIPs()) && slices.Contains(p.AllowedIPs().AsSlice(), subnet)
	})

	// B announces the same routes; B's policy approves nothing.
	advertiseRoutes(t, bExit, advertised)

	nv, ok := b.State().GetNodeByNodeKey(bExitKey)
	require.True(t, ok)
	require.ElementsMatch(t, advertised, nv.AnnouncedRoutes(), "precondition: B stored the announcement")

	assert.Never(t, func() bool { return len(approvedIn(b, bExitKey)) > 0 }, quietWindow, 100*time.Millisecond,
		"B's routes must not be auto-approved by A's autoApprovers")
	assert.Empty(t, approvedIn(b, bExitKey), "B's exit node has no approved route")

	_, ok = a.State().GetNodeByNodeKey(bExitKey)
	assert.False(t, ok, "A's state does not know B's exit node")

	aKeys := p06NodeKeys(a)
	bKeys := p06NodeKeys(b)

	noForeignRoutes := func(t *testing.T, c *servertest.TestClient, foreign []key.NodePublic, allowedExit *key.NodePublic) {
		t.Helper()

		nm := c.Netmap()
		for _, p := range nm.Peers {
			assert.NotContains(t, foreign, p.Key(), "%s sees another tailnet's node", c.Name)

			if allowedExit != nil && p.Key() == *allowedExit {
				continue
			}

			assert.False(t, tsaddr.ContainsExitRoutes(p.AllowedIPs()), "%s: peer %s offers a default route", c.Name, p.Key().ShortString())

			for _, r := range advertised {
				assert.NotContains(t, p.AllowedIPs().AsSlice(), r, "%s: peer %s carries %s", c.Name, p.Key().ShortString(), r)
				assert.NotContains(t, p.PrimaryRoutes().AsSlice(), r, "%s: peer %s is primary for %s", c.Name, p.Key().ShortString(), r)
			}
		}

		assert.False(t, tsaddr.ContainsExitRoutes(nm.SelfNode.AllowedIPs()), "%s: self must not be offered as exit node", c.Name)
		assertNoTokens(t, c.Name+" netmap", nm, p06KeyStrings(foreign))
	}

	t.Run("B_sees_no_route_of_A_and_none_of_its_own", func(t *testing.T) {
		settledFrames(t, bClients)

		for _, c := range bClients {
			noForeignRoutes(t, c, aKeys, nil)
		}
	})

	t.Run("A_sees_no_announcement_of_B", func(t *testing.T) {
		for _, c := range aClients {
			for _, p := range c.Netmap().Peers {
				assert.NotContains(t, bKeys, p.Key(), "%s sees a node of B", c.Name)
			}

			assertNoTokens(t, c.Name+" netmap", c.Netmap(), p06KeyStrings(bKeys))
		}

		p, ok := p06PeerByKey(aObs.Netmap(), aExitKey)
		require.True(t, ok)
		assert.True(t, tsaddr.ContainsExitRoutes(p.AllowedIPs()), "A's observer still uses A's exit node")
	})

	t.Run("B_admin_approved_exit_node_is_invisible_to_A", func(t *testing.T) {
		before := settledFrames(t, aClients)

		_, c, err := b.State().SetApprovedRoutes(findNodeID(t, b, "exit"), tsaddr.ExitRoutes())
		require.NoError(t, err)
		tc.Runtime(t, tenantB).Change(c)

		bObs.WaitForCondition(t, "positive control: B's observer sees B's approved exit node", clusterWait,
			peerOffersExit("exit"))

		assertQuiet(t, aClients, before, "approving an exit node in B")

		p, ok := p06PeerByKey(aObs.Netmap(), aExitKey)
		require.True(t, ok)
		assert.True(t, tsaddr.ContainsExitRoutes(p.AllowedIPs()), "A's observer's exit node is A's")
		assert.True(t, peerKeysEqual(aObs.Netmap(), aExitKey), "A's observer sees only A's exit node")

		for _, c := range aClients {
			assertNoTokens(t, c.Name+" netmap", c.Netmap(), p06KeyStrings(bKeys))
		}

		// B's observer: B's exit node only, and still no subnet nor anything of A.
		noForeignRoutes(t, bObs, aKeys, &bExitKey)

		bp, ok := p06PeerByKey(bObs.Netmap(), bExitKey)
		require.True(t, ok)
		assert.NotContains(t, bp.AllowedIPs().AsSlice(), subnet, "B never approved 10.40.0.0/16")
		assert.ElementsMatch(t, tsaddr.ExitRoutes(), approvedIn(b, bExitKey))
	})
}

// --- TestClusterP06TagOwnersBoundedByTailnet (S09) ---

// A and B both use tag:srv. A's tagOwners grant it to alice; B's grant it
// to carol only, though B also has a user named alice. Both policies grant
// tag:srv → *. B's alice cannot obtain tag:srv (policy check and a real
// registration requesting it), and A's tag:srv node reaches nothing of B.
func TestClusterP06TagOwnersBoundedByTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)

	aliceA := a.CreateUser(t, "alice")
	aliceB := b.CreateUser(t, "alice")
	carolB := b.CreateUser(t, "carol")

	const srvRule = `"acls": [{"action": "accept", "src": ["tag:srv"], "dst": ["*:*"]}]`

	tc.SetPolicy(t, tenantA, `{"tagOwners": {"tag:srv": ["alice@"]}, `+srvRule+`}`)
	tc.SetPolicy(t, tenantB, `{"tagOwners": {"tag:srv": ["carol@"]}, `+srvRule+`}`)

	keyA := a.CreatePreAuthKey(t, types.UserID(aliceA.ID))
	keyB := b.CreatePreAuthKey(t, types.UserID(aliceB.ID))

	aSrv := servertest.NewClusterClient(t, a, "srv", servertest.ClusterClientOptions{
		AuthKey: a.CreateTaggedPreAuthKey(t, types.UserID(aliceA.ID), []string{"tag:srv"}),
	})
	aLaptop := servertest.NewClusterClient(t, a, "laptop", servertest.ClusterClientOptions{AuthKey: keyA})

	bSrv := servertest.NewClusterClient(t, b, "srv", servertest.ClusterClientOptions{
		AuthKey: b.CreateTaggedPreAuthKey(t, types.UserID(carolB.ID), []string{"tag:srv"}),
	})
	bLaptop := servertest.NewClusterClient(t, b, "laptop", servertest.ClusterClientOptions{AuthKey: keyB})
	bExtra := servertest.NewClusterClient(t, b, "b-extra", servertest.ClusterClientOptions{AuthKey: keyB})

	aClients := []*servertest.TestClient{aSrv, aLaptop}
	bClients := []*servertest.TestClient{bSrv, bLaptop, bExtra}

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, false)
		}
	}

	nodeOf := func(s *servertest.TestServer, c *servertest.TestClient) types.NodeView {
		nv, ok := s.State().GetNodeByNodeKey(selfKey(t, c))
		require.True(t, ok)

		return nv
	}

	t.Run("tag_owners_resolved_in_own_tailnet", func(t *testing.T) {
		assert.True(t, slices.Contains(nodeOf(a, aSrv).Tags().AsSlice(), "tag:srv"),
			"positive control: A's srv is tagged")
		assert.True(t, slices.Contains(nodeOf(b, bSrv).Tags().AsSlice(), "tag:srv"),
			"positive control: B's srv is tagged by B's own key")

		assert.True(t, a.State().NodeCanHaveTag(nodeOf(a, aLaptop), "tag:srv"),
			"positive control: A's alice owns tag:srv in A")
		assert.False(t, b.State().NodeCanHaveTag(nodeOf(b, bLaptop), "tag:srv"),
			"B's alice must not own tag:srv by virtue of A's tagOwners")
		assert.False(t, b.State().NodeCanHaveTag(nodeOf(b, bExtra), "tag:srv"))
		assert.Empty(t, nodeOf(b, bLaptop).Tags().AsSlice(), "B's alice's node is untagged")
	})

	t.Run("A_tag_srv_reaches_nothing_of_B", func(t *testing.T) {
		bOnly := p06ForeignTokens(t, b, a)
		require.NotEmpty(t, bOnly.ips, "B holds an address A does not")
		assertTenantView(t, a, aClients, bOnly)
		assertTenantView(t, b, bClients, p06ForeignTokens(t, a, b))

		waitPeersExactly(t, aSrv, selfKey(t, aLaptop))

		srvNode := nodeOf(a, aSrv)
		for _, pid := range a.State().ListPeers(srvNode.ID()).All() {
			_, inB := b.State().GetNodeByNodeKey(pid.NodeKey())
			assert.False(t, inB, "A's tag:srv has a server-side peer of B")
		}

		rules, _ := a.State().Filter()
		assertNoIPs(t, "A's compiled filter", rules, bOnly.ips)
	})

	// Last: these registrations add nodes.
	t.Run("register_requesting_tag_srv", func(t *testing.T) {
		noiseKey := tc.NoiseKey(t)

		register := func(k, host string) (key.NodePublic, *servertest.RawResponse, *tailcfg.RegisterResponse, error) {
			sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, key.NewMachine())
			nk := key.NewNode().Public()

			resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
				NodeKey:  nk,
				Auth:     &tailcfg.RegisterResponseAuth{AuthKey: k},
				Hostinfo: &tailcfg.Hostinfo{Hostname: host, RequestTags: []string{"tag:srv"}},
			}, nil)

			return nk, resp, rr, err
		}

		nk, resp, rr, err := register(keyA, "a-wants-srv")
		require.NoError(t, err)
		require.True(t, resp.OK() && rr.MachineAuthorized, "positive control: A's alice may request tag:srv: %d %s",
			resp.Status, resp.Body)

		nv, ok := a.State().GetNodeByNodeKey(nk)
		require.True(t, ok)
		assert.Equal(t, []string{"tag:srv"}, nv.Tags().AsSlice(), "positive control: A's node got tag:srv")

		nk, resp, rr, err = register(keyB, "b-wants-srv")
		if err == nil {
			assert.False(t, resp.OK() && rr.MachineAuthorized,
				"B's alice must not get tag:srv through A's tagOwners: %d %s", resp.Status, resp.Body)
		}

		if nv, ok := b.State().GetNodeByNodeKey(nk); ok {
			assert.Empty(t, nv.Tags().AsSlice(), "B's node requesting tag:srv must not be tagged")
		}

		_, ok = a.State().GetNodeByNodeKey(nk)
		assert.False(t, ok, "B's registration must not land in A")
	})
}

// --- TestClusterP06WildcardStaysInTailnet (I05, AGENTS rule 8) ---

// A's policy is `*` → `*:*`; B's is {"grants":[]} (deny all). A's
// wildcard covers A's nodes only: A's peers, filter and compiled filter hold
// nothing of B. B's clients have no peers and no filter.
func TestClusterP06WildcardStaysInTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, `{"grants":[]}`)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)

	aClients := enroll(t, a, "alice", "laptop", "phone")
	bClients := enroll(t, b, "alice", "laptop", "phone", "b-extra")

	for _, c := range aClients {
		waitTenantMesh(t, c, aClients, false)
	}

	settledFrames(t, bClients)

	bOnly := p06ForeignTokens(t, b, a)
	require.NotEmpty(t, bOnly.ips, "B holds an address A does not")

	assertTenantView(t, a, aClients, bOnly)

	rules, _ := a.State().Filter()
	require.NotEmpty(t, rules, "positive control: A's wildcard compiles to rules")
	assertNoIPs(t, "A's compiled filter", rules, bOnly.ips)

	for _, c := range bClients {
		nm := c.Netmap()
		assert.Empty(t, nm.Peers, "%s: B's deny-all must leave it without peers", c.Name)
		assert.Zero(t, nm.PacketFilterRules.Len(), "%s: B's deny-all must give no filter rules", c.Name)
		assert.Empty(t, nm.PacketFilter, "%s: no filter matches", c.Name)
		assertNoTokens(t, c.Name+" netmap", nm, p06KeyStrings(p06NodeKeys(a)))

		nv, ok := b.State().GetNodeByNodeKey(selfKey(t, c))
		require.True(t, ok)
		assert.Zero(t, b.State().ListPeers(nv.ID()).Len(), "%s: server lists no peers", c.Name)
	}

	bRules, _ := b.State().Filter()
	assert.Empty(t, bRules, "B's compiled filter is empty")
}

// --- TestClusterP06PolicyDERPMapStaysInTailnet ---

// A's policy adds DERP region 901, B's adds 902. Each tailnet's clients get
// the cluster's region 900 plus their own policy region only.
func TestClusterP06PolicyDERPMapStaysInTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	aClients := enroll(t, a, "alice", "laptop")
	bClients := enroll(t, b, "alice", "laptop")

	waitRegions := func(c *servertest.TestClient, desc string, want ...tailcfg.DERPRegionID) {
		t.Helper()

		c.WaitForCondition(t, desc, clusterWait, func(nm *netmap.NetworkMap) bool {
			return p06RegionsEqual(nm, want...)
		})
	}

	waitRegions(aClients[0], "A: configured map", 900)
	waitRegions(bClients[0], "B: configured map", 900)

	derpPolicy := func(id int) string {
		return `{"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
			"derpMap": {"Regions": {"` + strconv.Itoa(id) + `": {"RegionID": ` + strconv.Itoa(id) + `, "RegionCode": "p06-` + strconv.Itoa(id) +
			`", "Nodes": [{"Name": "` + strconv.Itoa(id) + `a", "RegionID": ` + strconv.Itoa(id) + `, "HostName": "127.0.0.1", "DERPPort": -1}]}}}}`
	}

	before := settledFrames(t, bClients)

	tc.SetPolicy(t, tenantA, derpPolicy(901))
	waitRegions(aClients[0], "positive control: A gets its policy region", 900, 901)

	assertQuiet(t, bClients, before, "a derpMap policy in A")
	assert.True(t, p06RegionsEqual(bClients[0].Netmap(), 900), "B's clients must not get A's policy region")
	assert.NotContains(t, slices.Collect(maps.Keys(b.State().DERPMap().AsStruct().Regions)), tailcfg.DERPRegionID(901),
		"B's runtime DERP map must not hold A's region")

	tc.SetPolicy(t, tenantB, derpPolicy(902))
	bClients[0].WaitForCondition(t, "positive control: B gets its policy region", clusterWait, func(nm *netmap.NetworkMap) bool {
		return nm.DERPMap != nil && nm.DERPMap.Regions[902] != nil
	})
	assert.True(t, p06RegionsEqual(bClients[0].Netmap(), 900, 902),
		"B's clients get the cluster region and B's policy region only, not A's 901")
	assert.True(t, p06RegionsEqual(aClients[0].Netmap(), 900, 901), "A keeps only its own policy region")

	assert.NotContains(t, slices.Collect(maps.Keys(a.State().DERPMap().AsStruct().Regions)), tailcfg.DERPRegionID(902),
		"A's runtime DERP map must not hold B's region")
}

// --- helpers (P06) ---

func p06PeerByKey(nm *netmap.NetworkMap, k key.NodePublic) (tailcfg.NodeView, bool) {
	if nm == nil {
		return tailcfg.NodeView{}, false
	}

	for _, p := range nm.Peers {
		if p.Key() == k {
			return p, true
		}
	}

	return tailcfg.NodeView{}, false
}

func p06NodeKeys(s *servertest.TestServer) []key.NodePublic {
	var out []key.NodePublic
	for _, nv := range s.State().ListNodes().All() {
		out = append(out, nv.NodeKey())
	}

	return out
}

func p06KeyStrings(ks []key.NodePublic) []string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.String())
	}

	return out
}

// p06ForeignTokens lists what of theirs must never show up in ours' views
// when the two tailnets deliberately share hostnames and addresses: every
// key of theirs, and their hostnames, MagicDNS names and addresses that
// ours does not also use.
func p06ForeignTokens(t *testing.T, theirs, ours *servertest.TestServer) tokens {
	t.Helper()

	ourNames := map[string]bool{}
	var ourIPs []string

	for _, nv := range ours.State().ListNodes().All() {
		ourNames[nv.Hostname()] = true
		ourNames[nv.GivenName()] = true
		ourIPs = append(ourIPs, nv.IPsAsString()...)
	}

	var out tokens

	for _, nv := range theirs.State().ListNodes().All() {
		out.strs = append(out.strs, nv.NodeKey().String(), nv.MachineKey().String(), nv.DiscoKey().String())

		if !ourNames[nv.Hostname()] {
			out.strs = append(out.strs, nv.Hostname())
		}

		if !ourNames[nv.GivenName()] {
			out.strs = append(out.strs, nv.GivenName()+"."+servertest.ClusterBaseDomain)
		}

		for _, ip := range nv.IPsAsString() {
			if !slices.Contains(ourIPs, ip) {
				out.ips = append(out.ips, ip)
			}
		}
	}

	return out
}

func p06RegionsEqual(nm *netmap.NetworkMap, want ...tailcfg.DERPRegionID) bool {
	return nm != nil && nm.DERPMap != nil &&
		slices.Equal(slices.Sorted(maps.Keys(nm.DERPMap.Regions)), want)
}
