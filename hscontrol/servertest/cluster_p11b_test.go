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

func hasHost(rows []servertest.NodeRow, host string) bool {
	for _, r := range rows {
		if r.Hostname == host {
			return true
		}
	}

	return false
}

// I02/I13: a node deleted on the active right before it stops does not come
// back through the new active: no row, not in any peer's netmap.
func TestP11bDeletedNodeStaysDeletedAfterFailover(t *testing.T) {
	tc := newHACluster(t)
	a := tc.Primary()

	ta := tc.Tenant(t, tenantA)
	keyA := mintKey(t, ta, "alice")
	c1 := servertest.NewClusterClient(t, ta, "del-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	c2 := servertest.NewClusterClient(t, ta, "del-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, c1, selfKey(t, c2))

	b := tc.StartProcess(t)

	// Delete c2 and stop the active at once, without waiting for c1 to see
	// the deletion.
	deleteNode(t, tc, tenantA, c2)
	c2.Disconnect(t)
	tc.StopProcess(t, a)

	tc.WaitReady(t, b, 10*time.Second)
	tc.Use(b)

	require.False(t, hasHost(tc.NodeRows(t), "del-a2"), "deleted row stays deleted")

	c1.Reconnect(t)

	c3 := servertest.NewClusterClient(t, tc.Tenant(t, tenantA), "del-a3", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, c1, selfKey(t, c3))
	waitPeersExactly(t, c3, selfKey(t, c1))
	assert.False(t, hasHost(tc.NodeRows(t), "del-a2"))
}

// The ephemeral GC is rebuilt on the new active from the database: an
// ephemeral node that does not come back is removed one inactivity timeout
// after the takeover; one that reconnects is kept. The timer counts from
// the takeover on the new process's clock, never from timestamps written by
// the old one (no early deletion from clock skew).
func TestP11bEphemeralGCAfterFailover(t *testing.T) {
	// node.ephemeral.inactivity_timeout has a 65 s floor (config validation).
	const timeout = 66 * time.Second

	tc := servertest.NewTestClusterWith(t,
		servertest.ClusterOptions{FrontDoor: true, Lease: fastLease, EphemeralTimeout: timeout},
		servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)
	a := tc.Primary()

	ta := tc.Tenant(t, tenantA)
	u := ta.CreateUser(t, "eve")
	// Ephemeral pre-auth keys are single use: one per client.
	e1 := servertest.NewClusterClient(t, ta, "eph-back",
		servertest.ClusterClientOptions{AuthKey: ta.CreateEphemeralPreAuthKey(t, types.UserID(u.ID))})
	e2 := servertest.NewClusterClient(t, ta, "eph-gone",
		servertest.ClusterClientOptions{AuthKey: ta.CreateEphemeralPreAuthKey(t, types.UserID(u.ID))})
	waitPeersExactly(t, e1, selfKey(t, e2))

	b := tc.StartProcess(t)
	tc.StopProcess(t, a)
	tc.WaitReady(t, b, 10*time.Second)
	tc.Use(b)

	taken := time.Now()

	// Both rows survive the takeover itself.
	rows := tc.NodeRows(t)
	require.True(t, hasHost(rows, "eph-back"))
	require.True(t, hasHost(rows, "eph-gone"))

	e1.Reconnect(t)

	require.Eventually(t, func() bool { return !hasHost(tc.NodeRows(t), "eph-gone") },
		timeout+30*time.Second, 250*time.Millisecond, "the ephemeral node that did not come back is removed")

	gone := time.Since(taken)
	t.Logf("ephemeral node removed %s after the takeover (timeout %s)", gone, timeout)
	assert.GreaterOrEqual(t, gone, timeout-500*time.Millisecond, "not before the inactivity timeout")

	// Both were scheduled at the same instant on load; the reconnected one
	// had its timer cancelled, so it is still there after the other went.
	time.Sleep(2 * time.Second) //nolint:forbidigo // past the shared deadline
	assert.True(t, hasHost(tc.NodeRows(t), "eph-back"), "the reconnected ephemeral node is kept")
	e1.WaitForCondition(t, "eph-gone left e1's netmap", clusterWait, func(nm *netmap.NetworkMap) bool {
		return nm != nil && len(nm.Peers) == 0
	})
}
