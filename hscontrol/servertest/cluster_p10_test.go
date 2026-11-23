//go:build p05cluster

package servertest_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P10: a cluster serves a tailnet only while it owns it at the epoch it
// loaded it with. Writes are fenced in the database (tenantscope); these
// tests cover what the running cluster does when ownership changes.

const tenantC = "tenant-c"

// Ownership moved to another cluster: the runtime is retired, its tailnet's
// credentials no longer register here, the other tenant is unaffected (I12,
// S17, I10).
func TestClusterOwnershipMoveRetiresRuntime(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	keyA := mintKey(t, a, "alice")
	keyB := mintKey(t, b, "bob")
	servertest.NewClusterClient(t, a, "move-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	b1 := servertest.NewClusterClient(t, b, "move-b1", servertest.ClusterClientOptions{AuthKey: keyB})

	row := tc.TailnetRow(t, tenantA)
	tc.SetTailnetOwner(t, tenantA, "another-cluster", row.OwnerEpoch+1)

	tc.WaitRuntime(t, tenantA, false, clusterWait)

	before := tc.NodeRows(t)
	_, err := servertest.TryNewClusterClient(t, tc.Front(), "move-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	require.Error(t, err, "a key of a tailnet this cluster no longer owns must be refused")
	assert.Equal(t, before, tc.NodeRows(t), "no node created after the move")

	require.True(t, tc.HasRuntime(t, tenantB), "B is still served")
	b2 := servertest.NewClusterClient(t, b, "move-b2", servertest.ClusterClientOptions{AuthKey: keyB})
	waitPeersExactly(t, b1, selfKey(t, b2))
	waitPeersExactly(t, b2, selfKey(t, b1))
}

// The tailnet left ACTIVE (suspended): retired like a move.
func TestClusterSuspendRetiresRuntime(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)

	keyA := mintKey(t, tc.Tenant(t, tenantA), "alice")

	tc.SetTailnetStatus(t, tenantA, types.TailnetSuspended)
	tc.WaitRuntime(t, tenantA, false, clusterWait)

	_, err := servertest.TryNewClusterClient(t, tc.Front(), "susp-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	require.Error(t, err, "a key of a SUSPENDED tailnet must be refused")
	require.True(t, tc.HasRuntime(t, tenantB))
}

// A new epoch for the same cluster (e.g. after a CAS by placement): the old
// runtime is retired and a fresh one loaded at the new epoch; clients
// reconnect without logging in again and keep working.
func TestClusterEpochBumpReloadsRuntime(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	a := tc.Tenant(t, tenantA)
	keyA := mintKey(t, a, "alice")
	a1 := servertest.NewClusterClient(t, a, "epoch-a1", servertest.ClusterClientOptions{AuthKey: keyA})

	old := tc.Runtime(t, tenantA)
	row := tc.TailnetRow(t, tenantA)
	tc.SetTailnetOwner(t, tenantA, tc.ClusterID, row.OwnerEpoch+1)

	require.Eventually(t, func() bool {
		rt, ok := tc.Cluster.Runtime(tc.Tailnet(t, tenantA).ID)

		return ok && rt != old
	}, clusterWait, 50*time.Millisecond, "a new runtime is loaded at the new epoch")

	// The retired runtime closed a1's map stream; tailscaled reconnects by
	// itself (Direct does not, so the test does it), keeps its node and
	// reaches the reloaded runtime.
	require.Eventually(t, a1.PollEnded, clusterWait, 50*time.Millisecond,
		"the server closes the map stream of a retired runtime")

	nk1 := selfKey(t, a1)
	a1.Reconnect(t)

	a2 := servertest.NewClusterClient(t, tc.Tenant(t, tenantA), "epoch-a2", servertest.ClusterClientOptions{AuthKey: keyA})
	waitPeersExactly(t, a1, selfKey(t, a2))
	assert.Equal(t, nk1, selfKey(t, a1), "same node key after the reload: no new login")
	waitPeersExactly(t, a2, selfKey(t, a1))

	rows := tc.NodeRows(t)
	assert.Len(t, rows, 2, "a1 kept its node across the reload: %v", rows)
}

// A tailnet created and activated while the cluster runs is served without
// a restart, starts with a deny-all policy, and stays isolated.
func TestClusterLoadsNewTailnetLive(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, tc.Tenant(t, tenantA), "alice", "live-a1")[0]

	tc.CreateTailnetLive(t, tenantC)
	tc.WaitRuntime(t, tenantC, true, clusterWait)

	c := tc.Tenant(t, tenantC)
	cClients := enroll(t, c, "carol", "live-c1", "live-c2")

	// Deny-all until the owner writes a policy: no peers.
	for _, cl := range cClients {
		require.Empty(t, cl.Netmap().Peers, "%s: a new tailnet starts with a deny-all policy", cl.Name)
	}

	// The operator path: policy written to the database, revision bumped;
	// the running runtime reloads it without a restart.
	tc.SetPolicyViaDB(t, tenantC, allowAll)
	waitPeersExactly(t, cClients[0], selfKey(t, cClients[1]))
	waitPeersExactly(t, cClients[1], selfKey(t, cClients[0]))
	assert.Empty(t, a1.Netmap().Peers, "A sees nothing of C")
}

// I10: a tailnet at its device limit refuses new devices with a clear
// message; its existing devices keep working and re-login; another tailnet
// on the same cluster still enrolls (its own count).
func TestClusterQuotaRefusesNewDevices(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)
	tc.SetClusterQuota(t, 2)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	keyA := mintKey(t, a, "alice")
	a1 := servertest.NewClusterClient(t, a, "quota-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	a2 := servertest.NewClusterClient(t, a, "quota-a2", servertest.ClusterClientOptions{AuthKey: keyA})

	before := tc.NodeRows(t)
	_, err := servertest.TryNewClusterClient(t, a, "quota-a3", servertest.ClusterClientOptions{AuthKey: keyA})
	require.Error(t, err, "a third device exceeds A's limit of 2")
	assert.Contains(t, err.Error(), "device limit")
	assert.Equal(t, before, tc.NodeRows(t), "the refused device created no node")

	// An existing device re-registers (not a new enrollment).
	a1.Reconnect(t)
	waitPeersExactly(t, a1, selfKey(t, a2))

	// B counts its own devices.
	enroll(t, b, "bob", "quota-b1", "quota-b2")
}

// A device that is a node of tailnet A is still a new device for tailnet B
// (review of P10, finding A2): registering into B with B's auth key and
// OldNodeKey = its node key in A must count against B's quota. Before the
// fix, knownNode accepted a (machine key, node key) pair found in any
// tailnet and skipped B's quota.
func TestClusterQuotaNotBypassedByNodeOfOtherTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)
	tc.SetClusterQuota(t, 1)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	keyA, keyB := mintKey(t, a, "alice"), mintKey(t, b, "carol")
	enroll(t, b, "bob", "quota-b1") // B is full (limit 1)

	device := key.NewMachine()
	sess := servertest.NewRawNoiseSession(t, tc.URL, tc.NoiseKey(t), device)

	nkA := key.NewNode().Public()
	resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
		NodeKey:  nkA,
		Auth:     &tailcfg.RegisterResponseAuth{AuthKey: keyA},
		Hostinfo: &tailcfg.Hostinfo{Hostname: "device-in-a"},
	}, nil)
	require.NoError(t, err)
	require.True(t, resp.OK() && rr.MachineAuthorized && rr.Error == "", "control: the device registers in A (%d %s)", resp.Status, resp.Body)

	// A new Noise session with the same machine key, as a real client opens
	// one per profile: the first session is bound to A and refuses B.
	sessB := servertest.NewRawNoiseSession(t, tc.URL, tc.NoiseKey(t), device)

	before := tc.NodeRows(t)
	nkB := key.NewNode().Public()
	resp, rr, err = sessB.Register(ctx(t), &tailcfg.RegisterRequest{
		NodeKey:    nkB,
		OldNodeKey: nkA,
		Auth:       &tailcfg.RegisterResponseAuth{AuthKey: keyB},
		Hostinfo:   &tailcfg.Hostinfo{Hostname: "device-into-b"},
	}, nil)
	require.NoError(t, err)

	refused := !resp.OK() || rr.Error != "" || !rr.MachineAuthorized
	assert.True(t, refused, "B is full: a node of A must not enroll into B by naming its A node key as OldNodeKey (%d %s)", resp.Status, resp.Body)
	assert.Equal(t, before, tc.NodeRows(t), "no node was created")

	_, ok := b.State().GetNodeByNodeKey(nkB)
	assert.False(t, ok, "no node in B")
}

