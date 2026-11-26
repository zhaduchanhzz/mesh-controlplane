package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// Changes driven by clients, as tailscaled sends them: a non-streaming
// MapRequest with OmitPeers ("lite" update, hscontrol/poll.go) carrying new
// endpoints or a new Hostinfo. Probes time one change from the moment it is
// sent to the frame that carries it at every observer whose netmap holds
// the changed node; churn sends endpoint changes at a steady rate.

var errLiteStatus = errors.New("lite update: HTTP")

// markerEndpoint is the unique endpoint of change c: 198.18.0.0/15 is the
// benchmark range (RFC 2544), never a real address.
func markerEndpoint(c uint64) netip.AddrPort {
	return netip.AddrPortFrom(
		netip.AddrFrom4([4]byte{198, 18 + byte(c>>24&1), byte(c >> 16), byte(c >> 8)}), //nolint:gosec // wraps on purpose
		uint16(1024+c%60000), //nolint:gosec // < 61024
	)
}

// changeSeq numbers every client-driven change of the run.
var changeSeq atomic.Uint64

// liteUpdate reports r's endpoints and Hostinfo (r.state) to the server.
func liteUpdate(ctx context.Context, cfg config, r *result) error {
	r.mu.Lock()
	body, err := json.Marshal(&tailcfg.MapRequest{
		Version:   tailcfg.CurrentCapabilityVersion,
		NodeKey:   r.node.Public(),
		DiscoKey:  r.disco,
		Stream:    false,
		OmitPeers: true,
		Hostinfo:  r.hi,
		Endpoints: slices.Clone(r.endpoints),
	})
	direct := r.direct
	r.mu.Unlock()

	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.mapURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := direct.DoNoiseRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w %d", errLiteStatus, resp.StatusCode)
	}

	return nil
}

// setEndpoint gives r a new unique endpoint and returns it.
func setEndpoint(r *result) netip.AddrPort {
	ep := markerEndpoint(changeSeq.Add(1))

	r.mu.Lock()
	r.endpoints = []netip.AddrPort{ep}
	r.mu.Unlock()

	return ep
}

// setServiceMarker gives r a Hostinfo whose advertised service list is one
// service with a new unique description, and returns it. Peers see only
// part of a Hostinfo (types.PeerHostinfo: hostname, OS, services, SSH host
// keys, app connector); a change elsewhere (OSVersion, …) is stored but not
// sent to peers, so the probe changes the services, as a tailscaled
// starting or stopping a service would.
func setServiceMarker(r *result) string {
	n := changeSeq.Add(1)
	marker := "capload-" + strconv.FormatUint(n, 10)

	r.mu.Lock()
	hi := *r.hi
	hi.Services = []tailcfg.Service{{Proto: tailcfg.TCP, Port: uint16(1024 + n%60000), Description: marker}} //nolint:gosec // < 61024
	r.hi = &hi
	r.mu.Unlock()

	return marker
}

type churnSummary struct {
	PerMinute float64 `json:"changes_per_min"`
	Sent      int64   `json:"sent"`
	OK        int64   `json:"ok"`
	Failed    int64   `json:"failed"`
	Request   dist    `json:"request"`
}

// churn sends endpoint changes of random live nodes at perMin changes per
// minute until ctx ends. At most 64 requests are in flight; a tick with no
// free slot is skipped (counted as failed: the server is not keeping up).
func churn(ctx context.Context, cfg config, live []*result, perMin float64, rng *rand.Rand) churnSummary {
	s := churnSummary{PerMinute: perMin}
	if perMin <= 0 || len(live) == 0 {
		return s
	}

	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		lat             []time.Duration
		sent, ok, faild atomic.Int64
	)

	slots := make(chan struct{}, 64)
	t := time.NewTicker(time.Duration(float64(time.Minute) / perMin))

	defer t.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
		}

		r := live[rng.IntN(len(live))]
		if !r.alive.Load() {
			continue
		}

		select {
		case slots <- struct{}{}:
		default:
			faild.Add(1)

			continue
		}

		sent.Add(1)
		setEndpoint(r)

		wg.Go(func() {
			defer func() { <-slots }()

			// A change in flight when the window ends still completes.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.timeout)
			defer cancel()

			t0 := time.Now()

			err := liteUpdate(rctx, cfg, r)
			if err != nil {
				faild.Add(1)

				return
			}

			ok.Add(1)

			mu.Lock()
			defer mu.Unlock()

			lat = append(lat, time.Since(t0))
		})
	}

	wg.Wait()

	s.Sent, s.OK, s.Failed, s.Request = sent.Load(), ok.Load(), faild.Load(), distOf(lat)

	return s
}

// Probe kinds (probes.csv, summary.json).
const (
	probeEndpoint = "endpoint"
	probeHostinfo = "hostinfo"
)

