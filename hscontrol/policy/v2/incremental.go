package v2

import (
	"maps"
	"slices"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/types/views"
)

// Incremental netmap support (P00d, ADR 0005, tuning.incremental_netmap).
//
// A node joining or leaving changes, in general, what the policy resolves
// to: aliases such as user@, groups and tags resolve to node addresses, so
// the compiled filter, SSH rules, capabilities and the matchers that decide
// peer adjacency can all move. Only when none of them moves is the change
// local: every other node keeps its packet filter, SSH policy and
// capabilities, and the peer relation between two nodes that were both
// there before is what it was. Then the peer map can be patched for the
// joined and removed nodes instead of rebuilt, and the nodes that see a
// joined node need only that peer, not a full policy refresh.
//
// rulesGen counts every change that is not local (policy, users, a node
// set change that moved the rules, or one the incremental path does not
// handle). A peer map built at generation g may be patched only while
// rulesGen is still g.

// WithLocalNodeChanges makes [PolicyManager.SetNodes] report a node set
// change that is local, in the sense above, as no change: no NodesGeneration
// bump, so the caller sends the joined node as a peer delta and the removed
// one as a removal instead of a policy refresh to every node. Set from
// tuning.incremental_netmap.enabled; off, SetNodes is unchanged.
func WithLocalNodeChanges(on bool) Option {
	return func(pm *PolicyManager) { pm.localNodeChanges = on }
}

// RulesGeneration counts the changes that may have moved any node's packet
// filter, SSH policy, capabilities, or the peer relation of two nodes that
// did not change themselves. See [PolicyManager.IncrementalPeerMap].
func (pm *PolicyManager) RulesGeneration() uint64 {
	if pm == nil {
		return 0
	}

	return pm.rulesGen.Load()
}

// localSafeLocked reports whether the current policy is one under which a
// node joining or leaving can be local: the global filter decides every
// peer relation (no autogroup:self or via, which compile per node), the
// policy is not deny-all, has no SSH rules and no nodeAttrs (both resolve
// per node and are not in the filter hash), no relay or via targets, and no
// capability to the internet (which lists its sources on exit nodes only,
// a one-way relation).
func (pm *PolicyManager) localSafeLocked() bool {
	if pm.pol == nil || pm.denyAll || pm.needsPerNodeFilter {
		return false
	}

	if len(pm.pol.SSHs) > 0 || len(pm.pol.NodeAttrs) > 0 || pm.pol.RandomizeClientPort ||
		len(pm.nodeAttrsHashes) > 0 {
		return false
	}

	if (pm.relayTargetIPs != nil && len(pm.relayTargetIPs.Prefixes()) > 0) || len(pm.viaTargetTags) > 0 {
		return false
	}

	for k := range pm.matchers {
		if pm.matchers[k].CapDestsIsTheInternet() {
			return false
		}
	}

	return true
}

// onlyJoinLeave reports whether next differs from prev by added and removed
// nodes only, none of which serves routes (adding or removing a router can
// move a primary route other nodes route through), and every node in next
// is valid with a unique ID.
func onlyJoinLeave(prev, next views.Slice[types.NodeView]) bool {
	before := make(map[types.NodeID]types.NodeView, prev.Len())
	for _, n := range prev.All() {
		before[n.ID()] = n
	}

	seen := make(map[types.NodeID]bool, next.Len())

	for _, n := range next.All() {
		if !n.Valid() || seen[n.ID()] {
			return false
		}

		seen[n.ID()] = true

		old, existed := before[n.ID()]
		if existed {
			if n.HasPolicyChange(old) {
				return false
			}

			continue
		}

		if servesRoutesOrExit(n) {
			return false
		}
	}

	for id, old := range before {
		if !seen[id] && servesRoutesOrExit(old) {
			return false
		}
	}

	return true
}

func servesRoutesOrExit(n types.NodeView) bool {
	return !n.Valid() || len(n.SubnetRoutes()) > 0 || n.IsExitNode()
}

// BuildPeerMapGen is [PolicyManager.BuildPeerMap] and the rules generation
// it was built at, read under the same lock.
func (pm *PolicyManager) BuildPeerMapGen(nodes views.Slice[types.NodeView]) (map[types.NodeID][]types.NodeID, uint64) {
	if pm == nil {
		return nil, 0
	}

	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.buildPeerMapIndexedLocked(nodes), pm.rulesGen.Load()
}

