package lease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// P13c (ADR 0012 §3): a process never accepts a lease epoch at or below one
// it has seen; after a restore it stays standby until the epoch fence.

// rewriteLease sets the c1 row as a restore would: the epoch may go down,
// the monotonic trigger does not run (PostgreSQL: replication role, the
// test server's user is a superuser; SQLite: the trigger is dropped and
// created again).
func rewriteLease(t *testing.T, s *Store, holder string, epoch uint64, expired bool) {
	t.Helper()

	exp := s.nowPlus(time.Hour)
	if expired {
		exp = s.now()
	}

	q := `UPDATE cluster_leases SET holder = ?, epoch = ?, expires_at = ` + exp + ` WHERE cluster_id = 'c1'`

	if s.postgres {
		err := s.db.Transaction(func(tx *gorm.DB) error {
			err := tx.Exec(`SET LOCAL session_replication_role = replica`).Error
			if err != nil {
				return err
			}

			return tx.Exec(q, holder, int64(epoch)).Error //nolint:gosec // test epochs < 2^63
		})
		if err != nil {
			t.Fatal(err)
		}

		return
	}

	trigger := SQLiteDDL[strings.Index(SQLiteDDL, "CREATE TRIGGER"):]

	if err := s.db.Exec(`DROP TRIGGER trg_cluster_leases_epoch_monotonic`).Error; err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}

	if err := s.db.Exec(q, holder, int64(epoch)).Error; err != nil { //nolint:noinlineerr,gosec // test epochs < 2^63
		t.Fatal(err)
	}

	if err := s.db.Exec(trigger).Error; err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}
}

// The store never grants an epoch at or below the floor: the attempt
// changes nothing; a live lease reports its holder and epoch.
func TestAcquireAboveRefusesRegression(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if _, err := s.Acquire(ctx, "c1", "a", time.Hour); err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}

	_, err := s.AcquireAbove(ctx, "c1", "b", time.Hour, 0)

	var held *HeldError
	if !errors.As(err, &held) || !errors.Is(err, ErrHeld) || held.Epoch != 1 || held.Holder != "a" {
		t.Fatalf("live lease: got %v, want HeldError{a, 1}", err)
	}

	rewriteLease(t, s, "a", 3, true)

	_, err = s.AcquireAbove(ctx, "c1", "b", time.Hour, 5)
	if !errors.Is(err, ErrEpochRegression) {
		t.Fatalf("epoch 4 with floor 5: got %v, want ErrEpochRegression", err)
	}

	row, _, _ := s.Get(ctx, "c1")
	if row.Holder != "a" || row.Epoch != 3 {
		t.Fatalf("refused acquire changed the row: %+v", row)
	}

	_, err = s.AcquireAbove(ctx, "c2", "b", time.Hour, 1)
	if !errors.Is(err, ErrEpochRegression) {
		t.Fatalf("no row (epoch 1) with floor 1: got %v, want ErrEpochRegression", err)
	}

	if _, ok, _ := s.Get(ctx, "c2"); ok { //nolint:noinlineerr
		t.Fatal("refused acquire created a row")
	}

	l, err := s.AcquireAbove(ctx, "c1", "b", time.Hour, 3)
	if err != nil || l.Epoch != 4 {
		t.Fatalf("floor 3: got %+v %v, want epoch 4", l, err)
	}
}

// F08 in process: a manager that held epoch 2 sees the database back at
// epoch 1 (restore). It must not take the lease (that would grant epoch 2 a
// second time), counts the regression, and takes the lease once a fence put
// the epoch above what it saw.
func TestManagerRefusesEpochRegression(t *testing.T) {
	s := openStore(t)
	m, err := newManager(s, "c1", "b", fast)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan Lease, 4)
	run := func(ctx context.Context) {
		m.Run(ctx, func(_ context.Context, l Lease) error { got <- l; return nil }, func(Lease, string) {})
	}

	// Epoch 1 by another holder, expired; b takes epoch 2.
	if _, err := s.Acquire(context.Background(), "c1", "a", time.Hour); err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}

	rewriteLease(t, s, "a", 1, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { run(ctx); close(done) }()

	select {
	case l := <-got:
		if l.Epoch != 2 {
			t.Fatalf("first lease %+v, want epoch 2", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never acquired")
	}

	cancel()
	<-done

	if hwm := m.HighWaterMark(); hwm != 2 {
		t.Fatalf("high-water mark %d after holding epoch 2", hwm)
	}

	// Restore: the database is back at epoch 1, expired.
	rewriteLease(t, s, "a", 1, true)

	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})

	go func() { run(ctx); close(done) }()

	select {
	case l := <-got:
		cancel()
		<-done
		t.Fatalf("took lease %+v after the restore: epoch %d granted twice", l, l.Epoch)
	case <-time.After(4 * fast.AcquireInterval):
	}

	if _, ok := m.Current(); ok {
		t.Fatal("Current reports a lease after the restore")
	}

	if m.Regressions() == 0 {
		t.Fatal("regression not counted")
	}

	row, _, _ := s.Get(context.Background(), "c1")
	if row.Epoch != 1 || row.Holder != "a" {
		t.Fatalf("refused attempts changed the row: %+v", row)
	}

	// The fence: epoch far above, lease ended.
	const fenced = uint64(1) << 40

	rewriteLease(t, s, "fence:1", fenced, true)

	select {
	case l := <-got:
		if l.Epoch != fenced+1 {
			t.Fatalf("lease after fence %+v, want epoch %d", l, fenced+1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never acquired after the fence")
	}

	cancel()
	<-done
}

// A standby's high-water mark follows the epoch of the live lease it sees.
func TestManagerStandbySeesHeldEpoch(t *testing.T) {
	s := openStore(t)

	if _, err := s.Acquire(context.Background(), "c1", "a", time.Hour); err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}

	rewriteLease(t, s, "a", 9, false)

	m, err := newManager(s, "c1", "b", fast)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.Run(ctx, func(context.Context, Lease) error { return nil }, func(Lease, string) {})
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for m.HighWaterMark() != 9 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // polling the manager
	}

	cancel()
	<-done

	if hwm := m.HighWaterMark(); hwm != 9 {
		t.Fatalf("standby high-water mark %d, want the live epoch 9", hwm)
	}
}