// probeRec is one observation of one probe (probes.csv).
type probeRec struct {
	probe    int
	kind     string
	source   int
	tailnet  string
	sent     time.Time
	observer int
	latency  time.Duration // -1: not seen within the probe timeout
}

type probeSummary struct {
	Kind         string `json:"kind"`
	Probes       int    `json:"probes"`
	Skipped      int    `json:"skipped"`
	SendErrors   int    `json:"send_errors"`
	Observations int    `json:"observations"`
	Missed       int    `json:"missed"`
	// Latency is change sent -> frame received over every observation;
	// LastObserver is, per probe, the time until the last observer had it.
	Latency      dist `json:"latency"`
	LastObserver dist `json:"last_observer"`
	// OtherTailnetFrames (policy probes): frames with a packet filter received
	// by observers of other tailnets during the probe windows.
	OtherTailnetFrames int64 `json:"other_tailnet_filter_frames,omitempty"`
	Command            dist  `json:"command,omitzero"`
}

// probeRun times count client-driven changes, interval apart, alternating
// endpoint and Hostinfo changes.
func probeRun(ctx context.Context, cfg config, byKey map[key.NodePublic]*result, observers []*observer,
	rng *rand.Rand, recs *[]probeRec,
) []probeSummary {
	sums := map[string]*probeSummary{
		probeEndpoint: {Kind: probeEndpoint},
		probeHostinfo: {Kind: probeHostinfo},
	}
	lats := map[string][]time.Duration{}
	lasts := map[string][]time.Duration{}

	for i := range cfg.probes {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(cfg.probeInterval):
			}
		}

		kind := probeEndpoint
		if i%2 == 1 {
			kind = probeHostinfo
		}

		s := sums[kind]
		s.Probes++

		src, srcID := pickSource(byKey, observers, rng)
		if src == nil {
			s.Skipped++

			continue
		}

		// Observers whose netmap holds the source at the time of the change.
		var watch []*observer

		for _, o := range observers {
			if o.r.alive.Load() && o.hasPeer(srcID) {
				watch = append(watch, o)
			}
		}

		var match func(*tailcfg.MapResponse) bool

		if kind == probeEndpoint {
			ep := setEndpoint(src)
			match = func(mr *tailcfg.MapResponse) bool { return carriesEndpoint(mr, srcID, ep) }
		} else {
			marker := setServiceMarker(src)
			match = func(mr *tailcfg.MapResponse) bool { return carriesService(mr, srcID, marker) }
		}

		ws := make([]*waiter, len(watch))
		for j, o := range watch {
			ws[j] = o.wait(match)
		}

		sent := time.Now()

		err := liteUpdate(ctx, cfg, src)
		if err != nil {
			s.SendErrors++

			for j, o := range watch {
				o.cancel(ws[j])
			}

			continue
		}

		base := probeRec{probe: i, kind: kind, source: src.Index, tailnet: src.Tailnet, sent: sent}
		lat, last, missed := collect(watch, ws, sent, cfg.probeTimeout, base, recs)
		lats[kind] = append(lats[kind], lat...)
		s.Observations += len(watch)
		s.Missed += missed

		if missed == 0 && last > 0 {
			lasts[kind] = append(lasts[kind], last)
		}
	}

	out := make([]probeSummary, 0, len(sums))

	for _, kind := range []string{probeEndpoint, probeHostinfo} {
		s := sums[kind]
		s.Latency, s.LastObserver = distOf(lats[kind]), distOf(lasts[kind])
		out = append(out, *s)
	}

	return out
}

// collect waits for the waiters of one probe until timeout after sent and
// records one probeRec per observer (latency -1 when not seen in time). It
// returns the latencies seen, the slowest one and how many were missed.
func collect(watch []*observer, ws []*waiter, sent time.Time, timeout time.Duration, base probeRec,
	recs *[]probeRec,
) ([]time.Duration, time.Duration, int) {
	var (
		lat     []time.Duration
		worst   time.Duration
		missed  int
		expired bool
	)

	deadline := time.NewTimer(time.Until(sent.Add(timeout)))
	defer deadline.Stop()

	for j, o := range watch {
		rec := base
		rec.observer, rec.latency = o.r.Index, -1

		at, ok := await(ws[j], deadline.C, &expired)
		if ok {
			rec.latency = at.Sub(sent)
			lat = append(lat, rec.latency)
			worst = max(worst, rec.latency)
		} else {
			o.cancel(ws[j])

			missed++
		}

		*recs = append(*recs, rec)
	}

	return lat, worst, missed
}

// await prefers a frame that already arrived; once the deadline passed it
// no longer blocks.
func await(w *waiter, deadline <-chan time.Time, expired *bool) (time.Time, bool) {
	select {
	case at := <-w.done:
		return at, true
	default:
	}

	if *expired {
		return time.Time{}, false
	}

	select {
	case at := <-w.done:
		return at, true
	case <-deadline:
		*expired = true

		return time.Time{}, false
	}
}

