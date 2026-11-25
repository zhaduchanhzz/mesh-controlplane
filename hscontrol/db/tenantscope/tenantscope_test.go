package tenantscope

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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

// tailnetRow is the platform table the fence reads (no tailnet_id column, so
// the plugin itself does not scope it).
type tailnetRow struct {
	ID             string     `gorm:"primaryKey"`
	OwnerClusterID string     `gorm:"column:owner_cluster_id;not null"`
	OwnerEpoch     int64      `gorm:"column:owner_epoch;not null"`
	Status         string     `gorm:"column:status;not null;default:ACTIVE"`
	DeletedAt      *time.Time `gorm:"column:deleted_at"`
}

func (tailnetRow) TableName() string { return "tailnets" }

var errTxView = errors.New("transaction view")

const (
	testCluster = "c1"
	testEpoch   = 1
)

// global has no tailnet column; the plugin must leave it alone.
type global struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func ctxFor(tb testing.TB, id string) context.Context {
	tb.Helper()

	return fencedCtx(tb, id, testCluster, testEpoch)
}

// scopeOnlyCtx carries a scope but no ownership fence.
func scopeOnlyCtx(tb testing.TB, id string) context.Context {
	tb.Helper()

	s, err := tailnet.NewScope(tailnet.MustParseID(id), tailnet.SourceInternalService, "test", 1)
	if err != nil {
		tb.Fatal(err)
	}

	ctx, err := tailnet.WithScope(context.Background(), s)
	if err != nil {
		tb.Fatal(err)
	}

	return ctx
}

func fencedCtx(tb testing.TB, id, cluster string, epoch tailnet.Epoch) context.Context {
	tb.Helper()

	f, err := tailnet.NewFence(cluster, epoch)
	if err != nil {
		tb.Fatal(err)
	}

	ctx, err := tailnet.WithFence(scopeOnlyCtx(tb, id), f)
	if err != nil {
		tb.Fatal(err)
	}

	return ctx
}

// dialector picks SQLite by default, or PostgreSQL when
// MESH_TEST_POSTGRES_DSN is set (ADR 0005: production is shared PostgreSQL).
// Each PostgreSQL test gets its own schema so runs do not interfere.
func dialector(tb testing.TB) gorm.Dialector {
	tb.Helper()

	dsn := os.Getenv("MESH_TEST_POSTGRES_DSN")
	if dsn == "" {
		return sqlite.Open(filepath.Join(tb.TempDir(), "t.db"))
	}

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		tb.Fatal(err)
	}

	sch := fmt.Sprintf("ts_%d", time.Now().UnixNano())

	err = admin.Exec("CREATE SCHEMA " + sch).Error
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + sch + " CASCADE").Error

		sqlDB, err := admin.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return postgres.Open(dsn + " search_path=" + sch)
}

func openDB(tb testing.TB) *gorm.DB {
	tb.Helper()

	db, err := gorm.Open(dialector(tb), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		tb.Fatal(err)
	}
	// Close before TempDir removal (cleanups run LIFO); Windows locks open files.
	tb.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	err = db.AutoMigrate(&owner{}, &item{}, &global{}, &tailnetRow{})
	if err != nil {
		tb.Fatal(err)
	}

	for _, tn := range []string{idA, idB} {
		err := db.Create(&tailnetRow{ID: tn, OwnerClusterID: testCluster, OwnerEpoch: testEpoch, Status: "ACTIVE"}).Error
		if err != nil {
			tb.Fatal(err)
		}
	}
	// Seed both tenants directly, bypassing the plugin (registered after).
	for _, tn := range []string{idA, idB} {
		o := owner{TailnetID: tn, Name: "o-" + tn[:4]}

		err := db.Create(&o).Error
		if err != nil {
			tb.Fatal(err)
		}

		for i := range 2 {
			err := db.Create(&item{TailnetID: tn, OwnerID: o.ID, Name: fmt.Sprintf("i%d-%s", i, tn[:4])}).Error
			if err != nil {
				tb.Fatal(err)
			}
		}
	}

	err = db.Create(&global{Name: "g"}).Error
	if err != nil {
		tb.Fatal(err)
	}

	err = Register(db)
	if err != nil {
		tb.Fatal(err)
	}

	return db
}

