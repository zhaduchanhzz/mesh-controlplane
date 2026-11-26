//go:build p05cluster

package servertest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"tailscale.com/types/netmap"
)

// P12c A6 (AGENTS rule 8): an operator empties the policy of a tailnet that
// had a permissive one (revocation). The running cluster reloads it at the
// next reconcile and installs deny-all at once: its clients lose every peer
// and every filter rule. Keeping the previous, broader policy until a
// restart would let a revoked tenant keep its access.
func TestClusterEmptyPolicyReloadDenies(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	clients := enroll(t, tc.Tenant(t, tenantA), "alice", "revoke-a1", "revoke-a2")
	for _, c := range clients {
		waitTenantMesh(t, c, clients, false)
	}

	tc.SetPolicyViaDB(t, tenantA, "")

	for _, c := range clients {
		c.WaitForCondition(t, "deny-all after the policy was emptied", clusterWait, func(nm *netmap.NetworkMap) bool {
			return len(nm.Peers) == 0 && nm.PacketFilterRules.Len() == 0 && len(nm.PacketFilter) == 0
		})
	}

	rules, _ := tc.Tenant(t, tenantA).State().Filter()
	assert.Empty(t, rules, "the runtime's compiled filter is empty")
}
