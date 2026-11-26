//go:build p05cluster

// P14f F2 (ADR 0015, M07/M08): what a real client holds after every map
// response it was sent, initial full map plus every delta since, equals a
// full map built fresh for that node, at every quiescent point of a random
// sequence of changes in two tailnets served by one process; and a client
// never holds a node of the other tailnet.
//
// The client is the real Tailscale map session ([controlclient.Direct] of
// the version in go.mod): its netmap is the replay, applied with the
// client's own semantics (PeersChanged/PeersRemoved/patches, named packet
// filters, SSH policy and DNS kept when absent, DERP map merged). The
// oracle is the runtime's own initial-map path,
// [mapper.Batcher.MapResponseFromChange] with [change.FullSelf], which
// renders from current state without recording anything as sent.
//
// Long runs (nightly): P14F_M07_CHECKS (rapid checks per mode, default 2)
// and P14F_M07_STEPS (changes per check, default 14), e.g.
//
//	P14F_M07_CHECKS=50 P14F_M07_STEPS=40 go test -race -tags p05cluster \
//	  -run '^TestClusterP14fReplayEqualsFullMap$' -timeout 6h ./hscontrol/servertest/

package servertest_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
	"pgregory.net/rapid"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
)

// p14fTagOwners is in every policy, so tags stay valid across policy
// changes; alice and bob exist in both tailnets.
const p14fTagOwners = `"tagOwners": {"tag:srv": ["alice@"], "tag:router": ["alice@"]}`

// p14fPolicies are the policies a step switches between: allow-all, SSH,
// grants to a tag, autogroup:self, routes and exit nodes with auto
// approvers, a via grant, and a policy with no access rule (deny).
var p14fPolicies = []string{
	`{` + p14fTagOwners + `, "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]}`,
	`{` + p14fTagOwners + `, "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
	  "ssh": [{"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["root"]}]}`,
	`{` + p14fTagOwners + `, "grants": [
	  {"src": ["autogroup:member"], "dst": ["tag:srv"], "ip": ["*"]},
	  {"src": ["tag:srv", "tag:router"], "dst": ["tag:srv", "tag:router"], "ip": ["*"]}]}`,
	`{` + p14fTagOwners + `, "acls": [{"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self:*"]}]}`,
	`{` + p14fTagOwners + `, "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
	  "autoApprovers": {"routes": {"10.33.0.0/16": ["tag:router"]}, "exitNode": ["tag:router"]}}`,
	`{` + p14fTagOwners + `, "grants": [
	  {"src": ["*"], "dst": ["*"], "ip": ["*"]},
	  {"src": ["autogroup:member"], "dst": ["10.33.0.0/16"], "ip": ["*"], "via": ["tag:router"]}]}`,
	`{` + p14fTagOwners + `}`,
}

type p14fClient struct {
	c       *servertest.TestClient
	name    string
	online  bool
	routes  []netip.Prefix // announced
	checked int            // history entries already checked for M08
}

type p14fTailnet struct {
	slug    string
	srv     *servertest.TestServer
	users   map[string]types.UserID
	clients []*p14fClient
	keys    map[key.NodePublic]bool // every node key ever enrolled
}

func p14fEnv(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}

	return def
}

// TestClusterP14fReplayEqualsFullMap is M07 and M08 with the incremental
// netmap on (the default) and off (the kill switch).
func TestClusterP14fReplayEqualsFullMap(t *testing.T) {
	checks := p14fEnv("P14F_M07_CHECKS", 2)
	steps := p14fEnv("P14F_M07_STEPS", 14)

	if f := flag.Lookup("rapid.checks"); f != nil {
		old := f.Value.String()
		require.NoError(t, f.Value.Set(strconv.Itoa(checks)))

		defer func() { _ = f.Value.Set(old) }()
	}

	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprintf("incremental=%t", !off), func(t *testing.T) {
			rapid.Check(t, func(rt *rapid.T) {
				tb := &checkTB{T: t}
				defer tb.runCleanups()

				p14fRun(t, tb, rt, off, steps)
			})
		})
	}
}

