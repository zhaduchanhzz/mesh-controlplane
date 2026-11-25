package lease

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openStore gives each test its own cluster_leases table: SQLite by default,
// PostgreSQL in a schema of its own when MESH_TEST_POSTGRES_DSN is set.
func openStore(t *testing.T) *Store {
	t.Helper()

	var (
		dial gorm.Dialector
		ddl  = SQLiteDDL
	)

	dsn := os.Getenv("MESH_TEST_POSTGRES_DSN")
	if dsn == "" {
		dial = sqlite.Open(filepath.Join(t.TempDir(), "lease.db") + "?_pragma=busy_timeout(5000)")
	} else {
		admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}

		sch := fmt.Sprintf("lease_%d", time.Now().UnixNano())
		if err := admin.Exec("CREATE SCHEMA " + sch).Error; err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			_ = admin.Exec("DROP SCHEMA " + sch + " CASCADE").Error
			if d, err := admin.DB(); err == nil {
				_ = d.Close()
			}
		})

		dial = postgres.Open(dsn + " search_path=" + sch)
		ddl = PostgresDDL
	}

	db, err := gorm.Open(dial, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if d, err := db.DB(); err == nil {
			_ = d.Close()
		}
	})

	if err := db.Exec(ddl).Error; err != nil {
		t.Fatalf("ddl: %v", err)
	}

	return NewStore(db)
}

