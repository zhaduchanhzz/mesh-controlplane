package mapper

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"pgregory.net/rapid"
	"tailscale.com/tailcfg"
)

// P14f F4 (ADR 0015 D4): a peer entry that is the same for every viewer is
// rendered once per node revision and shared by every map that lists it.
// These tests hold the shared path to the per-viewer rendering it replaces.

// tb is what the helpers need from *testing.T and *rapid.T.
type tb interface {
	require.TestingT
	Helper()
}

// peerMap builds the self, packet filter and peer parts of a full map for
// id, as the full-map path does (buildFromChange with SendAllPeers).
func peerMap(t tb, m *mapper, id types.NodeID) *tailcfg.MapResponse {
	t.Helper()

	peers := m.state.ListPeers(id)

	resp, err := m.NewMapResponseBuilder(id).
		WithCapabilityVersion(tailcfg.CapabilityVersion(100)).
		WithSelfNode().
		WithPacketFilters().
		WithUserProfiles(peers).
		WithPeers(peers).
		Build()
	require.NoError(t, err)

	return resp
}

func mapJSON(t tb, r *tailcfg.MapResponse) string {
	t.Helper()

	c := *r
	c.ControlTime = nil

	b, err := json.Marshal(&c)
	require.NoError(t, err)

	return string(b)
}

// freshMapper renders like the code before F4: same state, cache off.
func freshMapper(td *TestData) *mapper {
	cfg := *td.Config
	cfg.Tuning.IncrementalNetmap.Enabled = false

	return newMapper(&cfg, td.State)
}

// requireSharedEqualsFresh checks, for every node, that the map built
// through the shared path (twice: fill, then hit) equals the per-viewer one.
func requireSharedEqualsFresh(t tb, td *TestData, step string) {
	t.Helper()

	shared := td.Batcher.mapper
	fresh := freshMapper(td)

	for range 2 {
		for i := range td.Nodes {
			id := td.Nodes[i].n.ID
			want := mapJSON(t, peerMap(t, fresh, id))
			got := mapJSON(t, peerMap(t, shared, id))
			require.Equal(t, want, got, "%s: node %d: shared peer list differs from the per-viewer one", step, id)
		}
	}
}

func setupSharedTailnet(t *testing.T, users, perUser int, pol string) *TestData {
	t.Helper()

	td, cleanup := newSharedTailnet(t, users, perUser, pol)
	t.Cleanup(cleanup)

	return td
}

func newSharedTailnet(t *testing.T, users, perUser int, pol string) (*TestData, func()) {
	t.Helper()

	td, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, users, perUser, largeBufferSize)

	td.Config.Tuning.IncrementalNetmap.Enabled = true

	_, err := td.State.SetPolicy([]byte(pol))
	require.NoError(t, err)

	return td, cleanup
}

// TestSharedPeerEntryRenderedOnce: under allow_all every node lists every
// other one; each peer's entry must be one object shared by all the maps
// that list it, not one rendering per viewer.
func TestSharedPeerEntryRenderedOnce(t *testing.T) {
	td := setupSharedTailnet(t, 5, 10, allowAllPolicyJSON)
	connectAllShared(t, td)

	m := td.Batcher.mapper

	entries := make(map[tailcfg.NodeID]*tailcfg.Node)

	for i := range td.Nodes {
		resp := peerMap(t, m, td.Nodes[i].n.ID)
		require.Len(t, resp.Peers, len(td.Nodes)-1)

		for _, p := range resp.Peers {
			if prev, ok := entries[p.ID]; ok {
				require.Same(t, prev, p, "peer %d rendered again for viewer %d", p.ID, td.Nodes[i].n.ID)
			} else {
				entries[p.ID] = p
			}
		}
	}

	// A change to one node renders that node again, and only it.
	n0 := td.Nodes[0].n.ID
	_, _, err := td.State.RenameNode(n0, "renamed-zero")
	require.NoError(t, err)

	resp := peerMap(t, m, td.Nodes[1].n.ID)
	for _, p := range resp.Peers {
		if p.ID == tailcfg.NodeID(n0) { //nolint:gosec // test node IDs are small
			require.NotSame(t, entries[p.ID], p, "renamed node served from the old entry")
			require.Contains(t, p.Name, "renamed-zero")

			continue
		}

		require.Same(t, entries[p.ID], p, "unchanged peer %d rendered again", p.ID)
	}

	requireSharedEqualsFresh(t, td, "after rename")

	// The delta-first baseline a full map sets (F3: hash and presence per
	// peer) is the same from a shared entry's memo as from hashing it,
	// also after a presence change.
	requireMemoMatches := func(step string) {
		t.Helper()

		for _, p := range peerMap(t, m, td.Nodes[2].n.ID).Peers {
			memo, ok1 := hashPeer(p)
			direct, ok2 := hashPeerUncached(p)
			require.True(t, ok1 && ok2)
			require.Equal(t, direct, memo, "%s: peer %d", step, p.ID)
		}
	}

	requireMemoMatches("online")

	_, err = td.State.Disconnect(td.Nodes[3].n.ID, 0)
	require.NoError(t, err)

	resp = peerMap(t, m, td.Nodes[2].n.ID)
	for _, p := range resp.Peers {
		if p.ID == tailcfg.NodeID(td.Nodes[3].n.ID) { //nolint:gosec // test node IDs are small
			require.NotNil(t, p.Online)
			require.False(t, *p.Online, "disconnected peer served from its online entry")
		}
	}

	requireMemoMatches("after disconnect")
	requireSharedEqualsFresh(t, td, "after disconnect")
}

