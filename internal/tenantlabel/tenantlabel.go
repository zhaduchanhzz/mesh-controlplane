// Package tenantlabel bounds the `tailnet` label of per-tailnet metrics
// (P08b, owner decision Q5): the label is the tailnet ID (never the slug,
// which tenants choose), and at most Limit distinct IDs get a label of their
// own per process. Tailnets past the cap share the label [Other], so a
// cluster serving many tailnets cannot blow up the metric cardinality.
//
// P13f L4: a label is held only by the tailnets this process serves. A
// tailnet runtime takes it with [Acquire] when it is built (an owned,
// ACTIVE tailnet under the cluster lease) and gives it back with [Release]
// when it stops; [Label] only looks a label up and never takes one, so a
// request naming a tailnet this process does not serve (another cluster's,
// a retired one) costs no label. The last Release of a tailnet deletes its
// series from every vector given to [Register].
//
// Per-tailnet metrics are served only on the operator metrics listener
// (AGENTS 9): a tailnet ID is an identifier of tenant data.
package tenantlabel

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Other is the label shared by tailnets past the cap, by tailnets this
// process does not serve, and by callers that have no tailnet ID.
const Other = "other"

// DefaultLimit is the number of tailnets labelled by their ID per process
// (owner decision Q5: 2000 tailnets per cluster).
const DefaultLimit = 2000

// labelName is the label of every per-tailnet vector.
const labelName = "tailnet"

// Deleter is a metric vector with a "tailnet" label
// (*prometheus.CounterVec, *prometheus.GaugeVec, ...).
type Deleter interface {
	DeletePartialMatch(labels prometheus.Labels) int
}

var (
	mu    sync.Mutex
	limit = DefaultLimit
	// held counts, per labelled tailnet, the runtimes holding its label.
	held = map[string]int{}
	vecs []Deleter

	labelled = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "headscale",
		Name:      "tailnet_metric_labels",
		Help:      "Served tailnets with a tailnet label of their own on per-tailnet metrics; the rest share the label \"other\".",
	})
	overflow = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "headscale",
		Name:      "tailnet_metric_label_overflow_total",
		Help:      "Tailnet runtimes that got the label \"other\" because the tailnet label cap was reached.",
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

// Register adds per-tailnet vectors whose series of a tailnet are deleted
// when its label is released. Call it from init.
func Register(vs ...Deleter) {
	mu.Lock()
	defer mu.Unlock()

	vecs = append(vecs, vs...)
}

// Acquire takes the label of tailnet id for one runtime that serves it and
// returns it: id while the cap allows, otherwise [Other]. Every Acquire
// that returned id must be matched by one [Release] of id.
func Acquire(id string) string {
	if id == "" {
		return Other
	}

	mu.Lock()
	defer mu.Unlock()

	if n, ok := held[id]; ok {
		held[id] = n + 1

		return id
	}

	if len(held) >= limit {
		overflow.Inc()

		return Other
	}

	held[id] = 1
	labelled.Set(float64(len(held)))

	return id
}

// Release gives back one runtime's label of tailnet id. The last release
// forgets the label and deletes the tailnet's series. Releasing a tailnet
// that holds no label does nothing.
func Release(id string) {
	mu.Lock()
	defer mu.Unlock()

	n, ok := held[id]
	if !ok {
		return
	}

	if n > 1 {
		held[id] = n - 1

		return
	}

	delete(held, id)
	labelled.Set(float64(len(held)))

	for _, v := range vecs {
		v.DeletePartialMatch(prometheus.Labels{labelName: id})
	}
}

// Label returns the metric label of tailnet id: id while a runtime of this
// process holds it, otherwise [Other]. It never takes a label.
func Label(id string) string {
	if id == "" {
		return Other
	}

	mu.Lock()
	defer mu.Unlock()

	if _, ok := held[id]; ok {
		return id
	}

	return Other
}

// reset forgets every label (tests).
func reset() {
	mu.Lock()
	defer mu.Unlock()

	held = map[string]int{}
	limit = DefaultLimit

	labelled.Set(0)
}
