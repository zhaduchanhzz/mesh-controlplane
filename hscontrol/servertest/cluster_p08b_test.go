//go:build p05cluster

package servertest_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P08b, live operator administration (owner decision Q2, direction A): the
// operator deletes or expires a node, approves routes and deletes a user of
// tailnet A while the cluster runs. A's peers see it through the delta path,
// B's clients get no frame, an ID of B named through A is 404 and changes
// nothing, and the endpoint exists only on the unix socket.

func adminPath(id tailnet.ID, rest string) string {
	return "/cluster/v1/tailnet/" + id.String() + rest
}

func nodeIDOf(t *testing.T, s *servertest.TestServer, c *servertest.TestClient) types.NodeID {
	t.Helper()

	nv, ok := s.State().GetNodeByNodeKey(selfKey(t, c))
	require.True(t, ok, "%s is a node", c.Name)

	return nv.ID()
}

func twoTailnetMesh(t *testing.T) (*servertest.TestCluster, []*servertest.TestClient, []*servertest.TestClient) {
	t.Helper()

	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	tc.SetPolicy(t, tenantB, allowAllWithSSH)

	aClients := enroll(t, tc.Tenant(t, tenantA), "alice", "p08b-a1", "p08b-a2", "p08b-a3")
	bClients := enroll(t, tc.Tenant(t, tenantB), "bob", "p08b-b1", "p08b-b2")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, true)
		}
	}

	return tc, aClients, bClients
}

func TestP08bAdminDeleteNodeLive(t *testing.T) {
	tc, aClients, bClients := twoTailnetMesh(t)
	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	idA, idB := tc.Tailnet(t, tenantA).ID, tc.Tailnet(t, tenantB).ID
	a1, a3 := aClients[0], aClients[2]

	before := settledFrames(t, bClients)
	victim := nodeIDOf(t, a, a3)

	code, body := tc.Admin(t, nil, http.MethodDelete, adminPath(idA, fmt.Sprintf("/node/%d", victim)), nil)
	require.Equal(t, http.StatusOK, code, "delete: %s", body)

	a1.WaitForCondition(t, "a3 gone from a1", clusterWait, func(*netmap.NetworkMap) bool {
		_, ok := a1.PeerByName("p08b-a3")

		return !ok
	})

	_, ok := a.State().GetNodeByID(victim)
	assert.False(t, ok, "deleted from A's NodeStore")

	for _, r := range tc.NodeRows(t) {
		assert.NotEqual(t, uint64(victim), r.ID, "deleted node row remains")
	}

	assertQuiet(t, bClients, before, "a live delete in A")

	// B's node through A: 404, nothing changes in B.
	b1 := nodeIDOf(t, b, bClients[0])
	for _, call := range []struct{ method, path string }{
		{http.MethodDelete, fmt.Sprintf("/node/%d", b1)},
		{http.MethodPost, fmt.Sprintf("/node/%d/expire", b1)},
	} {
		code, body := tc.Admin(t, nil, call.method, adminPath(idA, call.path), nil)
		assert.Equal(t, http.StatusNotFound, code, "%s %s via A: %s", call.method, call.path, body)
	}

	code, body = tc.Admin(t, nil, http.MethodPost, adminPath(idA, fmt.Sprintf("/node/%d/routes", b1)),
		map[string]any{"routes": []string{"10.9.0.0/24"}})
	assert.Equal(t, http.StatusNotFound, code, "routes of B's node via A: %s", body)

	code, _ = tc.Admin(t, nil, http.MethodDelete, adminPath(idA, "/user/bob"), nil)
	assert.Equal(t, http.StatusNotFound, code, "B's user via A")

	nb, ok := b.State().GetNodeByID(b1)
	require.True(t, ok, "B's node still exists")
	assert.False(t, nb.IsExpired(), "B's node not expired")
	assert.Empty(t, nb.ApprovedRoutes().AsSlice(), "B's node got no route")

	_, err := b.State().GetUserByName("bob")
	require.NoError(t, err, "B's user still exists")

	assertQuiet(t, bClients, before, "admin calls naming B's node or user through A")

	// Tailnets this process does not serve, and names that are not IDs.
	other, err := tailnet.ParseID("0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f")
	require.NoError(t, err)

	for _, p := range []string{
		adminPath(other, "/nodes"),
		"/cluster/v1/tailnet/" + tenantB + "/nodes",
		"/cluster/v1/tailnet/-/nodes",
		"/cluster/v1/tailnet/" + idB.String() + "x/nodes",
	} {
		code, _ := tc.Admin(t, nil, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusNotFound, code, "GET %s", p)
	}
}

