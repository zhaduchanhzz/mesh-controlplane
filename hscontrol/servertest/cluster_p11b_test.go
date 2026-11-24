//go:build p05cluster

package servertest_test

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/netmap"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P11b: one cluster, an active and a standby process on one database and one
// Noise key, behind a front door with a /ready health check. Failover must
// keep every node (no re-login, same node ID, key and addresses) and never
// let two processes serve.

var fastLease = &types.ClusterLeaseConfig{
	TTL: 3 * time.Second, RenewInterval: time.Second, AcquireInterval: 200 * time.Millisecond, SafetyMargin: time.Second,
}

func newHACluster(t *testing.T) *servertest.TestCluster {
	t.Helper()

	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{FrontDoor: true, Lease: fastLease},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	return tc
}

func selfAddrs(t *testing.T, c *servertest.TestClient) []netip.Prefix {
	t.Helper()

	nm := c.Netmap()
	require.NotNil(t, nm)

	return nm.GetAddresses().AsSlice()
}

// A standby answers /health (alive) but not /ready, opens no Noise session
// and holds no runtime, even when a client reaches it directly.
func TestP11bStandbyServesNothing(t *testing.T) {
	tc := newHACluster(t)
	a := tc.Primary()
	b := tc.StartProcess(t)

	assert.Equal(t, http.StatusOK, tc.ProcessGet(t, a, "/ready"), "active is ready")
	assert.Equal(t, http.StatusServiceUnavailable, tc.ProcessGet(t, b, "/ready"), "standby is not ready")
	assert.Equal(t, http.StatusOK, tc.ProcessGet(t, b, "/health"), "standby is alive")
	assert.Equal(t, http.StatusServiceUnavailable, tc.ProcessNoiseUpgrade(t, b), "standby refuses /ts2021")

	tc.Use(b)
	assert.False(t, tc.HasRuntime(t, tenantA), "standby loaded no tailnet")
	assert.False(t, tc.HasRuntime(t, tenantB), "standby loaded no tailnet")

	row := tc.LeaseRow(t)
	assert.Equal(t, a.Cluster.LeaseHolder(), row.Holder, "the lease names the active process")
	assert.False(t, row.Expired)
}

// F04: several processes of one cluster start; at every sample exactly one
// is ready.
func TestP11bExactlyOneActive(t *testing.T) {
	tc := newHACluster(t)
	procs := []*servertest.ClusterProcess{tc.Primary(), tc.StartProcess(t), tc.StartProcess(t)}

	for range 20 {
		ready := 0

		for _, p := range procs {
			if tc.ProcessGet(t, p, "/ready") == http.StatusOK {
				ready++
			}
		}

		require.Equal(t, 1, ready, "exactly one ready process")
		time.Sleep(150 * time.Millisecond)
	}
}

