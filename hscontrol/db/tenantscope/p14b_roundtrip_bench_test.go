package tenantscope

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// BenchmarkP14bFenceRoundTrips (P14b candidate #4): statements each client
// round trip of a fenced write costs on PostgreSQL, counted at the driver
// (pgx tracer: every Exec/Query, BEGIN and COMMIT included), next to the time
// per write on the test server. The fence and lease checks run through
// Statement.ConnPool directly, so gorm's logger would not see them.
//
// Expected per write statement today: fence (advisory lock + tailnets FOR
// SHARE), transaction bound (+ cluster lease lock), lease read. A
// transaction with two writes pays them twice.
//
// PostgreSQL only (MESH_TEST_POSTGRES_DSN), like the other fence benchmarks.
//
//	MESH_TEST_POSTGRES_DSN=… go test ./hscontrol/db/tenantscope -run '^$' -bench BenchmarkP14bFenceRoundTrips -count 6
func BenchmarkP14bFenceRoundTrips(b *testing.B) {
	for _, mode := range []string{modeNoFence, modeFence, modeLease} {
		b.Run(mode+"/update", func(b *testing.B) {
			db, ctx, ownerID, stmts := countingBenchDB(b, mode)

			it := item{Name: "bench", OwnerID: ownerID}

			err := db.WithContext(ctx).Create(&it).Error
			if err != nil {
				b.Fatal(err)
			}

			countStatements(b, stmts, func(i int) error {
				return db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).Update("name", strconv.Itoa(i)).Error
			})
		})

		b.Run(mode+"/create", func(b *testing.B) {
			db, ctx, ownerID, stmts := countingBenchDB(b, mode)

			countStatements(b, stmts, func(int) error {
				return db.WithContext(ctx).Create(&item{Name: "c", OwnerID: ownerID}).Error
			})
		})

		b.Run(mode+"/tx2", func(b *testing.B) {
			db, ctx, ownerID, stmts := countingBenchDB(b, mode)

			ids := make([]uint64, 2)
			for k := range ids {
				it := item{Name: "t", OwnerID: ownerID}

				err := db.WithContext(ctx).Create(&it).Error
				if err != nil {
					b.Fatal(err)
				}

				ids[k] = it.ID
			}

			countStatements(b, stmts, func(i int) error {
				return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					for _, id := range ids {
						err := tx.Model(&item{}).Where("id = ?", id).Update("name", strconv.Itoa(i)).Error
						if err != nil {
							return err
						}
					}

					return nil
				})
			})
		})
	}
}

// countStatements runs write in the benchmark loop and reports the
// statements it sent per iteration.
func countStatements(b *testing.B, stmts *atomic.Int64, write func(i int) error) {
	b.Helper()
	b.ResetTimer()

	start := stmts.Load()

	for i := 0; b.Loop(); i++ {
		err := write(i)
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ReportMetric(float64(stmts.Load()-start)/float64(b.N), "stmts/op")
}

func countingBenchDB(b *testing.B, mode string) (*gorm.DB, context.Context, uint64, *atomic.Int64) {
	b.Helper()

	dial, stmts := countingDialector(b)
	db, ctx, ownerID := benchDBOn(b, openDBWith(b, dial), mode)

	return db, ctx, ownerID, stmts
}

// stmtCounter is a pgx tracer counting every statement sent.
type stmtCounter struct{ n *atomic.Int64 }

func (c stmtCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)

	return ctx
}

func (stmtCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countingDialector is dialector's PostgreSQL branch (own schema per
// benchmark) on a pgx connection that counts statements.
func countingDialector(tb testing.TB) (gorm.Dialector, *atomic.Int64) {
	tb.Helper()

	dsn := os.Getenv("MESH_TEST_POSTGRES_DSN")
	if dsn == "" {
		tb.Skip("PostgreSQL only (MESH_TEST_POSTGRES_DSN)")
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

	cfg, err := pgx.ParseConfig(dsn + " search_path=" + sch)
	if err != nil {
		tb.Fatal(err)
	}

	n := &atomic.Int64{}
	cfg.Tracer = stmtCounter{n: n}

	return postgres.New(postgres.Config{Conn: stdlib.OpenDB(*cfg)}), n
}
