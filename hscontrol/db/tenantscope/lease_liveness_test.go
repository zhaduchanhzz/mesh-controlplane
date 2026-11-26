package tenantscope

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"gorm.io/gorm"
)

// P12c group B: the cluster lease stays renewable and takeable under the
// write load of its own cluster (B1), and a fenced transaction left open
// cannot hold a takeover forever (B2). PostgreSQL only: SQLite serialises
// writers, so neither starvation nor an open reader can block the lease
// there the same way.

// selfFenceDeadline is how long the active process may go without a
// successful renewal before it stops serving (lease.Manager), at the
// default timing.
var selfFenceDeadline = lease.DefaultConfig.TTL - lease.DefaultConfig.SafetyMargin

func postgresLeaseDB(t *testing.T) (*gorm.DB, *lease.Store) {
	t.Helper()

	db, s := leaseDB(t)
	if db.Name() != "postgres" {
		t.Skip("PostgreSQL only (MESH_TEST_POSTGRES_DSN)")
	}

	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(64)
		sqlDB.SetMaxIdleConns(64)
	}

	return db, s
}

// leaseWriteLoad runs n writers, each updating its own row of tailnet A in
// back-to-back fenced transactions that claim lease l, three statements per
// transaction (a registration or map update writes several rows). stop ends
// them and returns how many transactions committed.
func leaseWriteLoad(t *testing.T, db *gorm.DB, l lease.Lease, n int) func() int64 {
	t.Helper()

	ctx := leaseCtx(t, l.Holder, l.Epoch)

	var first item

	err := db.WithContext(ctx).Order("id").First(&first).Error
	if err != nil {
		t.Fatal(err)
	}

	var (
		done  atomic.Bool
		wrote atomic.Int64
		wg    sync.WaitGroup
	)

	for w := range n {
		it := item{Name: "w", OwnerID: first.OwnerID}

		err := db.WithContext(ctx).Create(&it).Error
		if err != nil {
			t.Fatal(err)
		}

		wg.Go(func() {
			for i := 0; !done.Load(); i++ {
				err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					for k := range 3 {
						err := tx.Model(&item{}).Where("id = ?", it.ID).
							Update("name", fmt.Sprintf("%d-%d-%d", w, i, k)).Error
						if err != nil {
							return err
						}
					}

					return nil
				})
				if err == nil {
					wrote.Add(1)
				}
			}
		})
	}

	return func() int64 {
		done.Store(true)
		wg.Wait()

		return wrote.Load()
	}
}

