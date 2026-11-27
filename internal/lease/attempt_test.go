package lease

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scaled is DefaultConfig divided by ten: the same ratios (TTL 10 s,
// renew 3 s, safety margin 2 s, so the first renewal leaves 5 s before
// the self-fence deadline), fast enough for a unit test.
var scaled = Config{
	TTL:             DefaultConfig.TTL / 10,
	RenewInterval:   DefaultConfig.RenewInterval / 10,
	AcquireInterval: DefaultConfig.AcquireInterval / 10,
	SafetyMargin:    DefaultConfig.SafetyMargin / 10,
}

// blackholeStore is a real store whose renewals can be made to hang until
// their context ends, like a statement written to a connection whose
// packets are dropped without a reset (PgBouncer blackholed, P13 drill K3;
// the primary's VM stopped hard, F05.3). ignoreCtx makes a hung renewal
// outlive its context as well (a driver that does not honour it).
type blackholeStore struct {
	*Store

	hang      func(n int64) bool // renewal n (1-based) hangs
	ignoreCtx bool
	release   chan struct{} // closed at the end of the test (ignoreCtx)

	calls       atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (b *blackholeStore) Renew(ctx context.Context, l Lease, ttl time.Duration) error {
	n := b.calls.Add(1)

	cur := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)

	for {
		m := b.maxInFlight.Load()
		if cur <= m || b.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}

	if b.hang(n) {
		if b.ignoreCtx {
			<-b.release

			return context.Canceled
		}

		<-ctx.Done()

		return ctx.Err()
	}

	return b.Store.Renew(ctx, l, ttl)
}

// runHeld starts m and waits until it holds the lease; the channel it
// returns receives every onLose reason.
func runHeld(t *testing.T, m *Manager) chan string {
	t.Helper()

	acquired := make(chan struct{}, 1)
	lost := make(chan string, 4)

	go m.Run(t.Context(), func(context.Context, Lease) error { acquired <- struct{}{}; return nil },
		func(_ Lease, r string) { lost <- r })

	select {
	case <-acquired:
		return lost
	case <-time.After(5 * time.Second):
		t.Fatal("never acquired")
	}

	return nil
}

// TestManagerRetriesAfterHungRenewal (P13 drill K3, F05.3/F05.4): one
// renewal hangs on a dead connection, the next ones would succeed at once
// (a fresh connection, or the second PgBouncer of the host list). The hung
// attempt must not hold the lease hostage until the self-fence deadline: it
// is abandoned after its own timeout and retried, so the process keeps its
// lease, with no failover (D12: killing a PgBouncer causes 0 failovers).
func TestManagerRetriesAfterHungRenewal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ignoreCtx bool
	}{
		{"hung renewal honours its context", false},
		{"hung renewal ignores its context", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &blackholeStore{
				Store:     openStore(t),
				hang:      func(n int64) bool { return n == 1 },
				ignoreCtx: tc.ignoreCtx,
				release:   make(chan struct{}),
			}
			defer close(b.release)

			m, err := newManager(b, "c1", "a", scaled)
			if err != nil {
				t.Fatal(err)
			}

			lost := runHeld(t, m)

			select {
			case r := <-lost:
				t.Fatalf("lease ended although only one renewal hung: %s (renewals tried: %d)", r, b.calls.Load())
			case <-time.After(3 * scaled.TTL):
			}

			l, ok := m.Current()
			if !ok || l.Epoch != 1 {
				t.Fatalf("Current %+v ok=%v, want epoch 1 still held", l, ok)
			}

			row, _, _ := b.Get(context.Background(), "c1")
			if row.Holder != "a" || row.Epoch != 1 || row.Expired {
				t.Fatalf("lease row %+v, want holder a, epoch 1, live", row)
			}

			if n := b.maxInFlight.Load(); n > renewInFlightBound {
				t.Fatalf("%d renewals in flight at once, at most %d allowed", n, renewInFlightBound)
			}
		})
	}
}

