package db

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"gorm.io/gorm"
)

// P12c A5: the schema invariants of tenant isolation, read back from the
// database (not from the schema files):
//   - every table with a tailnet_id column has it NOT NULL with a foreign key
//     to tailnets(id) (AGENTS rule 7, ADR 0005);
//   - the tenant tables tenantscope guards are exactly the tables with
//     tailnet_id, minus the platform table tailnet_moves;
//   - the SQLite schema (development, unit tests) and the PostgreSQL schema
//     (production) have the same tables, columns, NOT NULL columns, primary
//     keys, unique indexes and foreign keys.

// platformTailnetTables have a tailnet_id that names the tailnet a platform
// row is about, not a tenant scope (see tailnet_moves.go).
var platformTailnetTables = []string{"tailnet_moves"}

type schemaColumn struct {
	notNull bool
	pk      bool
}

type schemaTable struct {
	columns map[string]schemaColumn
	pk      string   // primary key columns, comma separated, in key order
	uniques []string // "cols[|partial]" of every unique index other than the primary key, sorted
	fks     []string // "cols->table(cols)", sorted
}

type schemaInfo map[string]*schemaTable

func introspect(t *testing.T, db *gorm.DB) schemaInfo {
	t.Helper()

	if db.Name() == dialectPostgres {
		return introspectPostgres(t, db)
	}

	return introspectSQLite(t, db)
}

func introspectSQLite(t *testing.T, db *gorm.DB) schemaInfo {
	t.Helper()

	var tables []string
	require.NoError(t, db.Raw(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).
		Scan(&tables).Error)

	info := schemaInfo{}

	for _, name := range tables {
		tbl := &schemaTable{columns: map[string]schemaColumn{}}

		var cols []struct {
			Name    string
			NotNull bool `gorm:"column:notnull"`
			PK      int  `gorm:"column:pk"`
		}
		require.NoError(t, db.Raw(`SELECT name, "notnull", pk FROM pragma_table_info(?)`, name).Scan(&cols).Error)

		pk := map[int]string{}

		for _, c := range cols {
			// SQLite allows NULL in a non-INTEGER primary key column, and
			// reports an INTEGER PRIMARY KEY as nullable; PostgreSQL makes
			// every primary key column NOT NULL. Compare the intent.
			tbl.columns[c.Name] = schemaColumn{notNull: c.NotNull || c.PK > 0, pk: c.PK > 0}
			if c.PK > 0 {
				pk[c.PK] = c.Name
			}
		}

		for _, k := range slices.Sorted(maps.Keys(pk)) {
			tbl.pk = joinCol(tbl.pk, pk[k])
		}

		var idx []struct {
			Name    string
			Unique  bool
			Origin  string
			Partial bool
		}
		require.NoError(t, db.Raw(`SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, name).Scan(&idx).Error)

		for _, ix := range idx {
			if !ix.Unique || ix.Origin == "pk" {
				continue
			}

			var icols []string
			require.NoError(t, db.Raw(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, ix.Name).Scan(&icols).Error)
			tbl.uniques = append(tbl.uniques, uniqueKey(icols, ix.Partial))
		}

		var fks []struct {
			ID    int
			Seq   int
			Table string
			From  string
			To    string
		}
		require.NoError(t, db.Raw(`SELECT id, seq, "table", "from", "to" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, name).
			Scan(&fks).Error)

		byID := map[int][3]string{}

		for _, fk := range fks {
			e := byID[fk.ID]
			e[0] = joinCol(e[0], fk.From)
			e[1] = fk.Table
			e[2] = joinCol(e[2], fk.To)
			byID[fk.ID] = e
		}

		for _, e := range byID {
			tbl.fks = append(tbl.fks, e[0]+"->"+e[1]+"("+e[2]+")")
		}

		slices.Sort(tbl.uniques)
		slices.Sort(tbl.fks)
		info[name] = tbl
	}

	return info
}

