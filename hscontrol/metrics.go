package hscontrol

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tenantlabel"
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
	// tailnetMapSent is mapresponse_sent_total (updates and keepalives) by
	// tailnet (P08b): what a change in one tailnet sent to clients of
	// another is visible per tailnet.
	tailnetMapSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Name:      "tailnet_mapresponses_sent_total",
		Help:      "Map frames written to clients, by tailnet and type (update, keepalive); tailnets past the label cap are summed under tailnet=\"other\".",
	}, []string{"tailnet", "type"})
	// tailnetMapSessions is the open long-poll map sessions by tailnet
	// (P08b): the tailnet ID while the label cap allows, "other" past it.
	tailnetMapSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: prometheusNamespace,
		Name:      "tailnet_map_sessions",
		Help:      "Open long-poll map sessions by tailnet; tailnets past the label cap are summed under tailnet=\"other\".",
	}, []string{"tailnet"})
)

// P13f L4: a tailnet's series of these vectors are deleted when its label
// is released (its runtime stopped).
func init() {
	tenantlabel.Register(tailnetMapSent, tailnetMapSessions, tailnetAPIRequests)
}
