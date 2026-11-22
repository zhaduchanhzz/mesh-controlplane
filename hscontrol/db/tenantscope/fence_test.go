package tenantscope

import (
	"context"
	"errors"
	"os"
	"sync"
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
	if err := db.WithContext(ctx).Order("id").Find(&items).Error; err != nil {
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
		if err := c.fn(); !errors.Is(err, ErrFenced) {
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

func TestFenceOtherTenantUnaffected(t *testing.T) {
	db := openDB(t)
	setOwner(t, db, idA, "c2", testEpoch+1)
	if err := db.WithContext(ctxFor(t, idB)).Model(&item{}).Where("name LIKE ?", "i0-%").
		Update("name", "b-ok").Error; err != nil {
		t.Fatalf("B's write with B's valid fence: %v", err)
	}
}

func TestFenceRequired(t *testing.T) {
	db := openDB(t)
	tx := db.WithContext(scopeOnlyCtx(t, idA))
	if err := tx.Create(&item{Name: "x"}).Error; !errors.Is(err, ErrNoFence) {
		t.Fatalf("Create without fence: err=%v, want ErrNoFence", err)
	}
	if err := tx.Model(&item{}).Where("1 = 1").Update("name", "x").Error; !errors.Is(err, ErrNoFence) {
		t.Fatalf("Update without fence: err=%v, want ErrNoFence", err)
	}
	if err := tx.Where("1 = 1").Delete(&item{}).Error; !errors.Is(err, ErrNoFence) {
		t.Fatalf("Delete without fence: err=%v, want ErrNoFence", err)
	}
	// Reads need no fence.
	var items []item
	if err := tx.Find(&items).Error; err != nil || len(items) != 2 {
		t.Fatalf("read without fence: err=%v rows=%d", err, len(items))
	}
}

func TestFenceValidWritesPass(t *testing.T) {
	db := openDB(t)
	tx := db.WithContext(ctxFor(t, idA))
	items := itemsOfCtx(t, db, ctxFor(t, idA))
	if err := tx.Create(&item{Name: "new", OwnerID: items[0].OwnerID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&item{}).Where("id = ?", items[0].ID).Update("name", "upd").Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Delete(&item{}, items[1].ID).Error; err != nil {
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
	if err := tx.Model(&item{}).Where("name LIKE ?", "i0-%").Update("name", "held").Error; err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		movedAt time.Time
		moveErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		moveErr = db.Model(&tailnetRow{}).Where("id = ?", idA).Update("owner_epoch", testEpoch+1).Error
		movedAt = time.Now()
	}()

	time.Sleep(500 * time.Millisecond)
	committedAt := time.Now()
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if moveErr != nil {
		t.Fatal(moveErr)
	}
	if movedAt.Before(committedAt) {
		t.Fatalf("ownership moved at %v, before the fenced writer committed at %v", movedAt, committedAt)
	}
	if err := db.WithContext(ctxFor(t, idA)).Model(&item{}).Where("1 = 1").Update("name", "late").Error; !errors.Is(err, ErrFenced) {
		t.Fatalf("write after move: err=%v, want ErrFenced", err)
	}
}