// IncrementalPeerMap returns the peer map of nodes derived from prev, the
// peer map built at rules generation prevGen for the node set that nodes
// differs from by joined (in nodes, not before) and removed (before, not in
// nodes), and the generation it holds for. It returns ok false, and the
// caller builds in full, unless the policy is one the patch is exact for
// (see localSafeLocked) and the rules have not moved since prevGen.
//
// The result equals [PolicyManager.BuildPeerMap] of nodes as sets per node;
// the order within a list may differ (consumers sort). It shares prev's
// slices: a list that gains a peer is appended to, which writes past the
// length prev's holders read and is safe because only the newest snapshot
// is ever extended (the NodeStore writer is the only caller). A list that
// loses a peer is copied.
//
//nolint:gocyclo // hot path: the patch cases stay inline in one pass (P00d)
func (pm *PolicyManager) IncrementalPeerMap(
	nodes views.Slice[types.NodeView],
	prev map[types.NodeID][]types.NodeID,
	prevGen uint64,
	joined, removed []types.NodeID,
) (map[types.NodeID][]types.NodeID, uint64, bool) {
	if pm == nil || prev == nil {
		return nil, 0, false
	}

	pm.mu.RLock()
	defer pm.mu.RUnlock()

	gen := pm.rulesGen.Load()
	if !pm.localNodeChanges || gen != prevGen || !pm.localSafeLocked() {
		return nil, gen, false
	}

	index := make(map[types.NodeID]int, nodes.Len())

	for i, n := range nodes.All() {
		if !n.Valid() {
			return nil, gen, false
		}

		if _, dup := index[n.ID()]; dup {
			return nil, gen, false
		}

		index[n.ID()] = i
	}

	gone := make(map[types.NodeID]bool, len(removed))

	for _, id := range removed {
		if _, still := index[id]; still {
			return nil, gen, false
		}

		gone[id] = true
	}

	isJoined := make(map[types.NodeID]int, len(joined))

	for i, id := range joined {
		j, ok := index[id]
		if !ok || servesRoutesOrExit(nodes.At(j)) {
			return nil, gen, false
		}

		if _, dup := isJoined[id]; dup {
			return nil, gen, false
		}

		if _, before := prev[id]; before {
			return nil, gen, false
		}

		isJoined[id] = i
	}

	ret := maps.Clone(prev)

	// Removals. Without one-way relations (no capability to the internet)
	// the map is symmetric, so prev[r] lists every node whose list holds r.
	touched := make(map[types.NodeID]bool)

	for id := range gone {
		for _, y := range prev[id] {
			if !gone[y] {
				touched[y] = true
			}
		}

		delete(ret, id)
	}

	for y := range touched {
		kept := slices.DeleteFunc(slices.Clone(ret[y]), func(p types.NodeID) bool { return gone[p] })
		if len(kept) == 0 {
			delete(ret, y)
		} else {
			ret[y] = kept
		}
	}

	if len(joined) == 0 {
		return ret, gen, true
	}

	// Joins: the same pair test as the global path of the full build,
	// a source bitset of one side against the destination bitset of the
	// other, plus same-user, for each joined node against every node.
	words := (len(pm.matchers) + 63) / 64 //nolint:mnd
	srcOf := make([][]uint64, nodes.Len())
	dstOf := make([][]uint64, nodes.Len())

	for i, n := range nodes.All() {
		ips := n.IPs()
		subnet := types.RouteRanges(n.SubnetRoutes())
		isExit := n.IsExitNode()

		for k := range pm.matchers {
			m := &pm.matchers[k]

			if matcherReachesSource(m, ips, subnet) {
				if srcOf[i] == nil {
					srcOf[i] = make([]uint64, words)
				}

				srcOf[i][k/64] |= 1 << (k % 64) //nolint:mnd
			}

			if matcherReachesDestination(m, ips, subnet, isExit) {
				if dstOf[i] == nil {
					dstOf[i] = make([]uint64, words)
				}

				dstOf[i][k/64] |= 1 << (k % 64) //nolint:mnd
			}
		}
	}

	canAccess := func(src, dst []uint64) bool {
		if src == nil || dst == nil {
			return false
		}

		for w := range src {
			if src[w]&dst[w] != 0 {
				return true
			}
		}

		return false
	}

	for xi, x := range joined {
		i := index[x]
		nx := nodes.At(i)

		for j, ny := range nodes.All() {
			y := ny.ID()
			if y == x {
				continue
			}

			// A pair of joined nodes is tested once, from the earlier one.
			if yi, both := isJoined[y]; both && yi < xi {
				continue
			}

			linked := canAccess(srcOf[i], dstOf[j]) || canAccess(srcOf[j], dstOf[i])
			if linked || sameUser(nx, ny) {
				ret[x] = append(ret[x], y)
				ret[y] = append(ret[y], x)
			}
		}
	}

	return ret, gen, true
}