// F01 (in process; the lab runs it with kill -9): the active process stops,
// the standby takes the lease at the next epoch and serves; clients come
// back through the same URL without logging in again, with the same node ID,
// node key and addresses; a node that joins after the failover is seen by
// the old ones; owner_epoch of the tailnets does not change.
func TestP11bFailoverKeepsNodesWithoutRelogin(t *testing.T) {
	tc := newHACluster(t)
	a := tc.Primary()

	ta := tc.Tenant(t, tenantA)
	keyA := mintKey(t, ta, "alice")
	c1 := servertest.NewClusterClient(t, ta, "ha-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	c2 := servertest.NewClusterClient(t, ta, "ha-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, c1, selfKey(t, c2))

	tb := tc.Tenant(t, tenantB)
	keyB := mintKey(t, tb, "bob")
	cb := servertest.NewClusterClient(t, tb, "ha-b1", servertest.ClusterClientOptions{AuthKey: keyB})

	rowsBefore := tc.NodeRows(t)
	addr1, addr2 := selfAddrs(t, c1), selfAddrs(t, c2)
	k1, k2 := selfKey(t, c1), selfKey(t, c2)
	ownerBefore := tc.TailnetRow(t, tenantA).OwnerEpoch
	leaseBefore := tc.LeaseRow(t).Epoch

	b := tc.StartProcess(t)
	require.Equal(t, http.StatusServiceUnavailable, tc.ProcessGet(t, b, "/ready"))

	stopped := time.Now()
	tc.StopProcess(t, a)

	took := tc.WaitReady(t, b, 10*time.Second)
	t.Logf("standby ready %s after the active stopped (graceful, lease released)", took)
	tc.Use(b)

	assert.Equal(t, leaseBefore+1, tc.LeaseRow(t).Epoch, "lease epoch +1")
	assert.Equal(t, b.Cluster.LeaseHolder(), tc.LeaseRow(t).Holder)
	assert.Equal(t, ownerBefore, tc.TailnetRow(t, tenantA).OwnerEpoch, "failover does not change owner_epoch")

	// The servertest client does not reconnect by itself (tailscaled does:
	// lab E2E); it reconnects through the same URL, with its node key, and
	// must not get a new node.
	c1.Reconnect(t)
	c2.Reconnect(t)
	cb.Reconnect(t)

	// A node joining on the new active is seen by the old ones: their map
	// streams are live again, through the same URL.
	ta2 := tc.Tenant(t, tenantA)
	c3 := servertest.NewClusterClient(t, ta2, "ha-a3", servertest.ClusterClientOptions{AuthKey: keyA})
	k3 := selfKey(t, c3)

	waitPeersExactly(t, c1, k2, k3)
	waitPeersExactly(t, c2, k1, k3)
	t.Logf("old clients see the new node %s after the active stopped", time.Since(stopped))

	// Same identity: no re-login, no new node for c1 c2 cb.
	assert.Equal(t, k1, selfKey(t, c1))
	assert.Equal(t, k2, selfKey(t, c2))
	assert.Equal(t, addr1, selfAddrs(t, c1))
	assert.Equal(t, addr2, selfAddrs(t, c2))

	rowsAfter := tc.NodeRows(t)
	require.Len(t, rowsAfter, len(rowsBefore)+1, "only c3 is new")

	for _, r := range rowsBefore {
		assert.Contains(t, rowsAfter, r, "node %d kept its ID and keys", r.ID)
	}

	// B, untouched by the move of processes, still isolated.
	cb.WaitForCondition(t, "b still served, sees no A node", clusterWait, func(nm *netmap.NetworkMap) bool {
		return nm != nil && len(nm.Peers) == 0
	})
}

// F02/F03 (in process): the active process can no longer renew (its lease
// expired in the database, as after a pause or a partition). It stops
// serving by itself; the standby takes over; the old process is standby,
// refuses /ts2021, and clients move to the new active.
func TestP11bLostLeaseStopsOldActive(t *testing.T) {
	tc := newHACluster(t)
	a := tc.Primary()

	ta := tc.Tenant(t, tenantA)
	keyA := mintKey(t, ta, "alice")
	c1 := servertest.NewClusterClient(t, ta, "lost-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	c2 := servertest.NewClusterClient(t, ta, "lost-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, c1, selfKey(t, c2))

	b := tc.StartProcess(t)
	epoch := tc.LeaseRow(t).Epoch

	tc.ExpireLease(t)

	tc.WaitReady(t, b, 10*time.Second)
	tc.Use(b)

	require.Eventually(t, func() bool { return tc.ProcessGet(t, a, "/ready") == http.StatusServiceUnavailable },
		5*time.Second, 50*time.Millisecond, "old active is no longer ready")
	assert.Equal(t, http.StatusServiceUnavailable, tc.ProcessNoiseUpgrade(t, a), "old active refuses /ts2021")
	assert.False(t, a.Cluster.Ready())
	assert.Equal(t, epoch+1, tc.LeaseRow(t).Epoch)
	assert.Equal(t, b.Cluster.LeaseHolder(), tc.LeaseRow(t).Holder)

	c1.Reconnect(t)
	c2.Reconnect(t)

	c3 := servertest.NewClusterClient(t, tc.Tenant(t, tenantA), "lost-a3", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, c1, selfKey(t, c2), selfKey(t, c3))
}
