package hscontrol

import (
	"net"
	"net/http"
	"net/netip"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/opsmetrics"
)

// operatorLoopbackAddr is what handlers behind [operatorOnly] see as the
// peer of an admitted operator: tsweb's own debug check (loopback or the
// Tailscale range) runs inside the debug mux, and the gate below is the only
// authority on this listener.
const operatorLoopbackAddr = "127.0.0.1:1"

// operatorOnly guards the metrics/debug listener (P08; S14, I09, AGENTS
// rule 9). A request is admitted only when its TCP peer is loopback or inside
// one of cidrs (metrics_operator_cidrs, empty by default):
//
//   - private ranges (RFC 1918, ULA) are not operators by default;
//   - the Tailscale range (100.64.0.0/10, fd7a:115c:a1e0::/48) is never an
//     operator, since every tenant node owns addresses there (config
//     validation refuses such CIDRs, and the check is repeated here);
//   - X-Forwarded-For and similar headers are never read: a reverse proxy in
//     front of this listener would make every caller look like the proxy, so
//     do not put one there (runbook, P08a N7).
//
// Everything on the listener goes through it, /metrics included.
func operatorOnly(cidrs []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, ok := peerIP(r.RemoteAddr)
		if !ok || !isOperatorIP(ip, cidrs) {
			log.Debug().Str("peer", r.RemoteAddr).Str("path", r.URL.Path).Msg("operator listener: refused")
			http.Error(w, "forbidden", http.StatusForbidden)

			return
		}

		if !ip.IsLoopback() {
			r = r.Clone(r.Context())
			r.RemoteAddr = operatorLoopbackAddr
		}

		r.Header.Del("X-Forwarded-For")
		next.ServeHTTP(w, r)
	})
}

func peerIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}

	return ip.Unmap(), true
}

// isOperatorIP is the one operator rule of every metrics listener
// (opsmetrics.IsOperatorIP; P13f shares it with mesh-bootstrap and
// mesh-placement).
func isOperatorIP(ip netip.Addr, cidrs []netip.Prefix) bool {
	return opsmetrics.IsOperatorIP(ip, cidrs)
}
