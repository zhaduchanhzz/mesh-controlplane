package n1probe

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/derp/derpserver"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// derpWithVerify runs an in-process DERP server over TLS whose client
// verification asks verify (as derper --verify-client-url, fail-open off).
func derpWithVerify(t *testing.T, allow bool) (host, ip string, port int, calls *atomic.Int64) {
	t.Helper()

	calls = new(atomic.Int64)

	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		var req tailcfg.DERPAdmitClientRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		_ = json.NewEncoder(w).Encode(tailcfg.DERPAdmitClientResponse{Allow: allow})
	}))
	t.Cleanup(verify.Close)

	s := derpserver.New(key.NewNode(), t.Logf)
	s.SetVerifyClientURL(verify.URL)
	s.SetVerifyClientURLFailOpen(false)
	t.Cleanup(func() { _ = s.Close() })

	srv := httptest.NewUnstartedServer(derpserver.Handler(s))
	srv.StartTLS()
	t.Cleanup(srv.Close)

	h, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	pn, err := strconv.Atoi(p)
	require.NoError(t, err)

	return "example.com", h, pn, calls
}

// A relay whose verify URL denies admits none of the random keys: each
// attempt ends without a frame.
func TestDERPCheckRefusedByVerify(t *testing.T) {
	host, ip, port, calls := derpWithVerify(t, false)

	got, err := DERPCheck(context.Background(), DERPCheckConfig{
		Host: host, IP: ip, Port: port, Count: 3, Timeout: 5 * time.Second, Logf: t.Logf, insecureForTests: true,
	})
	require.NoError(t, err)
	require.Len(t, got, 3)

	for _, a := range got {
		assert.False(t, a.Admitted, "attempt %d: %+v", a.Attempt, a)
		assert.NotEmpty(t, a.Error)
	}

	assert.EqualValues(t, 3, calls.Load(), "every attempt was verified")
}

// Positive control: a relay that admits is detected as admitting.
func TestDERPCheckDetectsAdmission(t *testing.T) {
	host, ip, port, _ := derpWithVerify(t, true)

	got, err := DERPCheck(context.Background(), DERPCheckConfig{
		Host: host, IP: ip, Port: port, Count: 2, Timeout: 5 * time.Second, Logf: t.Logf, insecureForTests: true,
	})
	require.NoError(t, err)

	for _, a := range got {
		assert.True(t, a.Admitted, "attempt %d: %+v", a.Attempt, a)
	}
}

// With certificate verification (the command's mode), the test server's
// self-signed certificate is refused: nothing is admitted.
func TestDERPCheckVerifiesCertificate(t *testing.T) {
	host, ip, port, calls := derpWithVerify(t, true)

	got, err := DERPCheck(context.Background(), DERPCheckConfig{
		Host: host, IP: ip, Port: port, Count: 1, Timeout: 5 * time.Second, Logf: t.Logf,
	})
	require.NoError(t, err)
	assert.False(t, got[0].Admitted, "%+v", got[0])
	assert.Zero(t, calls.Load(), "refused at TLS, before any verify")

	_, err = DERPCheck(context.Background(), DERPCheckConfig{Host: host, IP: ip})
	assert.Error(t, err, "port is required")
}
