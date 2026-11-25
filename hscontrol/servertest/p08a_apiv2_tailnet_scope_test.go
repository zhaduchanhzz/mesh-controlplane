package servertest_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p08aTailnet is one tenant on a shared multi-tailnet runtime: its verified
// TailnetID, an admin API key bound to it, and one device it owns.
type p08aTailnet struct {
	ID       string
	AdminKey string
	DeviceID string
}

// p08aTwoTailnets returns two tailnets of ONE cluster process (one
// database, one listener) with a platform-issued API key bound to each and a
// device each (P08b). Do not emulate tailnets with users (AGENTS.md: users
// are not tailnets). It needs the cluster harness (build tag p05cluster);
// without the tag it returns ok=false and the test is skipped, which is not
// a pass: the cluster suite runs it (p08a_twotailnets_cluster_test.go).

func p08aDo(t *testing.T, method, url, apiKey, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(b)
}

// TestP08aAPIv2AdminKeyCannotAddressOtherTailnet (I08, I09, I15; S14) is the
// intended /api/v2 contract for a multi-tailnet runtime:
//
//   - an admin API key is bound to exactly one verified TailnetID;
//   - /api/v2/tailnet/{id}/... with an id other than the key's tailnet is
//     404 "tailnet not found" (the same answer as a tailnet that does not
//     exist, so the API is not a tailnet existence oracle);
//   - "-" means the key's own tailnet, never "all tailnets";
//   - /api/v2/device/{id}/... has no {tailnet} segment, so the device's
//     tailnet must equal the key's tailnet, else 404, and nothing changes;
//   - /api/v1 (all-access, headscale-native) is a platform-admin API and
//     never answers a tenant-bound key with another tailnet's data.
//
// Status (P08b): runs in the cluster suite (-tags p05cluster) against the
// public /api/v2 of a cluster; skipped without the tag (not a pass).
func TestP08aAPIv2AdminKeyCannotAddressOtherTailnet(t *testing.T) {
	url, a, b, ok := p08aTwoTailnets(t)
	if !ok {
		t.Skip("needs the cluster harness: run with -tags p05cluster (P08b). Not a pass.")
	}

	// Positive control: A's key on A's own tailnet works.
	code, _ := p08aDo(t, http.MethodGet, url+"/api/v2/tailnet/"+a.ID+"/devices", a.AdminKey, "")
	require.Equal(t, http.StatusOK, code)

	cross := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/devices", ""},
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/keys", ""},
		// A valid body: huma validates bodies (422) before the handler
		// checks the tailnet, the same 422 for any tailnet.
		{http.MethodPost, "/api/v2/tailnet/" + b.ID + "/keys", `{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":false,"preauthorized":true}}}}`},
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/users", ""},
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/acl", ""},
		{http.MethodPost, "/api/v2/tailnet/" + b.ID + "/acl", `{"grants":[]}`},
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/settings", ""},
		{http.MethodGet, "/api/v2/device/" + b.DeviceID, ""},
		{http.MethodGet, "/api/v2/device/" + b.DeviceID + "/routes", ""},
		{http.MethodPost, "/api/v2/device/" + b.DeviceID + "/expire", ""},
		{http.MethodPost, "/api/v2/device/" + b.DeviceID + "/tags", `{"tags":[]}`},
		{http.MethodDelete, "/api/v2/device/" + b.DeviceID, ""},
	}

	for _, c := range cross {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			code, body := p08aDo(t, c.method, url+c.path, a.AdminKey, c.body)
			assert.Equal(t, http.StatusNotFound, code, "tailnet A's key reached tailnet B: %s", body)
			assert.NotContains(t, body, b.DeviceID)
		})
	}

	// "-" is the caller's tailnet only.
	code, body := p08aDo(t, http.MethodGet, url+"/api/v2/tailnet/-/devices", a.AdminKey, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, body, fmt.Sprintf("%q", b.DeviceID))

	// B's device is untouched and still visible to B.
	code, _ = p08aDo(t, http.MethodGet, url+"/api/v2/device/"+b.DeviceID, b.AdminKey, "")
	assert.Equal(t, http.StatusOK, code)

	// v1 never serves a tenant-bound key (P08b: not mounted on a cluster).
	code, _ = p08aDo(t, http.MethodGet, url+"/api/v1/node", a.AdminKey, "")
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, code)
}
