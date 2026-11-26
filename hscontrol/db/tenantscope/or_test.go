package tenantscope

import (
	"regexp"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// P12c A1: the tenant predicate is ANDed with the whole WHERE the caller
// built, so an OR (or a NOT, or a raw OR without the spaces GORM looks for)
// can never reach another tenant's rows, for SELECT, Row, UPDATE and DELETE.

const (
	a0 = "i0-0b6f" // item 0 of tenant A (see openDB)
	b0 = "i0-7e9d" // item 0 of tenant B
)

// itemsOf returns the names of tenant tn's items, read under tn's scope.
func itemsOf(t *testing.T, db *gorm.DB, tn string) []string {
	t.Helper()

	var names []string

	err := db.WithContext(ctxFor(t, tn)).Model(&item{}).Order("name").Pluck("name", &names).Error
	if err != nil {
		t.Fatal(err)
	}

	return names
}

func TestOrCannotEscapeScopeOnRead(t *testing.T) {
	db := openDB(t)
	dbA := db.WithContext(ctxFor(t, idA))

	// want is how many of A's own items the query matches; B's item i0
	// matches every query too.
	type orQuery struct {
		name string
		q    func(*gorm.DB) *gorm.DB
		want int64
	}

	queries := []orQuery{
		{"Where.Or", func(d *gorm.DB) *gorm.DB { return d.Where("name = ?", b0).Or("name = ?", a0) }, 1},
		// GORM moves the first non-Or expression to the front.
		{"Or first", func(d *gorm.DB) *gorm.DB { return d.Or("name = ?", a0).Where("name = ?", b0) }, 1},
		{"raw OR( without spaces", func(d *gorm.DB) *gorm.DB { return d.Where("name = ? OR(name = ?)", b0, a0) }, 1},
		{"map Or", func(d *gorm.DB) *gorm.DB { return d.Where(map[string]any{"name": b0}).Or(map[string]any{"name": a0}) }, 1},
		{"Not.Or", func(d *gorm.DB) *gorm.DB { return d.Not("name = ?", "nobody").Or("name = ?", b0) }, 2},
	}

	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			var items []item

			err := q.q(dbA).Find(&items).Error
			if err != nil {
				t.Fatal(err)
			}

			for _, it := range items {
				if it.TailnetID != idA {
					t.Fatalf("Find under A returned %q of tenant %s", it.Name, it.TailnetID[:4])
				}
			}

			var n int64

			err = q.q(dbA.Model(&item{})).Count(&n).Error
			if err != nil {
				t.Fatal(err)
			}

			if n != q.want {
				t.Fatalf("Count under A = %d, want %d (A's own matches)", n, q.want)
			}

			var row int64

			err = q.q(dbA.Model(&item{})).Select("count(*)").Row().Scan(&row)
			if err != nil {
				t.Fatal(err)
			}

			if row != q.want {
				t.Fatalf("Row count under A = %d, want %d (A's own matches)", row, q.want)
			}
		})
	}
}

// B's condition first: "b OR a AND tenant" would be "b OR (a AND tenant)".
func TestOrCannotEscapeScopeOnUpdate(t *testing.T) {
	db := openDB(t)
	dbA := db.WithContext(ctxFor(t, idA))

	res := dbA.Model(&item{}).Where("name = ?", b0).Or("name = ?", a0).Update("name", "renamed")
	if res.Error != nil {
		t.Fatal(res.Error)
	}

	if got := itemsOf(t, db, idB); len(got) != 2 || got[0] != b0 {
		t.Fatalf("A's Where.Or.Update changed B's items: %v", got)
	}

	if res.RowsAffected != 1 {
		t.Fatalf("A's update affected %d rows, want 1 (its own item)", res.RowsAffected)
	}
}

func TestOrCannotEscapeScopeOnDelete(t *testing.T) {
	db := openDB(t)
	dbA := db.WithContext(ctxFor(t, idA))

	res := dbA.Where("name = ?", b0).Or("name = ?", a0).Delete(&item{})
	if res.Error != nil {
		t.Fatal(res.Error)
	}

	if got := itemsOf(t, db, idB); len(got) != 2 {
		t.Fatalf("A's Where.Or.Delete deleted B's items: B has %v", got)
	}

	if res.RowsAffected != 1 {
		t.Fatalf("A's delete affected %d rows, want 1 (its own item)", res.RowsAffected)
	}
}

var (
	quoteRE       = regexp.MustCompile("[`\"]")
	placeholderRE = regexp.MustCompile(`\$\d+`)
)

// normSQL makes SQLite and PostgreSQL output comparable: no identifier
// quotes, "?" placeholders.
func normSQL(s string) string {
	return placeholderRE.ReplaceAllString(quoteRE.ReplaceAllString(s, ""), "?")
}

// The generated SQL, checked on both dialects: the tenant predicate comes
// first and the caller's WHERE is one parenthesised group after it.
func TestScopeWrapsCallerWhere(t *testing.T) {
	db := openDB(t)
	dry := db.WithContext(ctxFor(t, idA)).Session(&gorm.Session{DryRun: true})

	cases := []struct {
		name string
		q    func(*gorm.DB) *gorm.DB
		want string
	}{
		{
			"Where.Or",
			func(d *gorm.DB) *gorm.DB { return d.Where("name = ?", b0).Or("name = ?", a0) },
			"WHERE items.tailnet_id = ? AND (name = ? OR name = ?)",
		},
		{
			"raw OR(",
			func(d *gorm.DB) *gorm.DB { return d.Where("name = ? OR(name = ?)", b0, a0) },
			"WHERE items.tailnet_id = ? AND (name = ? OR(name = ?))",
		},
		{
			"no caller WHERE",
			func(d *gorm.DB) *gorm.DB { return d },
			"WHERE items.tailnet_id = ?",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var items []item

			stmt := c.q(dry).Find(&items).Statement
			got := normSQL(stmt.SQL.String())

			if !strings.HasSuffix(got, c.want) {
				t.Fatalf("SQL = %q\nwant suffix %q", got, c.want)
			}

			if len(stmt.Vars) == 0 || stmt.Vars[0] != idA {
				t.Fatalf("first bound value = %v, want tenant A", stmt.Vars)
			}
		})
	}
}
