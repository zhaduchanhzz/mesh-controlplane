package hscontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errDBDown = errors.New("database down")

// TestP12cHealthDoesNotPingDatabaseEveryRequest (L1): the public, unauthenticated
// /health of a cluster process must not turn every request into a round trip
// to the shared database (pool and PgBouncer of every cluster). Many
// requests, concurrent or not, within a short interval cost one ping; the
// answer still follows the database within that interval.
func TestP12cHealthDoesNotPingDatabaseEveryRequest(t *testing.T) {
	var (
		pings atomic.Int32
		down  atomic.Bool
	)

	c := &Cluster{healthPing: func(context.Context) error {
		pings.Add(1)

		if down.Load() {
			return errDBDown
		}

		return nil
	}}

	health := func() int {
		rec := httptest.NewRecorder()
		c.HealthHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil))

		return rec.Code
	}

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 25 {
				assert.Equal(t, http.StatusOK, health())
			}
		})
	}

	wg.Wait()

	require.LessOrEqual(t, pings.Load(), int32(1), "200 /health requests at once pinged the database %d times", pings.Load())

	// A database outage shows within about a second.
	down.Store(true)

	require.Eventually(t, func() bool { return health() == http.StatusInternalServerError },
		3*time.Second, 50*time.Millisecond, "/health still passes with the database down")

	before := pings.Load()

	for range 50 {
		assert.Equal(t, http.StatusInternalServerError, health())
	}

	assert.LessOrEqual(t, pings.Load()-before, int32(1), "failed pings are not retried on every request")
}
