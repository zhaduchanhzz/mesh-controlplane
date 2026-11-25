// Package tenantlabel bounds the `tailnet` label of per-tailnet metrics
// (P08b, owner decision Q5): the label is the tailnet ID (never the slug,
// which tenants choose), and at most Limit distinct IDs get a label of their
// own per process. Tailnets past the cap share the label [Other], so a
// cluster serving many tailnets cannot blow up the metric cardinality.
//
// Per-tailnet metrics are served only on the operator metrics listener
// (AGENTS 9): a tailnet ID is an identifier of tenant data.
package tenantlabel

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Other is the label shared by tailnets past the cap, and by callers that
// have no tailnet ID.
const Other = "other"

// DefaultLimit is the number of tailnets labelled by their ID per process
// (owner decision Q5: 2000 tailnets per cluster).
const DefaultLimit = 2000

var (
	mu       sync.Mutex
	limit    = DefaultLimit
	admitted = map[string]struct{}{}

	labelled = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "headscale",
		Name:      "tailnet_metric_labels",
		Help:      "Tailnets with a tailnet label of their own on per-tailnet metrics; the rest share the label \"other\".",
	})
	overflow = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "headscale",
		Name:      "tailnet_metric_label_overflow_total",
		Help:      "Per-tailnet metric updates recorded under \"other\" because the tailnet label cap was reached.",
	})
)

// SetLimit sets the cap; n <= 0 restores [DefaultLimit]. Tailnets already
// labelled keep their label. Call it before runtimes start.
func SetLimit(n int) {
	mu.Lock()
	defer mu.Unlock()

	if n <= 0 {
		n = DefaultLimit
	}

	limit = n
}

// Label returns the metric label of the tailnet id: id itself while the
// cap allows (once labelled, always labelled), otherwise [Other].
func Label(id string) string {
	if id == "" {
		return Other
	}

	mu.Lock()
	defer mu.Unlock()

	if _, ok := admitted[id]; ok {
		return id
	}

	if len(admitted) >= limit {
		overflow.Inc()

		return Other
	}

	admitted[id] = struct{}{}
	labelled.Set(float64(len(admitted)))

	return id
}

// reset forgets every label (tests).
func reset() {
	mu.Lock()
	defer mu.Unlock()

	admitted = map[string]struct{}{}
	limit = DefaultLimit
	labelled.Set(0)
}
