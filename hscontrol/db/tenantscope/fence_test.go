package tenantscope

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func setOwner(t *testing.T, db *gorm.DB, id, cluster string, epoch int64) {
	t.Helper()

	err := db.Model(&tailnetRow{}).Where("id = ?", id).
		Updates(map[string]any{"owner_cluster_id": cluster, "owner_epoch": epoch}).Error
	if err != nil {
		t.Fatal(err)
	}
}

func itemsOfCtx(t *testing.T, db *gorm.DB, ctx context.Context) []item {
	t.Helper()

	var items []item

	err := db.WithContext(ctx).Order("id").Find(&items).Error
	if err != nil {
		t.Fatal(err)
	}

	return items
}

// assertAllWritesFenced runs every mutation kind under stale and requires
// ErrFenced with the tenant's rows unchanged (I12, S17). Rows are read under
// fresh: reads are not fenced.
func assertAllWritesFenced(t *testing.T, db *gorm.DB, stale, fresh context.Context) {
	t.Helper()
	before := itemsOfCtx(t, db, fresh)
	tx := db.WithContext(stale)

	checks := []struct {
		name string
		fn   func() error
	}{
		{"Create", func() error { return tx.Create(&item{Name: "new", OwnerID: before[0].OwnerID}).Error }},
		{"Update", func() error {
			return tx.Model(&item{}).Where("id = ?", before[0].ID).Update("name", "changed").Error
		}},
		{"Save", func() error {
			upd := before[1]
			upd.Name = "saved"

			return tx.Save(&upd).Error
		}},
		{"Delete", func() error { return tx.Delete(&item{}, before[0].ID).Error }},
		{"Transaction", func() error {
			return tx.Transaction(func(inner *gorm.DB) error {
				return inner.Model(&item{}).Where("id = ?", before[1].ID).Update("name", "in-tx").Error
			})
		}},
	}
	for _, c := range checks {
		err := c.fn()
		if !errors.Is(err, ErrFenced) {
			t.Errorf("%s: err=%v, want ErrFenced", c.name, err)
		}
	}

	after := itemsOfCtx(t, db, fresh)
	if len(after) != len(before) {
		t.Fatalf("rows changed: %d -> %d", len(before), len(after))
	}

	for i := range before {
		if before[i].Name != after[i].Name {
			t.Fatalf("row %d changed: %q -> %q", before[i].ID, before[i].Name, after[i].Name)
		}
	}
}

func TestFenceStaleEpochRollsBack(t *testing.T) {
	db := openDB(t)
	setOwner(t, db, idA, testCluster, testEpoch+1)
	assertAllWritesFenced(t, db, ctxFor(t, idA), fencedCtx(t, idA, testCluster, testEpoch+1))
}

func TestFenceForeignClusterRollsBack(t *testing.T) {
	db := openDB(t)
	setOwner(t, db, idA, "c2", testEpoch)
	assertAllWritesFenced(t, db, ctxFor(t, idA), fencedCtx(t, idA, "c2", testEpoch))
}

// A1 (review P10): owner and epoch still match, but the tailnet left ACTIVE
// (suspended, being deleted, back to pending) or is soft-deleted. Status
// changes do not bump the epoch, so the fence itself must refuse: a runtime
// that has not been retired yet, or a request still in flight after retire,
// must not write.
func TestFenceInactiveTailnetRollsBack(t *testing.T) {
	for _, st := range []string{"SUSPENDED", "DELETING", "PENDING"} {
		t.Run(st, func(t *testing.T) {
			db := openDB(t)

			err := db.Model(&tailnetRow{}).Where("id = ?", idA).Update("status", st).Error
			if err != nil {
				t.Fatal(err)
			}

			assertAllWritesFenced(t, db, ctxFor(t, idA), ctxFor(t, idA))

			// B (ACTIVE) is unaffected.
			err = db.WithContext(ctxFor(t, idB)).Model(&item{}).Where("name LIKE ?", "i0-%").
				Update("name", "b-ok").Error
			if err != nil {
				t.Fatalf("B's write: %v", err)
			}
		})
	}

	t.Run("soft-deleted", func(t *testing.T) {
		db := openDB(t)

		now := time.Now().UTC()

		err := db.Model(&tailnetRow{}).Where("id = ?", idA).Update("deleted_at", &now).Error
		if err != nil {
			t.Fatal(err)
		}

		assertAllWritesFenced(t, db, ctxFor(t, idA), ctxFor(t, idA))
	})
}

func TestFenceOtherTenantUnaffected(t *testing.T) {
	db := openDB(t)
	setOwner(t, db, idA, "c2", testEpoch+1)

	err := db.WithContext(ctxFor(t, idB)).Model(&item{}).Where("name LIKE ?", "i0-%").
		Update("name", "b-ok").Error
	if err != nil {
		t.Fatalf("B's write with B's valid fence: %v", err)
	}
}

func TestFenceRequired(t *testing.T) {
	db := openDB(t)

	tx := db.WithContext(scopeOnlyCtx(t, idA))

	err := tx.Create(&item{Name: "x"}).Error
	if !errors.Is(err, ErrNoFence) {
		t.Fatalf("Create without fence: err=%v, want ErrNoFence", err)
	}

	err = tx.Model(&item{}).Where("1 = 1").Update("name", "x").Error
	if !errors.Is(err, ErrNoFence) {
		t.Fatalf("Update without fence: err=%v, want ErrNoFence", err)
	}

	err = tx.Where("1 = 1").Delete(&item{}).Error
	if !errors.Is(err, ErrNoFence) {
		t.Fatalf("Delete without fence: err=%v, want ErrNoFence", err)
	}
	// Reads need no fence.
	var items []item

	err = tx.Find(&items).Error
	if err != nil || len(items) != 2 {
		t.Fatalf("read without fence: err=%v rows=%d", err, len(items))
	}
}