// Concurrent enrollments must not exceed the limit (review of P10, finding
// A3): the quota is checked inside the transaction that inserts the node, so
// N devices racing for the last free places yield exactly limit nodes. Before
// the fix the check ran in its own short transaction ahead of the insert and
// every racer could see a free place.
func TestClusterQuotaExactUnderConcurrentEnrollment(t *testing.T) {
	const limit, racers = 3, 10

	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetClusterQuota(t, limit)

	a := tc.Tenant(t, tenantA)
	keyA := mintKey(t, a, "alice")

	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		accepted int
		refusals []string
	)

	for i := range racers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sess := servertest.NewRawNoiseSession(t, tc.URL, tc.NoiseKey(t), key.NewMachine())
			<-start

			resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
				NodeKey:  key.NewNode().Public(),
				Auth:     &tailcfg.RegisterResponseAuth{AuthKey: keyA},
				Hostinfo: &tailcfg.Hostinfo{Hostname: fmt.Sprintf("race-%d", i)},
			}, nil)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err != nil:
				refusals = append(refusals, err.Error())
			case resp.OK() && rr.MachineAuthorized && rr.Error == "":
				accepted++
			default:
				refusals = append(refusals, fmt.Sprintf("%d %s", resp.Status, rr.Error))
			}
		}()
	}

	close(start)
	wg.Wait()

	assert.Equal(t, limit, len(tc.NodeRows(t)), "nodes in A after %d concurrent enrollments (limit %d)", racers, limit)
	assert.Equal(t, limit, accepted, "accepted registrations; refusals: %v", refusals)

	for _, r := range refusals {
		assert.Contains(t, r, "device limit", "a refusal names the limit")
	}
}

// A tailnet whose owner cluster is not registered cannot enroll new devices:
// its limit is unknown (fail closed).
func TestClusterUnregisteredClusterRefusesEnrollment(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	keyA := mintKey(t, tc.Tenant(t, tenantA), "alice")
	tc.UnregisterCluster(t)

	_, err := servertest.TryNewClusterClient(t, tc.Front(), "unreg-a1", servertest.ClusterClientOptions{AuthKey: keyA})
	require.Error(t, err)
}