// TestSharedPeersFilterStaysPerRecipient: entries are shared, packet
// filters never are. Under a policy where everyone reaches testuser-1 but
// each user also reaches only its own devices, a testuser-0 node and a
// testuser-2 node list the same testuser-1 peer through one shared entry,
// while each carries its own filter, equal to the per-viewer one.
func TestSharedPeersFilterStaysPerRecipient(t *testing.T) {
	td := setupSharedTailnet(t, 3, 4, sharedSamplePolicies(t)["shared"])
	connectAllShared(t, td)

	m := td.Batcher.mapper
	fresh := freshMapper(td)

	a, b := td.Nodes[0].n.ID, td.Nodes[8].n.ID // testuser-0, testuser-2
	target := tailcfg.NodeID(td.Nodes[4].n.ID) //nolint:gosec // testuser-1; test node IDs are small

	ra, rb := peerMap(t, m, a), peerMap(t, m, b)

	entry := func(r *tailcfg.MapResponse) *tailcfg.Node {
		for _, p := range r.Peers {
			if p.ID == target {
				return p
			}
		}

		return nil
	}

	require.NotNil(t, entry(ra))
	require.Same(t, entry(ra), entry(rb), "the testuser-1 peer is one shared entry")

	fa, err := json.Marshal(ra.PacketFilters)
	require.NoError(t, err)
	fb, err := json.Marshal(rb.PacketFilters)
	require.NoError(t, err)
	require.NotEqual(t, string(fa), string(fb), "the two recipients' filters differ")

	wantA, err := json.Marshal(peerMap(t, fresh, a).PacketFilters)
	require.NoError(t, err)
	wantB, err := json.Marshal(peerMap(t, fresh, b).PacketFilters)
	require.NoError(t, err)
	require.JSONEq(t, string(wantA), string(fa))
	require.JSONEq(t, string(wantB), string(fb))
}

// TestSharedPeersOffWhenSwitchOff: the kill switch also turns the shared
// path off (every viewer renders its own entries, as before F4).
func TestSharedPeersOffWhenSwitchOff(t *testing.T) {
	td := setupSharedTailnet(t, 2, 5, allowAllPolicyJSON)
	td.Config.Tuning.IncrementalNetmap.Enabled = false
	m := td.Batcher.mapper

	a := peerMap(t, m, td.Nodes[0].n.ID)
	b := peerMap(t, m, td.Nodes[1].n.ID)

	for _, pa := range a.Peers {
		for _, pb := range b.Peers {
			if pa.ID == pb.ID {
				require.NotSame(t, pa, pb)
			}
		}
	}
}