func TestNoScopeFailsClosed(t *testing.T) {
	db := openDB(t)
	bg := db.WithContext(context.Background())

	var items []item

	err := bg.Find(&items).Error
	if !errors.Is(err, tailnet.ErrNoScope) || len(items) != 0 {
		t.Fatalf("Find without scope: err=%v rows=%d, want ErrNoScope and 0 rows", err, len(items))
	}

	var n int64

	err = bg.Model(&item{}).Count(&n).Error
	if !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Count without scope: err=%v", err)
	}

	err = bg.Model(&item{}).Where("1 = 1").Update("name", "pwned").Error
	if !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Update without scope: err=%v", err)
	}

	err = bg.Where("1 = 1").Delete(&item{}).Error
	if !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Delete without scope: err=%v", err)
	}

	err = bg.Create(&item{Name: "x"}).Error
	if !errors.Is(err, tailnet.ErrNoScope) {
		t.Fatalf("Create without scope: err=%v", err)
	}

	// Nothing changed.
	var all []item

	err = db.WithContext(ctxFor(t, idA)).Find(&all).Error
	if err != nil || len(all) != 2 {
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

		err := db.WithContext(ctxFor(t, tn)).Find(&items).Error
		if err != nil {
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

	err := db.WithContext(ctxFor(t, idB)).First(&b).Error
	if err != nil {
		t.Fatal(err)
	}

	var got item

	err = db.WithContext(ctxFor(t, idA)).First(&got, b.ID).Error
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

	err := db.WithContext(ctxFor(t, idB)).Find(&bItems).Error
	if err != nil {
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

	err = db.WithContext(ctxFor(t, idB)).Find(&bItems).Error
	if err != nil || len(bItems) != 2 {
		t.Fatalf("B after A's delete: %d rows, %v", len(bItems), err)
	}
}

func TestCreateStampsAndRejectsForeignTenant(t *testing.T) {
	db := openDB(t)
	ctxA := ctxFor(t, idA)

	var ownerA owner

	err := db.WithContext(ctxA).First(&ownerA).Error
	if err != nil {
		t.Fatal(err)
	}

	it := item{Name: "new", OwnerID: ownerA.ID}

	err = db.WithContext(ctxA).Create(&it).Error
	if err != nil {
		t.Fatal(err)
	}

	if it.TailnetID != idA {
		t.Fatalf("created row tailnet = %q, want A", it.TailnetID)
	}

	batch := []item{{Name: "b1", OwnerID: ownerA.ID}, {Name: "b2", OwnerID: ownerA.ID}}

	err = db.WithContext(ctxA).Create(&batch).Error
	if err != nil {
		t.Fatal(err)
	}

	for _, b := range batch {
		if b.TailnetID != idA {
			t.Fatalf("batch row tailnet = %q, want A", b.TailnetID)
		}
	}

	evil := item{TailnetID: idB, Name: "smuggled", OwnerID: ownerA.ID}

	err = db.WithContext(ctxA).Create(&evil).Error
	if !errors.Is(err, ErrForeignTenant) {
		t.Fatalf("create with B's tailnet under A: err=%v, want ErrForeignTenant", err)
	}
}

func TestSaveCannotOverwriteOtherTenant(t *testing.T) {
	db := openDB(t)

	var b item

	err := db.WithContext(ctxFor(t, idB)).First(&b).Error
	if err != nil {
		t.Fatal(err)
	}
	// Tenant A tries to Save a row carrying B's primary key.
	hijack := item{ID: b.ID, Name: "hijacked"}
	_ = db.WithContext(ctxFor(t, idA)).Save(&hijack).Error

	var after item

	err = db.WithContext(ctxFor(t, idB)).First(&after, b.ID).Error
	if err != nil {
		t.Fatal(err)
	}

	if after.Name != b.Name || after.TailnetID != idB {
		t.Fatalf("B's row changed via A's Save: %+v", after)
	}
}

func TestPreloadIsScoped(t *testing.T) {
	db := openDB(t)

	var items []item

	err := db.WithContext(ctxFor(t, idA)).Preload("Owner").Find(&items).Error
	if err != nil {
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

	err := db.WithContext(context.Background()).Find(&gs).Error
	if err != nil || len(gs) != 1 {
		t.Fatalf("global table without scope: %d rows, %v", len(gs), err)
	}
}

func TestTransactionKeepsScope(t *testing.T) {
	db := openDB(t)

	err := db.WithContext(ctxFor(t, idA)).Transaction(func(tx *gorm.DB) error {
		var items []item

		err := tx.Find(&items).Error
		if err != nil {
			return err
		}

		if len(items) != 2 {
			return fmt.Errorf("%w: tx sees %d items", errTxView, len(items))
		}

		for _, it := range items {
			if it.TailnetID != idA {
				return fmt.Errorf("%w: tx read another tenant's row", errTxView)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
