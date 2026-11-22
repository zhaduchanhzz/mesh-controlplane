package tenantscope

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

const (
	idA = "0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5f"
	idB = "7e9d1a3b-5c2f-4e8a-b6d4-1f0c3a5b7d9e"
)

type owner struct {
	ID        uint64 `gorm:"primaryKey"`
	TailnetID string `gorm:"column:tailnet_id;not null"`
	Name      string
}

type item struct {
	ID        uint64 `gorm:"primaryKey"`
	TailnetID string `gorm:"column:tailnet_id;not null"`
	OwnerID   uint64
	Owner     *owner
	Name      string
}

// global has no tailnet column; the plugin must leave it alone.
type global struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func ctxFor(t *testing.T, id string) context.Context {
	t.Helper()
	s, err := tailnet.NewScope(tailnet.MustParseID(id), tailnet.SourceInternalService, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := tailnet.WithScope(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}

	return ctx
}

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// Close before TempDir removal (cleanups run LIFO); Windows locks open files.
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&owner{}, &item{}, &global{}); err != nil {
		t.Fatal(err)
	}
	// Seed both tenants directly, bypassing the plugin (registered after).
	for _, tn := range []string{idA, idB} {
		o := owner{TailnetID: tn, Name: "o-" + tn[:4]}
		if err := db.Create(&o).Error; err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			if err := db.Create(&item{TailnetID: tn, OwnerID: o.ID, Name: fmt.Sprintf("i%d-%s", i, tn[:4])}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Create(&global{Name: "g"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Register(db); err != nil {
		t.Fatal(err)
	}

	return db
}

func TestNoScopeFailsClosed(t *testing.T) {
	db := openDB(t)
	bg := db.WithContext(context.Background())

	var items []item
	if err := bg.Find(&items).Error; !errors.Is(err, tailnet.ErrNoScope) || len(items) != 0 {
		t.Fatalf("Find without scope: err=%v rows=%d, want ErrNoScope and 0 rows", err, len(items))
	}
	var n int64
	if err := bg.Model(&item{}).Count(&n).Error; !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Count without scope: err=%v", err)
	}
	if err := bg.Model(&item{}).Where("1 = 1").Update("name", "pwned").Error; !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Update without scope: err=%v", err)
	}
	if err := bg.Where("1 = 1").Delete(&item{}).Error; !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Delete without scope: err=%v", err)
	}
	if err := bg.Create(&item{Name: "x"}).Error; !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Create without scope: err=%v", err)
	}

	// Nothing changed.
	var all []item
	if err := db.WithContext(ctxFor(t, idA)).Find(&all).Error; err != nil || len(all) != 2 {
		t.Fatalf("tenant A after unscoped attempts: %d rows, %v", len(all), err)
	}
	for _, it := range all {
		if it.Name == "pwned" {
			t.Fatal("unscoped update modified data")
		}
	}
}

func TestScopeLimitsReads(t *testing.T) {
	db := openDB(t)
	for _, tn := range []string{idA, idB} {
		var items []item
		if err := db.WithContext(ctxFor(t, tn)).Find(&items).Error; err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Fatalf("tenant %s sees %d items, want 2", tn[:4], len(items))
		}
		for _, it := range items {
			if it.TailnetID != tn {
				t.Fatalf("tenant %s read row of %s", tn[:4], it.TailnetID[:4])
			}
		}
	}
	// A primary-key lookup of B's row from A finds nothing.
	var b item
	if err := db.WithContext(ctxFor(t, idB)).First(&b).Error; err != nil {
		t.Fatal(err)
	}
	var got item
	err := db.WithContext(ctxFor(t, idA)).First(&got, b.ID).Error
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("A reading B's row by id: err=%v got=%+v", err, got)
	}
}

