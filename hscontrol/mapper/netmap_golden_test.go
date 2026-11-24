package mapper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// P00d M05: with tuning.incremental_netmap off, the change stream and the
// MapResponse frames every connected node receives for a fixed scenario
// must stay those of the full rebuild (P00c). The golden file records, per
// step, the changes State returns and a canonical summary of each frame
// per node: which fields are set, the peer, changed, removed and patched
// node IDs, and a hash of the packet filters. Keys, timestamps and
// ControlTime are random per run and are left out.
//
// The batcher runs with its ticker parked; the test queues each step's
// changes and runs one tick by hand, so the frames a node gets do not
// depend on timing. Refresh with HEADSCALE_UPDATE_GOLDEN=1 only for a
// change meant to alter the default path, and review the diff.

const netmapGoldenPath = "testdata/netmap_golden/incremental_off.golden"

const (
	goldenPolicyAllowAll = `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`
	goldenPolicyGroups   = `{
		"groups":{"group:ops":["alice@"]},
		"tagOwners":{"tag:srv":["alice@"]},
		"acls":[{"action":"accept","src":["group:ops"],"dst":["tag:srv:*","bob@:*"]}]
	}`
	goldenPolicySelf = `{"acls":[{"action":"accept","src":["autogroup:member"],"dst":["autogroup:self:*"]}]}`
)

type netmapGolden struct {
	t     *testing.T
	st    *state.State
	b     *Batcher
	chans map[types.NodeID]chan *tailcfg.MapResponse
	users map[string]*types.User
	out   strings.Builder
}

func newNetmapGolden(t *testing.T, incremental bool) *netmapGolden {
	t.Helper()

	prefixV4 := netip.MustParsePrefix("100.64.0.0/10")
	prefixV6 := netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	cfg := &types.Config{
		Database: types.DatabaseConfig{
			Type:   types.DatabaseSqlite,
			Sqlite: types.SqliteConfig{Path: filepath.Join(t.TempDir(), "headscale.db")},
		},
		Prefixes:  types.PrefixesConfig{V4: &prefixV4, V6: &prefixV6, Allocation: types.IPAllocationStrategySequential},
		DNSConfig: types.DNSConfig{BaseDomain: "headscale.test"},
		Policy:    types.PolicyConfig{Mode: types.PolicyModeDB},
		DERP: types.DERPConfig{DERPMap: &tailcfg.DERPMap{
			Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{999: {RegionID: 999}},
		}},
		Tuning: types.Tuning{
			// The ticker never fires; tick() runs each batch by hand.
			BatchChangeDelay:      time.Hour,
			BatcherWorkers:        2,
			NodeStoreBatchSize:    state.TestBatchSize,
			NodeStoreBatchTimeout: state.TestBatchTimeout,
			IncrementalNetmap: types.IncrementalNetmapConfig{
				Enabled:           incremental,
				ReconcileInterval: time.Hour,
				FullMapInterval:   time.Hour,
			},
		},
	}

	database, err := db.NewHeadscaleDatabase(cfg)
	require.NoError(t, err)

	users := map[string]*types.User{}
	for _, name := range []string{"alice", "bob", "carol"} {
		users[name] = database.CreateUserForTest(name)
	}

	for _, n := range []struct{ user, host string }{
		{"alice", "a1"}, {"alice", "a2"}, {"bob", "b1"}, {"bob", "b2"}, {"carol", "c1"},
	} {
		database.CreateRegisteredNodeForTest(users[n.user], n.host)
	}

	require.NoError(t, database.Close())

	st, err := state.NewState(cfg)
	require.NoError(t, err)

	st.SetDERPMap(cfg.DERP.DERPMap)

	g := &netmapGolden{
		t:     t,
		st:    st,
		chans: map[types.NodeID]chan *tailcfg.MapResponse{},
		users: users,
	}

	g.setPolicy("allow_all", goldenPolicyAllowAll)

	g.b = NewBatcher(cfg.Tuning.BatchChangeDelay, cfg.Tuning.BatcherWorkerCount(), newMapper(cfg, st))
	g.b.Start()

	t.Cleanup(func() {
		g.b.Close()
		_ = st.Close()
	})

	return g
}

func (g *netmapGolden) setPolicy(name, pol string) []change.Change {
	g.t.Helper()

	_, err := g.st.SetPolicyInDB(pol)
	require.NoError(g.t, err)

	cs, err := g.st.ReloadPolicy()
	require.NoError(g.t, err, "policy %s", name)

	return cs
}

// connect runs the poll path of a new stream: Connect, AddNode (initial
// map), then the connect changes in the next tick.
func (g *netmapGolden) connect(id types.NodeID) {
	g.t.Helper()

	cs, _ := g.st.Connect(id)
	ch := make(chan *tailcfg.MapResponse, 256)
	g.chans[id] = ch
	require.NoError(g.t, g.b.AddNode(id, ch, tailcfg.CurrentCapabilityVersion, func() {}))
	g.step(fmt.Sprintf("connect %d", id), cs)
}

