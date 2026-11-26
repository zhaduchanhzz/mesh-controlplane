package mapper

import (
	"encoding/json"
	"hash/maphash"
	"sync"

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
//   - A peer's hash leaves out Online and LastSeen: presence moves by
//     patches (NodeOnline/NodeOffline), and a lost patch means a full map
//     anyway (no connection, or a backlog collapsed to full). Every other
//     field, keys, endpoints, expiry and capabilities included, is hashed.
//   - A patch touching any other field (PeersChangedPatch with endpoints,
//     key, DERP region, capabilities, expiry) and a removal drop the peer's
//     hash, so the next PeersChanged for it is sent whole.
//   - Removals are not decided here: computePeerDiff and removalsFromSent
//     (per node, from what was sent) still produce them.

// deltaHashSeed is per process: hashes are never stored or compared across
// processes, and a random seed keeps tenants from crafting collisions.
var deltaHashSeed = maphash.MakeSeed()

// responseHashes holds the hashes of the parts of one response that
// delta-first compares, computed once per response and shared by every
// connection of the node.
type responseHashes struct {
	peers     map[tailcfg.NodeID]uint64
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

// hashPeer hashes a peer entry without its presence fields (Online,
// LastSeen), see the safety rules above.
func hashPeer(p *tailcfg.Node) (uint64, bool) {
	c := *p
	c.Online = nil
	c.LastSeen = nil

	return hashJSON(&c)
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
var emptyPeerChangeJSON = func() string {
	b, _ := json.Marshal(&tailcfg.PeerChange{})

	return string(b)
}()

// hashResponse computes the hashes delta-first needs for data. Parts that
// fail to hash are left out, which makes them count as changed.
func hashResponse(data *tailcfg.MapResponse) *responseHashes {
	h := &responseHashes{}

	if n := len(data.Peers) + len(data.PeersChanged); n > 0 {
		h.peers = make(map[tailcfg.NodeID]uint64, n)

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
	peers    map[tailcfg.NodeID]uint64
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
		s.peers = make(map[tailcfg.NodeID]uint64, len(data.Peers))

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
			if pc != nil && !patchKeepsHash(pc) {
				delete(s.peers, pc.NodeID)
			}
		}

		// OnlineChange and PeerSeenChange carry presence only, which the
		// hash leaves out.
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
