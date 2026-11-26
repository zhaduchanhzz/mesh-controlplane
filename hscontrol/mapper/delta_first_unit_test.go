package mapper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// Safety rules of delta-first (P14f F3) on sentState alone.

func dfPeer(id tailcfg.NodeID, name string) *tailcfg.Node {
	online := true
	seen := time.Unix(1_700_000_000, 0)

	return &tailcfg.Node{ID: id, Name: name, Online: &online, LastSeen: &seen}
}

func dfDeliver(s *sentState, r *tailcfg.MapResponse, initial bool) *tailcfg.MapResponse {
	h := hashResponse(r, nil)
	out := s.strip(r, h)
	s.record(r, h, initial)

	return out
}

func TestSentStateNoBaselineSendsEverything(t *testing.T) {
	var s sentState

	r := &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{dfPeer(1, "a")}}
	assert.Same(t, r, s.strip(r, hashResponse(r, nil)), "no baseline: unchanged")
}

func TestSentStateFullPeersNeverStripped(t *testing.T) {
	var s sentState

	init := &tailcfg.MapResponse{Peers: []*tailcfg.Node{dfPeer(1, "a"), dfPeer(2, "b")}}
	dfDeliver(&s, init, true)

	full := &tailcfg.MapResponse{Peers: []*tailcfg.Node{dfPeer(1, "a"), dfPeer(2, "b")}}
	got := s.strip(full, hashResponse(full, nil))
	assert.Len(t, got.Peers, 2, "a full peer list is the safety net, never stripped")
}

func TestSentStateStripsOnlyEqualAndNeverAdds(t *testing.T) {
	var s sentState

	dfDeliver(&s, &tailcfg.MapResponse{Peers: []*tailcfg.Node{dfPeer(1, "a"), dfPeer(2, "b")}}, true)

	r := &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{dfPeer(1, "a"), dfPeer(2, "b2"), dfPeer(3, "c")}}
	got := dfDeliver(&s, r, false)

	ids := make([]tailcfg.NodeID, 0, len(got.PeersChanged))
	for _, p := range got.PeersChanged {
		ids = append(ids, p.ID)
	}

	assert.Equal(t, []tailcfg.NodeID{2, 3}, ids, "equal peer dropped; changed and new kept; nothing added")

	again := dfDeliver(&s, &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{dfPeer(2, "b2"), dfPeer(3, "c")}}, false)
	assert.Nil(t, again.PeersChanged, "all equal: nil, not empty")
	assert.True(t, emptyMapResponse(again))
}

func TestSentStatePresenceTrackedOtherFieldsSent(t *testing.T) {
	var s sentState

	dfDeliver(&s, &tailcfg.MapResponse{Peers: []*tailcfg.Node{dfPeer(1, "a")}}, true)

	offline := dfPeer(1, "a")
	f := false
	offline.Online = &f
	r := &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{offline}}
	assert.Len(t, s.strip(r, hashResponse(r, nil)).PeersChanged, 1,
		"presence the client does not hold yet must be sent")

	// Once a presence patch told the client, the same entry is redundant.
	dfDeliver(&s, &tailcfg.MapResponse{OnlineChange: map[tailcfg.NodeID]bool{1: false}}, false)
	assert.Nil(t, s.strip(r, hashResponse(r, nil)).PeersChanged, "presence already held via patch")

	expiry := dfPeer(1, "a")
	expiry.Online = &f
	expiry.KeyExpiry = time.Unix(1_800_000_000, 0)
	r = &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{expiry}}
	assert.Len(t, s.strip(r, hashResponse(r, nil)).PeersChanged, 1, "key expiry must be sent")

	k := key.NewNode().Public()
	rekey := dfPeer(1, "a")
	rekey.Online = &f
	rekey.Key = k
	r = &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{rekey}}
	assert.Len(t, s.strip(r, hashResponse(r, nil)).PeersChanged, 1, "a new node key must be sent")
}

func TestSentStatePatchAndRemovalInvalidate(t *testing.T) {
	var s sentState

	dfDeliver(&s, &tailcfg.MapResponse{Peers: []*tailcfg.Node{dfPeer(1, "a"), dfPeer(2, "b"), dfPeer(3, "c")}}, true)

	online := true // the entry's presence: a presence-only patch keeps the peer known
	dfDeliver(&s, &tailcfg.MapResponse{PeersChangedPatch: []*tailcfg.PeerChange{
		{NodeID: 1, Online: &online}, // presence only: hash kept
		{NodeID: 2, DERPRegion: 7},   // other field: invalidated
	}}, false)
	dfDeliver(&s, &tailcfg.MapResponse{PeersRemoved: []tailcfg.NodeID{3}}, false)

	r := &tailcfg.MapResponse{PeersChanged: []*tailcfg.Node{dfPeer(1, "a"), dfPeer(2, "b"), dfPeer(3, "c")}}
	got := s.strip(r, hashResponse(r, nil))

	ids := make([]tailcfg.NodeID, 0, len(got.PeersChanged))
	for _, p := range got.PeersChanged {
		ids = append(ids, p.ID)
	}

	assert.Equal(t, []tailcfg.NodeID{2, 3}, ids, "patched (non-presence) and removed peers are sent whole")
}

func TestSentStatePacketFiltersAndProfiles(t *testing.T) {
	var s sentState

	base := map[string][]tailcfg.FilterRule{"base": {{SrcIPs: []string{"100.64.0.1"}}}}
	dfDeliver(&s, &tailcfg.MapResponse{
		Peers:         []*tailcfg.Node{},
		PacketFilters: base,
		UserProfiles:  []tailcfg.UserProfile{{ID: 1, DisplayName: "one"}},
	}, true)

	same := &tailcfg.MapResponse{
		PacketFilters: map[string][]tailcfg.FilterRule{"base": {{SrcIPs: []string{"100.64.0.1"}}}},
		UserProfiles:  []tailcfg.UserProfile{{ID: 1, DisplayName: "one"}},
	}
	got := dfDeliver(&s, same, false)
	assert.Nil(t, got.PacketFilters, "equal filter dropped (nil keeps the client's)")
	assert.Nil(t, got.UserProfiles, "equal profile dropped")

	// An empty filter is a real change (deny all) and must go out.
	deny := &tailcfg.MapResponse{
		PacketFilters: map[string][]tailcfg.FilterRule{"base": {}},
		UserProfiles:  []tailcfg.UserProfile{{ID: 1, DisplayName: "renamed"}},
	}
	got = dfDeliver(&s, deny, false)
	require.NotNil(t, got.PacketFilters)
	assert.Empty(t, got.PacketFilters["base"])
	assert.Len(t, got.UserProfiles, 1, "changed profile sent")
}
