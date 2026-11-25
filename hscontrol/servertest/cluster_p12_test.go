//go:build p05cluster

package servertest_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P12 step 3: two cluster IDs on one shared database (c1 and c2 of the
// lab), tailnets placed by the registry.

// notServedFor fails if other loads slug's runtime at any time within d
// (longer than the 2 s reconcile interval, so a wrong load would show).
func notServedFor(t *testing.T, other *servertest.TestCluster, slug string, d time.Duration) {
	t.Helper()

	assert.Never(t, func() bool { return other.HasRuntime(t, slug) }, d, 25*time.Millisecond,
		"cluster %s must never load %q, it does not own it", other.ClusterID, slug)
}

// Three tailnets placed on two clusters: each owner loads its tailnets, the
// other never does; clients see only their own tailnet; a credential sent
// to the cluster that does not own its tailnet is refused and creates no
// node (I12, F07, S17).
func TestTwoClustersServeOnlyOwnedTailnets(t *testing.T) {
	c1 := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "p12-seed"})
	c2 := c1.AddCluster(t)
	clusters := map[string]*servertest.TestCluster{c1.ClusterID: c1, c2.ClusterID: c2}

	slugs := []string{"p12-t1", "p12-t2", "p12-t3"}
	owners := map[string]string{}
	perCluster := map[string]int{}

	for _, s := range slugs {
		owners[s] = c1.PlaceTailnet(t, s)
		perCluster[owners[s]]++
	}

	// Q1: the seed is on c1, so placement spreads t1-t3 over both.
	require.Len(t, perCluster, 2, "both clusters get a placed tailnet: %v", owners)

	other := func(id string) *servertest.TestCluster {
		if id == c1.ClusterID {
			return c2
		}

		return c1
	}

	for _, s := range slugs {
		clusters[owners[s]].WaitRuntime(t, s, true, clusterWait)
	}

	for _, s := range slugs {
		notServedFor(t, other(owners[s]), s, 3*time.Second)
	}

	type pair struct{ a, b *servertest.TestClient }

	pairs := map[string]pair{}

	for _, s := range slugs {
		owner := clusters[owners[s]]
		owner.SetPolicyViaDB(t, s, allowAll)
		cl := enroll(t, owner.Tenant(t, s), "u-"+s, s+"-a", s+"-b")
		pairs[s] = pair{cl[0], cl[1]}
	}

	for _, s := range slugs {
		p := pairs[s]
		waitPeersExactly(t, p.a, selfKey(t, p.b))
		waitPeersExactly(t, p.b, selfKey(t, p.a))
	}

	// Every node is in the tailnet its client enrolled into.
	byTailnet := map[string]int{}
	for _, r := range c1.NodeRows(t) {
		byTailnet[r.TailnetID]++
	}

	for _, s := range slugs {
		assert.Equal(t, 2, byTailnet[c1.Tailnet(t, s).ID.String()], "%s has its 2 nodes", s)
	}

	// A key of each tailnet, at the cluster that does not own it.
	for _, s := range slugs {
		key := mintKey(t, clusters[owners[s]].Tenant(t, s), "late-"+s)
		before := c1.NodeRows(t)

		_, err := servertest.TryNewClusterClient(t, other(owners[s]).Front(), s+"-wrong", servertest.ClusterClientOptions{AuthKey: key})
		require.Error(t, err, "%s: the cluster that does not own the tailnet refuses its key", s)
		assert.Equal(t, before, c1.NodeRows(t), "%s: no node created at the wrong cluster", s)
	}
}

// MoveOwner c1 → c2 and back, several times: the source retires, the
// target loads; a write through the source's runtime after the hand-over is
// fenced in the database (no dual owner of data). Owner decision Q7
// (2026-11-25): no in-memory overlap either (R11) — at no sample may both
// clusters hold a runtime of the tailnet (ADR 0010, hand-over).
func TestTwoClustersMoveOwnerFencesSourceAndMeasuresR11(t *testing.T) {
	c1 := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "p12-mv"})
	c2 := c1.AddCluster(t)
	c1.SetPolicy(t, "p12-mv", allowAll)

	const slug = "p12-mv"

	c1.WaitRuntime(t, slug, true, clusterWait)

	from, to := c1, c2

	var overlaps, loads, retires []time.Duration

	for i := range 6 {
		src := from.Runtime(t, slug)

		start := time.Now()
		epoch := c1.MoveOwner(t, slug, from.ClusterID, to.ClusterID) + 1

		var srcGone, dstUp time.Duration

		both := 0

		deadline := start.Add(clusterWait)
		for time.Now().Before(deadline) && (srcGone == 0 || dstUp == 0) {
			now := time.Since(start)

			srcHas, dstHas := from.HasRuntime(t, slug), to.HasRuntime(t, slug)
			if srcHas && dstHas {
				both++
			}

			if srcGone == 0 && !srcHas {
				srcGone = now
			}

			if dstUp == 0 && dstHas {
				dstUp = now
			}

			time.Sleep(5 * time.Millisecond) //nolint:forbidigo // sampling both clusters
		}

		require.NotZero(t, srcGone, "move %d: source retired", i)
		require.NotZero(t, dstUp, "move %d: target loaded", i)

		row := c1.TailnetRow(t, slug)
		assert.Equal(t, to.ClusterID, row.OwnerClusterID)
		assert.Equal(t, epoch, row.OwnerEpoch, "one hand-over, epoch + 1")
		assert.Nil(t, row.MovingToClusterID)

		// The source's runtime object, retired, cannot write either.
		_, _, err := src.GetState().CreateUser(types.User{Name: fmt.Sprintf("late-%d", i)})
		require.ErrorIs(t, err, tenantscope.ErrFenced, "move %d: write by the source after the hand-over", i)

		overlap := max(srcGone-dstUp, 0)
		assert.Zero(t, both, "move %d: samples where both clusters held a runtime", i)
		overlaps = append(overlaps, overlap)
		loads = append(loads, dstUp)
		retires = append(retires, srcGone)

		t.Logf("R11 move %d %s→%s epoch=%d target_loaded=%s source_retired=%s overlap=%s",
			i, short(from.ClusterID), short(to.ClusterID), epoch, dstUp.Round(time.Millisecond),
			srcGone.Round(time.Millisecond), overlap.Round(time.Millisecond))

		// The tailnet is served by its new owner.
		to.WaitRuntime(t, slug, true, clusterWait)

		from, to = to, from
	}

	slices.Sort(overlaps)
	slices.Sort(loads)
	slices.Sort(retires)
	t.Logf("R11 summary n=%d overlap min=%s median=%s max=%s; target_loaded max=%s; source_retired max=%s",
		len(overlaps), overlaps[0], overlaps[len(overlaps)/2], overlaps[len(overlaps)-1], loads[len(loads)-1], retires[len(retires)-1])

	assert.Zero(t, overlaps[len(overlaps)-1], "R11: the target loads only after the source released")
	// The cost of the claim: source notices (<= 2 s) and drains, target
	// loads on its next tick (<= 2 s).
	assert.LessOrEqual(t, loads[len(loads)-1], 6*time.Second, "move completes")
}

