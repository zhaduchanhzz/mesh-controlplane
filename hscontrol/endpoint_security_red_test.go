package hscontrol

// P08a RED security tests (I09, I11, I15; S14, S15). They encode the
// contract P08 must implement and are EXPECTED TO FAIL on master 2b3e98d;
// see evidence/P08a-endpoint-inventory.md for the RED/GREEN table. They are
// not skipped: a red here is the open finding, not a flaky test.
//
// Intended rules (decided in P08a, implemented in P08):
//
//   Debug/metrics listener (S14, I09): an operator is a request whose TCP peer
//   is loopback, or is inside an explicit operator CIDR allowlist from config
//   (empty by default). ip.IsPrivate() is not an operator; neither is the
//   Tailscale CGNAT/ULA range, because every tenant node owns addresses
//   there. X-Forwarded-For is never trusted. /metrics follows the same rule
//   as /debug/* (no anonymous bypass).
//
//   /verify (S15, I11, I15): only an authenticated DERP server may call it
//   (credential bound to the DERP server's identity, e.g. mTLS or a per-DERP
//   verify secret mapped to the set of tailnets that DERP serves). An
//   unauthenticated public caller gets 401/403 with a body that does not
//   depend on the node key, so the endpoint is not an existence oracle. An
//   authenticated DERP is told Allow only for a node that exists, is not
//   expired, and belongs to a tailnet in that DERP's scope; unknown, expired
//   and deleted (revoked) keys are refused.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// TestP08aPublicRouterHasNoDiagnosticRoutes (S14, I09) is a regression
// guard: no debug, pprof, statsviz, expvar or metrics route is registered on
// the public router, with every optional route group on. Expected GREEN.
func TestP08aPublicRouterHasNoDiagnosticRoutes(t *testing.T) {
	for _, r := range publicRoutes(t) {
		p := strings.ToLower(r.Path)
		for _, bad := range []string{"/debug", "/metrics", "statsviz", "pprof", "/vars", "/varz"} {
			if strings.Contains(p, bad) && !strings.HasPrefix(p, "/api/v1/debug/") {
				t.Errorf("diagnostic route on the public router: %s", r)
			}
		}
	}

	// /api/v1/debug/node is an authenticated admin API operation (creates a
	// node), not a diagnostic page; pin that it stays authenticated.
	for _, r := range publicRoutes(t) {
		if strings.HasPrefix(r.Path, "/api/v1/debug/") {
			assert.Equal(t, "auth", r.Auth, "%s must require an API key", r)
		}
	}
}

// TestP08aDebugListenerRefusesNonOperator (S14, I09): the metrics/debug
// listener refuses a request whose peer is not an operator under the rule
// above. Host is "localhost" so only the remote-address rule is under test.
//
// Expected today: RED for statsviz from RFC1918/ULA peers
// (protectedDebugHandler admits ip.IsPrivate), RED for /debug/* from tenant
// CGNAT/ULA addresses (tsweb trusts the whole Tailscale range), RED for
// /metrics from anyone (no gate at all, and it also bypasses the Host guard).
func TestP08aDebugListenerRefusesNonOperator(t *testing.T) {
	app := p08aApp(t)
	handler := app.debugHTTPServer().Handler

	peers := map[string]string{
		"rfc1918 lab LAN":           "192.168.1.50:40000",
		"rfc1918 reverse proxy":     "10.0.0.5:40000",
		"ipv6 ULA":                  "[fd00::5]:40000",
		"tenant node CGNAT":         "100.64.0.7:40000",
		"tenant node Tailscale ULA": "[fd7a:115c:a1e0::7]:40000",
		"public internet":           "203.0.113.9:40000",
	}

	paths := []string{
		"/debug/statsviz/",
		"/debug/nodestore",
		"/debug/config",
		"/debug/pprof/goroutine",
		"/metrics",
	}

	for name, remote := range peers {
		for _, path := range paths {
			t.Run(name+" "+path, func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
				req.Host = "localhost"
				req.RemoteAddr = remote
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				assert.Equal(t, http.StatusForbidden, rec.Code,
					"RED S14/I09: %s from non-operator peer %s must be refused", path, remote)
			})
		}
	}

	// Positive control: loopback is an operator.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/overview", nil)
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "loopback operator must keep access")
}

