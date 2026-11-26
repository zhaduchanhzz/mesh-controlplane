//go:build p05cluster

package servertest_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/types/netmap"
)

// series builds the exposition key of name with labels; label pairs are
// written in name order by the Prometheus text format.
func series(name string, kv ...string) string {
	if len(kv) == 0 {
		return name
	}

	pairs := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, kv[i]+`="`+kv[i+1]+`"`)
	}

	return name + "{" + strings.Join(pairs, ",") + "}"
}

// P13f: the operator metrics listener of a cluster process shows its lease
// (held, epoch, time left before the self-fence deadline, renew latency),
// its shared database pool, how long reconcile takes and how many tailnet
// runtimes it serves. A standby process reports held 0.
func TestP13fClusterMetrics(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{FrontDoor: true},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})

	cluster := tc.Platform(t).ClusterID()
	active := tc.Primary()
	holder := active.Cluster.LeaseHolder()

	var m map[string]float64

	require.Eventually(t, func() bool {
		m = scrape(t, tc)

		return m[series("headscale_cluster_lease_held", "cluster", cluster, "holder", holder)] == 1 &&
			m[series("headscale_cluster_lease_renew_duration_seconds_count", "cluster", cluster)] >= 1 &&
			m["headscale_cluster_reconcile_duration_seconds_count"] >= 1
	}, 15*time.Second, 200*time.Millisecond, "lease, renew and reconcile metrics: %v", keysWith(m, "headscale_cluster"))

	assert.GreaterOrEqual(t, m[series("headscale_cluster_lease_epoch", "cluster", cluster, "holder", holder)], float64(1))

	left := m[series("headscale_cluster_lease_deadline_seconds", "cluster", cluster, "holder", holder)]
	assert.Positive(t, left)
	assert.LessOrEqual(t, left, (10 * time.Second).Seconds())

	assert.EqualValues(t, 2,
		m[series("headscale_cluster_tailnet_runtimes", "cluster", cluster, "holder", holder, "state", "serving")])

	assert.Positive(t, m[series("headscale_db_pool_open_connections", "pool", "platform")], "open connections of the shared pool")
	_, ok := m[series("headscale_db_pool_wait_count_total", "pool", "platform")]
	assert.True(t, ok, "pool wait count is exported")

	standby := tc.StartProcess(t)
	sh := standby.Cluster.LeaseHolder()

	require.Eventually(t, func() bool {
		m = scrape(t, tc)
		v, ok := m[series("headscale_cluster_lease_held", "cluster", cluster, "holder", sh)]

		return ok && v == 0
	}, 10*time.Second, 200*time.Millisecond, "the standby reports held 0: %v", keysWith(m, "lease_held"))
}

// P13f L4 (review P08b): the tailnet label of per-tailnet metrics was
// given on first use, before the API checked that this process serves the
// tailnet, and never given back, so requests for tailnets of other clusters
// and retired tailnets used up the label cap (2000). A label is now held by
// a served runtime only and released when it stops.
func TestP13fTailnetLabelsOnlyForServedTailnets(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA},
		servertest.TailnetSeed{Slug: "foreign", Owner: "some-other-cluster"})

	idA := tc.Tailnet(t, tenantA).ID.String()
	idForeign := tc.Tailnet(t, "foreign").ID.String()
	keyForeign := tc.MintAPIKey(t, "foreign")

	before := scrape(t, tc)["headscale_tailnet_metric_labels"]

	for range 5 {
		r := apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", keyForeign, "")
		require.Equal(t, http.StatusNotFound, r.code, "a tailnet of another cluster")
	}

	m := scrape(t, tc)
	assert.InDelta(t, before, m["headscale_tailnet_metric_labels"], 0,
		"requests for a tailnet this process does not serve took a label")

	for k := range m {
		assert.NotContains(t, k, idForeign, "a series is labelled with a tailnet this process does not serve")
	}

	// A served tailnet has its label until its runtime stops.
	require.Contains(t, m, series("headscale_tailnet_nodes", "tailnet", idA))

	tc.SetTailnetStatus(t, tenantA, types.TailnetSuspended)
	tc.WaitRuntime(t, tenantA, false, clusterWait)

	require.Eventually(t, func() bool {
		m = scrape(t, tc)
		for k := range m {
			if strings.Contains(k, idA) {
				return false
			}
		}

		return m["headscale_tailnet_metric_labels"] == before-1
	}, clusterWait, 200*time.Millisecond, "the retired tailnet's label and series are released: %v", keysWith(m, idA))
}

// P13f L9 (review P06/P08b): a policy reload that failed was recorded as
// applied, so the runtime never retried it: a transient failure (or a fix
// written without a new revision) left the tailnet on its previous policy
// until the next revision. The revision is recorded only once the reload
// succeeded; until then reconcile retries, and the failure is counted.
func TestP13fPolicyReloadFailureRetried(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	clients := enroll(t, tc.Tenant(t, tenantA), "alice", "l9-a1", "l9-a2")
	for _, c := range clients {
		waitTenantMesh(t, c, clients, false)
	}

	idA := tc.Tailnet(t, tenantA).ID.String()

	// A new revision whose policy does not parse: the runtime keeps the
	// previous policy (never allow-all, never deny-all by accident).
	tc.StoreRawPolicyForTest(t, tenantA, `{"grants": [ not json`, true)

	failures := series("headscale_tailnet_reload_failures_total", "kind", "policy", "tailnet", idA)

	var m map[string]float64

	require.Eventually(t, func() bool {
		m = scrape(t, tc)

		return m[failures] >= 1
	}, clusterWait, 200*time.Millisecond, "the failed reload is counted: %v", keysWith(m, "reload"))

	for _, c := range clients {
		assert.Len(t, c.Netmap().Peers, 1, "the previous policy stays in force while the new one fails")
	}

	// The same revision becomes readable (here: the row is fixed without a
	// new revision, as after a transient read error). The retry applies it.
	tc.StoreRawPolicyForTest(t, tenantA, `{"grants":[]}`, false)

	for _, c := range clients {
		c.WaitForCondition(t, "deny-all from the retried reload", clusterWait, func(nm *netmap.NetworkMap) bool {
			return len(nm.Peers) == 0
		})
	}

	m = scrape(t, tc)
	assert.Zero(t, m[series("headscale_cluster_tailnet_runtimes", "cluster", tc.Platform(t).ClusterID(),
		"holder", tc.Cluster.LeaseHolder(), "state", "reload_pending")], "nothing left to retry")
}

func keysWith(m map[string]float64, sub string) []string {
	var out []string

	for k := range m {
		if strings.Contains(k, sub) {
			out = append(out, k)
		}
	}

	return out
}
