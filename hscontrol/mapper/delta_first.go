package mapper

import (
	"encoding/json"
	"hash/maphash"
	"sync"
	"time"

	"tailscale.com/tailcfg"
)

// Delta-first sending (P14f F3, ADR 0015 D2). With the incremental netmap
// on, every connection remembers, per peer, per user profile and for its
// packet filter, a hash of what was last delivered to it. A response then
// carries, for that connection, only the PeersChanged entries, UserProfiles
// and PacketFilters that differ from what the client already holds: the rest
// would replace equal state on the client and cost encode, write and client
// work for nothing.
//
// Safety rules:
//   - Only what was delivered to the connection is recorded (a connection
//     under backpressure or failing records nothing), so the comparison is
//     always against the client's real state.
//   - A connection without a baseline (its initial map was not recorded)
//     gets everything: unknown means send.
//   - A full peer list (Peers) is never stripped: it is the safety net of
//     ADR 0015 D3 and resets the baseline.
//   - A peer entry is dropped only when both its hash, which covers every
//     field but presence (keys, endpoints, expiry, capabilities, routes…),
//     and its presence (Online, LastSeen) equal what the client holds.
//     Presence is tracked apart because patches move it: an entry sent
//     whole sets it, presence patches (OnlineChange, PeersChangedPatch with
//     Online/LastSeen only) update it.
//   - A patch touching any other field (PeersChangedPatch with endpoints,
//     key, DERP region, capabilities, expiry), a PeerSeenChange and a
//     removal drop what is known of the peer, so its next PeersChanged is
//     sent whole.
//   - Removals are not decided here: computePeerDiff and removalsFromSent
//     (per node, from what was sent) still produce them.

// deltaHashSeed is per process: hashes are never stored or compared across
// processes, and a random seed keeps tenants from crafting collisions.
var deltaHashSeed = maphash.MakeSeed()

// responseHashes holds the hashes of the parts of one response that
// delta-first compares, computed once per response and shared by every
// connection of the node.
type responseHashes struct {
	peers     map[tailcfg.NodeID]peerSent
	profiles  map[tailcfg.UserID]uint64
	filter    uint64
	hasFilter bool
}

// hashJSON hashes v's JSON encoding. Encoding sorts map keys, so equal
// content hashes equal. An encoding error yields ok false: the caller then
// treats the part as changed.
func hashJSON(v any) (uint64, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}

	return maphash.Bytes(deltaHashSeed, b), true
}

// peerSent is what a client holds of one peer: the hash of the entry
// without presence, and the presence itself.
type peerSent struct {
	hash     uint64
	online   int8  // -1: nil, 0: false, 1: true
	lastSeen int64 // UnixNano; 0: nil
}

func onlineOf(b *bool) int8 {
	switch {
	case b == nil:
		return -1
	case *b:
		return 1
	default:
		return 0
	}
}

func seenOf(t *time.Time) int64 {
	if t == nil {
		return 0
	}

	return t.UnixNano()
}

// hashPeer returns what a client holding p holds of it, see the safety
// rules above.
func hashPeer(p *tailcfg.Node) (peerSent, bool) {
	c := *p
	c.Online = nil
	c.LastSeen = nil

	v, ok := hashJSON(&c)

	return peerSent{hash: v, online: onlineOf(p.Online), lastSeen: seenOf(p.LastSeen)}, ok
}

// patchKeepsHash reports whether pc changes nothing a peer hash covers:
// only presence (Online, LastSeen). Compared through JSON so a field added
// to [tailcfg.PeerChange] later counts as a change.
func patchKeepsHash(pc *tailcfg.PeerChange) bool {
	c := *pc
	c.NodeID = 0
	c.Online = nil
	c.LastSeen = nil

	got, err := json.Marshal(&c)
	if err != nil {
		return false
	}

	return string(got) == emptyPeerChangeJSON
}

// emptyPeerChangeJSON is the encoding of a patch that changes nothing.
// An encoding error leaves a value no JSON encoding equals, so every patch
// then counts as a change.
var emptyPeerChangeJSON = func() string {
	b, err := json.Marshal(&tailcfg.PeerChange{})
	if err != nil {
		return "invalid"
	}

	return string(b)
}()

// hashResponse computes the hashes delta-first needs for data. Parts that
// fail to hash are left out, which makes them count as changed.
func hashResponse(data *tailcfg.MapResponse) *responseHashes {
	h := &responseHashes{}

	if n := len(data.Peers) + len(data.PeersChanged); n > 0 {
		h.peers = make(map[tailcfg.NodeID]peerSent, n)

		for _, list := range [][]*tailcfg.Node{data.Peers, data.PeersChanged} {
			for _, p := range list {
				if p == nil {
					continue
				}

				if v, ok := hashPeer(p); ok {
					h.peers[p.ID] = v
				}
			}
		}
	}

	if len(data.UserProfiles) > 0 {
		h.profiles = make(map[tailcfg.UserID]uint64, len(data.UserProfiles))

		for i := range data.UserProfiles {
			if v, ok := hashJSON(&data.UserProfiles[i]); ok {
				h.profiles[data.UserProfiles[i].ID] = v
			}
		}
	}

	if data.PacketFilters != nil {
		h.filter, h.hasFilter = hashJSON(data.PacketFilters)
	}

	return h
}

