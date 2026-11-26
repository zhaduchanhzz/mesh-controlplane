package mapper

import (
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
)

// Shared peer entries (P14f F4, ADR 0015 D4). Every map that lists a peer
// used to render that peer's [tailcfg.Node] again for its viewer: a full map
// for every node of an n-node tailnet rendered n² entries, and a reconnect
// storm did that all at once. Most entries do not depend on the viewer at
// all; those are now rendered once per node revision and the same entry is
// put in every map that lists the peer.
//
// An entry is shared only when it is the same for every viewer:
//   - the peer has no route at all (no approved subnet or exit route, no
//     primary route): [state.State.RoutesForPeer] then returns nothing for
//     any viewer (via steering, route reduction and co-router visibility all
//     start from the peer's routes);
//   - the peer is not expired when the entry is used (expiry moves with the
//     clock, not with a node write; an expired peer is always rendered);
//   - the viewer does not hold disable-ipv4 (that rewrites the entry for that
//     viewer, so such a viewer renders its own entries).
//
// The entry's other inputs are the peer's node value, whether the peer
// itself holds disable-ipv4, and the process-wide config (base domain and
// feature switches, fixed for the mapper's life). The node value is matched
// by view identity: the NodeStore keeps a node's view until the node is
// written ([types.NodeView.SameNode]).
//
// The cache belongs to one mapper, and there is one mapper per tailnet
// runtime (ADR 0007): entries never cross tailnets. Entries are never
// modified after they are stored; every reader only reads them.
//
// Off (tuning.incremental_netmap.enabled false), nothing is cached and every
// viewer renders its own entries, as before F4.

// sharedPeerEntry is one rendered peer entry and what it was rendered from.
type sharedPeerEntry struct {
	view       types.NodeView
	ipv4Off    bool
	baseDomain string
	node       *tailcfg.Node
}

// sharedPeers is the per-mapper cache of shared peer entries, by node ID.
type sharedPeers struct {
	entries sync.Map // types.NodeID -> *sharedPeerEntry
	puts    atomic.Int64
}

// sharedPeerCache returns the mapper's cache, or nil when the incremental
// netmap is off.
func (m *mapper) sharedPeerCache() *sharedPeers {
	if m == nil || m.cfg == nil || !m.cfg.Tuning.IncrementalNetmap.Enabled {
		return nil
	}

	if c := m.shared.Load(); c != nil {
		return c
	}

	m.shared.CompareAndSwap(nil, &sharedPeers{})

	return m.shared.Load()
}

// peerShareable reports whether peer's entry is the same for every viewer
// (see above): no route of any kind and not expired now.
func peerShareable(peer types.NodeView, primaries []netip.Prefix) bool {
	return len(primaries) == 0 &&
		len(peer.SubnetRoutes()) == 0 &&
		!peer.IsExitNode() &&
		len(peer.ExitRoutes()) == 0 &&
		!peer.IsExpired()
}

// get returns the shared entry for peer, or nil when there is none that
// holds for peer's current value.
func (c *sharedPeers) get(peer types.NodeView, ipv4Off bool, baseDomain string) *tailcfg.Node {
	v, ok := c.entries.Load(peer.ID())
	if !ok {
		return nil
	}

	e, _ := v.(*sharedPeerEntry)
	if e == nil || !e.view.SameNode(peer) || e.ipv4Off != ipv4Off || e.baseDomain != baseDomain {
		return nil
	}

	// Expiry moves with the clock: an entry rendered before the peer
	// expired must not be served after.
	if peer.IsExpired() {
		return nil
	}

	return e.node
}

// put stores node as peer's shared entry. live is the number of nodes in
// the tailnet, used to bound the cache: entries of deleted nodes are swept
// once the puts since the last sweep exceed it.
func (c *sharedPeers) put(peer types.NodeView, ipv4Off bool, baseDomain string, node *tailcfg.Node, live int, exists func(types.NodeID) bool) {
	e := &sharedPeerEntry{view: peer, ipv4Off: ipv4Off, baseDomain: baseDomain, node: node}
	rememberPeerHash(node)

	if old, loaded := c.entries.Swap(peer.ID(), e); loaded {
		if oe, _ := old.(*sharedPeerEntry); oe != nil && oe.node != node {
			forgetPeerHash(oe.node)
		}
	}

	if c.puts.Add(1) > int64(live)+64 {
		c.puts.Store(0)
		c.sweep(exists)
	}
}

// sweep drops the entries of nodes that no longer exist.
func (c *sharedPeers) sweep(exists func(types.NodeID) bool) {
	c.entries.Range(func(k, v any) bool {
		id, _ := k.(types.NodeID)
		if !exists(id) {
			if c.entries.CompareAndDelete(k, v) {
				if e, _ := v.(*sharedPeerEntry); e != nil {
					forgetPeerHash(e.node)
				}
			}
		}

		return true
	})
}

// sharedPeerHashes memoises [hashPeer] for shared entries: a shared entry is
// in every full map of the tailnet, and delta-first hashes every entry of
// every response it sends. Keyed by the entry's address; entries are never
// modified, and are dropped here when the cache drops them.
var sharedPeerHashes sync.Map // *tailcfg.Node -> uint64

func rememberPeerHash(node *tailcfg.Node) {
	if v, ok := hashPeerUncached(node); ok {
		sharedPeerHashes.Store(node, v)
	}
}

func forgetPeerHash(node *tailcfg.Node) {
	sharedPeerHashes.Delete(node)
}
