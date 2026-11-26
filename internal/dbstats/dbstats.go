// Package dbstats exports the connection pool statistics of database/sql
// pools (P13f): how many connections are open, in use and idle, and how
// often and how long callers waited for one. A pool saturated by one
// cluster's load shows here before its lease renewals or fenced writes
// time out (ADR 0006 §3.7).
//
// Pools are registered by name ("platform" for a cluster process,
// "bootstrap", "placement"); several pools under one name (tests running
// many processes in one binary) are summed. The statistics come from
// sql.DB.Stats(): no query reaches the database.
package dbstats

import (
	"database/sql"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type pool struct {
	name string
	db   *sql.DB
}

var pools = struct {
	mu sync.Mutex
	m  map[*pool]struct{}
}{m: map[*pool]struct{}{}}

func init() {
	prometheus.MustRegister(collector{})
}

// Register exports db's statistics under pool=name until unregister is
// called (before or after db is closed).
func Register(name string, db *sql.DB) (unregister func()) {
	p := &pool{name: name, db: db}

	pools.mu.Lock()
	pools.m[p] = struct{}{}
	pools.mu.Unlock()

	var once sync.Once

	return func() {
		once.Do(func() {
			pools.mu.Lock()
			delete(pools.m, p)
			pools.mu.Unlock()
		})
	}
}

func desc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc("headscale_db_pool_"+name, help, []string{"pool"}, nil)
}

var (
	maxOpenDesc  = desc("max_open_connections", "Maximum number of open connections of the pool (0: unlimited).")
	openDesc     = desc("open_connections", "Open connections, in use and idle.")
	inUseDesc    = desc("in_use_connections", "Connections in use.")
	idleDesc     = desc("idle_connections", "Idle connections.")
	waitCount    = desc("wait_count_total", "Times a caller waited for a connection because the pool was at its maximum.")
	waitDuration = desc("wait_duration_seconds_total", "Total time callers waited for a connection.")
	idleClosed   = desc("max_idle_closed_total", "Connections closed because the pool had max idle connections.")
	idleTimeout  = desc("max_idle_time_closed_total", "Connections closed because they were idle too long.")
	lifetime     = desc("max_lifetime_closed_total", "Connections closed because they reached their maximum lifetime.")
)

type collector struct{}

func (collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{maxOpenDesc, openDesc, inUseDesc, idleDesc, waitCount, waitDuration, idleClosed, idleTimeout, lifetime} {
		ch <- d
	}
}

func (collector) Collect(ch chan<- prometheus.Metric) {
	pools.mu.Lock()

	sum := map[string]sql.DBStats{}

	for p := range pools.m {
		s := p.db.Stats()
		t := sum[p.name]
		t.MaxOpenConnections += s.MaxOpenConnections
		t.OpenConnections += s.OpenConnections
		t.InUse += s.InUse
		t.Idle += s.Idle
		t.WaitCount += s.WaitCount
		t.WaitDuration += s.WaitDuration
		t.MaxIdleClosed += s.MaxIdleClosed
		t.MaxIdleTimeClosed += s.MaxIdleTimeClosed
		t.MaxLifetimeClosed += s.MaxLifetimeClosed
		sum[p.name] = t
	}

	pools.mu.Unlock()

	for name, s := range sum {
		ch <- prometheus.MustNewConstMetric(maxOpenDesc, prometheus.GaugeValue, float64(s.MaxOpenConnections), name)
		ch <- prometheus.MustNewConstMetric(openDesc, prometheus.GaugeValue, float64(s.OpenConnections), name)
		ch <- prometheus.MustNewConstMetric(inUseDesc, prometheus.GaugeValue, float64(s.InUse), name)
		ch <- prometheus.MustNewConstMetric(idleDesc, prometheus.GaugeValue, float64(s.Idle), name)
		ch <- prometheus.MustNewConstMetric(waitCount, prometheus.CounterValue, float64(s.WaitCount), name)
		ch <- prometheus.MustNewConstMetric(waitDuration, prometheus.CounterValue, s.WaitDuration.Seconds(), name)
		ch <- prometheus.MustNewConstMetric(idleClosed, prometheus.CounterValue, float64(s.MaxIdleClosed), name)
		ch <- prometheus.MustNewConstMetric(idleTimeout, prometheus.CounterValue, float64(s.MaxIdleTimeClosed), name)
		ch <- prometheus.MustNewConstMetric(lifetime, prometheus.CounterValue, float64(s.MaxLifetimeClosed), name)
	}
}