func (g *netmapGolden) join(user, host string, tags ...string) types.NodeID {
	g.t.Helper()

	uid := types.UserID(g.users[user].ID)
	pak, err := g.st.CreatePreAuthKey(&uid, false, false, nil, tags)
	require.NoError(g.t, err)

	nv, c, err := g.st.HandleNodeFromPreAuthKey(tailcfg.RegisterRequest{
		Auth:     &tailcfg.RegisterResponseAuth{AuthKey: pak.Key},
		NodeKey:  key.NewNode().Public(),
		Expiry:   time.Now().Add(24 * time.Hour),
		Hostinfo: &tailcfg.Hostinfo{Hostname: host},
	}, key.NewMachine().Public())
	require.NoError(g.t, err)

	g.step(fmt.Sprintf("join %s/%s -> %d", user, host, nv.ID()), []change.Change{c})
	g.connect(nv.ID())

	return nv.ID()
}

func (g *netmapGolden) node(host string) types.NodeView {
	g.t.Helper()

	for _, n := range g.st.ListNodes().All() {
		if n.Hostname() == host {
			return n
		}
	}

	g.t.Fatalf("no node %s", host)

	return types.NodeView{}
}

// step queues cs, runs one tick, waits for every node's bundle and writes
// the changes and the frames each node received.
func (g *netmapGolden) step(label string, cs []change.Change) {
	g.t.Helper()

	fmt.Fprintf(&g.out, "== %s\n", label)

	// Targeted changes come in adjacency order, which is map order; the
	// set is what reaches the nodes, so it is written sorted.
	lines := make([]string, 0, len(cs))
	for _, c := range cs {
		lines = append(lines, goldenChange(c))
	}

	slices.Sort(lines)

	for _, l := range lines {
		fmt.Fprintf(&g.out, "  change %s\n", l)
	}

	g.b.addToBatch(cs...)
	g.b.processBatchedChanges()
	g.waitIdle()

	ids := make([]types.NodeID, 0, len(g.chans))
	for id := range g.chans {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	for _, id := range ids {
		for _, resp := range drain(g.chans[id]) {
			fmt.Fprintf(&g.out, "  %d <- %s\n", id, goldenFrame(resp))
		}
	}
}

func (g *netmapGolden) waitIdle() {
	g.t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		busy := false

		g.b.nodes.Range(func(_ types.NodeID, nc *multiChannelNodeConn) bool {
			if nc != nil && (nc.inFlight.Load() || pendingLen(nc) > 0) {
				busy = true
			}

			return !busy
		})

		if !busy {
			return
		}

		time.Sleep(time.Millisecond)
	}

	g.t.Fatal("batcher did not drain")
}

func pendingLen(nc *multiChannelNodeConn) int {
	nc.pendingMu.Lock()
	defer nc.pendingMu.Unlock()

	return len(nc.pending)
}

func drain(ch chan *tailcfg.MapResponse) []*tailcfg.MapResponse {
	var out []*tailcfg.MapResponse

	for {
		select {
		case r := <-ch:
			out = append(out, r)
		default:
			return out
		}
	}
}

func goldenIDs[T ~uint64 | ~int64](ids []T) string {
	s := make([]int64, len(ids))
	for i, id := range ids {
		s[i] = int64(id)
	}

	slices.Sort(s)

	return fmt.Sprint(s)
}

func goldenChange(c change.Change) string {
	var b strings.Builder

	fmt.Fprintf(&b, "type=%s target=%d origin=%d", c.Type(), c.TargetNode, c.OriginNode)

	for _, f := range []struct {
		on   bool
		name string
	}{
		{c.IncludeSelf, "self"}, {c.IncludeDERPMap, "derp"}, {c.IncludeDNS, "dns"},
		{c.IncludeDomain, "domain"}, {c.IncludePolicy, "policy"}, {c.SendAllPeers, "allpeers"},
		{c.RequiresRuntimePeerComputation, "runtime"},
	} {
		if f.on {
			b.WriteString(" " + f.name)
		}
	}

	if len(c.PeersChanged) > 0 {
		b.WriteString(" changed=" + goldenIDs(c.PeersChanged))
	}

	if len(c.PeersRemoved) > 0 {
		b.WriteString(" removed=" + goldenIDs(c.PeersRemoved))
	}

	if len(c.DeletedNodes) > 0 {
		b.WriteString(" deleted=" + goldenIDs(c.DeletedNodes))
	}

	if len(c.PeerPatches) > 0 {
		ids := make([]tailcfg.NodeID, 0, len(c.PeerPatches))
		for _, p := range c.PeerPatches {
			ids = append(ids, p.NodeID)
		}

		b.WriteString(" patches=" + goldenIDs(ids))
	}

	return b.String()
}