func TestScopeLimitsWrites(t *testing.T) {
	db := openDB(t)
	ctxA := ctxFor(t, idA)

	res := db.WithContext(ctxA).Model(&item{}).Where("1 = 1").Update("name", "renamed")
	if res.Error != nil || res.RowsAffected != 2 {
		t.Fatalf("A bulk update: %v rows=%d, want 2", res.Error, res.RowsAffected)
	}
	var bItems []item
	if err := db.WithContext(ctxFor(t, idB)).Find(&bItems).Error; err != nil {
		t.Fatal(err)
	}
	for _, it := range bItems {
		if it.Name == "renamed" {
			t.Fatal("A's bulk update changed B's rows")
		}
	}

	res = db.WithContext(ctxA).Where("1 = 1").Delete(&item{})
	if res.Error != nil || res.RowsAffected != 2 {
		t.Fatalf("A bulk delete: %v rows=%d", res.Error, res.RowsAffected)
	}
	if err := db.WithContext(ctxFor(t, idB)).Find(&bItems).Error; err != nil || len(bItems) != 2 {
		t.Fatalf("B after A's delete: %d rows, %v", len(bItems), err)
	}
}

func TestCreateStampsAndRejectsForeignTenant(t *testing.T) {
	db := openDB(t)
	ctxA := ctxFor(t, idA)

	it := item{Name: "new"}
	if err := db.WithContext(ctxA).Create(&it).Error; err != nil {
		t.Fatal(err)
	}
	if it.TailnetID != idA {
		t.Fatalf("created row tailnet = %q, want A", it.TailnetID)
	}

	batch := []item{{Name: "b1"}, {Name: "b2"}}
	if err := db.WithContext(ctxA).Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	for _, b := range batch {
		if b.TailnetID != idA {
			t.Fatalf("batch row tailnet = %q, want A", b.TailnetID)
		}
	}

	evil := item{TailnetID: idB, Name: "smuggled"}
	if err := db.WithContext(ctxA).Create(&evil).Error; !errors.Is(err, ErrForeignTenant) {
		t.Fatalf("create with B's tailnet under A: err=%v, want ErrForeignTenant", err)
	}
}

func TestSaveCannotOverwriteOtherTenant(t *testing.T) {
	db := openDB(t)
	var b item
	if err := db.WithContext(ctxFor(t, idB)).First(&b).Error; err != nil {
		t.Fatal(err)
	}
	// Tenant A tries to Save a row carrying B's primary key.
	hijack := item{ID: b.ID, Name: "hijacked"}
	_ = db.WithContext(ctxFor(t, idA)).Save(&hijack).Error

	var after item
	if err := db.WithContext(ctxFor(t, idB)).First(&after, b.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Name != b.Name || after.TailnetID != idB {
		t.Fatalf("B's row changed via A's Save: %+v", after)
	}
}

func TestPreloadIsScoped(t *testing.T) {
	db := openDB(t)
	var items []item
	if err := db.WithContext(ctxFor(t, idA)).Preload("Owner").Find(&items).Error; err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Owner == nil || it.Owner.TailnetID != idA {
			t.Fatalf("preloaded owner %+v for A's item", it.Owner)
		}
	}
}

func TestTablesWithoutTailnetColumnUnaffected(t *testing.T) {
	db := openDB(t)
	var gs []global
	if err := db.WithContext(context.Background()).Find(&gs).Error; err != nil || len(gs) != 1 {
		t.Fatalf("global table without scope: %d rows, %v", len(gs), err)
	}
}

func TestTransactionKeepsScope(t *testing.T) {
	db := openDB(t)
	err := db.WithContext(ctxFor(t, idA)).Transaction(func(tx *gorm.DB) error {
		var items []item
		if err := tx.Find(&items).Error; err != nil {
			return err
		}
		if len(items) != 2 {
			return fmt.Errorf("tx sees %d items", len(items))
		}
		for _, it := range items {
			if it.TailnetID != idA {
				return errors.New("tx read another tenant's row")
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