func p14fRun(t *testing.T, tb *checkTB, rt *rapid.T, off bool, steps int) {
	t.Helper()

	tc := servertest.NewTestClusterWith(tb, servertest.ClusterOptions{IncrementalNetmapOff: off},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})

	nets := map[string]*p14fTailnet{}
	for _, slug := range []string{tenantA, tenantB} {
		n := &p14fTailnet{slug: slug, srv: tc.Tenant(tb, slug), users: map[string]types.UserID{}, keys: map[key.NodePublic]bool{}}
		for _, u := range []string{"alice", "bob"} {
			n.users[u] = types.UserID(n.srv.CreateUser(tb, u).ID)
		}

		tc.SetPolicy(tb, slug, p14fPolicies[0])
		nets[slug] = n
	}

	seq := 0
	join := func(n *p14fTailnet, user string) {
		seq++
		name := fmt.Sprintf("p14f-%s-n%d", n.slug, seq)
		k := n.srv.CreatePreAuthKey(tb, n.users[user])
		c := servertest.NewClusterClient(tb, n.srv, name, servertest.ClusterClientOptions{AuthKey: k})
		n.keys[selfKeyTB(tb, c)] = true
		n.clients = append(n.clients, &p14fClient{c: c, name: name, online: true})
	}

	for _, n := range nets {
		join(n, "alice")
		join(n, "bob")
		join(n, "alice")
	}

	p14fCheckAll(t, tc, nets, "start")

	for step := range steps {
		slug := rapid.SampledFrom([]string{tenantA, tenantB}).Draw(rt, fmt.Sprintf("tailnet%d", step))
		n := nets[slug]
		op := rapid.IntRange(0, 11).Draw(rt, fmt.Sprintf("op%d", step))
		pick := rapid.IntRange(0, 1<<20).Draw(rt, fmt.Sprintf("pick%d", step))
		what := p14fApply(t, tb, tc, n, op, pick, step, join)

		when := fmt.Sprintf("step %d: %s in %s", step, what, slug)
		t.Log(when)
		p14fCheckAll(t, tc, nets, when)
	}
}