// A failover inside c1 (active stopped, standby takes the lease) does not
// touch c2: same runtime, same lease holder and epoch, its clients keep
// their map stream and peers; c1's clients reconnect without a new login.
func TestFailoverInOneClusterLeavesTheOtherAlone(t *testing.T) {
	c1 := servertest.NewTestClusterWith(t, servertest.ClusterOptions{FrontDoor: true}, servertest.TailnetSeed{Slug: "p12-fa"})
	standby := c1.StartProcess(t)
	c2 := c1.AddCluster(t)

	// Q1: c1 already owns a tailnet, so the next one goes to c2.
	require.Equal(t, c2.ClusterID, c1.PlaceTailnet(t, "p12-fb"))
	c2.WaitRuntime(t, "p12-fb", true, clusterWait)

	c1.SetPolicy(t, "p12-fa", allowAll)
	c2.SetPolicyViaDB(t, "p12-fb", allowAll)

	a := enroll(t, c1.Tenant(t, "p12-fa"), "alice", "fa-1", "fa-2")
	b := enroll(t, c2.Tenant(t, "p12-fb"), "bob", "fb-1", "fb-2")
	waitPeersExactly(t, a[0], selfKey(t, a[1]))
	waitPeersExactly(t, b[0], selfKey(t, b[1]))

	rtB := c2.Runtime(t, "p12-fb")
	leaseB := c2.LeaseRow(t)
	leaseA := c1.LeaseRow(t)
	nkA0, nkA1 := selfKey(t, a[0]), selfKey(t, a[1])

	c1.StopProcess(t, c1.Primary())
	took := c1.WaitReady(t, standby, clusterWait)
	c1.Use(standby)
	t.Logf("c1 failover: standby ready after %s", took.Round(time.Millisecond))

	assert.Greater(t, c1.LeaseRow(t).Epoch, leaseA.Epoch, "c1 lease taken over at a higher epoch")

	// c2 saw nothing of it.
	assert.Same(t, rtB, c2.Runtime(t, "p12-fb"), "c2 runtime untouched")
	assert.Equal(t, leaseB.Epoch, c2.LeaseRow(t).Epoch, "c2 lease epoch unchanged")
	assert.Equal(t, leaseB.Holder, c2.LeaseRow(t).Holder, "c2 lease holder unchanged")
	assert.False(t, b[0].PollEnded(), "c2 client's map stream not closed")
	assert.False(t, b[1].PollEnded())
	waitPeersExactly(t, b[0], selfKey(t, b[1]))

	// c1's clients come back on the standby without logging in again.
	for _, c := range a {
		require.Eventually(t, c.PollEnded, clusterWait, 50*time.Millisecond)
		c.Reconnect(t)
	}

	waitPeersExactly(t, a[0], nkA1)
	waitPeersExactly(t, a[1], nkA0)
	assert.Equal(t, nkA0, selfKey(t, a[0]), "same node key: no new login")
	assert.Equal(t, nkA1, selfKey(t, a[1]), "same node key: no new login")
	notServedFor(t, c2, "p12-fa", 3*time.Second)
}

// ADR 0010 §4: the move of a tailnet whose owner cluster has no process
// left (no live lease) is completed by the target, which then serves it.
func TestTwoClustersMoveFromDeadClusterIsTakenOver(t *testing.T) {
	c1 := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "p12-dead"})
	c2 := c1.AddCluster(t)

	const slug = "p12-dead"

	c1.WaitRuntime(t, slug, true, clusterWait)
	c1.StopProcess(t, c1.Primary()) // releases c1's lease; nothing serves c1

	epoch := c1.MoveOwner(t, slug, c1.ClusterID, c2.ClusterID)

	c2.WaitRuntime(t, slug, true, clusterWait)

	row := c1.TailnetRow(t, slug)
	assert.Equal(t, c2.ClusterID, row.OwnerClusterID)
	assert.Equal(t, epoch+1, row.OwnerEpoch)
	assert.Nil(t, row.MovingToClusterID)

	c2.SetPolicyViaDB(t, slug, allowAll)
	cl := enroll(t, c2.Tenant(t, slug), "dana", "dead-1", "dead-2")
	waitPeersExactly(t, cl[0], selfKey(t, cl[1]))
}

func short(id string) string { return id[len(id)-8:] }
