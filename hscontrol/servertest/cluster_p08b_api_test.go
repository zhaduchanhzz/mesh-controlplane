//go:build p05cluster

package servertest_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/netmap"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P08b, the public /api/v2 of a cluster (owner decision Q3): a tenant key
// manages its own devices and pre-auth keys live, B gets no frame; policy,
// DNS settings and device trust settings are refused; only valid
// platform-issued keys get in; every key and tailnet is rate limited.

type apiResp struct {
	code int
	body string
}

func apiCall(t *testing.T, base, method, path, key, body string) apiResp {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	require.NoError(t, err)

	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return apiResp{resp.StatusCode, string(b)}
}

func TestP08bTenantAPIWritesLiveAndIsolated(t *testing.T) {
	tc, aClients, bClients := twoTailnetMesh(t)
	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	keyA := tc.MintUserAPIKey(t, tenantA, "alice")
	a1, a2, a3 := aClients[0], aClients[1], aClients[2]

	before := settledFrames(t, bClients)

	// Rename a2.
	id2 := nodeIDOf(t, a, a2)
	r := apiCall(t, tc.URL, http.MethodPost, fmt.Sprintf("/api/v2/device/%d/name", id2), keyA, `{"name":"renamed-a2"}`)
	require.Equal(t, http.StatusOK, r.code, r.body)
	a1.WaitForCondition(t, "a1 sees a2 renamed", clusterWait, func(nm *netmap.NetworkMap) bool {
		for _, p := range nm.Peers {
			if strings.HasPrefix(p.Name(), "renamed-a2.") {
				return true
			}
		}

		return false
	})

	// Expire a2, then delete a3.
	r = apiCall(t, tc.URL, http.MethodPost, fmt.Sprintf("/api/v2/device/%d/expire", id2), keyA, "")
	require.Equal(t, http.StatusOK, r.code, r.body)

	id3 := nodeIDOf(t, a, a3)
	r = apiCall(t, tc.URL, http.MethodDelete, fmt.Sprintf("/api/v2/device/%d", id3), keyA, "")
	require.Equal(t, http.StatusOK, r.code, r.body)
	a1.WaitForCondition(t, "a3 gone from a1", clusterWait, func(*netmap.NetworkMap) bool {
		_, ok := a1.PeerByName("p08b-a3")

		return !ok
	})

	// A pre-auth key created through the API enrolls into A only.
	r = apiCall(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/keys", keyA,
		`{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":false,"preauthorized":true}}}}`)
	require.Equal(t, http.StatusOK, r.code, r.body)

	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &created))
	require.NotEmpty(t, created.Key)

	a4 := servertest.NewClusterClient(t, a, "p08b-a4", servertest.ClusterClientOptions{AuthKey: created.Key})
	a1.WaitForCondition(t, "a1 sees a4", clusterWait, func(*netmap.NetworkMap) bool {
		_, ok := a1.PeerByName("p08b-a4")

		return ok
	})
	assertRowTailnet(t, tc, selfKey(t, a4), tc.Tailnet(t, tenantA).ID.String())

	// Revoke it through the API.
	r = apiCall(t, tc.URL, http.MethodDelete, "/api/v2/tailnet/-/keys/"+created.ID, keyA, "")
	require.Equal(t, http.StatusOK, r.code, r.body)

	assertQuiet(t, bClients, before, "tenant API writes in A")

	// Nothing of B changed.
	for _, c := range bClients {
		nv, ok := b.State().GetNodeByNodeKey(selfKey(t, c))
		require.True(t, ok)
		assert.False(t, nv.IsExpired())
	}
}

func TestP08bTenantAPIRefusals(t *testing.T) {
	tc := servertest.NewTestCluster(t,
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB},
		servertest.TailnetSeed{Slug: "foreign", Owner: "some-other-cluster"})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	a := tc.Tenant(t, tenantA)
	a1 := enroll(t, a, "alice", "p08b-a1")[0]
	id1 := nodeIDOf(t, a, a1)
	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID.String()

	p := tc.Platform(t)
	revBefore := tailnetRevisions(t, tc, idA)

	// Operator-only writes: 403 on the key's own tailnet, nothing changes.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v2/tailnet/-/acl", `{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`},
		{http.MethodPost, "/api/v2/tailnet/" + idA + "/acl", `{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`},
		{http.MethodPatch, "/api/v2/tailnet/-/settings", `{"devicesApprovalOn":true}`},
		{http.MethodPost, fmt.Sprintf("/api/v2/device/%d/authorized", id1), `{"authorized":true}`},
		{http.MethodPost, fmt.Sprintf("/api/v2/device/%d/key", id1), `{"keyExpiryDisabled":true}`},
		{http.MethodPost, fmt.Sprintf("/api/v2/device/%d/update-client", id1), ""},
		{http.MethodPost, "/api/v2/tailnet/-/keys", `{"keyType":"client","scopes":["devices:core"],"tags":["tag:x"]}`},
	} {
		r := apiCall(t, tc.URL, c.method, c.path, keyA, c.body)
		assert.Equal(t, http.StatusForbidden, r.code, "%s %s: %s", c.method, c.path, r.body)
	}

	assert.Equal(t, revBefore, tailnetRevisions(t, tc, idA), "no policy or config revision moved")

	nv, ok := a.State().GetNodeByID(id1)
	require.True(t, ok, "a1 still exists")
	assert.False(t, nv.IsExpired(), "a1 not expired")

	// Reads of the policy and settings are allowed.
	assert.Equal(t, http.StatusOK, apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "").code)
	assert.Equal(t, http.StatusOK, apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/settings", keyA, "").code)

	// Credentials: none, garbage, an OAuth-token-shaped bearer, an auth key,
	// a revoked and an expired API key are all 401 with the same body.
	revoked := tc.MintAPIKey(t, tenantA)
	revokeKey(t, tc, idA, revoked)

	expired := tc.MintAPIKey(t, tenantB)
	require.NoError(t, p.RegistryDB().Exec(`UPDATE credentials SET expiration = ? WHERE kind = 'api' AND tailnet_id = ?`,
		time.Now().UTC().Add(-time.Minute), tc.Tailnet(t, tenantB).ID.String()).Error)

	authKey := mintKey(t, a, "bob")

	var bodies []string

	for name, key := range map[string]string{
		"none": "", "garbage": "nope", "oauth token": "hskey-oauthtok-AAAAAAAAAAAA-" + strings.Repeat("a", 64),
		"auth key": authKey, "revoked": revoked, "expired": expired,
		"tampered": keyA[:len(keyA)-1] + flipLast(keyA[len(keyA)-1:]),
	} {
		r := apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", key, "")
		assert.Equal(t, http.StatusUnauthorized, r.code, "%s: %s", name, r.body)
		bodies = append(bodies, r.body)
	}

	for _, b := range bodies[1:] {
		assert.Equal(t, bodies[0], b, "every refused credential gets the same answer")
	}

	// HTTP Basic with the key as username (the Tailscale SDK) works.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.URL+"/api/v2/tailnet/-/devices", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(keyA+":")))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "basic auth")

	// A key of a tailnet this cluster does not serve: 404.
	keyForeign := tc.MintAPIKey(t, "foreign")
	r := apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", keyForeign, "")
	assert.Equal(t, http.StatusNotFound, r.code, r.body)

	// No v1, no OAuth token endpoint.
	assert.Equal(t, http.StatusNotFound, apiCall(t, tc.URL, http.MethodGet, "/api/v1/node", keyA, "").code)
	r = apiCall(t, tc.URL, http.MethodPost, "/api/v2/oauth/token", keyA, "grant_type=client_credentials")
	assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, r.code, r.body)
}

