package lease

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Lease metrics (P13f), served by the operator metrics listener of the
// cluster process (never the public one, AGENTS rule 9). Labels are the
// cluster ID and the holder name of the process (host name plus a random
// suffix per start); neither is tenant data.
//
// held/epoch/deadline are read from the running managers at scrape time, so
// a stopped manager disappears instead of leaving a stale "held 1".

// Lease events counted in headscale_cluster_lease_events_total.
const (
	eventAcquired     = "acquired"
	eventAcquireLate  = "acquire_late"
	eventAcquireError = "acquire_error"
	eventRenewError   = "renew_error"
	eventLost         = "lost"
	eventSelfFence    = "self_fence"
	eventStartFailed  = "start_failed"
	eventReleased     = "released"
)

const (
	labelCluster = "cluster"
	labelHolder  = "holder"
)

var (
	renewDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "headscale",
		Name:      "cluster_lease_renew_duration_seconds",
		Help:      "Time one cluster lease renewal took (successful or not), by cluster.",
		Buckets:   []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{labelCluster})

	leaseEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "headscale",
		Name:      "cluster_lease_events_total",
		Help: "Cluster lease events by cluster: acquired, acquire_late (acquired too late to serve), " +
			"acquire_error, renew_error, lost (expired or taken over), self_fence (deadline passed " +
			"without a renewal), start_failed (loading the tailnets failed), released.",
	}, []string{labelCluster, "event"})

	heldDesc = prometheus.NewDesc("headscale_cluster_lease_held",
		"1 while this process holds the lease of its cluster and may serve, 0 while standby.",
		[]string{labelCluster, labelHolder}, nil)
	epochDesc = prometheus.NewDesc("headscale_cluster_lease_epoch",
		"Epoch of the last lease this process acquired (0 before the first).",
		[]string{labelCluster, labelHolder}, nil)
	deadlineDesc = prometheus.NewDesc("headscale_cluster_lease_deadline_seconds",
		"Seconds until this process stops serving on its own unless a renewal succeeds (self-fence "+
			"deadline, monotonic clock); 0 while standby.",
		[]string{labelCluster, labelHolder}, nil)
)

// running is the set of managers inside Run.
var running = struct {
	mu sync.Mutex
	m  map[*Manager]struct{}
}{m: map[*Manager]struct{}{}}

func init() {
	prometheus.MustRegister(managerCollector{})
}

func register(m *Manager) {
	running.mu.Lock()
	running.m[m] = struct{}{}
	running.mu.Unlock()
}

func unregister(m *Manager) {
	running.mu.Lock()
	delete(running.m, m)
	running.mu.Unlock()
}

func countEvent(cluster, event string) {
	leaseEvents.WithLabelValues(cluster, event).Inc()
}

// observe reports the lease state of m: held, last epoch, time left.
func (m *Manager) observe() (bool, uint64, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	left := time.Until(m.deadline)
	if !m.active || left <= 0 {
		return false, m.cur.Epoch, 0
	}

	return true, m.cur.Epoch, left
}

type managerCollector struct{}

func (managerCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{heldDesc, epochDesc, deadlineDesc} {
		ch <- d
	}
}

func (managerCollector) Collect(ch chan<- prometheus.Metric) {
	running.mu.Lock()

	ms := make([]*Manager, 0, len(running.m))
	for m := range running.m {
		ms = append(ms, m)
	}

	running.mu.Unlock()

	// Two managers with one cluster and holder (tests) would make the
	// scrape fail; the first one is reported.
	seen := make(map[[2]string]bool, len(ms))

	for _, m := range ms {
		k := [2]string{m.cluster, m.holder}
		if seen[k] {
			continue
		}

		seen[k] = true

		held, epoch, left := m.observe()

		h := 0.0
		if held {
			h = 1
		}

		for _, v := range []struct {
			d *prometheus.Desc
			v float64
		}{{heldDesc, h}, {epochDesc, float64(epoch)}, {deadlineDesc, left.Seconds()}} {
			ch <- prometheus.MustNewConstMetric(v.d, prometheus.GaugeValue, v.v, m.cluster, m.holder)
		}
	}
}
