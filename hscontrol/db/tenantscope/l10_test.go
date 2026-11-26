package tenantscope

import (
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
)

// P12c L10: a slice of rows without primary keys is not "keyed" (GORM adds
// no condition for them, so the DELETE would hit every row of the tenant),
// and a batch create from maps is stamped like a single map.

func TestDeleteSliceWithoutKeysRefused(t *testing.T) {
	db := openDB(t)
	dbA := db.WithContext(ctxFor(t, idA))

	err := dbA.Delete(&[]item{{}, {}}).Error
	if !errors.Is(err, gorm.ErrMissingWhereClause) {
		t.Errorf("Delete of rows without primary keys: err=%v, want ErrMissingWhereClause", err)
	}

	if got := itemsOf(t, db, idA); len(got) != 2 {
		t.Fatalf("A's items after the refused delete: %v", got)
	}

	// Rows with keys still delete exactly those rows.
	var mine []item
	if err := dbA.Find(&mine).Error; err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}

	first := mine[:1]

	err = dbA.Delete(&first).Error
	if err != nil {
		t.Fatal(err)
	}

	if got := itemsOf(t, db, idA); len(got) != 1 {
		t.Fatalf("A's items after deleting one keyed row: %v", got)
	}
}

func TestCreateFromMapsStamped(t *testing.T) {
	db := openDB(t)
	dbA := db.WithContext(ctxFor(t, idA))

	var ownerA owner
	if err := dbA.First(&ownerA).Error; err != nil { //nolint:noinlineerr
		t.Fatal(err)
	}

	// Built, not run: GORM itself cannot scan the returned keys of a batch
	// of maps into the maps; what matters here is how the plugin stamps it.
	create := func(rows []map[string]any) (stmt *gorm.Statement, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()

		tx := dbA.Session(&gorm.Session{DryRun: true}).Model(&item{}).Create(rows)

		return tx.Statement, tx.Error
	}

	rows := []map[string]any{{"name": "m1", "owner_id": ownerA.ID}, {"name": "m2", "owner_id": ownerA.ID}}

	stmt, err := create(rows)
	if err != nil {
		t.Fatalf("batch create from maps under A: %v", err)
	}

	for i, r := range rows {
		if r["tailnet_id"] != idA {
			t.Errorf("row %d stamped with %v, want tenant A", i, r["tailnet_id"])
		}
	}

	stamped := 0

	for _, v := range stmt.Vars {
		if v == idA {
			stamped++
		}
	}

	if stamped != len(rows) {
		t.Errorf("INSERT binds tenant A %d times, want %d: %s %v", stamped, len(rows), stmt.SQL.String(), stmt.Vars)
	}

	_, err = create([]map[string]any{{"name": "m3", "owner_id": ownerA.ID, "tailnet_id": idB}})
	if !errors.Is(err, ErrForeignTenant) {
		t.Fatalf("batch map naming B under A: err=%v, want ErrForeignTenant", err)
	}

	if got := itemsOf(t, db, idB); len(got) != 2 {
		t.Fatalf("B's items after A's refused batch: %v", got)
	}
}
