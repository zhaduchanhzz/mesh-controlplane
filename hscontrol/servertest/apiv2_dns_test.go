package servertest_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// P12d: outside cluster mode the DNS settings come from the configuration
// file. The Tailscale DNS endpoints read them; writes are 501, as for the
// tailnet settings.
func TestAPIv2DNSSingleTailnetReadsConfig(t *testing.T) {
	srv := servertest.NewServer(t, servertest.WithRealListener())
	owner := srv.CreateUser(t, "dns")
	key := srv.CreateAPIKey(t, owner)

	do := func(method, path, body string) (int, string) {
		t.Helper()

		r := apiDoSingle(t, srv.URL, method, path, key, body)

		return r.code, r.body
	}

	code, body := do(http.MethodGet, "/api/v2/tailnet/-/dns/preferences", "")
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"magicDNS"`)

	for _, p := range []string{"nameservers", "searchpaths", "split-dns"} {
		code, body := do(http.MethodGet, "/api/v2/tailnet/-/dns/"+p, "")
		assert.Equal(t, http.StatusOK, code, "%s: %s", p, body)
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "nameservers", `{"dns":["9.9.9.9"]}`},
		{http.MethodPost, "searchpaths", `{"searchPaths":["corp.example"]}`},
		{http.MethodPatch, "split-dns", `{"corp.example":["10.0.0.53"]}`},
		{http.MethodPut, "split-dns", `{}`},
		{http.MethodPost, "preferences", `{"magicDNS":false}`},
	} {
		code, body := do(c.method, "/api/v2/tailnet/-/dns/"+c.path, c.body)
		assert.Equal(t, http.StatusNotImplemented, code, "%s %s: %s", c.method, c.path, body)
	}

	// Another tailnet than "-": 404, as everywhere.
	code, _ = do(http.MethodGet, "/api/v2/tailnet/other/dns/nameservers", "")
	assert.Equal(t, http.StatusNotFound, code)
}

type singleResp struct {
	code int
	body string
}

func apiDoSingle(t *testing.T, base, method, path, key, body string) singleResp {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return singleResp{resp.StatusCode, string(b)}
}