// p14fApply makes one change in n and says what it was.
func p14fApply(t *testing.T, tb *checkTB, tc *servertest.TestCluster, n *p14fTailnet, op, pick, step int,
	join func(*p14fTailnet, string),
) string {
	t.Helper()

	st := n.srv.State()
	rt := tc.Runtime(tb, n.slug)

	online := slices.DeleteFunc(slices.Clone(n.clients), func(c *p14fClient) bool { return !c.online })
	if len(online) == 0 {
		op = 0
	}

	var c *p14fClient
	if len(online) > 0 {
		c = online[pick%len(online)]
	}

	id := func(c *p14fClient) types.NodeID {
		nv, ok := st.GetNodeByNodeKey(selfKeyTB(tb, c.c))
		require.True(tb, ok, "%s is in its runtime", c.name)

		return nv.ID()
	}

	switch op {
	case 0: // join
		join(n, []string{"alice", "bob"}[pick%2])

		return "join " + n.clients[len(n.clients)-1].name
	case 1: // delete
		if len(online) < 2 {
			return "nothing (delete needs two nodes online)"
		}

		cs, err := st.DeleteNode(mustNode(tb, st, c))
		require.NoError(tb, err)
		rt.Change(cs...)
		c.c.Disconnect(tb)
		n.clients = slices.DeleteFunc(n.clients, func(o *p14fClient) bool { return o == c })

		return "delete " + c.name
	case 2: // offline / online
		var off []*p14fClient
		for _, o := range n.clients {
			if !o.online {
				off = append(off, o)
			}
		}

		if len(off) > 0 && pick%2 == 0 {
			o := off[pick%len(off)]
			o.c.Reconnect(tb)
			o.online = true

			return "reconnect " + o.name
		}

		if len(online) < 2 {
			return "nothing (offline needs two nodes online)"
		}

		c.c.Disconnect(tb)
		c.online = false

		return "offline " + c.name
	case 3: // hostinfo with announced routes
		i := pick % 3
		c.routes = [][]netip.Prefix{
			nil,
			{netip.MustParsePrefix(fmt.Sprintf("10.33.%d.0/24", step%250))},
			{netip.MustParsePrefix(fmt.Sprintf("10.33.%d.0/24", step%250)), netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")},
		}[i]
		c.c.Direct().SetHostinfo(&tailcfg.Hostinfo{
			BackendLogID: "servertest-" + c.name, Hostname: c.name,
			OS: fmt.Sprintf("os-%d", step), RoutableIPs: c.routes,
		})
		require.NoError(tb, sendUpdate(c.c), "hostinfo map request")
		// The runtime records the hostinfo before the step is checked.
		require.Eventually(tb, func() bool {
			nv, ok := st.GetNodeByNodeKey(selfKeyTB(tb, c.c))
			return ok && nv.Hostinfo().Valid() && nv.Hostinfo().OS() == fmt.Sprintf("os-%d", step)
		}, clusterWait, 50*time.Millisecond)

		return fmt.Sprintf("hostinfo %s routes %v", c.name, c.routes)
	case 4: // tags
		tag := []string{"tag:srv", "tag:router"}[pick%2]
		_, ch, err := st.SetNodeTags(id(c), []string{tag})
		if err != nil {
			return fmt.Sprintf("tag %s %s refused: %v", c.name, tag, err)
		}

		rt.Change(ch)

		return fmt.Sprintf("tag %s %s", c.name, tag)
	case 5: // approve a subset of the announced routes
		var approve []netip.Prefix
		for i, r := range c.routes {
			if pick>>i&1 == 1 {
				approve = append(approve, r)
			}
		}

		_, ch, err := st.SetApprovedRoutes(id(c), approve)
		require.NoError(tb, err)
		rt.Change(ch)

		return fmt.Sprintf("approve %s %v", c.name, approve)
	case 6: // rename
		name := fmt.Sprintf("p14f-r%d", step)
		_, ch, err := st.RenameNode(id(c), name)
		require.NoError(tb, err)
		rt.Change(ch)

		return fmt.Sprintf("rename %s to %s", c.name, name)
	case 7: // key expiry in the future
		exp := time.Now().Add(time.Duration(24+pick%240) * time.Hour).UTC().Truncate(time.Second)
		_, ch, err := st.SetNodeExpiry(id(c), &exp)
		require.NoError(tb, err)
		rt.Change(ch)

		return fmt.Sprintf("expiry %s %s", c.name, exp.Format(time.RFC3339))
	case 8, 9: // policy
		i := pick % len(p14fPolicies)
		tc.SetPolicy(tb, n.slug, p14fPolicies[i])

		return fmt.Sprintf("policy %d", i)
	case 10: // a user added, or added and removed (UserAdded/UserRemoved)
		u, ch, err := st.CreateUser(types.User{Name: fmt.Sprintf("p14f-u%d", step)})
		require.NoError(tb, err)
		rt.Change(ch)

		if pick%2 == 0 {
			return "user added " + u.Name
		}

		ch, err = st.DeleteUser(types.UserID(u.ID))
		require.NoError(tb, err)
		rt.Change(ch)

		return "user added and deleted " + u.Name
	default: // DNS
		d := &types.TenantDNS{
			Nameservers:   types.TenantNameservers{Global: []string{fmt.Sprintf("192.0.2.%d", 1+pick%200)}},
			SearchDomains: []string{fmt.Sprintf("s%d.example", pick%7)},
		}
		if pick%3 == 0 {
			d = nil
		}

		require.NoError(tb, tc.SetTailnetDNS(tb, n.slug, d))

		return fmt.Sprintf("dns %v", d != nil)
	}
}

func mustNode(tb testing.TB, st interface {
	GetNodeByNodeKey(key.NodePublic) (types.NodeView, bool)
}, c *p14fClient,
) types.NodeView {
	tb.Helper()

	nv, ok := st.GetNodeByNodeKey(selfKeyTB(tb, c.c))
	require.True(tb, ok, "%s is in its runtime", c.name)

	return nv
}

func selfKeyTB(tb testing.TB, c *servertest.TestClient) key.NodePublic {
	tb.Helper()

	nm := c.Netmap()
	require.NotNil(tb, nm, "%s has a netmap", c.Name)

	return nm.NodeKey
}

// p14fCheckAll waits until every online client of both tailnets holds
// what a fresh full map gives it (M07), and checks every netmap any
// client got since the last check for a node of the other tailnet (M08).
func p14fCheckAll(t *testing.T, tc *servertest.TestCluster, nets map[string]*p14fTailnet, when string) {
	t.Helper()

	other := map[string]string{tenantA: tenantB, tenantB: tenantA}

	for slug, n := range nets {
		foreign := nets[other[slug]].keys
		rt := tc.Runtime(t, slug)

		for _, c := range n.clients {
			if !c.online {
				continue
			}

			var last string

			ok := assertEventually(func() bool {
				last = p14fDiff(t, rt.MapBatcher().MapResponseFromChange, c.c.Netmap())
				return last == ""
			})
			if !ok {
				t.Fatalf("M07 %s: %s (%s) differs from a fresh full map: %s", when, c.name, slug, last)
			}
		}

		for _, c := range n.clients {
			h := c.c.History()
			for i := c.checked; i < len(h); i++ {
				for _, p := range h[i].Peers {
					if foreign[p.Key()] {
						t.Fatalf("M08 BLOCKER %s: %s (%s) got a node of %s in netmap %d", when, c.name, slug, other[slug], i)
					}
				}
			}

			c.checked = len(h)
		}
	}
}

func assertEventually(cond func() bool) bool {
	deadline := time.Now().Add(clusterWait)
	for {
		if cond() {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// p14fDiff compares a client's netmap with a full map built now for the
// same node; "" means equal.
func p14fDiff(t *testing.T, render func(types.NodeID, change.Change) (*tailcfg.MapResponse, error), nm *netmap.NetworkMap) string {
	t.Helper()

	if nm == nil || !nm.SelfNode.Valid() {
		return "no netmap yet"
	}

	id := types.NodeID(nm.SelfNode.ID()) //nolint:gosec // node IDs are positive

	full, err := render(id, change.FullSelf(id))
	if err != nil {
		return "rendering the full map: " + err.Error()
	}

	if full == nil {
		return "empty full map"
	}

	var diffs []string

	add := func(what, got, want string) {
		if got != want {
			diffs = append(diffs, fmt.Sprintf("%s:\n  client %s\n  full   %s", what, got, want))
		}
	}

	// Peers: same IDs, same fields.
	got := map[tailcfg.NodeID]string{}
	for _, p := range nm.Peers {
		got[p.ID()] = p14fNode(p)
	}

	want := map[tailcfg.NodeID]string{}
	for _, p := range full.Peers {
		want[p.ID] = p14fNode(p.View())
	}

	add("peer IDs", fmt.Sprint(slices.Sorted(maps.Keys(got))), fmt.Sprint(slices.Sorted(maps.Keys(want))))

	for id, w := range want {
		if g, ok := got[id]; ok {
			add(fmt.Sprintf("peer %d", id), g, w)
		}
	}

	if full.Node != nil {
		add("self", p14fSelf(nm.SelfNode), p14fSelf(full.Node.View()))
	}

	add("packet filter", p14fJSON(nm.PacketFilterRules.AsSlice()), p14fJSON(p14fFilterRules(full)))
	add("SSH policy", p14fSSH(nm.SSHPolicy), p14fSSH(full.SSHPolicy))

	wantDNS := tailcfg.DNSConfig{}
	if full.DNSConfig != nil {
		wantDNS = *full.DNSConfig
	}

	add("DNS", p14fJSON(nm.DNS), p14fJSON(wantDNS))

	if full.DERPMap != nil && nm.DERPMap != nil {
		add("DERP regions", fmt.Sprint(slices.Sorted(maps.Keys(nm.DERPMap.Regions))), fmt.Sprint(slices.Sorted(maps.Keys(full.DERPMap.Regions))))
	}

	// User profiles of the users the netmap refers to: self and peers, as
	// the full map has them. A full map also lists the owner of a tagged
	// self node (tagged-devices), which nothing refers to: a tagged self
	// node is its own user (mapper taggedSelfUserID). A client never drops
	// a profile, so profiles nothing refers to are not compared.
	refs := map[tailcfg.UserID]bool{}
	if full.Node != nil {
		refs[full.Node.User] = true
	}

	for _, p := range full.Peers {
		refs[p.User] = true
	}

	for id := range refs {
		i := slices.IndexFunc(full.UserProfiles, func(up tailcfg.UserProfile) bool { return up.ID == id })
		if i < 0 {
			diffs = append(diffs, fmt.Sprintf("the full map refers to user %d without a profile", id))
			continue
		}

		up := full.UserProfiles[i]

		g := "missing"
		gp, ok := nm.UserProfiles[id]

		if ok {
			g = gp.LoginName() + "|" + gp.DisplayName()
		}

		// Findings F2-1 and F2-2 (evidence/P14f-f12.md): the profile of a
		// tagged node's own identity is not sent when it has no peer
		// (TestClusterP14fSelfTagProfile) nor refreshed when it is renamed
		// (TestClusterP14fTaggedRenameProfile). P14F_M07_SKIP_SELF_PROFILE=1
		// skips only the self node's own profile, to look for other
		// differences; the default is strict.
		if os.Getenv("P14F_M07_SKIP_SELF_PROFILE") == "1" && full.Node != nil && id == full.Node.User {
			continue
		}

		add(fmt.Sprintf("user profile %d", id), g, up.LoginName+"|"+up.DisplayName)
	}

	return strings.Join(diffs, "\n")
}

// p14fFilterRules is the client's packet filter after only this full map:
// "base" from PacketFilter, then PacketFilters ("*": nil clears), the
// named filters concatenated in name order (controlclient map.go).
func p14fFilterRules(resp *tailcfg.MapResponse) []tailcfg.FilterRule {
	named := map[string][]tailcfg.FilterRule{}
	if resp.PacketFilter != nil {
		named["base"] = resp.PacketFilter
	}

	if m := resp.PacketFilters; m != nil {
		if v, ok := m["*"]; ok && v == nil {
			clear(named)
		}

		for k, v := range m {
			switch {
			case k == "*":
			case v != nil:
				named[k] = v
			default:
				delete(named, k)
			}
		}
	}

	var out []tailcfg.FilterRule
	for _, k := range slices.Sorted(maps.Keys(named)) {
		out = append(out, named[k]...)
	}

	return out
}

// p14fNode is a peer's fields a full map sets, after the client's own
// upgrade (legacy DERP string, AllowedIPs defaulting to the addresses).
// LastSeen is left out: it moves with every connect and is patched.
func p14fNode(v tailcfg.NodeView) string {
	n := v.AsStruct()
	p14fUpgrade(n)

	return p14fJSON(struct {
		ID                tailcfg.NodeID
		Name              string
		Key               key.NodePublic
		KeyExpiry         time.Time
		Machine           key.MachinePublic
		DiscoKey          key.DiscoPublic
		Addresses         []netip.Prefix
		AllowedIPs        []netip.Prefix
		PrimaryRoutes     []netip.Prefix
		Endpoints         []netip.AddrPort
		HomeDERP          tailcfg.DERPRegionID
		Tags              []string
		User              tailcfg.UserID
		Online            *bool
		MachineAuthorized bool
		Expired           bool
		CapMap            tailcfg.NodeCapMap
		Hostinfo          tailcfg.HostinfoView
		IsWireGuardOnly   bool
		IsJailed          bool
	}{
		n.ID, n.Name, n.Key, n.KeyExpiry, n.Machine, n.DiscoKey, n.Addresses, n.AllowedIPs, n.PrimaryRoutes,
		n.Endpoints, n.HomeDERP, n.Tags, n.User, n.Online, n.MachineAuthorized, n.Expired, n.CapMap,
		n.Hostinfo, n.IsWireGuardOnly, n.IsJailed,
	})
}

// p14fSelf is the self node's fields a full map sets; capabilities become
// CapMap keys, as the client does.
func p14fSelf(v tailcfg.NodeView) string {
	n := v.AsStruct()
	p14fUpgrade(n)

	caps := map[tailcfg.NodeCapability]bool{}
	for _, c := range n.Capabilities {
		caps[c] = true
	}

	for c := range n.CapMap {
		caps[c] = true
	}

	return p14fJSON(struct {
		Name          string
		Addresses     []netip.Prefix
		AllowedIPs    []netip.Prefix
		PrimaryRoutes []netip.Prefix
		Tags          []string
		User          tailcfg.UserID
		KeyExpiry     time.Time
		Caps          []tailcfg.NodeCapability
		Expired       bool
	}{n.Name, n.Addresses, n.AllowedIPs, n.PrimaryRoutes, n.Tags, n.User, n.KeyExpiry, slices.Sorted(maps.Keys(caps)), n.Expired})
}

func p14fUpgrade(n *tailcfg.Node) {
	if n.LegacyDERPString != "" && n.HomeDERP == 0 {
		if ap, err := netip.ParseAddrPort(n.LegacyDERPString); err == nil && ap.Addr().String() == tailcfg.DerpMagicIP {
			n.HomeDERP = tailcfg.DERPRegionID(ap.Port())
		}
	}

	n.LegacyDERPString = ""

	if n.AllowedIPs == nil {
		n.AllowedIPs = slices.Clone(n.Addresses)
	}
}

// p14fSSH treats no policy and a policy without rules alike: both leave
// a fresh client with no SSH rules.
func p14fSSH(p *tailcfg.SSHPolicy) string {
	if p == nil || len(p.Rules) == 0 {
		return "no rules"
	}

	return p14fJSON(p)
}

func p14fJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "marshal: " + err.Error()
	}

	return string(b)
}

// TestClusterP14fSelfTagProfile is the smallest case M07 found (finding
// F2-1, evidence/P14f-f12.md): a node tagged while connected, left with no
// peer by the policy, must still end up holding the profile of the user
// its self node now names (its own tagged identity), in both modes.
func TestClusterP14fSelfTagProfile(t *testing.T) {
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprintf("incremental=%t", !off), func(t *testing.T) {
			tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{IncrementalNetmapOff: off},
				servertest.TailnetSeed{Slug: tenantA})
			srv := tc.Tenant(t, tenantA)
			alice := srv.CreateUser(t, "alice")
			// Grants to tag:srv only: tag:router ends up with no peer.
			tc.SetPolicy(t, tenantA, p14fPolicies[2])

			k := srv.CreatePreAuthKey(t, types.UserID(alice.ID))
			c1 := servertest.NewClusterClient(t, srv, "p14f-self-1", servertest.ClusterClientOptions{AuthKey: k})
			c2 := servertest.NewClusterClient(t, srv, "p14f-self-2", servertest.ClusterClientOptions{AuthKey: k})
			nv, ok := srv.State().GetNodeByNodeKey(selfKey(t, c1))
			require.True(t, ok)

			_, ch, err := srv.State().SetNodeTags(nv.ID(), []string{"tag:router"})
			require.NoError(t, err)
			tc.Runtime(t, tenantA).Change(ch)

			c1.WaitForCondition(t, "self node is tagged, no peer", clusterWait, func(nm *netmap.NetworkMap) bool {
				return nm.SelfNode.Valid() && nm.SelfNode.Tags().Len() == 1 && len(nm.Peers) == 0
			})

			_ = c2

			var last string

			ok = assertEventually(func() bool {
				last = p14fDiff(t, tc.Runtime(t, tenantA).MapBatcher().MapResponseFromChange, c1.Netmap())
				return last == ""
			})
			require.True(t, ok, "tagged self node vs a fresh full map: %s", last)
		})
	}
}

