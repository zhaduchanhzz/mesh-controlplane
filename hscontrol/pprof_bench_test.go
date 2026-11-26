package hscontrol

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// pprofLoopbackStatus is what the cluster metrics listener answers a
// loopback peer on /debug/pprof/: nothing (404) unless this test binary was
// built with the benchpprof tag.
func pprofLoopbackStatus() int {
	if benchPprofEnabled {
		return http.StatusOK
	}

	return http.StatusNotFound
}

// TestBenchPprofOnlyOnLoopbackMetricsListener (P14 Q7, AGENTS rule 9): pprof
// exists only in a benchpprof build, only on the cluster metrics listener and
// only for loopback peers. Not for metrics_operator_cidrs (they may read
// /metrics, not heap or goroutine dumps of every tailnet), not for the lab
// LAN or the Tailscale range, and never on the public cluster listener, with
// or without the tag. CI runs this without the tag (absent everywhere);
// `go test -tags benchpprof` runs it with the tag (loopback metrics only).
func TestBenchPprofOnlyOnLoopbackMetricsListener(t *testing.T) {
	t.Parallel()

	host, err := clusterHost("http://bench.lab.internal:8080")
	require.NoError(t, err)

	c := &Cluster{host: host, cfg: &types.Config{
		MetricsOperatorCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/28")},
	}}
	metrics, public := c.MetricsHandler(), c.HTTPHandler()

	// An admitted operator that is not loopback: the metrics mux has no
	// pprof route (404) without the tag; with it the loopback check refuses.
	operator := http.StatusNotFound
	if benchPprofEnabled {
		operator = http.StatusForbidden
	}

	tests := []struct {
		name string
		h    http.Handler
		peer string
		host string
		want int
	}{
		{"metrics, loopback v4", metrics, "127.0.0.1:5000", "localhost", pprofLoopbackStatus()},
		{"metrics, loopback v6", metrics, "[::1]:5000", "localhost", pprofLoopbackStatus()},
		{"metrics, operator CIDR", metrics, "192.0.2.5:5000", "localhost", operator},
		{"metrics, lab LAN", metrics, "192.168.1.20:5000", "localhost", http.StatusForbidden},
		{"metrics, tenant CGNAT", metrics, "100.64.0.7:5000", "localhost", http.StatusForbidden},
		{"metrics, tenant ULA", metrics, "[fd7a:115c:a1e0::7]:5000", "localhost", http.StatusForbidden},
		{"metrics, internet", metrics, "203.0.113.9:5000", "localhost", http.StatusForbidden},
		{"public, loopback, cluster host", public, "127.0.0.1:5000", "bench.lab.internal:8080", http.StatusNotFound},
		{"public, lab LAN, cluster host", public, "192.168.1.20:5000", "bench.lab.internal:8080", http.StatusNotFound},
		{"public, loopback, other host", public, "127.0.0.1:5000", "localhost", http.StatusMisdirectedRequest},
	}

	// Not /profile or /trace: they sample for seconds.
	paths := []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap", "/debug/pprof/cmdline"}

	for _, tt := range tests {
		for _, path := range paths {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				t.Parallel()

				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
				req.Host = tt.host
				req.RemoteAddr = tt.peer
				rec := httptest.NewRecorder()
				tt.h.ServeHTTP(rec, req)

				assert.Equal(t, tt.want, rec.Code)

				if tt.want != http.StatusOK {
					body := rec.Body.String()
					assert.NotContains(t, body, "goroutine", "profile content leaked")
					assert.NotContains(t, body, "Types of profiles", "pprof index leaked")
				}
			})
		}
	}

	// /metrics is unchanged by the tag.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	metrics.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestBenchPprofNotInReleaseBuilds: the release pipeline builds without build
// tags, so the benchpprof file can only be in a binary someone built by hand
// with -tags benchpprof.
func TestBenchPprofNotInReleaseBuilds(t *testing.T) {
	t.Parallel()

	for _, f := range []string{"../.goreleaser.yml", "../Makefile", "../flake.nix", "../Dockerfile.integration"} {
		b, err := os.ReadFile(f)
		if os.IsNotExist(err) {
			continue
		}

		require.NoError(t, err)
		assert.NotContains(t, strings.ToLower(string(b)), "benchpprof", f)
	}
}
