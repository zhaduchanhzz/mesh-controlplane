package mapper

import (
	"fmt"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
)

func testConfigIncremental() *types.Config {
	cfg := &types.Config{}
	cfg.Tuning.IncrementalNetmap.Enabled = true

	return cfg
}

// Fixes for the independent security review of P14f (review IDs R1-R8,
// evidence/P14f-review.md).

// TestReviewR1SharedEntriesFreedWithMapper: a stopped runtime's mapper,
// with its shared peer cache, must be collectable. A process-wide memo
// keyed by entry kept every entry of every stopped runtime alive.
func TestReviewR1SharedEntriesFreedWithMapper(t *testing.T) {
	td := setupSharedTailnet(t, 3, 10, allowAllPolicyJSON)
	connectAllShared(t, td)

	cfg := *td.Config
	cfg.Tuning.IncrementalNetmap.Enabled = true

	var entries []weak.Pointer[tailcfg.Node]

	// K runtime lifecycles, each a full-map storm through its own mapper.
	for k := range 5 {
		m := newMapper(&cfg, td.State)

		for i := range td.Nodes {
			resp := peerMap(t, m, td.Nodes[i].n.ID)
			if k == 0 && i == 0 {
				for _, p := range resp.Peers {
					entries = append(entries, weak.Make(p))
				}
			}
		}
	}

	require.NotEmpty(t, entries)

	runtime.GC()
	runtime.GC()

	alive := 0

	for _, w := range entries {
		if w.Value() != nil {
			alive++
		}
	}

	assert.Zero(t, alive, "%d of %d shared entries of a stopped mapper are still reachable", alive, len(entries))
}

// baselineBytesPerPeer measures the bytes a connection's delta-first
// baseline of n peers allocates.
func baselineBytesPerPeer(t *testing.T, n int) float64 {
	t.Helper()

	peers := make([]*tailcfg.Node, n)
	for i := range peers {
		peers[i] = dfPeer(tailcfg.NodeID(i+1), fmt.Sprintf("p%d", i)) //nolint:gosec // test
	}

	r := &tailcfg.MapResponse{Peers: peers}
	h := hashResponse(r, nil)

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	s := &sentState{}
	s.record(r, h, true)

	runtime.GC()
	runtime.ReadMemStats(&after)

	require.Len(t, s.peers, n)
	runtime.KeepAlive(s)

	// Bytes allocated by record: the baseline map (sized for n up front).
	return float64(after.TotalAlloc-before.TotalAlloc) / float64(n)
}

// TestReviewR2BaselineBytesPerPeer: a baseline is one entry per peer per
// connection, so it is O(n²) per tailnet; it must be compact. Before the
// review fix (a 24-byte value per peer): 65.6 B/peer at 10k, 52.5 at 50k;
// packed into 8 bytes: 29.6 and 23.6 (a Go map slot is at least the 16
// bytes of key and value; the rest is the map's own overhead). The bound
// on the tailnet's total is tuning.max_peer_entries, now also on policy
// writes.
func TestReviewR2BaselineBytesPerPeer(t *testing.T) {
	for _, n := range []int{10_000, 50_000} {
		b := baselineBytesPerPeer(t, n)
		t.Logf("baseline of %d peers: %.1f B/peer", n, b)
		assert.LessOrEqual(t, b, 32.0, "baseline of %d peers: %.1f B/peer", n, b)
	}
}

// TestReviewR2BaselineCap: a connection holding more peers than the cap
// keeps no baseline (it is sent everything, which is safe).
func TestReviewR2BaselineCap(t *testing.T) {
	n := maxBaselinePeers + 1

	peers := make([]*tailcfg.Node, n)
	for i := range peers {
		peers[i] = dfPeer(tailcfg.NodeID(i+1), "p") //nolint:gosec // test
	}

	r := &tailcfg.MapResponse{Peers: peers}

	var s sentState
	s.record(r, hashResponse(r, nil), true)
	assert.Nil(t, s.peers, "a baseline over the cap is dropped")

	d := &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{dfPeer(1, "p")}}
	assert.Same(t, d, s.strip(d, hashResponse(d, nil)), "no baseline: nothing stripped")
}

