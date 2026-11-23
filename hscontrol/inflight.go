package hscontrol

import (
	"sync"
	"time"
)

// inflightGate counts the Noise requests a tailnet runtime is serving (P10
// review B1). A request enters after its session is bound to the runtime and
// leaves when its handler returns, map long-polls included. Retiring the
// runtime closes the gate (no new request enters) and drains it before the
// runtime's State is closed, so no handler runs on a closed State.
//
// Unlike a sync.WaitGroup, enter after close is a defined refusal rather than
// a misuse, and drain has a timeout.
type inflightGate struct {
	mu     sync.Mutex
	n      int
	closed bool
	idle   chan struct{} // closed when closed && n == 0
}

// enter admits one request; false once the gate is closed.
func (g *inflightGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		return false
	}

	g.n++

	return true
}

// leave ends a request admitted by enter.
func (g *inflightGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.n--
	if g.closed && g.n == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// drain closes the gate and waits up to timeout for admitted requests to
// leave. It reports whether the gate is empty.
func (g *inflightGate) drain(timeout time.Duration) bool {
	g.mu.Lock()
	g.closed = true

	if g.n == 0 {
		g.mu.Unlock()

		return true
	}

	if g.idle == nil {
		g.idle = make(chan struct{})
	}

	idle := g.idle
	g.mu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

// inFlight is the number of admitted requests (tests, logs).
func (g *inflightGate) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.n
}