// TestSharedPeersPerViewerParts covers what differs per viewer: routes
// (subnet, exit, via), autogroup:self, disable-ipv4 on the viewer and on
// the peer, and expiry passing while an entry is cached.
func TestSharedPeersPerViewerParts(t *testing.T) {
	for name, pol := range sharedSamplePolicies(t) {
		t.Run(name, func(t *testing.T) {
			td := setupSharedTailnet(t, 3, 4, pol)
			connectAllShared(t, td)
			requireSharedEqualsFresh(t, td, "initial")

			// Routes: a subnet router, an exit node, both approved.
			announceAndApprove(t, td, 1, netip.MustParsePrefix("10.33.0.0/24"))
			announceAndApprove(t, td, 2, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"))
			requireSharedEqualsFresh(t, td, "routes")

			// Expiry passing while the entry is cached.
			soon := time.Now().Add(150 * time.Millisecond)
			_, _, err := td.State.SetNodeExpiry(td.Nodes[3].n.ID, &soon)
			require.NoError(t, err)
			requireSharedEqualsFresh(t, td, "before expiry")
			require.Eventually(t, func() bool { return time.Now().After(soon.Add(10 * time.Millisecond)) },
				2*time.Second, 20*time.Millisecond)
			requireSharedEqualsFresh(t, td, "after expiry")

			// Rename, disconnect.
			_, _, err = td.State.RenameNode(td.Nodes[4].n.ID, "other-name")
			require.NoError(t, err)

			_, err = td.State.Disconnect(td.Nodes[5].n.ID, 0)
			require.NoError(t, err)
			requireSharedEqualsFresh(t, td, "rename+disconnect")
		})
	}
}

// TestSharedPeersTwoTailnets: two runtimes with the same node IDs (one per
// tailnet, as in cluster mode with per-tailnet state) never serve each
// other's entries.
func TestSharedPeersTwoTailnets(t *testing.T) {
	a := setupSharedTailnet(t, 2, 5, allowAllPolicyJSON)
	b := setupSharedTailnet(t, 2, 5, allowAllPolicyJSON)

	keysOf := func(td *TestData) map[string]bool {
		k := make(map[string]bool)
		for _, n := range td.State.ListNodes().All() {
			k[n.NodeKey().String()] = true
		}

		return k
	}

	ka, kb := keysOf(a), keysOf(b)

	for range 3 {
		for _, td := range []*TestData{a, b} {
			own, other := ka, kb
			if td == b {
				own, other = kb, ka
			}

			for i := range td.Nodes {
				for _, p := range peerMap(t, td.Batcher.mapper, td.Nodes[i].n.ID).Peers {
					require.True(t, own[p.Key.String()], "ISOLATION: peer key not of this tailnet")
					require.False(t, other[p.Key.String()], "ISOLATION: peer key of the other tailnet")
				}
			}
		}
	}
}

// TestSharedPeersProperty: random changes, after each one every node's
// shared map equals its per-viewer map.
func TestSharedPeersProperty(t *testing.T) {
	pols := sharedSamplePolicies(t)
	names := make([]string, 0, len(pols))

	for n := range pols {
		names = append(names, n)
	}

	slices.Sort(names)

	// One tailnet for every check: setting one up costs far more than a
	// step, and the property holds after every step whatever came before.
	td := setupSharedTailnet(t, 3, 4, allowAllPolicyJSON)
	connectAllShared(t, td)

	rapid.Check(t, func(rt *rapid.T) {
		steps := rapid.IntRange(1, 12).Draw(rt, "steps")
		for s := range steps {
			i := rapid.IntRange(0, len(td.Nodes)-1).Draw(rt, "node")
			id := td.Nodes[i].n.ID

			var err error

			switch op := rapid.IntRange(0, 7).Draw(rt, "op"); op {
			case 0:
				_, _, err = td.State.RenameNode(id, fmt.Sprintf("n%d-%d", id, s))
			case 1:
				_, err = td.State.Disconnect(id, 0)
			case 2:
				td.State.Connect(id)
			case 3:
				announceAndApprove(rt, td, i, netip.MustParsePrefix(fmt.Sprintf("10.%d.0.0/24", 40+i)))
			case 4:
				announceAndApprove(rt, td, i, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"))
			case 5:
				_, _, err = td.State.SetApprovedRoutes(id, nil)
			case 6:
				_, err = td.State.SetPolicy([]byte(pols[names[rapid.IntRange(0, len(names)-1).Draw(rt, "pol")]]))
			case 7:
				past := time.Now().Add(-time.Minute)
				_, _, err = td.State.SetNodeExpiry(id, &past)
			}

			if err != nil {
				rt.Logf("op error (ignored): %v", err)
			}

			requireSharedEqualsFresh(rt, td, fmt.Sprintf("step %d", s))
		}
	})
}

const allowAllPolicyJSON = `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`

func sharedSamplePolicies(t *testing.T) map[string]string {
	t.Helper()

	return map[string]string{
		"allow_all": allowAllPolicyJSON,
		"isolated":  `{"acls":[{"action":"accept","src":["testuser-1@"],"dst":["testuser-1@:*"]}]}`,
		"self":      `{"acls":[{"action":"accept","src":["autogroup:member"],"dst":["autogroup:self:*"]}]}`,
		"shared": `{"acls":[
			{"action":"accept","src":["autogroup:member"],"dst":["autogroup:self:*"]},
			{"action":"accept","src":["*"],"dst":["testuser-1@:*"]}]}`,
		"grants_caps_internet": `{"grants":[
			{"src":["*"],"dst":["*"],"ip":["*"]},
			{"src":["*"],"dst":["autogroup:internet"],"ip":["*"]},
			{"src":["testuser-1@"],"dst":["testuser-2@"],"app":{"example.com/cap/x":[{"k":"v"}]}}]}`,
		"subnet_only": `{"acls":[
			{"action":"accept","src":["testuser-1@"],"dst":["10.33.0.0/24:*"]},
			{"action":"accept","src":["*"],"dst":["*:*"]}]}`,
		"disable_ipv4": `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}],
			"nodeAttrs":[{"target":["testuser-2@"],"attr":["disable-ipv4"]}]}`,
	}
}

func connectAllShared(t tb, td *TestData) {
	t.Helper()

	for i := range td.Nodes {
		td.State.Connect(td.Nodes[i].n.ID)
	}
}

// announceAndApprove makes node i announce and get approved routes.
func announceAndApprove(t tb, td *TestData, i int, routes ...netip.Prefix) {
	t.Helper()

	id := td.Nodes[i].n.ID

	nv, ok := td.State.GetNodeByID(id)
	require.True(t, ok)

	_, err := td.State.UpdateNodeFromMapRequest(id, tailcfg.MapRequest{
		NodeKey:  nv.NodeKey(),
		DiscoKey: nv.DiscoKey(),
		Hostinfo: &tailcfg.Hostinfo{Hostname: nv.Hostname(), RoutableIPs: routes},
	})
	require.NoError(t, err)

	_, _, err = td.State.SetApprovedRoutes(id, routes)
	require.NoError(t, err)
}
