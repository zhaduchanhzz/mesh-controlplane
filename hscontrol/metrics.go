package hscontrol

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"tailscale.com/envknob"
)

var debugHighCardinalityMetrics = envknob.Bool("HEADSCALE_DEBUG_HIGH_CARDINALITY_METRICS")

var mapResponseLastSentSeconds *prometheus.GaugeVec

func init() {
	if debugHighCardinalityMetrics {
		mapResponseLastSentSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "mapresponse_last_sent_seconds",
			Help:      "last sent metric to node.id",
		}, []string{"type", "id"})
	}
}

const prometheusNamespace = "headscale"

var (
	mapResponseSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "mapresponse_sent_total",
		Help:      "total count of mapresponses sent to clients",
	}, []string{"status", "type"})
	mapResponseEndpointUpdates = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "mapresponse_endpoint_updates_total",
		Help:      "total count of endpoint updates received",
	}, []string{"status"})
	mapResponseEnded = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "mapresponse_ended_total",
		Help:      "total count of new mapsessions ended",
	}, []string{"reason"})
	// tailnetMapSessions is the open long-poll map sessions by tailnet
	// (P08b): the tailnet ID while the label cap allows, "other" past it.
	tailnetMapSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: prometheusNamespace,
		Name:      "tailnet_map_sessions",
		Help:      "Open long-poll map sessions by tailnet; tailnets past the label cap are summed under tailnet=\"other\".",
	}, []string{"tailnet"})
)