func introspectPostgres(t *testing.T, db *gorm.DB) schemaInfo {
	t.Helper()

	var tables []string
	require.NoError(t, db.Raw(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`).Scan(&tables).Error)

	info := schemaInfo{}

	for _, name := range tables {
		tbl := &schemaTable{columns: map[string]schemaColumn{}}

		var cols []struct {
			ColumnName string
			IsNullable string
		}
		require.NoError(t, db.Raw(`SELECT column_name, is_nullable FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ?`, name).Scan(&cols).Error)

		for _, c := range cols {
			tbl.columns[c.ColumnName] = schemaColumn{notNull: c.IsNullable == "NO"}
		}

		// attnames lists the columns of an int2vector in key order.
		const attnames = `(SELECT string_agg(a.attname, ',' ORDER BY k.ord)
			FROM unnest(%s) WITH ORDINALITY AS k(attnum, ord)
			JOIN pg_attribute a ON a.attrelid = %s AND a.attnum = k.attnum)`

		var idx []struct {
			Cols    string
			Primary bool
			Partial bool
		}
		require.NoError(t, db.Raw(`SELECT `+fmt.Sprintf(attnames, "ix.indkey", "ix.indrelid")+` AS cols,
				ix.indisprimary AS "primary", ix.indpred IS NOT NULL AS partial
			FROM pg_index ix
			JOIN pg_class tc ON tc.oid = ix.indrelid
			JOIN pg_namespace n ON n.oid = tc.relnamespace
			WHERE n.nspname = current_schema() AND tc.relname = ? AND ix.indisunique`, name).Scan(&idx).Error)

		for _, ix := range idx {
			if ix.Primary {
				tbl.pk = ix.Cols
				for _, c := range strings.Split(ix.Cols, ",") {
					col := tbl.columns[c]
					col.pk = true
					tbl.columns[c] = col
				}

				continue
			}

			tbl.uniques = append(tbl.uniques, uniqueKey(strings.Split(ix.Cols, ","), ix.Partial))
		}

		var fks []struct {
			Cols    string
			RefName string
			RefCols string
		}
		require.NoError(t, db.Raw(`SELECT `+fmt.Sprintf(attnames, "c.conkey", "c.conrelid")+` AS cols,
				rt.relname AS ref_name, `+fmt.Sprintf(attnames, "c.confkey", "c.confrelid")+` AS ref_cols
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_class rt ON rt.oid = c.confrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE c.contype = 'f' AND n.nspname = current_schema() AND t.relname = ?`, name).Scan(&fks).Error)

		for _, fk := range fks {
			tbl.fks = append(tbl.fks, fk.Cols+"->"+fk.RefName+"("+fk.RefCols+")")
		}

		slices.Sort(tbl.uniques)
		slices.Sort(tbl.fks)
		info[name] = tbl
	}

	return info
}

func joinCol(list, col string) string {
	if list == "" {
		return col
	}

	return list + "," + col
}

func uniqueKey(cols []string, partial bool) string {
	k := strings.Join(cols, ",")
	if partial {
		k += "|partial"
	}

	return k
}

// tablesWithTailnetID lists the tables that have a tailnet_id column.
func (info schemaInfo) tablesWithTailnetID() []string {
	var out []string

	for name, tbl := range info {
		if _, ok := tbl.columns["tailnet_id"]; ok {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out
}

// checkTenantColumns: tailnet_id NOT NULL and a foreign key to tailnets(id).
func checkTenantColumns(t *testing.T, info schemaInfo) {
	t.Helper()

	tenantTables := info.tablesWithTailnetID()
	require.NotEmpty(t, tenantTables)

	for _, name := range tenantTables {
		tbl := info[name]
		assert.True(t, tbl.columns["tailnet_id"].notNull, "%s.tailnet_id must be NOT NULL", name)
		assert.Contains(t, tbl.fks, "tailnet_id->tailnets(id)", "%s.tailnet_id must reference tailnets(id)", name)
	}

	guarded := slices.DeleteFunc(slices.Clone(tenantTables), func(n string) bool {
		return slices.Contains(platformTailnetTables, n)
	})
	want := tenantscope.TenantTables()
	slices.Sort(want)
	assert.Equal(t, want, guarded,
		"tenantscope.TenantTables must be the tables with tailnet_id (minus platform tables %v)", platformTailnetTables)
}

func TestSchemaTenantColumns(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			checkTenantColumns(t, introspect(t, p.db))
		})
	}
}

// TestSchemaSQLiteMatchesPostgres compares the two fresh schemas
// structurally. Column types differ on purpose (integer/bigint,
// datetime/timestamptz, blob/bytea, numeric/boolean) and are not compared.
func TestSchemaSQLiteMatchesPostgres(t *testing.T) {
	lite := openPlatform(t, sqliteTestConfig(t.TempDir()+"/schema.db"))
	pg := openPlatform(t, postgresConfig(t))

	a, b := introspect(t, lite.db), introspect(t, pg.db)

	require.Equal(t, slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b)), "tables")

	for name, ta := range a {
		tb := b[name]

		assert.Equal(t, slices.Sorted(maps.Keys(ta.columns)), slices.Sorted(maps.Keys(tb.columns)), "%s: columns", name)

		for col, ca := range ta.columns {
			assert.Equal(t, ca.notNull, tb.columns[col].notNull, "%s.%s: NOT NULL", name, col)
		}

		assert.Equal(t, ta.pk, tb.pk, "%s: primary key", name)
		assert.Equal(t, ta.uniques, tb.uniques, "%s: unique indexes", name)
		assert.Equal(t, ta.fks, tb.fks, "%s: foreign keys", name)
	}
}
