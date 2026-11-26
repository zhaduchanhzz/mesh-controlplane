package lease

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestManagerStepDown (P13e, ADR 0014 §3): StepDown ends the held lease
// from outside the loop: onLose is called at once with a "stepped down"
// reason, Current is false, the lease is released (a standby takes it
// without waiting for the TTL), and this manager does not take it again for
// a whole TTL. A standby's StepDown does nothing.
func TestManagerStepDown(t *testing.T) {
	s := openStore(t)
	a, _ := newManager(s, "c1", "a", fast)
	b, _ := newManager(s, "c1", "b", fast)

	if a.StepDown("nothing held") {
		t.Fatal("StepDown reported a lease before any was taken")
	}

	ctx := t.Context()

	var aTakes atomic.Int32

	aUp := make(chan struct{}, 4)
	aLost := make(chan string, 4)

	go a.Run(ctx, func(context.Context, Lease) error { aTakes.Add(1); aUp <- struct{}{}; return nil },
		func(_ Lease, r string) { aLost <- r })

	select {
	case <-aUp:
	case <-time.After(5 * time.Second):
		t.Fatal("a never acquired")
	}

	start := time.Now()

	if !a.StepDown("registry names another key") {
		t.Fatal("StepDown did not see the held lease")
	}

	select {
	case r := <-aLost:
		if !strings.Contains(r, "stepped down") {
			t.Fatalf("onLose reason %q", r)
		}

		t.Logf("onLose after %s: %s", time.Since(start).Round(time.Millisecond), r)
	case <-time.After(2 * time.Second):
		t.Fatal("no onLose after StepDown")
	}

	if _, ok := a.Current(); ok {
		t.Fatal("Current still reports the lease after a step-down")
	}

	// Released, not left to expire: a standby takes it well within the TTL.
	bUp := make(chan Lease, 1)

	go b.Run(ctx, func(_ context.Context, l Lease) error { bUp <- l; return nil }, func(Lease, string) {})

	select {
	case l := <-bUp:
		if l.Holder != "b" || l.Epoch != 2 {
			t.Fatalf("standby lease %+v", l)
		}

		if d := time.Since(start); d > fast.TTL {
			t.Fatalf("standby took over after %s, not before the TTL %s", d, fast.TTL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("standby never took over")
	}

	if n := aTakes.Load(); n != 1 {
		t.Fatalf("a took the lease %d times; it must wait a TTL after stepping down", n)
	}
}
