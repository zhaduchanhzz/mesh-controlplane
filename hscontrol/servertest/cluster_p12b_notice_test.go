//go:build p05cluster

package servertest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

func hasMoveNotice(c *servertest.TestClient) bool {
	for _, s := range c.HealthMessages() {
		if strings.Contains(s, "--force-reauth") {
			return true
		}
	}

	return false
}

// P12b (owner Q1): a scheduled move of tailnet A reaches A's connected
// clients as a control health message (time, reason, what to run), never a
// client of B; cancelling the move removes it.
func TestScheduledMoveNotifiesOnlyItsClients(t *testing.T) {
	c1 := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	c2 := c1.AddCluster(t)
	c1.SetPolicy(t, tenantA, allowAll)
	c1.SetPolicy(t, tenantB, allowAll)

	a := enroll(t, c1.Tenant(t, tenantA), "alice", "mn-a1")[0]
	b := enroll(t, c1.Tenant(t, tenantB), "bob", "mn-b1")[0]

	require.False(t, hasMoveNotice(a))

	reg := c1.Registry(t)
	m, err := reg.ScheduleMove(context.Background(), servertest.TestOperator,
		c1.Tailnet(t, tenantA).ID.String(), c2.ClusterID, time.Now().UTC().Add(time.Hour), "rebalance")
	require.NoError(t, err)

	assert.Eventually(t, func() bool { return hasMoveNotice(a) }, clusterWait, 100*time.Millisecond,
		"A's client is told about the move")

	for _, s := range a.HealthMessages() {
		assert.NotContains(t, s, c2.ClusterID, "the target cluster is never named")
	}

	assert.Never(t, func() bool { return hasMoveNotice(b) }, 3*time.Second, 100*time.Millisecond,
		"B's client is not told about A's move")

	// Owner Q3: A's own API shows it read-only (time, reason); B's does not.
	keyA, keyB := c1.MintAPIKey(t, tenantA), c1.MintAPIKey(t, tenantB)
	r := apiCall(t, c1.URL, http.MethodGet, "/api/v2/tailnet/-/scheduled-move", keyA, "")
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Contains(t, r.body, `"scheduled":true`)
	assert.Contains(t, r.body, "rebalance")
	assert.NotContains(t, r.body, c2.ClusterID, "the target cluster is never named")

	r = apiCall(t, c1.URL, http.MethodGet, "/api/v2/tailnet/-/scheduled-move", keyB, "")
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Contains(t, r.body, `"scheduled":false`)
	assert.NotContains(t, r.body, "rebalance")

	_, err = reg.CancelScheduledMove(context.Background(), servertest.TestOperator, m.ID)
	require.NoError(t, err)

	assert.Eventually(t, func() bool { return !hasMoveNotice(a) }, clusterWait, 100*time.Millisecond,
		"the notice is removed once the move is cancelled")
}