func TestP08bAdminExpireRoutesUserLive(t *testing.T) {
	tc, aClients, bClients := twoTailnetMesh(t)
	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	idA, idB := tc.Tailnet(t, tenantA).ID, tc.Tailnet(t, tenantB).ID
	a1, a2 := aClients[0], aClients[1]

	// a1 announces a route before the quiet window starts.
	route := netip.MustParsePrefix("10.78.0.0/24")
	a1.Direct().SetHostinfo(&tailcfg.Hostinfo{BackendLogID: "servertest-p08b-a1", Hostname: "p08b-a1", RoutableIPs: []netip.Prefix{route}})
	require.NoError(t, sendUpdate(a1))
	require.Eventually(t, func() bool {
		nv, ok := a.State().GetNodeByID(nodeIDOf(t, a, a1))

		return ok && slices.Contains(nv.AnnouncedRoutes(), route)
	}, clusterWait, 100*time.Millisecond, "A records a1's announced route")

	before := settledFrames(t, bClients)

	// Expire a2: a1 sees it expired.
	idA2 := nodeIDOf(t, a, a2)
	code, body := tc.Admin(t, nil, http.MethodPost, adminPath(idA, fmt.Sprintf("/node/%d/expire", idA2)), nil)
	require.Equal(t, http.StatusOK, code, "expire: %s", body)

	a1.WaitForCondition(t, "a1 sees a2 expired", clusterWait, func(*netmap.NetworkMap) bool {
		p, ok := a1.PeerByName("p08b-a2")

		return !ok || p.Expired() || (!p.KeyExpiry().IsZero() && p.KeyExpiry().Before(time.Now()))
	})
	assertQuiet(t, bClients, before, "a live expire in A")

	// Approve a1's route: a3 sees it as a primary route of a1.
	idA1 := nodeIDOf(t, a, a1)
	code, body = tc.Admin(t, nil, http.MethodPost, adminPath(idA, fmt.Sprintf("/node/%d/routes", idA1)),
		map[string]any{"routes": []string{route.String()}})
	require.Equal(t, http.StatusOK, code, "approve: %s", body)

	a3 := aClients[2]
	a3.WaitForCondition(t, "a3 sees a1's approved route", clusterWait, func(*netmap.NetworkMap) bool {
		p, ok := a3.PeerByName("p08b-a1")

		return ok && slices.Contains(p.PrimaryRoutes().AsSlice(), route)
	})
	assertQuiet(t, bClients, before, "a live route approval in A")

	// Bad bodies are refused before anything changes.
	for _, bad := range []any{
		map[string]any{"routes": []string{"10.78.0.1/24"}},
		map[string]any{"routes": []string{"nope"}},
		map[string]any{"routes": []string{}, "extra": 1},
	} {
		code, body := tc.Admin(t, nil, http.MethodPost, adminPath(idA, fmt.Sprintf("/node/%d/routes", idA1)), bad)
		assert.Equal(t, http.StatusBadRequest, code, "%v: %s", bad, body)
	}

	// The listing has A's nodes only, with the route.
	code, body = tc.Admin(t, nil, http.MethodGet, adminPath(idA, "/nodes"), nil)
	require.Equal(t, http.StatusOK, code)

	var list struct {
		Nodes []struct {
			ID       uint64   `json:"id"`
			Hostname string   `json:"hostname"`
			Approved []string `json:"approved_routes"`
			Expired  bool     `json:"expired"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(body, &list))

	var hosts []string
	for _, n := range list.Nodes {
		hosts = append(hosts, n.Hostname)
		if n.ID == uint64(idA1) {
			assert.Equal(t, []string{route.String()}, n.Approved)
		}

		if n.ID == uint64(idA2) {
			assert.True(t, n.Expired, "a2 listed as expired")
		}
	}

	assert.ElementsMatch(t, []string{"p08b-a1", "p08b-a2", "p08b-a3"}, hosts)
	assertNoTokens(t, "A's admin listing", string(body), []string{"p08b-b1", "p08b-b2", "bob"})

	// Users: an empty user goes, a user with nodes stays (409).
	a.CreateUser(t, "spare")

	code, body = tc.Admin(t, nil, http.MethodDelete, adminPath(idA, "/user/spare"), nil)
	require.Equal(t, http.StatusOK, code, "delete user: %s", body)

	_, err := a.State().GetUserByName("spare")
	require.Error(t, err, "spare deleted")

	code, _ = tc.Admin(t, nil, http.MethodDelete, adminPath(idA, "/user/alice"), nil)
	assert.Equal(t, http.StatusConflict, code, "a user with nodes is not deleted")

	_, err = a.State().GetUserByName("alice")
	require.NoError(t, err)

	assertQuiet(t, bClients, before, "admin calls in A")

	// B's own listing is B only.
	code, body = tc.Admin(t, nil, http.MethodGet, adminPath(idB, "/nodes"), nil)
	require.Equal(t, http.StatusOK, code)
	assertNoTokens(t, "B's admin listing", string(body), []string{"p08b-a1", "p08b-a2", "p08b-a3", "alice"})

	_ = b
}

// The admin API is on the unix socket only: neither the public listener
// nor the metrics listener answers it.
func TestP08bAdminNotOnNetworkListeners(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	idA := tc.Tailnet(t, tenantA).ID

	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
		req, err := http.NewRequestWithContext(t.Context(), m, tc.URL+adminPath(idA, "/nodes"), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, resp.StatusCode,
			"public listener %s admin path", m)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, adminPath(idA, "/nodes"), nil)
	req.RemoteAddr = "127.0.0.1:5555"
	tc.Cluster.MetricsHandler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "metrics listener admin path")

	code, _ := tc.Admin(t, nil, http.MethodGet, adminPath(idA, "/nodes"), nil)
	assert.Equal(t, http.StatusOK, code, "unix socket admin path")
}

// A standby process answers 503 and changes nothing; the active one serves.
func TestP08bAdminStandbyRefuses(t *testing.T) {
	tc := newHACluster(t)
	active := tc.Primary()
	standby := tc.StartProcess(t)
	idA := tc.Tailnet(t, tenantA).ID

	code, body := tc.Admin(t, standby, http.MethodGet, adminPath(idA, "/nodes"), nil)
	assert.Equal(t, http.StatusServiceUnavailable, code, "standby: %s", body)

	code, _ = tc.Admin(t, standby, http.MethodDelete, adminPath(idA, "/node/1"), nil)
	assert.Equal(t, http.StatusServiceUnavailable, code, "standby delete")

	code, body = tc.Admin(t, active, http.MethodGet, adminPath(idA, "/nodes"), nil)
	assert.Equal(t, http.StatusOK, code, "active: %s", body)
}