func TestP08bTenantAPIRateLimits(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{
		TenantAPI: &types.TenantAPIConfig{
			GlobalRate: 1000, GlobalBurst: 1000,
			TailnetRate: 1, TailnetBurst: 4,
			KeyRate: 1, KeyBurst: 2,
		},
	}, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})

	idA := tc.Tailnet(t, tenantA).ID.String()
	keyA1, keyA2, keyA3 := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantA)
	keyB := tc.MintAPIKey(t, tenantB)

	get := func(key string) int {
		return apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", key, "").code
	}

	// Per key: burst 2.
	assert.Equal(t, []int{200, 200, 429}, []int{get(keyA1), get(keyA1), get(keyA1)}, "key A1 burst")

	// Per tailnet: A has used 2 (+1 refused by its key, which spends no
	// tailnet token); burst 4 leaves 2 for A2, then A3 is refused although
	// its own bucket is full.
	assert.Equal(t, []int{200, 200}, []int{get(keyA2), get(keyA2)}, "key A2 within A's budget")
	assert.Equal(t, http.StatusTooManyRequests, get(keyA3), "A's tailnet budget is spent")

	// B is not limited by A.
	assert.Equal(t, []int{200, 200}, []int{get(keyB), get(keyB)}, "B unaffected by A's limit")

	r := apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", keyA3, "")
	assert.Equal(t, http.StatusTooManyRequests, r.code)

	// Per-tailnet metrics: served and limited by tailnet ID, on the
	// operator metrics listener only.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	tc.Cluster.MetricsHandler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	m := rec.Body.String()
	assert.Contains(t, m, `headscale_tailnet_api_requests_total{outcome="limited",tailnet="`+idA+`"}`)
	assert.Contains(t, m, `headscale_tailnet_api_requests_total{outcome="served",tailnet="`+idA+`"}`)
	assert.Contains(t, m, `headscale_tailnet_nodes{tailnet="`+idA+`"}`)
	assert.NotContains(t, m, tenantA, "metric labels are tailnet IDs, never slugs")
}

// A standby refuses the API.
func TestP08bTenantAPIStandby(t *testing.T) {
	tc := newHACluster(t)
	key := tc.MintAPIKey(t, tenantA)
	standby := tc.StartProcess(t)

	assert.Equal(t, http.StatusOK, apiCall(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/devices", key, "").code, "active via front door")

	r := apiCall(t, "http://"+standby.Addr, http.MethodGet, "/api/v2/tailnet/-/devices", key, "")
	// The standby's listener answers only the cluster host name (421 for
	// an IP literal); either way it never serves the API.
	assert.Contains(t, []int{http.StatusServiceUnavailable, http.StatusMisdirectedRequest}, r.code, r.body)
}

func tailnetRevisions(t *testing.T, tc *servertest.TestCluster, id string) [2]uint64 {
	t.Helper()

	var rev struct {
		PolicyRevision uint64
		ConfigRevision uint64
	}
	require.NoError(t, tc.Platform(t).RegistryDB().Raw(
		`SELECT policy_revision, config_revision FROM tailnets WHERE id = ?`, id).Scan(&rev).Error)

	return [2]uint64{rev.PolicyRevision, rev.ConfigRevision}
}

// revokeKey revokes API key key of tailnet id by dropping its hash, as the
// registry does.
func revokeKey(t *testing.T, tc *servertest.TestCluster, id, key string) {
	t.Helper()

	ident := strings.TrimPrefix(key, "hskey-api-")[:12]
	res := tc.Platform(t).RegistryDB().Exec(
		`UPDATE credentials SET revoked = ?, hash = NULL WHERE kind = 'api' AND tailnet_id = ? AND identifier = ?`,
		time.Now().UTC(), id, ident)
	require.NoError(t, res.Error)
	require.EqualValues(t, 1, res.RowsAffected)
}