// TestReviewR3FullMapCarriesFilterAndProfiles: a frame with a full peer
// list is the safety net; it must carry the filter and profiles even when
// the baseline says the client holds them, so a client holding anything
// else heals there.
func TestReviewR3FullMapCarriesFilterAndProfiles(t *testing.T) {
	var s sentState

	filterF := []tailcfg.FilterRule{{SrcIPs: []string{"100.64.0.1"}, DstPorts: []tailcfg.NetPortRange{{IP: "*", Ports: tailcfg.PortRangeAny}}}}
	profile := tailcfg.UserProfile{ID: 7, LoginName: "u@", DisplayName: "u"}

	// Baseline: the server believes the client holds F and the profile.
	dfDeliver(&s, &tailcfg.MapResponse{
		Peers:         []*tailcfg.Node{dfPeer(1, "a")},
		PacketFilters: map[string][]tailcfg.FilterRule{"base": filterF},
		UserProfiles:  []tailcfg.UserProfile{profile},
	}, true)

	// A delta with the same parts is stripped of them (F3)...
	d := &tailcfg.MapResponse{
		PeersChanged:  []*tailcfg.Node{dfPeer(2, "b")},
		PacketFilters: map[string][]tailcfg.FilterRule{"base": filterF},
		UserProfiles:  []tailcfg.UserProfile{profile},
	}
	got := s.strip(d, hashResponse(d, nil))
	assert.Nil(t, got.PacketFilters, "delta: equal filter stripped")

	// ...but a full map carries them whole.
	full := &tailcfg.MapResponse{
		Peers:         []*tailcfg.Node{dfPeer(1, "a")},
		PacketFilters: map[string][]tailcfg.FilterRule{"base": filterF},
		UserProfiles:  []tailcfg.UserProfile{profile},
	}
	got = s.strip(full, hashResponse(full, nil))
	assert.NotNil(t, got.PacketFilters, "full map: filter sent even if believed held")
	assert.Len(t, got.UserProfiles, 1, "full map: profiles sent even if believed held")
	assert.Len(t, got.Peers, 1)
}

// TestReviewR6TwoConnectionsUnderBackpressure: one node, two streams; one
// stream is blocked while the filter goes A -> B, then drains before B -> A.
// Each stream must end up holding A: the blocked one never got B, so the
// return to A must not be stripped for the other, nor B assumed for it.
func TestReviewR6TwoConnectionsUnderBackpressure(t *testing.T) {
	cfg := testConfigIncremental()
	m := newMapper(cfg, nil)
	mc := newMultiChannelNodeConn(1, m)

	chFree := make(chan *tailcfg.MapResponse, 8)
	chBlocked := make(chan *tailcfg.MapResponse, 1)
	free := makeConnectionEntry("free", chFree)
	blocked := makeConnectionEntry("blocked", chBlocked)
	mc.addConnection(free)
	mc.addConnection(blocked)

	filter := func(src string) map[string][]tailcfg.FilterRule {
		return map[string][]tailcfg.FilterRule{"base": {{SrcIPs: []string{src}, DstPorts: []tailcfg.NetPortRange{{IP: "*", Ports: tailcfg.PortRangeAny}}}}}
	}

	// Initial map with filter A on both streams (the baseline).
	initial := &tailcfg.MapResponse{Peers: []*tailcfg.Node{dfPeer(2, "p")}, PacketFilters: filter("A")}
	require.NoError(t, mc.send(initial))

	held := map[*connectionEntry]map[string][]tailcfg.FilterRule{}
	drain := func(e *connectionEntry, ch chan *tailcfg.MapResponse) {
		for {
			select {
			case r := <-ch:
				if r.PacketFilters != nil {
					held[e] = r.PacketFilters
				}
			default:
				return
			}
		}
	}
	drain(free, chFree)
	drain(blocked, chBlocked)

	require.Equal(t, filter("A"), held[free])
	require.Equal(t, filter("A"), held[blocked])

	// Block the second stream: fill its buffer.
	chBlocked <- &tailcfg.MapResponse{}

	// A -> B: the free stream gets it, the blocked one does not.
	err := mc.send(&tailcfg.MapResponse{PacketFilters: filter("B")})
	require.ErrorIs(t, err, errConnectionBackpressure)
	drain(free, chFree)
	assert.Equal(t, filter("B"), held[free])

	// The blocked stream drains (its filler frame only), then B -> A.
	<-chBlocked
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, mc.send(&tailcfg.MapResponse{PacketFilters: filter("A")}))
	drain(free, chFree)
	drain(blocked, chBlocked)

	assert.Equal(t, filter("A"), held[free], "free stream ends with A")
	assert.Equal(t, filter("A"), held[blocked], "blocked stream ends with A (it never held B)")
}

// TestReviewR8SweepsBoundedByTailnetSize: the shared cache sweeps, each a
// pass over every entry, once per tailnet-size puts, not once per viewer's
// peer count (which is small under isolating policies).
func TestReviewR8SweepsBoundedByTailnetSize(t *testing.T) {
	td := setupSharedTailnet(t, 20, 10, `{"acls":[{"action":"accept","src":["autogroup:member"],"dst":["autogroup:self:*"]}]}`)
	connectAllShared(t, td)

	m := td.Batcher.mapper

	for range 3 {
		dropSharedPeers(m)

		for i := range td.Nodes {
			_ = peerMap(t, m, td.Nodes[i].n.ID)
		}
	}

	c := m.sharedPeerCache()
	require.NotNil(t, c)

	live := len(td.Nodes)
	puts := live * 9 // one full map per node, 9 same-user peers each (last round)
	assert.LessOrEqual(t, c.sweeps.Load(), int64(puts/live+1),
		"%d sweeps for %d puts in a tailnet of %d", c.sweeps.Load(), puts, live)
}
