package placementapi

import (
	"crypto/x509"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p13eKey is a well-formed Noise (machine) public key for tests.
const p13eKey = "mkey:" + "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

// TestClusterNoiseKeyFormat (P13e, ADR 0014 §3): the registry names the
// Noise key clients must see; a value that is not an mkey: public key is
// refused (400) on create and on PATCH instead of being stored, where the
// cluster's processes would refuse to serve with any key. "" still clears
// it (P12: the next process to take the lease records its own).
func TestClusterNoiseKeyFormat(t *testing.T) {
	e := newEnv(t)
	c := e.client(e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice))

	bad := []string{
		"mkey:c1",
		"not-a-key",
		"nodekey:" + strings.Repeat("5a", 32),
		"mkey:" + strings.Repeat("5a", 31),
		"mkey:" + strings.Repeat("zz", 32),
		"privkey:" + strings.Repeat("5a", 32),
	}

	for i, k := range bad {
		cl := map[string]any{
			"id": "bad", "endpoint_url": "https://bad.cp.example.test:8443", "class": "shared",
			"max_devices": 10, "noise_public_key": k,
		}
		code, out := e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: "bad" + string(rune('a'+i)), body: cl})
		assert.Equal(t, http.StatusBadRequest, code, "create with noise_public_key %q: %v", k, out)
	}

	cl := map[string]any{"id": "c1", "endpoint_url": "https://c1.cp.example.test:8443", "class": "shared", "state": "ACTIVE", "max_devices": 100}
	code, out := e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: "c1", body: cl})
	require.Equal(t, http.StatusCreated, code, out)

	for i, k := range bad {
		code, out := e.do(t, c, call{method: http.MethodPatch, path: "/v1/clusters/c1", key: "p" + string(rune('a'+i)), body: map[string]any{"noise_public_key": k}})
		assert.Equal(t, http.StatusBadRequest, code, "PATCH noise_public_key %q: %v", k, out)
	}

	code, out = e.do(t, c, call{method: http.MethodPatch, path: "/v1/clusters/c1", key: "ok", body: map[string]any{"noise_public_key": p13eKey}})
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, p13eKey, out["noise_public_key"])

	code, out = e.do(t, c, call{method: http.MethodPatch, path: "/v1/clusters/c1", key: "clear", body: map[string]any{"noise_public_key": ""}})
	require.Equal(t, http.StatusOK, code, out)
	assert.Nil(t, out["noise_public_key"], "cleared")
}