func TestFenceValidWritesPass(t *testing.T) {
	db := openDB(t)
	tx := db.WithContext(ctxFor(t, idA))

	items := itemsOfCtx(t, db, ctxFor(t, idA))

	err := tx.Create(&item{Name: "new", OwnerID: items[0].OwnerID}).Error
	if err != nil {
		t.Fatal(err)
	}

	err = tx.Model(&item{}).Where("id = ?", items[0].ID).Update("name", "upd").Error
	if err != nil {
		t.Fatal(err)
	}

	err = tx.Delete(&item{}, items[1].ID).Error
	if err != nil {
		t.Fatal(err)
	}
}

// On PostgreSQL the fence reads the tailnets row FOR SHARE: an ownership move
// (UPDATE tailnets) waits for a writer's transaction, so a write never
// commits under an owner that changed after its check.
func TestFenceBlocksConcurrentMove(t *testing.T) {
	if os.Getenv("MESH_TEST_POSTGRES_DSN") == "" {
		t.Skip("PostgreSQL only (SQLite serialises writers)")
	}

	db := openDB(t)

	tx := db.WithContext(ctxFor(t, idA)).Begin()
	defer tx.Rollback()

	err := tx.Model(&item{}).Where("name LIKE ?", "i0-%").Update("name", "held").Error
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		movedAt time.Time
		moveErr error
	)

	wg.Go(func() {
		moveErr = db.Model(&tailnetRow{}).Where("id = ?", idA).Update("owner_epoch", testEpoch+1).Error
		movedAt = time.Now()
	})

	// The move must stay blocked behind the open transaction; holding it
	// open for a while is the test.
	time.Sleep(500 * time.Millisecond) //nolint:forbidigo // holds the transaction open: elapsed time is the subject

	committedAt := time.Now()

	err = tx.Commit().Error
	if err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	if moveErr != nil {
		t.Fatal(moveErr)
	}

	if movedAt.Before(committedAt) {
		t.Fatalf("ownership moved at %v, before the fenced writer committed at %v", movedAt, committedAt)
	}

	err = db.WithContext(ctxFor(t, idA)).Model(&item{}).Where("1 = 1").Update("name", "late").Error
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("write after move: err=%v, want ErrFenced", err)
	}
}

// TestOwnershipChangeNotStarvedByWriters (P10c, review B3): on PostgreSQL an
// ownership change made with LockOwnership completes promptly while writers
// of the tailnet run back to back. Before P10c the UPDATE waited for a
// moment with no FOR SHARE holder, which a steady write load never gives
// (26-136 s per move with 8 writers). Writers keep committing afterwards.
func TestOwnershipChangeNotStarvedByWriters(t *testing.T) {
	db := openDB(t)
	if db.Name() != "postgres" {
		t.Skip("PostgreSQL only (MESH_TEST_POSTGRES_DSN)")
	}

	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(64)
	}

	var it item

	err = db.WithContext(ctxFor(t, idA)).Order("id").First(&it).Error
	if err != nil {
		t.Fatal(err)
	}

	var epoch atomic.Uint64
	epoch.Store(testEpoch)
	stop := writeLoad(t, db, it.OwnerID, 16, &epoch)

	time.Sleep(300 * time.Millisecond) //nolint:forbidigo // lets the writers reach steady load before the moves

	var worst time.Duration

	for range 20 {
		next := epoch.Load() + 1
		start := time.Now()

		err := moveEpoch(db, next)
		if err != nil {
			stop()
			t.Fatalf("ownership change under load: %v", err)
		}

		worst = max(worst, time.Since(start))

		epoch.Store(next)
	}

	before := stop()

	if worst > 2*time.Second {
		t.Fatalf("slowest ownership change took %s under write load, want < 2s", worst)
	}

	if before == 0 {
		t.Fatal("no write committed under the fence")
	}

	t.Logf("20 changes, slowest %s; %d writes committed", worst, before)
}

// TestOwnershipChangeBoundedByLockTimeout: a write transaction that never
// commits cannot hang an ownership change; it fails after
// OwnershipLockTimeout and changes nothing.
func TestOwnershipChangeBoundedByLockTimeout(t *testing.T) {
	db := openDB(t)
	if db.Name() != "postgres" {
		t.Skip("PostgreSQL only (MESH_TEST_POSTGRES_DSN)")
	}

	saved := OwnershipLockTimeout
	OwnershipLockTimeout = 300 * time.Millisecond

	t.Cleanup(func() { OwnershipLockTimeout = saved })

	held := make(chan struct{})
	release := make(chan struct{})

	writer := make(chan error, 1)
	go func() {
		writer <- db.WithContext(ctxFor(t, idA)).Transaction(func(tx *gorm.DB) error {
			err := tx.Model(&item{}).Where("name LIKE ?", "i0-%").Update("name", "held").Error
			if err != nil {
				close(held)

				return err
			}

			close(held)
			<-release

			return nil
		})
	}()

	<-held

	start := time.Now()
	err := moveEpoch(db, testEpoch+1)
	took := time.Since(start)

	close(release)

	werr := <-writer
	if werr != nil {
		t.Fatalf("held writer: %v", werr)
	}

	if err == nil {
		t.Fatal("ownership change succeeded while a writer held the tailnet")
	}

	if took > 3*time.Second {
		t.Fatalf("ownership change waited %s, want about OwnershipLockTimeout", took)
	}

	var row tailnetRow

	err = db.First(&row, "id = ?", idA).Error
	if err != nil {
		t.Fatal(err)
	}

	if row.OwnerEpoch != testEpoch {
		t.Fatalf("epoch moved to %d despite the failed change", row.OwnerEpoch)
	}
}
