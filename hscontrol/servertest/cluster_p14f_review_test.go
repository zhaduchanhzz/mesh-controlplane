//go:build p05cluster

package servertest_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"tailscale.com/types/netmap"
)

// TestP14fReviewTenantPolicyOverPeerBound (P14f review R2): a tenant whose
// nodes joined under its default deny cannot then link them all past the
// tailnet's peer bound with its own key: 400 with a reason, nothing stored,
// no revision moved, the deny stays in force; validate says why.
func TestP14fReviewTenantPolicyOverPeerBound(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI, MaxPeerEntries: 6},
		servertest.TailnetSeed{Slug: tenantA})

	clients := enroll(t, tc.Tenant(t, tenantA), "alice", "r2-a1", "r2-a2", "r2-a3", "r2-a4")
	id := tc.Tailnet(t, tenantA).ID.String()
	key := tc.MintAPIKey(t, tenantA)

	rev0 := tailnetRevisions(t, tc, id)
	before := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", key, "", map[string]string{"Accept": "application/hujson"})
	require.Equal(t, http.StatusOK, before.code)

	allow := `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`

	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", key, allow, nil)
	assert.Equal(t, http.StatusBadRequest, r.code, r.body)
	assert.Contains(t, r.body, "peer", "the refusal says why")

	after := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", key, "", map[string]string{"Accept": "application/hujson"})
	assert.Equal(t, before.body, after.body, "nothing stored")
	assert.Equal(t, rev0, tailnetRevisions(t, tc, id), "no revision moved")

	for _, c := range clients {
		assert.True(t, func(nm *netmap.NetworkMap) bool { return len(nm.Peers) == 0 }(c.Netmap()),
			"%s still denied", c.Name)
	}

	v := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl/validate", key, allow, nil)
	require.Equal(t, http.StatusOK, v.code, v.body)
	assert.Contains(t, v.body, "peer", "validate gives the reason")
}
