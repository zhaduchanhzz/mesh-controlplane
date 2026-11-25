//go:build p05cluster

package servertest_test

import (
	"strconv"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// p08aTwoTailnets: one cluster process serving A and B, an API key minted
// by the platform for each, one device each.
func p08aTwoTailnets(t *testing.T) (url string, a, b p08aTailnet, ok bool) {
	t.Helper()

	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	tc.SetPolicy(t, tenantB, allowAllWithSSH)

	out := map[string]*p08aTailnet{tenantA: &a, tenantB: &b}
	for slug, tn := range out {
		s := tc.Tenant(t, slug)
		c := enroll(t, s, "user-"+slug, "dev-"+slug)[0]

		tn.ID = tc.Tailnet(t, slug).ID.String()
		tn.AdminKey = tc.MintAPIKey(t, slug)
		tn.DeviceID = strconv.FormatUint(uint64(nodeIDOf(t, s, c)), 10)
	}

	return tc.URL, a, b, true
}
