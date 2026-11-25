package v2

import (
	"encoding/binary"
	"net/netip"
	"slices"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/policy/matcher"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"go4.org/netipx"
	"tailscale.com/types/views"
)

// buildPeerMapIndexedLocked returns exactly what
// [PolicyManager.buildPeerMapPairwiseLocked] returns, peer order included,
// without testing every node pair.
//
// The pair scan links a and b when:
//
//  1. a global matcher has one of them as source and the other as
//     destination;
//  2. a matcher only one of them holds (autogroup:self, via) does;
//  3. they belong to the same user; or, one way only,
//  4. b is an exit node and a is the source of a capability to the
//     internet: b lists a.
//
// Each side of (1) depends on one node only, so nodes with the same
// global source and destination matcher sets are interchangeable: the
// build groups them into classes and decides (1) once per class pair.
// (2) is decided per held matcher from the nodes it reaches, found
// through an address index; (3) and (4) are per-user and per-exit lists.
// A node's peers are the union of those lists, in input order, which is
// the order the pair scan appends them in.
//
// Must be called with pm.mu held.
//
//nolint:gocyclo // one pass per rule above
func (pm *PolicyManager) buildPeerMapIndexedLocked(nodes views.Slice[types.NodeView]) map[types.NodeID][]types.NodeID {
	// Deny all, as in the pair scan: no peers, not even same-user ones.
	if pm.denyAll {
		return make(map[types.NodeID][]types.NodeID)
	}

	n := nodes.Len()
	ids := make([]types.NodeID, n)
	seen := make(map[types.NodeID]struct{}, n)

	for i, nv := range nodes.All() {
		// The pair scan defines the result for these; they do not come
		// from a NodeStore snapshot, which is keyed by ID.
		if !nv.Valid() {
			return pm.buildPeerMapPairwiseLocked(nodes)
		}

		id := nv.ID()
		if _, dup := seen[id]; dup {
			return pm.buildPeerMapPairwiseLocked(nodes)
		}

		seen[id] = struct{}{}
		ids[i] = id
	}

	info := make([]peerNode, n)
	words := (len(pm.matchers) + 63) / 64 //nolint:mnd
	bits := make([]uint64, 2*words)       // source words, then destination words
	key := make([]byte, 8*len(bits))      //nolint:mnd
	classByKey := make(map[string]int32)

	var (
		classes []peerClass
		byUser  = make(map[uint][]int32)
	)

	for i, nv := range nodes.All() {
		in := &info[i]
		in.ips = nv.IPs()
		in.subnet = types.RouteRanges(nv.SubnetRoutes())
		in.isExit = nv.IsExitNode()
		in.user = -1

		if !nv.IsTagged() {
			if uid, ok := nv.UserID().GetOk(); ok {
				byUser[uid] = append(byUser[uid], int32(i)) //nolint:gosec // n fits int32
				in.user = int64(uid)
			}
		}

		// (1): this node's side of every global matcher, as in the
		// pair scan's bitset path.
		clear(bits)

		for k := range pm.matchers {
			m := &pm.matchers[k]

			if matcherReachesSource(m, in.ips, in.subnet) {
				bits[k/64] |= 1 << (k % 64) //nolint:mnd
				in.capSrc = in.capSrc || m.CapDestsIsTheInternet()
			}

			if matcherReachesDestination(m, in.ips, in.subnet, in.isExit) {
				bits[words+k/64] |= 1 << (k % 64) //nolint:mnd
			}
		}

		for w, b := range bits {
			binary.LittleEndian.PutUint64(key[8*w:], b)
		}

		c, ok := classByKey[string(key)]
		if !ok {
			c = int32(len(classes)) //nolint:gosec // at most n classes
			classByKey[string(key)] = c

			classes = append(classes, peerClass{
				src:  slices.Clone(bits[:words]),
				dst:  slices.Clone(bits[words:]),
				none: !slices.ContainsFunc(bits, func(b uint64) bool { return b != 0 }),
			})
		}

		classes[c].members = append(classes[c].members, int32(i)) //nolint:gosec
		in.class = c
	}

	// (1) per class pair; a class linked to itself links its members.
	linked := make([][]int32, len(classes))

	for a := range classes {
		if classes[a].none {
			continue
		}

		for b := a; b < len(classes); b++ {
			if classes[b].none {
				continue
			}

			if bitsIntersect(classes[a].src, classes[b].dst) || bitsIntersect(classes[b].src, classes[a].dst) {
				linked[a] = append(linked[a], int32(b)) //nolint:gosec
				if b != a {
					linked[b] = append(linked[b], int32(a)) //nolint:gosec
				}
			}
		}
	}

	// (2): the pair scan tests both directions of each node's own
	// matchers against the other node, so a held matcher links its
	// holder, as source, to all its destinations and, as destination,
	// to all its sources.
	var extra [][]int32

	if pm.needsPerNodeFilter {
		extra = make([][]int32, n)
		ix := newPeerIndex(info)

		link := func(i int, js []int32) {
			for _, j := range js {
				if int(j) == i {
					continue
				}

				extra[i] = append(extra[i], j)
				extra[j] = append(extra[j], int32(i)) //nolint:gosec
			}
		}

		for i, nv := range nodes.All() {
			in := &info[i]
			ms := nodeOnlyMatchers(pm.compiledGrants, nv, pm.userNodeIdx, viaPeerRule)

			for k := range ms {
				m := &ms[k]

				if matcherReachesSource(m, in.ips, in.subnet) {
					in.capSrc = in.capSrc || m.CapDestsIsTheInternet()
					link(i, ix.destinations(m))
				}

				if matcherReachesDestination(m, in.ips, in.subnet, in.isExit) {
					link(i, ix.sources(m))
				}
			}
		}
	}

	// (4): exit nodes list every capability source.
	var capSources []int32

	for i := range info {
		if info[i].capSrc {
			capSources = append(capSources, int32(i)) //nolint:gosec
		}
	}

	ret := make(map[types.NodeID][]types.NodeID, n)
	mark := make([]int32, n)
	buf := make([]int32, 0, n)

	for i := range info {
		in := &info[i]
		stamp := int32(i + 1) //nolint:gosec
		mark[i] = stamp       // never its own peer
		buf = buf[:0]

		// The lists are each in input order; the result needs sorting
		// only when a later list adds to an earlier one, or the unordered
		// (2) list adds anything.
		sorted := true
		add := func(list []int32, ordered bool) {
			before := len(buf)

			for _, j := range list {
				if mark[j] != stamp {
					mark[j] = stamp
					buf = append(buf, j)
				}
			}

			if len(buf) > before && (before > 0 || !ordered) {
				sorted = false
			}
		}

		for _, c := range linked[in.class] {
			add(classes[c].members, true)
		}

		if in.user >= 0 {
			add(byUser[uint(in.user)], true)
		}

		if extra != nil {
			add(extra[i], false)
		}

		if in.isExit {
			add(capSources, true)
		}

		if len(buf) == 0 {
			continue
		}

		if !sorted {
			slices.Sort(buf)
		}

		peers := make([]types.NodeID, len(buf))
		for k, j := range buf {
			peers[k] = ids[j]
		}

		ret[ids[i]] = peers
	}

	return ret
}

