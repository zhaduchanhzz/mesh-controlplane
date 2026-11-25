package n1probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runner stops starting operations at the deadline, never runs more
// than maxInflight at once, and buckets the statuses.
func TestRunStopsAndCapsInflight(t *testing.T) {
	var cur, peak atomic.Int64

	statuses := []int{200, 404, 429, 503, 0}

	var n atomic.Int64

	fn := func(ctx context.Context) (int, time.Duration, error) {
		c := cur.Add(1)
		defer cur.Add(-1)

		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}

		time.Sleep(30 * time.Millisecond)

		s := statuses[n.Add(1)%int64(len(statuses))]
		if s == 0 {
			return 0, 0, context.DeadlineExceeded
		}

		return s, 30 * time.Millisecond, nil
	}

	t0 := time.Now()
	res := run(context.Background(), fn, 400, time.Now().Add(500*time.Millisecond), 4)
	el := time.Since(t0)

	assert.Less(t, el, 2*time.Second, "stops soon after the deadline")
	assert.LessOrEqual(t, peak.Load(), int64(4), "in-flight cap")
	assert.Positive(t, res.Skipped, "the cap skipped operations at 400/s")
	assert.Equal(t, res.Started, res.S2xx+res.S4xx+res.S429+res.S5xx+res.Errors, "every started op is counted once")

	for _, c := range []int{res.S2xx, res.S4xx, res.S429, res.S5xx, res.Errors} {
		assert.Positive(t, c)
	}

	assert.InDelta(t, 30, res.P50ms, 25)
}

// The bootstrap flood sends the key's proof, never the secret, and counts
// 200 and 429.
func TestFloodBootstrapCountsAndSendsProofOnly(t *testing.T) {
	const secret = "s3cretpart0123456789"

	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		raw, _ := json.Marshal(body)
		assert.NotContains(t, string(raw), secret, "the secret never leaves the probe")
		assert.Equal(t, "/v1/bootstrap", r.URL.Path)

		if calls.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = w.Write([]byte(`{"control_url":"https://x"}`))
	}))
	t.Cleanup(srv.Close)

	res, err := Flood(context.Background(), FloodConfig{
		Kind: KindBootstrap, BootstrapURL: srv.URL, Key: "hskey-auth-abc123-" + secret, Tenant: "a",
		Rate: 50, Duration: 400 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Positive(t, res.S2xx)
	assert.Positive(t, res.S429)
	assert.Equal(t, KindBootstrap, res.Kind)

	_, err = Flood(context.Background(), FloodConfig{Kind: "nope", Rate: 1, Duration: time.Second})
	require.ErrorIs(t, err, errUnknownKind)
}
