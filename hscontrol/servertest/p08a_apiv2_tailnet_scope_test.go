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

// p08aTwoTailnets returns two tailnets on ONE runtime (one process, one
// database). It cannot exist on master 2b3e98d: there is no tailnet_id, no
// CreateTailnet and no tailnet-bound API key (P03b schema, P04 credential
// binding, P05 shared runtime). It returns ok=false until then. Do not
// emulate tailnets with users (AGENTS.md: users are not tailnets) and do not
// use serve-tailnets: its runtimes have separate databases, so a pass there
// would prove nothing about the shared runtime.
func p08aTwoTailnets(t *testing.T) (url string, a, b p08aTailnet, ok bool) {
	t.Helper()

	return "", p08aTailnet{}, p08aTailnet{}, false
}

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
// Status: SKIPPED, blocked on P04/P05 (no way to build two tailnets on one
// runtime yet). This is not a pass.
func TestP08aAPIv2AdminKeyCannotAddressOtherTailnet(t *testing.T) {
	url, a, b, ok := p08aTwoTailnets(t)
	if !ok {
		t.Skip("BLOCKED (P04/P05): master has no tailnet_id, CreateTailnet or tailnet-bound API key, " +
			"and /api/v2 only accepts tailnet \"-\". Cross-tailnet API isolation cannot be exercised yet; " +
			"see evidence/P08a-endpoint-inventory.md. Not a pass.")
	}

	// Positive control: A's key on A's own tailnet works.
	code, _ := p08aDo(t, http.MethodGet, url+"/api/v2/tailnet/"+a.ID+"/devices", a.AdminKey, "")
	require.Equal(t, http.StatusOK, code)

	cross := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/devices", ""},
		{http.MethodGet, "/api/v2/tailnet/" + b.ID + "/keys", ""},
		{http.MethodPost, "/api/v2/tailnet/" + b.ID + "/keys", `{"capabilities":{}}`},
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

	// v1 never serves a tenant-bound key.
	code, _ = p08aDo(t, http.MethodGet, url+"/api/v1/node", a.AdminKey, "")
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, code)
}
