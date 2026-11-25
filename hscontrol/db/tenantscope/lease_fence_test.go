package tenantscope

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

// leaseDB is openDB plus the cluster_leases table of P11b.
func leaseDB(t *testing.T) (*gorm.DB, *lease.Store) {
	t.Helper()
	db := openDB(t)

	ddl := lease.SQLiteDDL
	if db.Name() == "postgres" {
		ddl = lease.PostgresDDL
	}

	err := db.Exec(ddl).Error
	if err != nil {
		t.Fatal(err)
	}

	return db, lease.NewStore(db)
}

// leaseCtx is ctxFor(id) whose fence also claims the lease (holder, epoch).
func leaseCtx(t testing.TB, id, holder string, epoch uint64) context.Context {
	t.Helper()

	f, err := tailnet.NewFence(testCluster, testEpoch)
	if err != nil {
		t.Fatal(err)
	}

	f, err = f.WithLease(holder, epoch)
	if err != nil {
		t.Fatal(err)
	}

	ctx, err := tailnet.WithFence(scopeOnlyCtx(t, id), f)
	if err != nil {
		t.Fatal(err)
	}

	return ctx
}

func TestLeaseFenceLiveLeaseWrites(t *testing.T) {
	db, s := leaseDB(t)

	l, err := s.Acquire(context.Background(), testCluster, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	err = db.WithContext(leaseCtx(t, idA, l.Holder, l.Epoch)).Model(&item{}).
		Where("name LIKE ?", "i0-%").Update("name", "ok").Error
	if err != nil {
		t.Fatalf("write under the live lease: %v", err)
	}
}

// F02 / I12: the active process was paused past its TTL; a standby took the
// lease. Every write of the old holder rolls back, whatever its kind.
func TestLeaseFenceTakenOverRollsBack(t *testing.T) {
	db, s := leaseDB(t)
	ctx := context.Background()

	old, err := s.Acquire(ctx, testCluster, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond)

	nl, err := s.Acquire(ctx, testCluster, "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	assertAllWritesFenced(t, db, leaseCtx(t, idA, old.Holder, old.Epoch), leaseCtx(t, idA, nl.Holder, nl.Epoch))

	// The new holder writes.
	if err := db.WithContext(leaseCtx(t, idA, nl.Holder, nl.Epoch)).Model(&item{}).
		Where("name LIKE ?", "i0-%").Update("name", "new-holder").Error; err != nil {
		t.Fatalf("new holder: %v", err)
	}
}

// Clock skew (ticket P11b): the process still believes it holds the lease
// (its monotonic deadline is not reached), but by the database clock the
// lease has expired and nobody took it over yet. The database decides.
func TestLeaseFenceExpiredByDatabaseClockRollsBack(t *testing.T) {
	db, s := leaseDB(t)

	l, err := s.Acquire(context.Background(), testCluster, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond)

	assertAllWritesFenced(t, db, leaseCtx(t, idA, l.Holder, l.Epoch), ctxFor(t, idA))
}

// A wrong epoch for the right holder name (an old incarnation) and a lease
// that does not exist both roll back.
func TestLeaseFenceWrongEpochOrNoRowRollsBack(t *testing.T) {
	t.Run("wrong epoch", func(t *testing.T) {
		db, s := leaseDB(t)

		l, err := s.Acquire(context.Background(), testCluster, "a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		assertAllWritesFenced(t, db, leaseCtx(t, idA, l.Holder, l.Epoch+1), ctxFor(t, idA))
	})

	t.Run("no lease row", func(t *testing.T) {
		db, _ := leaseDB(t)
		assertAllWritesFenced(t, db, leaseCtx(t, idA, "a", 1), ctxFor(t, idA))
	})
}

// Operator writes (CLI) carry no lease and are fenced on ownership only,
// whoever holds the lease.
func TestLeaseFenceOperatorWriteNeedsNoLease(t *testing.T) {
	db, s := leaseDB(t)

	if _, err := s.Acquire(context.Background(), testCluster, "a", time.Minute); err != nil {
		t.Fatal(err)
	}

	err := db.WithContext(ctxFor(t, idA)).Model(&item{}).Where("name LIKE ?", "i0-%").
		Update("name", "operator").Error
	if err != nil {
		t.Fatalf("operator write: %v", err)
	}
}

func TestFenceWithLeaseValidation(t *testing.T) {
	f, err := tailnet.NewFence(testCluster, testEpoch)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		holder string
		epoch  uint64
	}{{"", 1}, {"a", 0}} {
		if _, err := f.WithLease(c.holder, c.epoch); !errors.Is(err, tailnet.ErrInvalidFence) {
			t.Errorf("WithLease(%q, %d): %v", c.holder, c.epoch, err)
		}
	}

	if _, _, ok := f.Lease(); ok {
		t.Error("a fence without WithLease claims a lease")
	}
}
