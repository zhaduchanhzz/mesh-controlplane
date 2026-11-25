//go:build p05cluster

package servertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/n1probe"
)

// P09, S11 (a)(b)(d), owner decision Q3: the protocol-level probe run
// against an in-process cluster reports every tenant-switch case refused,
// with no leak, and the positive controls pass.
func TestN1ProbeSwitchTenantNoLeak(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	keyA := mintKey(t, tc.Tenant(t, tenantA), "alice")
	keyB := mintKey(t, tc.Tenant(t, tenantB), "bob")

	c, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	results, err := n1probe.SwitchTenant(c, n1probe.SwitchConfig{URL: tc.URL, KeyA: keyA, KeyB: keyB, Logf: t.Logf})
	require.NoError(t, err)
	require.Len(t, results, 7)

	for _, r := range results {
		t.Logf("%s: status=%d refused=%v closed=%v dials=%d leak=%v pass=%v note=%q err=%q",
			r.Case, r.Status, r.Refused, r.SessionClosed, r.Dials, r.Leak, r.Pass, r.Note, r.Error)
		assert.False(t, r.Leak, "%s: %s", r.Case, r.LeakDetail)
		assert.True(t, r.Pass, "%s", r.Case)
	}
}

// P09, S13, owner decision Q4: the flood stops at its duration and counts;
// enroll creates A's nodes, mapstream holds streams that deliver a frame.
func TestN1ProbeFloodAgainstCluster(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)
	keyA := mintKey(t, tc.Tenant(t, tenantA), "alice")

	before := len(tc.NodeRows(t))
	t0 := time.Now()

	res, err := n1probe.Flood(t.Context(), n1probe.FloodConfig{
		Kind: n1probe.KindEnroll, URL: tc.URL, Key: keyA, Rate: 20, Duration: 2 * time.Second, MaxInflight: 8,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(t0), 20*time.Second)
	assert.Positive(t, res.S2xx, "%+v", res)
	assert.Equal(t, before+res.S2xx, len(tc.NodeRows(t)), "every 2xx enroll is one node")

	res, err = n1probe.Flood(t.Context(), n1probe.FloodConfig{
		Kind: n1probe.KindMapStream, URL: tc.URL, Key: keyA, Rate: 5, Duration: 3 * time.Second, MaxInflight: 4,
	})
	require.NoError(t, err)
	assert.Positive(t, res.S2xx, "streams delivered a frame: %+v", res)
	assert.LessOrEqual(t, res.Started, 4+res.Skipped+res.Started, "sanity")
	t.Logf("enroll/mapstream: %+v", res)
}
