package hscontrol

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// P09, S17 (owner decision Q5): evidence to tell, after a process was
// paused past its lease (F02: SIGSTOP, SIGCONT), whether a frame it counted
// was written before the pause or after the resume, and when its last
// servable() check passed. Process-wide values of the latest frame written
// to any client, on the metrics listener only (AGENTS 9): no node, tailnet
// or key in them.

// lastFrame is the latest frame written by this process: the time its last
// servable() check passed (gate) and the time its socket write returned
// (done). Kept together so the pair always describes one frame.
var lastFrame struct {
	sync.Mutex

	gate, done int64 // unix nanoseconds
}

func recordFrameWritten(gate, done time.Time) {
	lastFrame.Lock()
	defer lastFrame.Unlock()

	if done.UnixNano() > lastFrame.done {
		lastFrame.gate, lastFrame.done = gate.UnixNano(), done.UnixNano()
	}
}

// lastFrameTimes returns the gate and done times (Unix ns) of the latest
// frame written.
func lastFrameTimes() (int64, int64) {
	lastFrame.Lock()
	defer lastFrame.Unlock()

	return lastFrame.gate, lastFrame.done
}

var (
	_ = promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: prometheusNamespace,
		Name:      "mapresponse_last_gate_unix_nanos",
		Help:      "Unix time (ns) at which the last servable() check of the latest map frame written by this process passed.",
	}, func() float64 { g, _ := lastFrameTimes(); return float64(g) })

	_ = promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: prometheusNamespace,
		Name:      "mapresponse_last_write_unix_nanos",
		Help:      "Unix time (ns) at which the socket write of the latest map frame written by this process returned.",
	}, func() float64 { _, d := lastFrameTimes(); return float64(d) })

	// mapResponseNotServable counts frames dropped by the servable() gate,
	// by where the gate refused them.
	mapResponseNotServable = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "mapresponse_not_servable_total",
		Help:      "Map frames not written because the runtime was retired or the cluster lease was no longer held, by stage (before_encode, before_write).",
	}, []string{"stage"})
)

// mapWriteHook, when set (tests only), runs after a frame is encoded and
// before it is written: the place where a pause can separate the first
// servable() check from the socket write.
var mapWriteHook func()
