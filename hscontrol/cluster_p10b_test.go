package hscontrol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// retireFixture is a cluster with one runtime and one Noise session bound to
// it, as after a registration routed by session_route.
func retireFixture(t *testing.T) (*Cluster, *tenantRuntime, *noiseServer) {
	t.Helper()

	app := createTestApp(t)
	// stopRuntime closes the batcher; the fixture's cleanup must not again.
	app.mapBatcher.Close()
	app.mapBatcher = nil

	id := tailnet.MustParseID("0b6f3c1e-2a7d-4e59-9c3b-5d8e1f2a4b6c")
	rt := &tenantRuntime{id: id, slug: "a", epoch: 1, h: app}
	c := &Cluster{runtimes: map[tailnet.ID]*tenantRuntime{id: rt}, order: []tailnet.ID{id}}
	ns := &noiseServer{headscale: app, router: c}

	return c, rt, ns
}

// TestP10bRetireWaitsForInFlightRequest (review of P10, finding B1): a
// request admitted into a runtime keeps its State open until it returns;
// retire does not block its caller (reconcile serves every other tailnet);
// a retired runtime admits no new request; the State is closed once the
// request returns.
func TestP10bRetireWaitsForInFlightRequest(t *testing.T) {
	c, rt, ns := retireFixture(t)
	st := rt.h.state

	release, err := ns.enter()
	require.NoError(t, err, "a bound session admits a request")

	start := time.Now()
	c.retire(rt, "test")
	assert.Less(t, time.Since(start), time.Second, "retire must not wait for the runtime's requests")

	_, ok := c.Runtime(rt.id)
	assert.False(t, ok, "a retired runtime is out of routing at once")

	_, err = ns.enter()
	require.ErrorIs(t, err, errRuntimeRetired, "a retired runtime admits no new request")

	time.Sleep(500 * time.Millisecond)
	require.False(t, st.Closed(), "State closed under a request still in flight")

	release()
	require.Eventually(t, st.Closed, 5*time.Second, 20*time.Millisecond, "State is closed once the request returned")

	c.stopping.Wait()
}

// TestP10bRetireDrainTimeoutLeavesStateOpen: a request that never returns
// does not get a closed State under it; the stop gives up after
// runtimeDrainTimeout and leaves the State open (writes stay fenced).
func TestP10bRetireDrainTimeoutLeavesStateOpen(t *testing.T) {
	saved := runtimeDrainTimeout
	runtimeDrainTimeout = 200 * time.Millisecond

	t.Cleanup(func() { runtimeDrainTimeout = saved })

	c, rt, ns := retireFixture(t)
	st := rt.h.state

	release, err := ns.enter()
	require.NoError(t, err)

	c.retire(rt, "test")
	c.stopping.Wait() // returns after the drain timeout

	assert.False(t, st.Closed(), "State must stay open while a request is stuck")

	release()
	require.NoError(t, st.Close())
}

// TestP10bInflightGate: enter after drain is refused, drain waits for leave.
func TestP10bInflightGate(t *testing.T) {
	var g inflightGate

	require.True(t, g.enter())
	require.True(t, g.enter())

	done := make(chan bool, 1)

	go func() { done <- g.drain(5 * time.Second) }()

	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()

		return g.closed
	}, time.Second, time.Millisecond)
	require.False(t, g.enter(), "a closed gate refuses")

	g.leave()

	select {
	case <-done:
		t.Fatal("drain returned with one request still in")
	case <-time.After(100 * time.Millisecond):
	}

	g.leave()
	assert.True(t, <-done)
	assert.True(t, g.drain(time.Millisecond), "draining an empty gate again succeeds")
}
