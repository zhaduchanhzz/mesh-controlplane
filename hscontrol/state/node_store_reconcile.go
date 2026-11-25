package state

import (
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// Run-time reconciliation of the incremental netmap (P00d, ADR 0005
// measure 3): a full build in the background, compared with the adjacency
// the incremental path produced. Any difference replaces the adjacency
// with the full build and names the nodes whose peers differed, so the
// caller can send them a full map.

// reconcilePeerMaps is the NodeStore op that runs a reconciliation on the
// writer, against the snapshot the batch publishes.
const reconcilePeerMaps = 7

var (
	nodeStoreNetmapDivergence = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "nodestore_netmap_divergence_total",
		Help: "Peer entries in which the incremental adjacency differed from a full build at reconciliation: " +
			"extra (listed, the full build does not: an isolation fault) or missing.",
	}, []string{"kind"})
	// tailnetNetmapDivergence is nodestore_netmap_divergence_total by
	// tailnet (P08b, R10 of P00d): which tenant an isolation fault is in.
	tailnetNetmapDivergence = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "tailnet_netmap_divergence_total",
		Help:      "nodestore_netmap_divergence_total by tailnet; tailnets past the label cap are summed under tailnet=\"other\".",
	}, []string{"tailnet", "kind"})
	nodeStoreReconcileDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: prometheusNamespace,
		Name:      "nodestore_netmap_reconcile_duration_seconds",
		Help:      "Duration of a netmap reconciliation (full build and comparison).",
		Buckets:   prometheus.ExponentialBuckets(0.001, 4, 8), //nolint:mnd
	})
	nodeStoreReconciles = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "nodestore_netmap_reconciles_total",
		Help: "Netmap reconciliations by outcome: equal, diverged, or skipped " +
			"(the policy rules moved since the adjacency was built; a rebuild follows).",
	}, []string{"outcome"})
)

// ReconcileResult is what a reconciliation found.
type ReconcileResult struct {
	// Checked is false when nothing was compared: no reconcile builder is
	// set, or the policy rules moved since the adjacency was built.
	Checked bool
	// Diverged lists the nodes whose peers differed; their adjacency is
	// now the full build's. They need a full map.
	Diverged []types.NodeID
	// Extra and Missing count differing peer entries.
	Extra, Missing int
}

// SetReconcilePeers sets the full build reconciliation compares with: it
// must build adjacency for the nodes under the current policy without
// changing the policy manager, and return the rules generation it holds
// for. Call before [NodeStore.Start].
func (s *NodeStore) SetReconcilePeers(fn peerBuilder) {
	s.fullPeers = fn
}

// ReconcilePeerMaps compares the current adjacency with a full build on the
// writer goroutine and replaces it on any difference. It blocks until done.
func (s *NodeStore) ReconcilePeerMaps() ReconcileResult {
	result := make(chan ReconcileResult, 1)

	w := work{
		op:              reconcilePeerMaps,
		reconcileResult: result,
	}

	select {
	case s.writeQueue <- w:
	case <-s.stopped:
		return ReconcileResult{}
	}

	return <-result
}

// reconcileSnapshot runs a reconciliation against snap before it is
// published, replacing its adjacency on a difference.
func (s *NodeStore) reconcileSnapshot(snap *Snapshot) ReconcileResult {
	if s.fullPeers == nil {
		return ReconcileResult{}
	}

	timer := prometheus.NewTimer(nodeStoreReconcileDuration)
	defer timer.ObserveDuration()

	full, gen := s.fullPeers(snap.allNodes)
	if gen != snap.rulesGen {
		nodeStoreReconciles.WithLabelValues("skipped").Inc()

		return ReconcileResult{}
	}

	res := ReconcileResult{Checked: true}

	ids := make(map[types.NodeID]struct{}, len(snap.nodesByID))
	for id := range snap.peersByNode {
		ids[id] = struct{}{}
	}

	for id := range full {
		ids[id] = struct{}{}
	}

	for id := range ids {
		have := slices.Sorted(slices.Values(snap.peersByNode[id]))
		want := slices.Sorted(slices.Values(full[id]))

		if slices.Equal(have, want) {
			continue
		}

		res.Diverged = append(res.Diverged, id)

		extra, missing := 0, 0

		for _, p := range have {
			if !slices.Contains(want, p) {
				extra++
			}
		}

		for _, p := range want {
			if !slices.Contains(have, p) {
				missing++
			}
		}

		// A duplicate entry differs without being extra or missing.
		if extra == 0 && missing == 0 {
			extra++
		}

		res.Extra += extra
		res.Missing += missing
	}

	if len(res.Diverged) == 0 {
		nodeStoreReconciles.WithLabelValues("equal").Inc()

		return res
	}

	nodeStoreReconciles.WithLabelValues("diverged").Inc()
	nodeStoreNetmapDivergence.WithLabelValues("extra").Add(float64(res.Extra))
	nodeStoreNetmapDivergence.WithLabelValues("missing").Add(float64(res.Missing))
	tailnetNetmapDivergence.WithLabelValues(s.metricLabel, "extra").Add(float64(res.Extra))
	tailnetNetmapDivergence.WithLabelValues(s.metricLabel, "missing").Add(float64(res.Missing))

	var entries int64
	for _, peers := range full {
		entries += int64(len(peers))
	}

	snap.peersByNode, snap.peerEntries = full, entries

	slices.Sort(res.Diverged)

	// Counts only: node IDs and addresses stay out of the log.
	log.Warn().
		Int("nodes", len(res.Diverged)).
		Int("extra", res.Extra).
		Int("missing", res.Missing).
		Msg("incremental netmap diverged from a full build; replaced with the full build")

	return res
}
