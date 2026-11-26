//go:build p05cluster

package servertest_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P12c group B (review 2026-11-26): availability of a cluster process under
// one tenant's load or unauthenticated garbage (B4), runtimes routable only
// once started (B5), Noise sessions forgotten when they close (B6), public
// /health and /ready (L1, L2).

// garbageAPIKey returns a token shaped like a platform API key (it reaches
// the database lookup) whose identifier names no key.
func garbageAPIKey(t *testing.T, minted string, n int) string {
	t.Helper()

	const prefix = "hskey-api-"

	rest, ok := strings.CutPrefix(minted, prefix)
	require.True(t, ok, "minted key has the platform prefix")

	i := strings.IndexByte(rest, '-')
	require.Positive(t, i, "minted key has an identifier")

	return prefix + fmt.Sprintf("%0*d", i, n) + rest[i:]
}

// TestP12cAPIOneTenantCannotStarveAnother (B4, invariant I10): the global
// bucket of the public /api/v2 is spent only by requests that pass their
// key's and tailnet's buckets, and API keys that do not authenticate spend
// a separate pre-authentication budget. Tenant A over its limit, and a
// flood of garbage keys, leave tenant B served.
func TestP12cAPIOneTenantCannotStarveAnother(t *testing.T) {
	ta := config.Default().Tuning.TenantAPI
	ta.GlobalRate, ta.GlobalBurst = 1, 10
	ta.TailnetRate, ta.TailnetBurst = 1, 3
	ta.KeyRate, ta.KeyBurst = 100, 100

	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: &ta},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})

	keyA, keyB := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantB)

	get := func(key string) int {
		return apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", key, "").code
	}

	require.Equal(t, http.StatusOK, get(keyB), "B before any load")

	// A hammers far over its tailnet's budget (burst 3) and the global
	// burst (10).
	codesA := map[int]int{}
	for range 40 {
		codesA[get(keyA)]++
	}

	assert.LessOrEqual(t, codesA[http.StatusOK], 3, "A is held to its tailnet budget: %v", codesA)
	assert.Equal(t, http.StatusOK, get(keyB), "B after A went over its limit (A's refused requests must not spend the global bucket): A got %v", codesA)

	// Garbage keys shaped like API keys, far over any pre-authentication
	// budget.
	codesG := map[int]int{}
	for n := range 300 {
		codesG[get(garbageAPIKey(t, keyB, n))]++
	}

	for code := range codesG {
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusTooManyRequests}, code, "garbage key answers: %v", codesG)
	}

	assert.Positive(t, codesG[http.StatusTooManyRequests], "the lookups of unauthenticated keys are bounded: %v", codesG)
	assert.Equal(t, http.StatusOK, get(keyB), "B after a flood of garbage keys: garbage got %v", codesG)
}

// TestP12cRuntimeNotRoutableBeforeStart (B5): a tailnet's runtime is
// reachable only once it started. A request that arrives while the runtime
// is built but not started (its map batcher does not exist yet) is refused
// cleanly, with no panic, and served once the runtime started.
func TestP12cRuntimeNotRoutableBeforeStart(t *testing.T) {
	const other = "p12c-other-cluster"

	tc := servertest.NewTestCluster(t,
		servertest.TailnetSeed{Slug: tenantA},
		servertest.TailnetSeed{Slug: tenantB, Owner: other, SeedNode: true})

	b := tc.Tailnet(t, tenantB)

	inWindow := make(chan struct{})
	release := make(chan struct{})

	var once bool

	tc.Cluster.SetLoadHook(func(id tailnet.ID) {
		if id != b.ID || once {
			return
		}

		once = true

		close(inWindow)
		<-release
	})
	t.Cleanup(func() { tc.Cluster.SetLoadHook(nil) })

	// B moves to this cluster: the next reconcile loads it.
	row := tc.TailnetRow(t, tenantB)
	tc.SetTailnetOwner(t, tenantB, tc.ClusterID, row.OwnerEpoch+1)

	select {
	case <-inWindow:
	case <-time.After(15 * time.Second):
		close(release)
		t.Fatal("B's runtime was never loaded")
	}

	noiseKey := tc.NoiseKey(t)

	sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, b.NodeMachine)
	resp, err := sess.LiteMap(t.Context(), b.NodeKey, b.NodeDisco, "192.0.2.1:41641")

	close(release)

	require.NoError(t, err, "map request in the load-to-start window")
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusServiceUnavailable}, resp.Status,
		"map request to a runtime not started yet: %d %s", resp.Status, resp.Body)

	tc.WaitRuntime(t, tenantB, true, 15*time.Second)

	require.Eventually(t, func() bool {
		s := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, b.NodeMachine)
		r, err := s.LiteMap(t.Context(), b.NodeKey, b.NodeDisco, "192.0.2.1:41641")

		return err == nil && r.OK()
	}, 10*time.Second, 100*time.Millisecond, "B is served once its runtime started")
}

// TestP12cNoiseSessionsForgottenOnClose (B6): a Noise session bound to a
// tailnet's runtime is forgotten when its connection closes, so reconnects
// do not grow the runtime's session set until it retires.
func TestP12cNoiseSessionsForgottenOnClose(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA, SeedNode: true})
	a := tc.Tailnet(t, tenantA)
	h := tc.Runtime(t, tenantA)
	noiseKey := tc.NoiseKey(t)

	const n = 10

	for round := range 3 {
		sessions := make([]*servertest.RawNoiseSession, 0, n)

		for range n {
			s := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, a.NodeMachine)

			r, err := s.LiteMap(t.Context(), a.NodeKey, a.NodeDisco, "192.0.2.1:41641")
			require.NoError(t, err)
			require.True(t, r.OK(), "round %d: map request: %d %s", round, r.Status, r.Body)

			sessions = append(sessions, s)
		}

		assert.Equal(t, n, h.BoundSessions(), "round %d: every open session is bound", round)

		for _, s := range sessions {
			require.NoError(t, s.Close())
		}

		assert.Eventually(t, func() bool { return h.BoundSessions() == 0 }, 10*time.Second, 50*time.Millisecond,
			"round %d: closed sessions are forgotten; %d still bound", round, h.BoundSessions())
	}
}

// TestP12cReadyBodyHasNoLeaseEpoch (L2): the public /ready says only
// whether this process serves; it does not expose the lease epoch (how
// often, and when, the cluster failed over). Status codes are unchanged:
// 200 on the active process, 503 on a standby (HAProxy and the E2E
// harnesses read only the code).
func TestP12cReadyBodyHasNoLeaseEpoch(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})

	resp, err := http.Get(tc.URL + "/ready") //nolint:noctx // test client, bounded by the test timeout
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"status":"ready"}`, string(body))
	assert.NotContains(t, string(body), "epoch")

	standby := tc.StartProcess(t)
	assert.Equal(t, http.StatusServiceUnavailable, tc.ProcessGet(t, standby, "/ready"), "standby /ready")
}