// TestClusterP14fTaggedRenameProfile is M07's second finding (F2-2,
// evidence/P14f-f12.md): a tagged node's own identity is named after the
// node (mapper taggedSelfUserID), so renaming it must reach the node as a
// new profile of that identity.
func TestClusterP14fTaggedRenameProfile(t *testing.T) {
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprintf("incremental=%t", !off), func(t *testing.T) {
			tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{IncrementalNetmapOff: off},
				servertest.TailnetSeed{Slug: tenantA})
			srv := tc.Tenant(t, tenantA)
			alice := srv.CreateUser(t, "alice")
			tc.SetPolicy(t, tenantA, p14fPolicies[0])

			k := srv.CreatePreAuthKey(t, types.UserID(alice.ID))
			c1 := servertest.NewClusterClient(t, srv, "p14f-ren-1", servertest.ClusterClientOptions{AuthKey: k})
			c2 := servertest.NewClusterClient(t, srv, "p14f-ren-2", servertest.ClusterClientOptions{AuthKey: k})
			waitPeersExactly(t, c1, selfKey(t, c2))

			nv, ok := srv.State().GetNodeByNodeKey(selfKey(t, c1))
			require.True(t, ok)

			rt := tc.Runtime(t, tenantA)
			_, ch, err := srv.State().SetNodeTags(nv.ID(), []string{"tag:srv"})
			require.NoError(t, err)
			rt.Change(ch)

			render := rt.MapBatcher().MapResponseFromChange

			var last string

			require.True(t, assertEventually(func() bool {
				last = p14fDiff(t, render, c1.Netmap())
				return last == ""
			}), "after tagging: %s", last)

			_, ch, err = srv.State().RenameNode(nv.ID(), "p14f-ren-renamed")
			require.NoError(t, err)
			rt.Change(ch)

			require.True(t, assertEventually(func() bool {
				last = p14fDiff(t, render, c1.Netmap())
				return last == ""
			}), "after renaming the tagged node: %s", last)
		})
	}
}
