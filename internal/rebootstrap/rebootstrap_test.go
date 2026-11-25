package rebootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testKey    = "hskey-auth-0123456789ab-s3cretS3cretS3cret"
	testSecret = "s3cretS3cretS3cret"
)

func TestCredentialOfNeverKeepsTheSecret(t *testing.T) {
	c, err := CredentialOf(testKey + "\n")
	require.NoError(t, err)
	assert.Equal(t, "0123456789ab", c.KeyID)
	assert.Len(t, c.Proof, 64)
	assert.NotContains(t, fmt.Sprintf("%+v", c), testSecret)

	for _, bad := range []string{"", "tskey-auth-x", "hskey-api-0123456789ab-x", "hskey-auth-XYZ-x", "hskey-auth-0123456789ab-"} {
		_, err := CredentialOf(bad)
		require.ErrorIs(t, err, ErrBadKey, bad)
	}
}

// Resolve waits out MOVING (503 + Retry-After) and returns the new owner's
// URL; the request carries the key id and proof, never the secret.
func TestResolveWaitsWhileMoving(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.NotContains(t, string(body), testSecret, "the secret never leaves the tool")
		assert.Equal(t, "/v1/bootstrap", r.URL.Path)

		var req struct {
			Credential struct{ Kind, KeyID, Proof string } `json:"credential"`
			Tenant     string                              `json:"tenant"`
		}
		assert.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "acme", req.Tenant)

		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(`{"control_url":"https://c2.example:8443","issued_at":"2026-11-26T10:00:00Z"}`))
	}))
	defer srv.Close()

	var slept time.Duration

	r := &Resolver{
		BaseURL: srv.URL, Tenant: "acme", MaxWait: 2 * time.Minute,
		Sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil },
	}

	cred, err := CredentialOf(testKey)
	require.NoError(t, err)

	u, err := r.Resolve(context.Background(), cred)
	require.NoError(t, err)
	assert.Equal(t, "https://c2.example:8443", u)
	assert.Equal(t, 60*time.Second, slept, "honours Retry-After")
	assert.EqualValues(t, 3, calls.Load())
}

func TestResolveFailures(t *testing.T) {
	status := http.StatusServiceUnavailable
	answer := ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(status)

			return
		}

		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	cred, _ := CredentialOf(testKey)
	r := &Resolver{BaseURL: srv.URL, MaxWait: time.Minute, Sleep: func(context.Context, time.Duration) error { return nil }}

	_, err := r.Resolve(context.Background(), cred)
	require.ErrorIs(t, err, ErrStillMoving, "gives up after MaxWait")

	status = http.StatusNotFound
	_, err = r.Resolve(context.Background(), cred)
	require.ErrorIs(t, err, ErrRefused)

	status, answer = http.StatusOK, `{"control_url":"http://plain.example"}`
	_, err = r.Resolve(context.Background(), cred)
	require.Error(t, err, "only an https control URL is used")
}

// Run keeps at most `concurrency` commands in flight, records failures and
// computes t50/t90/t100 over all targets.
func TestRunBoundsConcurrencyAndMeasures(t *testing.T) {
	var inFlight, peak atomic.Int32

	targets := make([]string, 20)
	for i := range targets {
		targets[i] = fmt.Sprintf("t%02d", i)
	}

	run := func(_ context.Context, target, login string) ([]byte, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}

		defer inFlight.Add(-1)

		time.Sleep(20 * time.Millisecond)

		if target == "t07" {
			return []byte("auth failed for " + testKey), errors.New("exit status 1")
		}

		return []byte("ok " + login), nil
	}

	s := Run(context.Background(), "https://c2.example:8443", targets, 8, time.Second, run)
	assert.LessOrEqual(t, peak.Load(), int32(8))
	assert.Equal(t, 20, s.Targets)
	assert.Equal(t, 19, s.OK)
	assert.Equal(t, 1, s.Failed)
	assert.Positive(t, s.T50MS)
	assert.GreaterOrEqual(t, s.T90MS, s.T50MS)
	assert.EqualValues(t, -1, s.T100MS, "one target never re-logged")

	for _, r := range s.Results {
		assert.NotContains(t, r.OutputTail, testSecret, "output is redacted")

		if r.Target == "t07" {
			assert.False(t, r.OK)
			assert.Contains(t, r.OutputTail, "hskey-auth-[REDACTED]")
		}
	}
}

func TestRunPerTargetTimeout(t *testing.T) {
	run := func(ctx context.Context, _, _ string) ([]byte, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}

	s := Run(context.Background(), "https://c2", []string{"slow"}, 1, 50*time.Millisecond, run)
	assert.Equal(t, 1, s.Failed)
	assert.True(t, strings.Contains(s.Results[0].Error, "deadline"))
}

func TestSummarizePercentiles(t *testing.T) {
	var res []Result
	for i := 1; i <= 10; i++ {
		res = append(res, Result{Target: fmt.Sprint(i), OK: true, EndMS: int64(i * 100)})
	}

	s := Summarize("u", 8, res)
	assert.EqualValues(t, 500, s.T50MS)
	assert.EqualValues(t, 900, s.T90MS)
	assert.EqualValues(t, 1000, s.T100MS)

	assert.EqualValues(t, -1, Summarize("u", 8, nil).T50MS)
}

// With NotURL (the source cluster's URL) the operator tool keeps polling
// until bootstrap gives another URL, and Poll caps the server's Retry-After.
func TestResolveWaitsForANewURL(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1, 2:
			_, _ = w.Write([]byte(`{"control_url":"https://c1.example:8443"}`))
		case 3:
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte(`{"control_url":"https://c2.example:8445"}`))
		}
	}))
	defer srv.Close()

	var slept time.Duration

	r := &Resolver{
		BaseURL: srv.URL, NotURL: "https://c1.example:8443/", Poll: 500 * time.Millisecond, MaxWait: time.Minute,
		Sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil },
	}

	cred, _ := CredentialOf(testKey)
	u, err := r.Resolve(context.Background(), cred)
	require.NoError(t, err)
	assert.Equal(t, "https://c2.example:8445", u)
	assert.Equal(t, 1500*time.Millisecond, slept, "three waits, each capped at Poll")

	calls.Store(0)
	r.MaxWait = 700 * time.Millisecond
	_, err = r.Resolve(context.Background(), cred)
	require.ErrorIs(t, err, ErrStillMoving, "the old URL until the wait ends")
}