func goldenFrame(r *tailcfg.MapResponse) string {
	var parts []string

	add := func(on bool, s string) {
		if on {
			parts = append(parts, s)
		}
	}

	add(r.Node != nil, "self")
	add(r.DNSConfig != nil, "dns")
	add(r.DERPMap != nil, "derp")
	add(r.Domain != "", "domain")
	add(r.SSHPolicy != nil, "ssh")
	add(len(r.UserProfiles) > 0, fmt.Sprintf("profiles=%d", len(r.UserProfiles)))

	if r.Peers != nil {
		ids := make([]tailcfg.NodeID, 0, len(r.Peers))
		for _, p := range r.Peers {
			ids = append(ids, p.ID)
		}

		parts = append(parts, "peers="+goldenIDs(ids))
	}

	if len(r.PeersChanged) > 0 {
		ids := make([]tailcfg.NodeID, 0, len(r.PeersChanged))
		for _, p := range r.PeersChanged {
			ids = append(ids, p.ID)
		}

		parts = append(parts, "changed="+goldenIDs(ids))
	}

	if len(r.PeersRemoved) > 0 {
		parts = append(parts, "removed="+goldenIDs(r.PeersRemoved))
	}

	if len(r.PeersChangedPatch) > 0 {
		ps := make([]string, 0, len(r.PeersChangedPatch))
		for _, p := range r.PeersChangedPatch {
			on := "-"
			if p.Online != nil {
				on = fmt.Sprint(*p.Online)
			}

			ps = append(ps, fmt.Sprintf("%d:%s", p.NodeID, on))
		}

		slices.Sort(ps)
		parts = append(parts, "patch=["+strings.Join(ps, " ")+"]")
	}

	if r.PacketFilter != nil || r.PacketFilters != nil {
		js, _ := json.Marshal(struct {
			F  []tailcfg.FilterRule
			Fs map[string][]tailcfg.FilterRule
		}{r.PacketFilter, r.PacketFilters})
		sum := sha256.Sum256(js)
		parts = append(parts, "filter="+hex.EncodeToString(sum[:])[:12])
	}

	if len(parts) == 0 {
		return "(empty)"
	}

	return strings.Join(parts, " ")
}

// runNetmapScenario is the fixed scenario: joins under allow-all (the case
// the incremental path targets), deletes, tags, a group policy, a node
// outside every alias, autogroup:self, renames and a user removed.
func runNetmapScenario(g *netmapGolden) string {
	for _, h := range []string{"a1", "a2", "b1", "b2", "c1"} {
		g.connect(g.node(h).ID())
	}

	g.join("alice", "a3")
	g.join("carol", "c2")
	cs, err := g.st.DeleteNode(g.node("b2"))
	require.NoError(g.t, err)
	g.step("delete b2", cs)

	g.step("policy groups", g.setPolicy("groups", goldenPolicyGroups))

	_, c, err := g.st.SetNodeTags(g.node("a3").ID(), []string{"tag:srv"})
	require.NoError(g.t, err)
	g.step("tag a3 tag:srv", []change.Change{c})

	g.join("carol", "c3")
	g.join("bob", "b3")

	_, c, err = g.st.RenameNode(g.node("a1").ID(), "a1-renamed")
	require.NoError(g.t, err)
	g.step("rename a1", []change.Change{c})

	cs, err = g.st.DeleteNode(g.node("b1"))
	require.NoError(g.t, err)
	g.step("delete b1", cs)

	g.step("policy self", g.setPolicy("self", goldenPolicySelf))
	g.join("bob", "b4")

	for _, h := range []string{"c1", "c2", "c3"} {
		cs, err = g.st.DeleteNode(g.node(h))
		require.NoError(g.t, err)
		g.step("delete "+h, cs)
	}

	c, err = g.st.DeleteUser(types.UserID(g.users["carol"].ID))
	require.NoError(g.t, err)
	g.step("delete user carol", []change.Change{c})

	g.step("policy allow_all", g.setPolicy("allow_all", goldenPolicyAllowAll))
	g.join("alice", "a4")

	return g.out.String()
}

func TestNetmapGoldenIncrementalOff(t *testing.T) {
	got := runNetmapScenario(newNetmapGolden(t, false))

	if os.Getenv("HEADSCALE_UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(netmapGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(netmapGoldenPath, []byte(got), 0o600))
	}

	want, err := os.ReadFile(netmapGoldenPath)
	require.NoError(t, err, "missing golden; run with HEADSCALE_UPDATE_GOLDEN=1 to generate")
	require.Equal(t, string(want), got, "P00d M05: with the switch off the frames must match the P00c golden")

	// The scenario is deterministic: a second run gives the same bytes.
	again := runNetmapScenario(newNetmapGolden(t, false))
	require.Equal(t, got, again, "the golden scenario must be deterministic")
}