// peerNode is what the indexed build needs of one node, computed once.
type peerNode struct {
	ips    []netip.Addr
	subnet []netipx.IPRange
	isExit bool
	capSrc bool
	user   int64 // -1: tagged or no user
	class  int32
}

// peerClass is a set of nodes with the same global matcher sides.
type peerClass struct {
	src, dst []uint64
	none     bool // neither side of any global matcher
	members  []int32
}

func bitsIntersect(a, b []uint64) bool {
	for w := range a {
		if a[w]&b[w] != 0 {
			return true
		}
	}

	return false
}

// peerIndex finds the nodes a matcher side reaches without testing every
// node: by address for node IPs, plus the few nodes with subnet routes or
// exit routes, which are tested as the pair scan tests them. Results are
// cached per range slice, which matchers copied from one another share
// (all of a user's autogroup:self matchers, a via grant's sources).
type peerIndex struct {
	info   []peerNode
	addrs  []indexedAddr // sorted by address
	routed []int32       // nodes with subnet routes
	exits  []int32
	mark   []uint32
	stamp  uint32
	srcs   map[rangesKey][]int32
	dsts   map[rangesKey][]int32
}

type indexedAddr struct {
	addr netip.Addr
	node int32
}

type rangesKey struct {
	first    *netipx.IPRange
	n        int
	internet bool
}

func keyOf(r []netipx.IPRange, internet bool) rangesKey {
	k := rangesKey{n: len(r), internet: internet}
	if len(r) > 0 {
		k.first = &r[0]
	}

	return k
}

func newPeerIndex(info []peerNode) *peerIndex {
	ix := &peerIndex{
		info: info,
		mark: make([]uint32, len(info)),
		srcs: make(map[rangesKey][]int32),
		dsts: make(map[rangesKey][]int32),
	}

	for i := range info {
		for _, a := range info[i].ips {
			ix.addrs = append(ix.addrs, indexedAddr{addr: a, node: int32(i)}) //nolint:gosec
		}

		if len(info[i].subnet) > 0 {
			ix.routed = append(ix.routed, int32(i)) //nolint:gosec
		}

		if info[i].isExit {
			ix.exits = append(ix.exits, int32(i)) //nolint:gosec
		}
	}

	slices.SortFunc(ix.addrs, func(a, b indexedAddr) int { return a.addr.Compare(b.addr) })

	return ix
}

// sources returns, in input order, the nodes [matcherReachesSource] holds for.
func (ix *peerIndex) sources(m *matcher.Match) []int32 {
	k := keyOf(m.SrcRanges(), false)
	if got, ok := ix.srcs[k]; ok {
		return got
	}

	got := ix.collect(m.SrcRanges(), func(j int32) bool {
		return m.SrcsOverlapsRanges(ix.info[j].subnet)
	}, false)
	ix.srcs[k] = got

	return got
}

// destinations returns, in input order, the nodes
// [matcherReachesDestination] holds for.
func (ix *peerIndex) destinations(m *matcher.Match) []int32 {
	k := keyOf(m.DestRanges(), m.DestsIsTheInternet())
	if got, ok := ix.dsts[k]; ok {
		return got
	}

	got := ix.collect(m.DestRanges(), func(j int32) bool {
		return m.DestsOverlapsRanges(ix.info[j].subnet)
	}, m.DestsIsTheInternet())
	ix.dsts[k] = got

	return got
}

func (ix *peerIndex) collect(ranges []netipx.IPRange, routeHit func(int32) bool, exits bool) []int32 {
	ix.stamp++

	var out []int32

	add := func(j int32) {
		if ix.mark[j] != ix.stamp {
			ix.mark[j] = ix.stamp
			out = append(out, j)
		}
	}

	for _, r := range ranges {
		k, _ := slices.BinarySearchFunc(ix.addrs, r.From(), func(e indexedAddr, a netip.Addr) int {
			return e.addr.Compare(a)
		})

		for ; k < len(ix.addrs) && ix.addrs[k].addr.Compare(r.To()) <= 0; k++ {
			add(ix.addrs[k].node)
		}
	}

	for _, j := range ix.routed {
		if routeHit(j) {
			add(j)
		}
	}

	if exits {
		for _, j := range ix.exits {
			add(j)
		}
	}

	slices.Sort(out)

	return out
}
