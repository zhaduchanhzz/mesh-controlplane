package tenantscope

import (
	"errors"
	"testing"

	"gorm.io/gorm/clause"
)

// P12c A2: the lexical check that finds a tenant table used as a table in
// a string the caller gave GORM.
func TestRefersTo(t *testing.T) {
	p := newPlugin([]string{"items"})

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT id FROM nodes", "nodes"},
		{"JOIN public.nodes ON x = y", "nodes"},
		{`JOIN "Nodes" n ON n.id = x`, "Nodes"},
		{"JOIN `credentials` c", "credentials"},
		{"id IN (SELECT tailnet_id FROM main . users)", "users"},
		{"FROM tailnets, policies", "policies"},
		{"INSERT INTO tailnet_settings VALUES (?)", "tailnet_settings"},
		{"x IN (SELECT 1 FROM items)", "items"},
		{"[nodes]", "nodes"},
		{"a[1] = 2 AND b IN (SELECT 1 FROM nodes)", "nodes"},
		// Not table uses.
		{"nodes.ephemeral = true", ""},
		{`"nodes"."id" = ?`, ""},
		{"name = 'nodes'", ""},
		{"name = 'it''s nodes'", ""},
		{"x = 1 -- FROM nodes", ""},
		{"x = 1 /* FROM nodes */", ""},
		{"FROM tailnets", ""},
		{"nodesx = 1 AND xnodes = 2", ""},
	}

	for _, c := range cases {
		got, found := p.refersTo(c.sql)
		if found != (c.want != "") || got != c.want {
			t.Errorf("refersTo(%q) = %q, %t; want %q", c.sql, got, found, c.want)
		}
	}
}

func TestSimpleTable(t *testing.T) {
	cases := []struct {
		sql    string
		want   string
		simple bool
	}{
		{`"nodes"`, "nodes", true},
		{"`main`.`nodes`", "nodes", true},
		{`"public"."Nodes"`, "nodes", true},
		{"nodes AS n", "nodes", true},
		{"nodes n", "nodes", true},
		{"nodes, users", "", false},
		{"(SELECT * FROM nodes) AS n", "", false},
	}

	for _, c := range cases {
		got, ok := simpleTable(&clause.Expr{SQL: c.sql})
		if ok != c.simple || got != c.want {
			t.Errorf("simpleTable(%q) = %q, %t; want %q, %t", c.sql, got, ok, c.want, c.simple)
		}
	}
}

// The plugin's own test models: a table expression, a string join and a
// clause naming the tenant table "items" are refused; the confined model
// with a qualified column, and an unrelated table, are not.
func TestUnconfinedTenantTableRefused(t *testing.T) {
	db := openDB(t)
	dbA := db.WithContext(ctxFor(t, idA))

	type nameOnly struct{ Name string }

	var out []nameOnly

	refused := map[string]error{
		"Table(items)":            dbA.Table("items").Find(&out).Error,
		"Table(items i)":          dbA.Table("items i").Find(&out).Error,
		"Table((?) x) with items": dbA.Table("(SELECT id, name FROM items) x").Find(&out).Error,
		"Model(global).Joins":     dbA.Model(&global{}).Joins("JOIN items ON 1 = 1").Find(&out).Error,
		"Model(global).Where":     dbA.Model(&global{}).Where("EXISTS (SELECT 1 FROM items)").Find(&out).Error,
		"Model(item).Order":       dbA.Model(&item{}).Order("(SELECT max(name) FROM owners)").Find(&out).Error,
		"Model(item).Group":       dbA.Model(&item{}).Select("name").Group("name").Having("count(*) > (SELECT count(*) FROM items)").Find(&out).Error,
		"Clauses(From items)":     dbA.Model(&global{}).Clauses(clause.From{Tables: []clause.Table{{Name: "items"}}}).Find(&out).Error,
		"Table(items).Create":     dbA.Table("items").Create(map[string]any{"name": "x", "tailnet_id": idB}).Error,
	}

	for what, err := range refused {
		if !errors.Is(err, ErrUnscopable) {
			t.Errorf("%s under A: err=%v, want ErrUnscopable", what, err)
		}
	}

	var items []item

	err := dbA.Where("items.name = ?", a0).Find(&items).Error
	if err != nil || len(items) != 1 {
		t.Fatalf("confined model with a qualified column: %d rows, %v", len(items), err)
	}

	err = dbA.Model(&item{}).Joins("Owner").Find(&items).Error
	if err != nil || len(items) != 2 {
		t.Fatalf("association join of a confined model: %d rows, %v", len(items), err)
	}

	var gs []global

	err = dbA.Table("globals").Find(&gs).Error
	if err != nil || len(gs) != 1 {
		t.Fatalf("Table(globals): %d rows, %v", len(gs), err)
	}
}
