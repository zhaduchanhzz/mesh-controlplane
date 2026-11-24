package mapper

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
)

// Batcher support for the incremental netmap (P00d, ADR 0005, measures 3
// and 4). Off (tuning.incremental_netmap.enabled false) none of this runs.

// incrementalNetmap reports whether the incremental netmap is on.
func (b *Batcher) incrementalNetmap() bool {
	return b.mapper != nil && b.mapper.cfg != nil && b.mapper.cfg.Tuning.IncrementalNetmap.Enabled
}

// SendFullMaps queues a full map for each of ids, and for no other node:
// the node's pending changes collapse into one full update, rendered from
// current state at the next tick, whose peer list replaces what the node
// holds and whose missing peers go out as removals. Used when
// reconciliation replaced the nodes' adjacency, and by the periodic
// refresh. Not [change.FullSelf] through AddWork, which forces a full map
// on every node (IsFull ignores the target).
func (b *Batcher) SendFullMaps(ids []types.NodeID) {
	for _, id := range ids {
		if nc, ok := b.nodes.Load(id); ok && nc != nil {
			nc.collapsePendingToFull(nil)
		}
	}
}

// removalsFromSent adds, for each deleted node, a removal to every node
// that was sent it (lastSentPeers or partialPeers) and is not already
// getting one in changes. A removal's recipients then never depend on the
// adjacency alone (R5): the incremental path drops the policy refresh that
// otherwise covered a node the adjacency missed.
func (b *Batcher) removalsFromSent(changes []change.Change) []change.Change {
	var deleted []types.NodeID

	targeted := map[types.NodeID]map[types.NodeID]bool{}

	for _, c := range changes {
		deleted = append(deleted, c.DeletedNodes...)

		if c.TargetNode != 0 && len(c.PeersRemoved) > 0 {
			if targeted[c.TargetNode] == nil {
				targeted[c.TargetNode] = map[types.NodeID]bool{}
			}

			for _, id := range c.PeersRemoved {
				targeted[c.TargetNode][id] = true
			}
		}
	}

	if len(deleted) == 0 {
		return changes
	}

	b.nodes.Range(func(id types.NodeID, nc *multiChannelNodeConn) bool {
		if nc == nil {
			return true
		}

		var missing []types.NodeID

		for _, gone := range deleted {
			if gone == id || targeted[id][gone] {
				continue
			}

			_, sent := nc.lastSentPeers.Load(gone.NodeID())
			_, partial := nc.partialPeers.Load(gone.NodeID())

			if sent || partial {
				missing = append(missing, gone)
			}
		}

		if len(missing) > 0 {
			changes = append(changes, change.Change{
				Reason:       "removal of a peer the node was sent",
				TargetNode:   id,
				PeersRemoved: missing,
			})
		}

		return true
	})

	return changes
}

// fullMapRefresher sends every connected node a full map once per
// interval, spread over the interval in slices so a tailnet is never
// refreshed all at once (measure 4: clients repair themselves).
type fullMapRefresher struct {
	mu     sync.Mutex
	cursor types.NodeID
}

// fullMapRefreshTicks is how many slices an interval is cut into.
const fullMapRefreshTicks = 60

// RunFullMapRefresh sends full maps until done closes: every interval /
// fullMapRefreshTicks (at least a second), the next share of nodes in ID
// order. Every node connected for a whole interval gets one full map in it.
func (b *Batcher) RunFullMapRefresh(interval time.Duration, done <-chan struct{}) {
	tick := max(interval/fullMapRefreshTicks, time.Second)
	ticks := max(int(interval/tick), 1)

	t := time.NewTicker(tick)
	defer t.Stop()

	var r fullMapRefresher

	for {
		select {
		case <-done:
			return
		case <-b.done:
			return
		case <-t.C:
			b.SendFullMaps(r.next(b, ticks))
		}
	}
}

// next returns the next ceil(n/ticks) connected nodes after the cursor, in
// ID order, wrapping around.
func (r *fullMapRefresher) next(b *Batcher, ticks int) []types.NodeID {
	r.mu.Lock()
	defer r.mu.Unlock()

	var ids []types.NodeID

	b.nodes.Range(func(id types.NodeID, nc *multiChannelNodeConn) bool {
		if nc != nil {
			ids = append(ids, id)
		}

		return true
	})

	if len(ids) == 0 {
		return nil
	}

	slices.SortFunc(ids, cmp.Compare)

	share := (len(ids) + ticks - 1) / ticks
	start, _ := slices.BinarySearch(ids, r.cursor+1)

	out := make([]types.NodeID, 0, share)
	for i := range min(share, len(ids)) {
		out = append(out, ids[(start+i)%len(ids)])
	}

	r.cursor = out[len(out)-1]

	return out
}
