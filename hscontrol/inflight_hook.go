package hscontrol

import (
	"sync"
	"testing"
)

// HoldRequestForTest admits one request into h's runtime, as a Noise request
// bound to it does, and returns its release (idempotent). Until released,
// retiring the runtime waits for it in its drain (up to runtimeDrainTimeout).
// Tests only: servertest holds a hand-over in its drain (P12c C1).
func (h *Headscale) HoldRequestForTest(tb testing.TB) func() {
	tb.Helper()

	if !h.inflight.enter() {
		tb.Fatalf("hscontrol: runtime already retired, cannot hold a request")
	}

	var once sync.Once

	return func() { once.Do(h.inflight.leave) }
}
