package tenantscope

import (
	"testing"
)

// BenchmarkScopedStatements measures the plugin's work on the statements
// a runtime runs most (P12c: the WHERE grouping, the table check and the
// assignment check are on every statement). SQLite by default, PostgreSQL
// with MESH_TEST_POSTGRES_DSN.
func BenchmarkScopedStatements(b *testing.B) {
	db := openDB(b)
	dbA := db.WithContext(ctxFor(b, idA))

	b.Run("First by id", func(b *testing.B) {
		var it item
		if err := dbA.First(&it).Error; err != nil { //nolint:noinlineerr
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			var got item
			if err := dbA.Where("id = ?", it.ID).First(&got).Error; err != nil { //nolint:noinlineerr
				b.Fatal(err)
			}
		}
	})

	b.Run("Find with two conditions", func(b *testing.B) {
		b.ReportAllocs()

		for range b.N {
			var got []item
			if err := dbA.Where("name <> ?", "x").Where("owner_id > ?", 0).Find(&got).Error; err != nil { //nolint:noinlineerr
				b.Fatal(err)
			}
		}
	})

	b.Run("Update one column", func(b *testing.B) {
		var it item
		if err := dbA.First(&it).Error; err != nil { //nolint:noinlineerr
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			if err := dbA.Model(&item{}).Where("id = ?", it.ID).Update("name", "n").Error; err != nil { //nolint:noinlineerr
				b.Fatal(err)
			}
		}
	})
}
