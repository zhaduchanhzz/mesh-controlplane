package bootstrap

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// requests counts POST /v1/bootstrap by outcome (P13f), on the operator
// metrics listener of mesh-bootstrap only. Outcomes are fixed strings: no
// key, tenant or client address is a label.
var requests = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "mesh_bootstrap",
	Name:      "requests_total",
	Help: "Bootstrap requests by outcome: ok, refused, invalid, moving, unavailable, " +
		"limited_ip, limited_global, limited_tenant.",
}, []string{"outcome"})

func init() {
	for _, o := range []string{
		OutcomeOK.String(), OutcomeRefused.String(), OutcomeInvalid.String(), OutcomeMoving.String(),
		OutcomeUnavailable.String(), "limited_ip", "limited_global", "limited_tenant",
	} {
		requests.WithLabelValues(o)
	}
}