func TestAcquireFirstLeaseIsEpochOne(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	l, err := s.Acquire(ctx, "c1", "a", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if l.Epoch != 1 || l.Holder != "a" || l.Cluster != "c1" {
		t.Fatalf("got %+v", l)
	}

	// Another cluster has its own lease.
	l2, err := s.Acquire(ctx, "c2", "b", 5*time.Second)
	if err != nil || l2.Epoch != 1 {
		t.Fatalf("c2: %+v %v", l2, err)
	}
}

// F04 (part): a live lease is never taken, not even by the same holder name.
func TestLiveLeaseIsNotTaken(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if _, err := s.Acquire(ctx, "c1", "a", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	for _, h := range []string{"b", "a"} {
		_, err := s.Acquire(ctx, "c1", h, 5*time.Second)
		if !errors.Is(err, ErrHeld) {
			t.Fatalf("holder %s: want ErrHeld, got %v", h, err)
		}
	}

	row, ok, err := s.Get(ctx, "c1")
	if err != nil || !ok || row.Holder != "a" || row.Epoch != 1 || row.Expired {
		t.Fatalf("row %+v ok=%v err=%v", row, ok, err)
	}
}

// F01/F02 (database part): after expiry a standby takes over at epoch+1; the
// old holder can neither renew nor release the new lease.
func TestTakeoverAfterExpiryAndOldHolderIsLost(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	old, err := s.Acquire(ctx, "c1", "a", 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond)

	nl, err := s.Acquire(ctx, "c1", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if nl.Epoch != old.Epoch+1 || nl.Holder != "b" {
		t.Fatalf("takeover: %+v", nl)
	}

	if err := s.Renew(ctx, old, 5*time.Second); !errors.Is(err, ErrLost) {
		t.Fatalf("old renew: want ErrLost, got %v", err)
	}

	if err := s.Release(ctx, old); err != nil {
		t.Fatal(err)
	}

	row, _, _ := s.Get(ctx, "c1")
	if row.Holder != "b" || row.Epoch != nl.Epoch || row.Expired {
		t.Fatalf("old release touched the new lease: %+v", row)
	}

	if err := s.Renew(ctx, nl, 5*time.Second); err != nil {
		t.Fatalf("new renew: %v", err)
	}
}

func TestRenewAfterOwnExpiryIsLost(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	l, err := s.Acquire(ctx, "c1", "a", 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond)

	if err := s.Renew(ctx, l, 5*time.Second); !errors.Is(err, ErrLost) {
		t.Fatalf("want ErrLost, got %v", err)
	}
}

func TestReleaseLetsStandbyTakeOverAtOnce(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	l, err := s.Acquire(ctx, "c1", "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Release(ctx, l); err != nil {
		t.Fatal(err)
	}

	nl, err := s.Acquire(ctx, "c1", "b", time.Minute)
	if err != nil || nl.Epoch != 2 {
		t.Fatalf("%+v %v", nl, err)
	}
}

// The trigger holds even for writes that bypass Store (I12, I14).
func TestEpochTriggerRefusesDecreaseAndSilentHolderChange(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if _, err := s.Acquire(ctx, "c1", "a", time.Minute); err != nil {
		t.Fatal(err)
	}

	for name, q := range map[string]string{
		"decrease":          `UPDATE cluster_leases SET epoch = 0 WHERE cluster_id = 'c1'`,
		"holder same epoch": `UPDATE cluster_leases SET holder = 'z' WHERE cluster_id = 'c1'`,
	} {
		err := s.db.WithContext(ctx).Exec(q).Error
		if err == nil {
			t.Errorf("%s: update accepted", name)
		}
	}
}

// F04: many processes race for an expired lease; exactly one wins.
func TestConcurrentAcquireExactlyOneWins(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if _, err := s.Acquire(ctx, "c1", "seed", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	time.Sleep(700 * time.Millisecond)

	var (
		wg   sync.WaitGroup
		wins atomic.Int32
		errs = make(chan error, 8)
	)

	for i := range 8 {
		wg.Go(func() {
			_, err := s.Acquire(ctx, "c1", fmt.Sprintf("h%d", i), time.Minute)

			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrHeld):
			default:
				errs <- err
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		// SQLite may report a busy database under contention: that is a lost
		// race, not a second winner.
		t.Logf("acquire error (counted as lost): %v", err)
	}

	if wins.Load() != 1 {
		t.Fatalf("%d winners, want exactly 1", wins.Load())
	}

	row, _, _ := s.Get(ctx, "c1")
	if row.Epoch != 2 {
		t.Fatalf("epoch %d, want 2", row.Epoch)
	}
}

func TestConfigValidate(t *testing.T) {
	err := DefaultConfig.Validate()
	if err != nil {
		t.Fatal(err)
	}

	bad := []Config{
		{TTL: 500 * time.Millisecond, RenewInterval: 100 * time.Millisecond, AcquireInterval: time.Second, SafetyMargin: 100 * time.Millisecond},
		{TTL: 10 * time.Second, RenewInterval: 9 * time.Second, AcquireInterval: time.Second, SafetyMargin: 2 * time.Second},
		{TTL: 10 * time.Second, RenewInterval: 3 * time.Second, AcquireInterval: time.Second, SafetyMargin: 6 * time.Second},
		{TTL: 10 * time.Second, RenewInterval: 3 * time.Second, AcquireInterval: 0, SafetyMargin: 2 * time.Second},
	}
	for i, c := range bad {
		if c.Validate() == nil {
			t.Errorf("config %d accepted: %+v", i, c)
		}
	}
}

// ---------------------------------------------------------------- manager --

var fast = Config{TTL: 1200 * time.Millisecond, RenewInterval: 300 * time.Millisecond, AcquireInterval: 100 * time.Millisecond, SafetyMargin: 300 * time.Millisecond}

// stuckStore acquires, then hangs on every renew: the database is
// unreachable (F03).
type stuckStore struct{ *Store }

func (stuckStore) Renew(ctx context.Context, _ Lease, _ time.Duration) error {
	<-ctx.Done()

	return ctx.Err()
}

// F03: without reaching the database the active process stops serving at
// its monotonic deadline, before the lease can expire in the database.
func TestManagerSelfFencesWhenRenewHangs(t *testing.T) {
	s := openStore(t)

	m, err := newManager(stuckStore{s}, "c1", "a", fast)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	acquired := make(chan time.Time, 1)
	lost := make(chan string, 1)

	go m.Run(ctx, func(context.Context, Lease) error { acquired <- time.Now(); return nil },
		func(_ Lease, reason string) { lost <- reason })

	var t0 time.Time
	select {
	case t0 = <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("never acquired")
	}

	select {
	case reason := <-lost:
		elapsed := time.Since(t0)
		if elapsed > fast.TTL-fast.SafetyMargin+200*time.Millisecond {
			t.Fatalf("self-fence after %s, want about %s", elapsed, fast.TTL-fast.SafetyMargin)
		}

		if _, ok := m.Current(); ok {
			t.Fatal("Current still reports the lease after onLose")
		}

		t.Logf("self-fenced after %s: %s", elapsed, reason)
	case <-time.After(5 * time.Second):
		t.Fatal("never self-fenced")
	}

	// The row is still live in the database at that moment: the process
	// stopped first.
	row, _, _ := s.Get(context.Background(), "c1")
	if row.Expired {
		t.Fatal("lease already expired in the database when the process stopped; margin not honoured")
	}
}

// F01/F02 (in process): the active manager is stopped without releasing
// (crash); a standby takes over within TTL + AcquireInterval at epoch 2.
func TestManagerStandbyTakesOverAfterCrash(t *testing.T) {
	s := openStore(t)

	a, _ := newManager(s, "c1", "a", fast)
	b, _ := newManager(s, "c1", "b", fast)

	actx, acancel := context.WithCancel(context.Background())
	aUp := make(chan struct{}, 1)
	aDone := make(chan struct{})

	go func() {
		a.Run(actx, func(context.Context, Lease) error { aUp <- struct{}{}; return nil }, func(Lease, string) {})
		close(aDone)
	}()

	<-aUp

	crash := time.Now()

	acancel()
	<-aDone

	// Undo a's graceful release, as a crash would not have done it: put the
	// expiry back to the future.
	err := s.db.Exec(`UPDATE cluster_leases SET expires_at = ` + s.nowPlus(fast.TTL) + ` WHERE cluster_id = 'c1'`).Error
	if err != nil {
		t.Fatal(err)
	}

	bctx := t.Context()

	bUp := make(chan Lease, 1)

	go b.Run(bctx, func(_ context.Context, l Lease) error { bUp <- l; return nil }, func(Lease, string) {})

	select {
	case l := <-bUp:
		if l.Epoch != 2 || l.Holder != "b" {
			t.Fatalf("standby lease %+v", l)
		}

		if d := time.Since(crash); d > fast.TTL+3*fast.AcquireInterval+time.Second {
			t.Fatalf("takeover after %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("standby never took over")
	}

	if _, ok := a.Current(); ok {
		t.Fatal("crashed manager still reports a lease")
	}
}

// Graceful shutdown releases: the standby takes over without waiting a TTL.
func TestManagerReleaseOnShutdown(t *testing.T) {
	s := openStore(t)
	slow := Config{TTL: 30 * time.Second, RenewInterval: 5 * time.Second, AcquireInterval: 100 * time.Millisecond, SafetyMargin: 5 * time.Second}

	a, _ := newManager(s, "c1", "a", slow)
	b, _ := newManager(s, "c1", "b", slow)

	actx, acancel := context.WithCancel(context.Background())
	aUp := make(chan struct{}, 1)
	aDone := make(chan struct{})

	go func() {
		a.Run(actx, func(context.Context, Lease) error { aUp <- struct{}{}; return nil }, func(Lease, string) {})
		close(aDone)
	}()

	<-aUp

	bctx := t.Context()

	bUp := make(chan struct{}, 1)

	go b.Run(bctx, func(context.Context, Lease) error { bUp <- struct{}{}; return nil }, func(Lease, string) {})

	acancel()
	<-aDone

	t0 := time.Now()

	select {
	case <-bUp:
		if d := time.Since(t0); d > 3*time.Second {
			t.Fatalf("takeover after release took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no takeover after release")
	}
}

// A start failure releases the lease and leaves the process standby.
func TestManagerStartFailureReleases(t *testing.T) {
	s := openStore(t)
	a, _ := newManager(s, "c1", "a", fast)

	ctx := t.Context()

	var tries atomic.Int32

	lost := make(chan string, 4)

	go a.Run(ctx, func(context.Context, Lease) error {
		tries.Add(1)

		return errors.New("boom")
	}, func(_ Lease, r string) { lost <- r })

	select {
	case r := <-lost:
		if r != "start failed" {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no onLose after start failure")
	}

	if _, ok := a.Current(); ok {
		t.Fatal("still active after start failure")
	}
}