// pickSource picks a live node capload drives that is a peer of a random
// observer (so at least one observer must see the change).
func pickSource(byKey map[key.NodePublic]*result, observers []*observer, rng *rand.Rand) (*result, tailcfg.NodeID) {
	for range 50 {
		o := observers[rng.IntN(len(observers))]
		if !o.r.alive.Load() {
			continue
		}

		o.mu.Lock()

		var cands []tailcfg.NodeID

		for id, k := range o.peers {
			if r, ok := byKey[k]; ok && r.alive.Load() {
				cands = append(cands, id)
			}
		}

		var (
			id tailcfg.NodeID
			r  *result
		)

		if len(cands) > 0 {
			slices.Sort(cands)
			id = cands[rng.IntN(len(cands))]
			r = byKey[o.peers[id]]
		}

		o.mu.Unlock()

		if r != nil {
			return r, id
		}
	}

	return nil, 0
}

func carriesEndpoint(mr *tailcfg.MapResponse, id tailcfg.NodeID, ep netip.AddrPort) bool {
	for _, pc := range mr.PeersChangedPatch {
		if pc.NodeID == id && slices.Contains(pc.Endpoints, ep) {
			return true
		}
	}

	for _, list := range [][]*tailcfg.Node{mr.PeersChanged, mr.Peers} {
		for _, n := range list {
			if n.ID == id && slices.Contains(n.Endpoints, ep) {
				return true
			}
		}
	}

	return false
}

func carriesService(mr *tailcfg.MapResponse, id tailcfg.NodeID, marker string) bool {
	for _, list := range [][]*tailcfg.Node{mr.PeersChanged, mr.Peers} {
		for _, n := range list {
			if n.ID != id || !n.Hostinfo.Valid() {
				continue
			}

			for _, s := range n.Hostinfo.Services().All() {
				if s.Description == marker {
					return true
				}
			}
		}
	}

	return false
}

// policyRun runs the operator's policy-change command (-policy-cmd, e.g.
// `capseed -mode policy-flip` over ssh) count times, interval apart, for
// tailnet tn, and times the first frame with a packet filter at every live
// observer of tn. Observers of other tailnets must not get a new packet
// filter: their filter frames during the windows are counted.
const policySettle = 5 * time.Second

func policyRun(ctx context.Context, cfg config, tn string, observers []*observer, recs *[]probeRec) probeSummary {
	s := probeSummary{Kind: "policy"}

	var lat, last, cmdDur []time.Duration

	for i := range cfg.policyChanges {
		// The first change waits too: frames of the previous phase (a
		// register burst changes filters in every tailnet it touches) must
		// not count as effects of this one.
		wait := cfg.policyInterval
		if i == 0 {
			wait = policySettle
		}

		select {
		case <-ctx.Done():
			return s
		case <-time.After(wait):
		}

		s.Probes++

		var (
			watch, others []*observer
			othersBefore  int64
		)

		for _, o := range observers {
			switch {
			case !o.r.alive.Load():
			case o.r.Tailnet == tn:
				watch = append(watch, o)
			default:
				others = append(others, o)
				othersBefore += o.filterFrames.Load()
			}
		}

		hasFilter := func(mr *tailcfg.MapResponse) bool { return mr.PacketFilter != nil || mr.PacketFilters != nil }

		ws := make([]*waiter, len(watch))
		for j, o := range watch {
			ws[j] = o.wait(hasFilter)
		}

		sent := time.Now()

		cmd := exec.CommandContext(ctx, "sh", "-c", cfg.policyCmd) //nolint:gosec // the operator's own command

		cmd.Env = append(os.Environ(), "CAPLOAD_TAILNET="+tn, "CAPLOAD_STEP="+strconv.Itoa(i))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr

		err := cmd.Run()

		cmdDur = append(cmdDur, time.Since(sent))

		if err != nil {
			s.SendErrors++

			for j, o := range watch {
				o.cancel(ws[j])
			}

			continue
		}

		base := probeRec{probe: i, kind: "policy", source: -1, tailnet: tn, sent: sent}
		got, worst, missed := collect(watch, ws, sent, cfg.probeTimeout, base, recs)
		lat = append(lat, got...)
		s.Observations += len(watch)
		s.Missed += missed

		if missed == 0 && worst > 0 {
			last = append(last, worst)
		}

		var othersAfter int64
		for _, o := range others {
			othersAfter += o.filterFrames.Load()
		}

		s.OtherTailnetFrames += othersAfter - othersBefore
	}

	s.Latency, s.LastObserver, s.Command = distOf(lat), distOf(last), distOf(cmdDur)

	return s
}