// TestManagerSelfFencesWhenEveryRenewalHangs: retrying never extends what
// a process may serve without the database. When every renewal hangs, the
// process still stops at its deadline (last success + TTL - margin), the
// row is still live then, attempts never overlap past the bound, and the
// retries are spaced (not a busy loop).
func TestManagerSelfFencesWhenEveryRenewalHangs(t *testing.T) {
	for _, ignoreCtx := range []bool{false, true} {
		b := &blackholeStore{
			Store:     openStore(t),
			hang:      func(int64) bool { return true },
			ignoreCtx: ignoreCtx,
			release:   make(chan struct{}),
		}

		m, err := newManager(b, "c1", "a", scaled)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())

		acquired := make(chan time.Time, 1)
		lost := make(chan time.Time, 1)

		go m.Run(ctx, func(context.Context, Lease) error { acquired <- time.Now(); return nil },
			func(Lease, string) { lost <- time.Now() })

		t0 := <-acquired

		select {
		case at := <-lost:
			window := scaled.TTL - scaled.SafetyMargin
			if d := at.Sub(t0); d > window+100*time.Millisecond {
				t.Fatalf("ignoreCtx=%v: self-fence after %s, want at most about %s", ignoreCtx, d, window)
			}

			row, _, _ := b.Get(context.Background(), "c1")
			if row.Expired {
				t.Fatalf("ignoreCtx=%v: row expired before the process stopped", ignoreCtx)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("ignoreCtx=%v: never self-fenced", ignoreCtx)
		}

		cancel()
		close(b.release)

		if n := b.maxInFlight.Load(); n > renewInFlightBound {
			t.Fatalf("ignoreCtx=%v: %d renewals in flight at once, at most %d", ignoreCtx, n, renewInFlightBound)
		}

		// Within one window: the first at RenewInterval, then retries
		// with backoff; a busy loop would make hundreds.
		if n := b.calls.Load(); n > 12 {
			t.Fatalf("ignoreCtx=%v: %d renewals in one window: retries are not spaced", ignoreCtx, n)
		}

		t.Logf("ignoreCtx=%v: %d renewals tried before the self-fence", ignoreCtx, b.calls.Load())
	}
}

// auditStore records when each successful acquire or renewal started (at
// the store, so no earlier than the manager's own start), to check the
// self-fence deadline against them. Odd renewals succeed only after their
// context ended (the UPDATE committed, the answer came late); even ones
// succeed at once. Late successes then arrive after newer ones.
type auditStore struct {
	*Store

	mu        sync.Mutex
	latest    time.Time // start of the newest attempt known to have succeeded
	calls     int64
	lateDelay time.Duration
}

func (a *auditStore) succeeded(start time.Time) {
	a.mu.Lock()
	if start.After(a.latest) {
		a.latest = start
	}
	a.mu.Unlock()
}

func (a *auditStore) AcquireAbove(ctx context.Context, cluster, holder string, ttl time.Duration, floor uint64) (Lease, error) {
	start := time.Now()

	l, err := a.Store.AcquireAbove(ctx, cluster, holder, ttl, floor)
	if err == nil {
		a.succeeded(start)
	}

	return l, err
}

func (a *auditStore) Renew(ctx context.Context, l Lease, ttl time.Duration) error {
	start := time.Now()

	a.mu.Lock()
	a.calls++
	n := a.calls
	a.mu.Unlock()

	if n%2 == 1 {
		// Commits now, answers late (after its context ended).
		err := a.Store.Renew(context.WithoutCancel(ctx), l, ttl)
		if err == nil {
			a.succeeded(start)
		}

		select {
		case <-ctx.Done():
		case <-time.After(a.lateDelay):
		}

		time.Sleep(a.lateDelay) //nolint:forbidigo // the late answer is the subject

		return err
	}

	err := a.Store.Renew(ctx, l, ttl)
	if err == nil {
		a.succeeded(start)
	}

	return err
}

// TestManagerDeadlineNeverPassesASuccess: whatever order the answers come
// in, the self-fence deadline is never later than the start of the newest
// successful acquire or renewal plus TTL - margin. A late answer of an older
// attempt never extends it from the time it arrived.
func TestManagerDeadlineNeverPassesASuccess(t *testing.T) {
	a := &auditStore{Store: openStore(t), lateDelay: scaled.RenewInterval}

	m, err := newManager(a, "c1", "a", scaled)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	up := make(chan struct{}, 1)

	go m.Run(ctx, func(context.Context, Lease) error { up <- struct{}{}; return nil }, func(Lease, string) {})

	<-up

	window := scaled.TTL - scaled.SafetyMargin
	end := time.Now().Add(4 * scaled.TTL)

	for time.Now().Before(end) {
		m.mu.Lock()
		deadline, active := m.deadline, m.active
		m.mu.Unlock()

		a.mu.Lock()
		bound := a.latest.Add(window)
		a.mu.Unlock()

		if active && deadline.After(bound) {
			t.Fatalf("self-fence deadline %s past the newest success + window by %s",
				deadline.Format(time.StampMilli), deadline.Sub(bound))
		}

		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // sampling the deadline
	}
}

// renewInFlightBound is how many renewals may be in flight at once: one,
// plus one retry past an attempt that outlived its own timeout.
const renewInFlightBound = 2