// TestLeaseRenewNotStarvedByFencedWriters (P12c B1): every fenced write of
// the cluster reads its cluster_leases row in the write's transaction.
// Before P12c that read was FOR SHARE: row share locks do not queue, so
// under a steady write load the renewal (an UPDATE of the same row) never
// found a moment without a share holder, and the active process self-fenced
// although it was healthy. The renewal must finish well inside the
// self-fence deadline while 16 writers run back to back.
func TestLeaseRenewNotStarvedByFencedWriters(t *testing.T) {
	db, s := postgresLeaseDB(t)

	l, err := s.Acquire(context.Background(), testCluster, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	loadStart := time.Now()
	stop := leaseWriteLoad(t, db, l, 16)

	time.Sleep(300 * time.Millisecond) //nolint:forbidigo // lets the writers reach steady load before timing

	const renewals = 5

	var (
		took   []time.Duration
		failed []error
	)

	for range renewals {
		ctx, cancel := context.WithTimeout(context.Background(), selfFenceDeadline)
		t0 := time.Now()
		err := s.Renew(ctx, l, time.Minute)

		cancel()

		took = append(took, time.Since(t0))

		if err != nil {
			failed = append(failed, err)
		}
	}

	wrote := stop()
	loaded := time.Since(loadStart)
	worst := slices.Max(took)

	t.Logf("%d renewals under 16 fenced writers: latencies %v, worst %s (self-fence deadline %s); %d write transactions committed (%.0f/s)",
		renewals, took, worst, selfFenceDeadline, wrote, float64(wrote)/loaded.Seconds())

	if len(failed) > 0 {
		t.Fatalf("%d/%d renewals failed within the self-fence deadline: %v", len(failed), renewals, failed[0])
	}

	if worst > time.Second {
		t.Fatalf("slowest renewal took %s under write load, want < 1s (self-fence deadline %s)", worst, selfFenceDeadline)
	}

	if wrote == 0 {
		t.Fatal("no fenced write committed")
	}
}

// TestLeaseStandbyPollDoesNotWaitForWriters (P12c B1): a standby polls
// Acquire every AcquireInterval. While the lease is live the poll must
// answer ErrHeld at once, neither waiting for the active process's open
// write transactions nor making its writers wait.
func TestLeaseStandbyPollDoesNotWaitForWriters(t *testing.T) {
	db, s := postgresLeaseDB(t)

	l, err := s.Acquire(context.Background(), testCluster, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	tx := db.WithContext(leaseCtx(t, l.Holder, l.Epoch)).Begin()
	defer tx.Rollback()

	err = tx.Model(&item{}).Where("name LIKE ?", "i0-%").Update("name", "held").Error
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	t0 := time.Now()
	_, err = s.Acquire(ctx, testCluster, "b", time.Minute)
	took := time.Since(t0)

	if !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("standby poll while a write transaction is open: err=%v after %s, want ErrHeld", err, took)
	}

	if took > 500*time.Millisecond {
		t.Fatalf("standby poll took %s, want an immediate ErrHeld", took)
	}

	// The active process still writes after the poll.
	err = db.WithContext(leaseCtx(t, l.Holder, l.Epoch)).Model(&item{}).
		Where("name LIKE ?", "i1-%").Update("name", "after-poll").Error
	if err != nil {
		t.Fatalf("write after the standby poll: %v", err)
	}
}

// TestLeaseTakeoverWaitsForInFlightFencedWrite (P12c B1, safety): a
// takeover never commits while a fenced write of the old holder is between
// its lease check and its commit, and every write of the old holder that
// starts after the takeover rolls back with ErrFenced, also one that was
// already waiting when the takeover committed.
func TestLeaseTakeoverWaitsForInFlightFencedWrite(t *testing.T) {
	db, s := postgresLeaseDB(t)

	old, err := s.Acquire(context.Background(), testCluster, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	oldCtx := leaseCtx(t, old.Holder, old.Epoch)

	// T1: in flight, its lease check passed while the lease was live.
	tx := db.WithContext(oldCtx).Begin()
	defer tx.Rollback()

	err = tx.Model(&item{}).Where("name LIKE ?", "i0-%").Update("name", "in-flight").Error
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond) //nolint:forbidigo // lets the 1 s lease expire in the database: elapsed time is the subject

	type acquired struct {
		l   lease.Lease
		err error
		at  time.Time
	}

	takeover := make(chan acquired, 1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		l, err := s.Acquire(ctx, testCluster, "b", time.Minute)
		takeover <- acquired{l, err, time.Now()}
	}()

	// T2: another write of the old holder, started while the takeover
	// waits for T1.
	time.Sleep(300 * time.Millisecond) //nolint:forbidigo // the takeover must still be waiting: a negative check

	late := make(chan error, 1)

	go func() {
		late <- db.WithContext(oldCtx).Model(&item{}).Where("name LIKE ?", "i1-%").Update("name", "late").Error
	}()

	select {
	case a := <-takeover:
		t.Fatalf("takeover returned while a fenced write of the old holder was in flight: %+v", a)
	case <-time.After(300 * time.Millisecond):
	}

	committedAt := time.Now()

	err = tx.Commit().Error
	if err != nil {
		t.Fatalf("in-flight write of the old holder: %v", err)
	}

	a := <-takeover
	if a.err != nil {
		t.Fatalf("takeover after the in-flight write committed: %v", a.err)
	}

	if a.l.Epoch != old.Epoch+1 || a.l.Holder != "b" {
		t.Fatalf("takeover lease %+v", a.l)
	}

	if a.at.Before(committedAt) {
		t.Fatalf("takeover committed at %v, before the old holder's in-flight write at %v", a.at, committedAt)
	}

	err = <-late
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("old holder's write queued behind the takeover: err=%v, want ErrFenced", err)
	}

	assertAllWritesFenced(t, db, oldCtx, leaseCtx(t, a.l.Holder, a.l.Epoch))
}
