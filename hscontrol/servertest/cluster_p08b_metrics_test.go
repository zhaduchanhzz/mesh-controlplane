//go:build p05cluster

package servertest_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// scrape reads the operator metrics listener of the cluster.
func scrape(t *testing.T, tc *servertest.TestCluster) map[string]float64 {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	tc.Cluster.MetricsHandler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))

	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "#") {
			continue
		}

		i := strings.LastIndexByte(l, ' ')
		if i < 0 {
			continue
		}

		v, err := strconv.ParseFloat(l[i+1:], 64)
		if err == nil {
			out[l[:i]] = v
		}
	}

	return out
}

// P08b, per-tailnet metrics (owner decision Q5): nodes and open map
// sessions of each tailnet under its ID, never its slug; the process-wide
// nodestore_nodes is the sum over the runtimes (it was the last writer's).
func TestP08bTailnetMetrics(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	tc.SetPolicy(t, tenantB, allowAllWithSSH)

	aClients := enroll(t, tc.Tenant(t, tenantA), "alice", "m-a1", "m-a2")
	bClients := enroll(t, tc.Tenant(t, tenantB), "bob", "m-b1")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, true)
		}
	}

	idA, idB := tc.Tailnet(t, tenantA).ID.String(), tc.Tailnet(t, tenantB).ID.String()
	series := func(name, id string) string { return name + `{tailnet="` + id + `"}` }

	var m map[string]float64

	require.Eventually(t, func() bool {
		m = scrape(t, tc)

		return m[series("headscale_tailnet_map_sessions", idA)] == 2 &&
			m[series("headscale_tailnet_map_sessions", idB)] == 1
	}, 10*time.Second, 100*time.Millisecond, "map sessions by tailnet: %v", m)

	assert.Positive(t, m[`headscale_tailnet_mapresponses_sent_total{tailnet="`+idA+`",type="update"}`])
	assert.Positive(t, m[`headscale_tailnet_mapresponses_sent_total{tailnet="`+idB+`",type="update"}`])
	assert.EqualValues(t, 2, m[series("headscale_tailnet_nodes", idA)])
	assert.EqualValues(t, 1, m[series("headscale_tailnet_nodes", idB)])
	assert.GreaterOrEqual(t, m["headscale_nodestore_nodes"], float64(3), "the sum over runtimes, not one runtime's count")

	for k := range m {
		assert.NotContains(t, k, tenantA, "labels are tailnet IDs, never slugs")
		assert.NotContains(t, k, tenantB, "labels are tailnet IDs, never slugs")
	}
}