func postVerify(t *testing.T, h http.Handler, nodeKey key.NodePublic, remote string) (int, string) {
	t.Helper()

	body, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: nodeKey})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/verify", bytes.NewReader(body))
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	b, err := io.ReadAll(rec.Result().Body)
	require.NoError(t, err)

	return rec.Code, string(b)
}

// TestP08aVerifyRejectsUnauthenticatedPublicCaller (S15, I11, I15): /verify
// on the public router must not answer an unauthenticated caller, and its
// answer must not differ between a registered and an unknown node key.
//
// Expected today: RED. /verify is mounted on the public router without any
// authentication and returns {"Allow":true} for any registered node key of
// any user (and, once P05 lands, of any tailnet in the process): a node-key
// existence oracle.
func TestP08aVerifyRejectsUnauthenticatedPublicCaller(t *testing.T) {
	app := p08aApp(t)
	user := app.state.CreateUserForTest("p08a-verify-public")
	node := putTestNodeInStore(t, app, user, "registered")
	router := app.HTTPHandler()

	const attacker = "203.0.113.9:40000"

	knownCode, knownBody := postVerify(t, router, node.NodeKey, attacker)
	unknownCode, unknownBody := postVerify(t, router, key.NewNode().Public(), attacker)

	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, knownCode,
		"RED S15/I11: unauthenticated /verify must be refused, got %d %q", knownCode, knownBody)
	assert.NotContains(t, knownBody, `"Allow":true`,
		"RED S15/I15: unauthenticated caller learned that a node key is registered")
	assert.Equal(t, unknownCode, knownCode,
		"RED I15: status must not depend on whether the node key exists")
	assert.Equal(t, unknownBody, knownBody,
		"RED I15: body must not depend on whether the node key exists")
}

// TestP08aVerifyRefusesUnknownExpiredRevoked (S15, I11): an authenticated
// DERP (simulated by calling handleVerifyRequest, the function the embedded
// DERP verify transport calls) admits only live nodes.
//
// Expected today: unknown and deleted GREEN; expired RED ("expiry ... do not
// gate it", pinned upstream by TestHandleVerifyRequest_AdmitsByNodeKey, which
// P08 must change).
func TestP08aVerifyRefusesUnknownExpiredRevoked(t *testing.T) {
	app := p08aApp(t)
	user := app.state.CreateUserForTest("p08a-verify-states")

	live := putTestNodeInStore(t, app, user, "live")

	expired := app.state.CreateNodeForTest(user, "expired")
	expired.Expiry = new(time.Now().Add(-time.Hour))
	app.state.PutNodeInStoreForTest(*expired)

	revoked := putTestNodeInStore(t, app, user, "revoked")
	rv, ok := app.state.GetNodeByID(revoked.ID)
	require.True(t, ok)
	_, err := app.state.DeleteNode(rv)
	require.NoError(t, err)

	for _, tt := range []struct {
		name string
		key  key.NodePublic
		want bool
	}{
		{"live (positive control)", live.NodeKey, true},
		{"unknown", key.NewNode().Public(), false},
		{"expired", expired.NodeKey, false},
		{"revoked (deleted)", revoked.NodeKey, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: tt.key})
			require.NoError(t, err)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/verify", bytes.NewReader(body))

			var out bytes.Buffer
			require.NoError(t, app.handleVerifyRequest(req, &out))

			var resp tailcfg.DERPAdmitClientResponse
			require.NoError(t, json.Unmarshal(out.Bytes(), &resp))

			assert.Equal(t, tt.want, resp.Allow, "S15/I11: DERP admission for %s node key", tt.name)
		})
	}
}
