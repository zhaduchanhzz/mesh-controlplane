package bootstrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doFrom is do with the client address the listener reports (the PROXY v2
// source when the bootstrap runs behind the lab HAProxy, P13f B7).
func doFrom(h http.Handler, remote, payload string) response {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://"+testHost+PathBootstrap, strings.NewReader(payload))
	req.Host = testHost
	req.RemoteAddr = remote
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	return response{rr.Code, rr.Header(), rr.Body.String()}
}

// P13f B7 (review P11): one address flooding POST /v1/bootstrap with
// unauthenticated requests drained the global bucket, and every tenant got
// 429. With the client address known (PROXY v2 from the trusted front
// door), the default limits stop the flooding address at its own bucket,
// before it is charged to the global one: another client's tenant is still
// answered.
func TestB7FloodFromOneAddressDoesNotStarveOthers(t *testing.T) {
	h := newTestHandler(fixture(), DefaultHandlerConfig(testHost))

	now := time.Now()
	h.now = func() time.Time { return now } // no refill during the flood

	limited := 0

	for range 1000 {
		r := doFrom(h, "203.0.113.7:40000", body("fffffffffff0", secretA, ""))
		if r.status == http.StatusTooManyRequests {
			limited++
		}
	}

	assert.Positive(t, limited, "the flooding address was never limited")

	r := doFrom(h, "198.51.100.9:50000", body(keyActive, secretA, ""))
	require.Equal(t, http.StatusOK, r.status, "a tenant behind another address is answered during the flood")

	// An IPv6 client cannot escape its bucket by walking its /64.
	v6Limited := 0

	for i := range 200 {
		addr := fmt.Sprintf("[2001:db8:1:2::%x]:443", i+1)
		if doFrom(h, addr, body("fffffffffff0", secretA, "")).status == http.StatusTooManyRequests {
			v6Limited++
		}
	}

	assert.Positive(t, v6Limited, "addresses of one IPv6 /64 share a bucket")
	assert.Equal(t, http.StatusOK, doFrom(h, "[2001:db8:9::1]:443", body(keyActive, secretA, "")).status,
		"another /64 is not limited")
}
