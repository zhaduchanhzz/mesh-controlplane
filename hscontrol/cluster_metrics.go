package hscontrol

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tenantlabel"
)

// Cluster metrics (P13f), on the operator metrics listener only (AGENTS
// rule 9). Lease metrics are in internal/lease, fence refusals in
// tenantscope, pool statistics in internal/dbstats.

// Kinds of a tailnet's settings a reconcile reloads (L9).
const (
	reloadPolicy = "policy"
	reloadDNS    = "dns"
)

var (
	reconcileDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: prometheusNamespace,
		Name:      "cluster_reconcile_duration_seconds",
		Help:      "Time one reconcile of the served tailnets took (hand-overs, take-overs, loads, retires, reloads) on the active process.",
		Buckets:   []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	})

	// tailnetReloadFailures counts failed reloads of a tailnet's policy or
	// DNS settings (P13f L9): the revision stays unapplied and reconcile
	// retries it.
	tailnetReloadFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "tailnet_reload_failures_total",
		Help: "Failed reloads of a tailnet's new policy or DNS settings, by tailnet and kind (policy, dns); " +
			"the previous settings stay in force and the reload is retried every reconcile.",
	}, []string{"tailnet", "kind"})

	runtimesDesc = prometheus.NewDesc(prometheus.BuildFQName(prometheusNamespace, "cluster", "tailnet_runtimes"),
		"Tailnet runtimes of this process by state: serving (routable), stopping (retired, draining), "+
			"reload_pending (serving, with a policy or DNS revision not applied yet).",
		[]string{"cluster", "holder", "state"}, nil)
)

func init() {
	tenantlabel.Register(tailnetReloadFailures)
	prometheus.MustRegister(clusterCollector{})
}

// servingClusters is the set of clusters inside Serve.
var servingClusters = struct {
	mu sync.Mutex
	m  map[*Cluster]struct{}
}{m: map[*Cluster]struct{}{}}

func registerCluster(c *Cluster) func() {
	servingClusters.mu.Lock()
	servingClusters.m[c] = struct{}{}
	servingClusters.mu.Unlock()

	return func() {
		servingClusters.mu.Lock()
		delete(servingClusters.m, c)
		servingClusters.mu.Unlock()
	}
}

type clusterCollector struct{}

func (clusterCollector) Describe(ch chan<- *prometheus.Desc) { ch <- runtimesDesc }

func (clusterCollector) Collect(ch chan<- prometheus.Metric) {
	servingClusters.mu.Lock()

	cs := make([]*Cluster, 0, len(servingClusters.m))
	for c := range servingClusters.m {
		cs = append(cs, c)
	}

	servingClusters.mu.Unlock()

	seen := map[[2]string]bool{}

	for _, c := range cs {
		k := [2]string{c.platform.ClusterID(), c.lease.Holder()}
		if seen[k] {
			continue
		}

		seen[k] = true

		c.mu.RLock()

		serving, stopping, pending := len(c.runtimes), len(c.retiring), 0

		for _, rt := range c.runtimes {
			if rt.policyPending.Load() || rt.configPending.Load() {
				pending++
			}
		}

		c.mu.RUnlock()

		for state, n := range map[string]int{"serving": serving, "stopping": stopping, "reload_pending": pending} {
			ch <- prometheus.MustNewConstMetric(runtimesDesc, prometheus.GaugeValue, float64(n), k[0], k[1], state)
		}
	}
}
