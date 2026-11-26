//go:build p05cluster

package servertest_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// P12c group B (review 2026-11-26): availability of a cluster process under
// one tenant's load or unauthenticated garbage (B4), runtimes routable only
// once started (B5), Noise sessions forgotten when they close (B6), public
// /health and /ready (L1, L2).

// garbageAPIKey returns a token shaped like a platform API key (it reaches
// the database lookup) whose identifier names no key.
func garbageAPIKey(t *testing.T, real string, n int) string {
	t.Helper()

	const prefix = "hskey-api-"

	rest, ok := strings.CutPrefix(real, prefix)
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
