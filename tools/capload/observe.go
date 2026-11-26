package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/util/zstdframe"
)

// Observers are the K streams whose every frame is decoded (-observe): each
// keeps the peer set its client would hold, so a probe knows which
// observers must see a change, and every decoded peer is checked against
// the tailnet capload enrolled it in (isolation: a peer of another tailnet
// in any frame is a stop condition, AGENTS rule 13). Other streams only
// count frames, as in P00c, so the load host does not model tailscaled's CPU.

// frameRec is one decoded frame of an observer (frames.csv).
type frameRec struct {
	at                                      time.Time
	bytes, peers, changed, patches, removed int
	keepAlive, filter                       bool
}

// maxFrameRecs bounds frames.csv per observer.
const maxFrameRecs = 20000

type observer struct {
	r *result

	mu      sync.Mutex
	peers   map[tailcfg.NodeID]key.NodePublic
	waiters []*waiter
	recs    []frameRec

	// filterFrames counts frames carrying a packet filter: a policy change
	// of another tailnet must not produce any (cross-tenant effect).
	filterFrames atomic.Int64
}

// waiter is a pending probe at one observer: done gets the receive time of
// the first frame match accepts.
type waiter struct {
	match func(*tailcfg.MapResponse) bool
	done  chan time.Time
}

func newObserver(r *result) *observer {
	return &observer{r: r, peers: map[tailcfg.NodeID]key.NodePublic{}}
}

// hasPeer reports whether the observer's netmap holds peer id.
func (o *observer) hasPeer(id tailcfg.NodeID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	_, ok := o.peers[id]

	return ok
}

func (o *observer) peerList() []tailcfg.NodeID {
	o.mu.Lock()
	defer o.mu.Unlock()

	ids := make([]tailcfg.NodeID, 0, len(o.peers))
	for id := range o.peers {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	return ids
}

func (o *observer) wait(match func(*tailcfg.MapResponse) bool) *waiter {
	w := &waiter{match: match, done: make(chan time.Time, 1)}

	o.mu.Lock()
	o.waiters = append(o.waiters, w)
	o.mu.Unlock()

	return w
}

func (o *observer) cancel(w *waiter) {
	o.mu.Lock()
	o.waiters = slices.DeleteFunc(o.waiters, func(x *waiter) bool { return x == w })
	o.mu.Unlock()
}

// frame applies one received frame (zstd-compressed JSON) at time at.
func (o *observer) frame(iso *isolation, msg []byte, n int, at time.Time) {
	mr, err := decodeMap(msg)
	if err != nil {
		iso.decodeErrors.Add(1)

		return
	}

	iso.check(o.r, mr)

	if mr.PacketFilter != nil || mr.PacketFilters != nil {
		o.filterFrames.Add(1)
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if mr.Peers != nil {
		clear(o.peers)

		for _, p := range mr.Peers {
			o.peers[p.ID] = p.Key
		}
	}

	for _, p := range mr.PeersChanged {
		o.peers[p.ID] = p.Key
	}

	for _, id := range mr.PeersRemoved {
		delete(o.peers, id)
	}

	if len(o.recs) < maxFrameRecs {
		o.recs = append(o.recs, frameRec{
			at: at, bytes: n, peers: len(mr.Peers), changed: len(mr.PeersChanged),
			patches: len(mr.PeersChangedPatch), removed: len(mr.PeersRemoved),
			keepAlive: mr.KeepAlive, filter: mr.PacketFilter != nil || mr.PacketFilters != nil,
		})
	}

	kept := o.waiters[:0]

	for _, w := range o.waiters {
		if w.match(mr) {
			w.done <- at
		} else {
			kept = append(kept, w)
		}
	}

	o.waiters = kept
}

func decodeMap(compressed []byte) (*tailcfg.MapResponse, error) {
	b, err := zstdframe.AppendDecode(nil, compressed, zstdframe.MaxDecodedSize(1<<30))
	if err != nil {
		return nil, err
	}

	var mr tailcfg.MapResponse

	err = json.Unmarshal(b, &mr)
	if err != nil {
		return nil, err
	}

	return &mr, nil
}

// isolation checks every decoded peer against the tailnet capload enrolled
// its node key in. keyTailnet is built before any client starts (node keys
// are generated up front) and never written after.
type isolation struct {
	keyTailnet map[key.NodePublic]string

	frames, peers, violations, unknown, decodeErrors atomic.Int64

	mu      sync.Mutex
	samples []string
}

const maxIsolationSamples = 20

func (iso *isolation) check(r *result, mr *tailcfg.MapResponse) {
	iso.frames.Add(1)

	for _, list := range [][]*tailcfg.Node{mr.Peers, mr.PeersChanged} {
		for _, p := range list {
			iso.peers.Add(1)

			tn, ok := iso.keyTailnet[p.Key]

			switch {
			case !ok:
				iso.unknown.Add(1)
			case tn != r.Tailnet:
				iso.violations.Add(1)
				iso.sample(fmt.Sprintf("node %d (tailnet %q) got peer %s of tailnet %q",
					r.Index, r.Tailnet, p.Key.ShortString(), tn))
			}
		}
	}
}

func (iso *isolation) sample(s string) {
	iso.mu.Lock()
	defer iso.mu.Unlock()

	if len(iso.samples) < maxIsolationSamples {
		iso.samples = append(iso.samples, s)
	}
}

type isolationSummary struct {
	FramesDecoded int64    `json:"frames_decoded"`
	PeersChecked  int64    `json:"peers_checked"`
	Violations    int64    `json:"violations"`
	UnknownPeers  int64    `json:"unknown_peers"`
	DecodeErrors  int64    `json:"decode_errors"`
	Samples       []string `json:"violation_samples,omitempty"`
}

func (iso *isolation) summary() isolationSummary {
	iso.mu.Lock()
	defer iso.mu.Unlock()

	return isolationSummary{
		FramesDecoded: iso.frames.Load(), PeersChecked: iso.peers.Load(), Violations: iso.violations.Load(),
		UnknownPeers: iso.unknown.Load(), DecodeErrors: iso.decodeErrors.Load(), Samples: slices.Clone(iso.samples),
	}
}

// pickObservers chooses k streams of the first phase: round-robin over the
// tailnets, consecutive nodes within each (so a user's devices, which are
// consecutive in the plan, are observed together).
func pickObservers(rs []*result, k int) {
	var (
		order  []string
		byTail = map[string][]*result{}
	)

	for _, r := range rs {
		if _, ok := byTail[r.Tailnet]; !ok {
			order = append(order, r.Tailnet)
		}

		byTail[r.Tailnet] = append(byTail[r.Tailnet], r)
	}

	for picked, round := 0, 0; picked < k; round++ {
		progress := false

		for _, tn := range order {
			if picked == k {
				break
			}

			if round < len(byTail[tn]) {
				r := byTail[tn][round]
				r.obs = newObserver(r)
				picked++
				progress = true
			}
		}

		if !progress {
			break
		}
	}
}
