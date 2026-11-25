package hscontrol

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestP08ClusterHostGuard (S11c): the public cluster listener answers only
// the server_url hostname. Another hostname (another cluster, a tenant-style
// name), a raw IP, a malformed Host, or a TLS connection whose SNI differs
// from Host gets 421 on every path, /health and /key included.
func TestP08ClusterHostGuard(t *testing.T) {
	t.Parallel()

	host, err := clusterHost("https://c1.cp.gta5web.online:8443")
	require.NoError(t, err)

	c := &Cluster{host: host}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := c.hostGuard(ok)

	tests := []struct {
		name string
		host string
		sni  string // "" = plain HTTP
		want int
	}{
		{"own host", "c1.cp.gta5web.online:8443", "", http.StatusOK},
		{"own host, TLS, SNI matches", "c1.cp.gta5web.online:8443", "c1.cp.gta5web.online", http.StatusOK},
		{"case and trailing dot", "C1.CP.gta5web.online.:8443", "c1.cp.gta5web.online", http.StatusOK},
		{"own host, no port", "c1.cp.gta5web.online", "", http.StatusOK},
		{"other cluster", "c2.cp.gta5web.online:8443", "", http.StatusMisdirectedRequest},
		{"tenant-style host", "a.cp.gta5web.online:8443", "", http.StatusMisdirectedRequest},
		{"raw IPv4", "116.118.3.46:8443", "", http.StatusMisdirectedRequest},
		{"raw IPv6", "[2001:db8::1]:8443", "", http.StatusMisdirectedRequest},
		{"empty", "", "", http.StatusMisdirectedRequest},
		{"bad port", "c1.cp.gta5web.online:99999", "", http.StatusMisdirectedRequest},
		{"suffix trick", "c1.cp.gta5web.online.evil.example", "", http.StatusMisdirectedRequest},
		{"TLS, SNI of another host", "c1.cp.gta5web.online:8443", "a.cp.gta5web.online", http.StatusMisdirectedRequest},
		{"TLS, SNI is our host, Host is not", "a.cp.gta5web.online:8443", "c1.cp.gta5web.online", http.StatusMisdirectedRequest},
		{"TLS, no SNI", "c1.cp.gta5web.online:8443", "-", http.StatusMisdirectedRequest},
	}

	for _, tt := range tests {
		for _, path := range []string{"/health", "/key", "/ts2021", "/"} {
			t.Run(tt.name+path, func(t *testing.T) {
				t.Parallel()

				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
				req.Host = tt.host

				switch tt.sni {
				case "":
					req.TLS = nil
				case "-":
					req.TLS = &tls.ConnectionState{}
				default:
					req.TLS = &tls.ConnectionState{ServerName: tt.sni}
				}

				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				assert.Equal(t, tt.want, rec.Code)
			})
		}
	}
}

// TestP08ClusterServerURLMustBeHostname: a cluster with an IP-literal or
// host-less server_url would accept no request at all, so it must not start.
func TestP08ClusterServerURLMustBeHostname(t *testing.T) {
	t.Parallel()

	for _, u := range []string{"http://127.0.0.1:8080", "https://[::1]:8443", "", "not a url", "https://"} {
		_, err := clusterHost(u)
		require.ErrorIs(t, err, errClusterServerURL, u)
	}

	for _, u := range []string{"http://localhost:8080", "https://c1.cp.gta5web.online:8443"} {
		_, err := clusterHost(u)
		assert.NoError(t, err, u)
	}
}

// TestP08ClusterConfigRefusesTailSQL (N4): tailsql would expose the shared
// database of every tailnet of the cluster.
func TestP08ClusterConfigRefusesTailSQL(t *testing.T) {
	t.Parallel()

	cfg := &types.Config{
		ClusterID: "c1",
		ServerURL: "https://c1.cp.gta5web.online:8443",
		Policy:    types.PolicyConfig{Mode: types.PolicyModeDB},
		DNSConfig: types.DNSConfig{BaseDomain: "c1.tailnet.internal"}, // P06: required
	}
	require.NoError(t, validateClusterConfig(cfg), "control: a valid cluster config")

	cfg.TailSQL.Enabled = true
	require.ErrorIs(t, validateClusterConfig(cfg), errClusterTailSQL)

	cfg.TailSQL.Enabled = false
	cfg.ServerURL = "http://10.0.0.1:8080"
	require.ErrorIs(t, validateClusterConfig(cfg), errClusterServerURL)
}

// TestP08ClusterMetricsOperatorsOnly (S14, I09, AGENTS rule 9): the cluster
// metrics listener serves /metrics to loopback and metrics_operator_cidrs
// only, never to the Tailscale range or anyone else, and serves nothing but
// /metrics.
func TestP08ClusterMetricsOperatorsOnly(t *testing.T) {
	t.Parallel()

	c := &Cluster{cfg: &types.Config{
		MetricsOperatorCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/28")},
	}}
	h := c.MetricsHandler()

	tests := []struct {
		peer string
		path string
		want int
	}{
		{"127.0.0.1:5000", "/metrics", http.StatusOK},
		{"[::1]:5000", "/metrics", http.StatusOK},
		{"192.0.2.5:5000", "/metrics", http.StatusOK},
		{"192.0.2.99:5000", "/metrics", http.StatusForbidden},
		{"192.168.1.20:5000", "/metrics", http.StatusForbidden},
		{"100.64.0.7:5000", "/metrics", http.StatusForbidden},
		{"[fd7a:115c:a1e0::7]:5000", "/metrics", http.StatusForbidden},
		{"203.0.113.9:5000", "/metrics", http.StatusForbidden},
		{"127.0.0.1:5000", "/debug/", http.StatusNotFound},
		{"127.0.0.1:5000", "/debug/pprof/", http.StatusNotFound},
		{"127.0.0.1:5000", "/debug/statsviz/", http.StatusNotFound},
		{"127.0.0.1:5000", "/api/v1/node", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.peer+tt.path, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			req.RemoteAddr = tt.peer
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Code)

			if tt.want != http.StatusOK {
				assert.NotContains(t, rec.Body.String(), "# HELP")
			}
		})
	}
}