// sentState is what one connection's client holds, as far as delta-first
// compares it. The zero value has no baseline.
type sentState struct {
	mu sync.Mutex

	// peers is nil until a full peer list (the initial map) was delivered.
	peers    map[tailcfg.NodeID]peerSent
	profiles map[tailcfg.UserID]uint64

	filter      uint64
	filterKnown bool
}

// strip returns data without the PeersChanged entries, UserProfiles and
// PacketFilters this connection already holds, or data itself when nothing
// is dropped. Empty slices become nil so an all-stripped response counts as
// empty ([emptyMapResponse]).
func (s *sentState) strip(data *tailcfg.MapResponse, h *responseHashes) *tailcfg.MapResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		peers      []*tailcfg.Node
		profiles   []tailcfg.UserProfile
		dropFilter bool
		changed    bool
	)

	// Never strip a full peer list; only deltas.
	if s.peers != nil && len(data.PeersChanged) > 0 {
		peers = make([]*tailcfg.Node, 0, len(data.PeersChanged))

		for _, p := range data.PeersChanged {
			if p != nil {
				hv, hok := h.peers[p.ID]
				if sv, sok := s.peers[p.ID]; hok && sok && hv == sv {
					changed = true
					continue
				}
			}

			peers = append(peers, p)
		}
	}

	if s.profiles != nil && len(data.UserProfiles) > 0 {
		profiles = make([]tailcfg.UserProfile, 0, len(data.UserProfiles))

		for _, up := range data.UserProfiles {
			hv, hok := h.profiles[up.ID]
			if sv, sok := s.profiles[up.ID]; hok && sok && hv == sv {
				changed = true
				continue
			}

			profiles = append(profiles, up)
		}
	}

	if data.PacketFilters != nil && s.filterKnown && h.hasFilter && h.filter == s.filter {
		dropFilter = true
		changed = true
	}

	if !changed {
		return data
	}

	out := *data

	if s.peers != nil && len(data.PeersChanged) > 0 {
		out.PeersChanged = nil
		if len(peers) > 0 {
			out.PeersChanged = peers
		}
	}

	if s.profiles != nil && len(data.UserProfiles) > 0 {
		out.UserProfiles = nil
		if len(profiles) > 0 {
			out.UserProfiles = profiles
		}
	}

	if dropFilter {
		out.PacketFilters = nil
	}

	return &out
}

// record notes that data was delivered to the connection. initial marks the
// stream's first frame, whose peer list (possibly empty) is the baseline.
func (s *sentState) record(data *tailcfg.MapResponse, h *responseHashes, initial bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if initial || data.Peers != nil {
		s.peers = make(map[tailcfg.NodeID]peerSent, len(data.Peers))

		for _, p := range data.Peers {
			if p == nil {
				continue
			}

			if v, ok := h.peers[p.ID]; ok {
				s.peers[p.ID] = v
			}
		}
	}

	if s.peers != nil {
		for _, p := range data.PeersChanged {
			if p == nil {
				continue
			}

			if v, ok := h.peers[p.ID]; ok {
				s.peers[p.ID] = v
			} else {
				delete(s.peers, p.ID)
			}
		}

		for _, id := range data.PeersRemoved {
			delete(s.peers, id)
		}

		for _, pc := range data.PeersChangedPatch {
			if pc == nil {
				continue
			}

			cur, ok := s.peers[pc.NodeID]
			if !ok {
				continue
			}

			if !patchKeepsHash(pc) {
				delete(s.peers, pc.NodeID)
				continue
			}

			if pc.Online != nil {
				cur.online = onlineOf(pc.Online)
			}

			if pc.LastSeen != nil {
				cur.lastSeen = seenOf(pc.LastSeen)
			}

			s.peers[pc.NodeID] = cur
		}

		for id, online := range data.OnlineChange {
			if cur, ok := s.peers[id]; ok {
				cur.online = onlineOf(&online)
				s.peers[id] = cur
			}
		}

		// PeerSeenChange moves LastSeen by rules of the client's own
		// (true: now); forget the peer rather than guess.
		for id := range data.PeerSeenChange {
			delete(s.peers, id)
		}
	}

	if initial && s.profiles == nil {
		s.profiles = make(map[tailcfg.UserID]uint64, len(data.UserProfiles))
	}

	if s.profiles != nil {
		for _, up := range data.UserProfiles {
			if v, ok := h.profiles[up.ID]; ok {
				s.profiles[up.ID] = v
			} else {
				delete(s.profiles, up.ID)
			}
		}
	}

	if data.PacketFilters != nil {
		s.filter, s.filterKnown = h.filter, h.hasFilter
	}
}

// deltaFirst reports whether delta-first sending is on: it follows the
// incremental netmap switch, so the output with the switch off stays that
// of the full rebuild (golden M05).
func (m *mapper) deltaFirst() bool {
	return m != nil && m.cfg != nil && m.cfg.Tuning.IncrementalNetmap.Enabled
}
