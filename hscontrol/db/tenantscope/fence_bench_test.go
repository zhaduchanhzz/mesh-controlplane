package tenantscope

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// Benchmarks of the ownership fence (P10 review B3). Every INSERT/UPDATE/
// DELETE on a tenant table reads the tailnets row in the statement's
// transaction (FOR SHARE on PostgreSQL). These measure:
//
//   - nofence vs fence: the cost of that read per write, with the fence
//     callbacks removed from an otherwise identical plugin;
//   - serial vs parallel on ONE tailnet: the hot-row case (every writer of a
//     tailnet shares a lock on the same tailnets row; on PostgreSQL concurrent
//     FOR SHARE lockers become a MultiXact);
//   - tx10: ten updates in one transaction, ten fence reads;
//   - owner move under write load: the UPDATE of the tailnets row that
//     moves ownership waits for writers holding FOR SHARE.
//
// SQLite by default; PostgreSQL with MESH_TEST_POSTGRES_DSN (production).

// withoutFence removes the fence callbacks, leaving scoping in place.
func withoutFence(b *testing.B, db *gorm.DB) {
	b.Helper()
	cb := db.Callback()
	for _, err := range []error{
		cb.Create().Remove(name + ":fence_create"),
		cb.Update().Remove(name + ":fence_update"),
		cb.Delete().Remove(name + ":fence_delete"),
	} {
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchDB(b *testing.B, fenced bool) (*gorm.DB, context.Context, uint64) {
	b.Helper()
	db := openDB(b)
	if !fenced {
		withoutFence(b, db)
	}
	if sqlDB, err := db.DB(); err == nil && db.Dialector.Name() == "postgres" {
		sqlDB.SetMaxOpenConns(64)
		sqlDB.SetMaxIdleConns(64)
	}
	ctx := ctxFor(b, idA)
	var it item
	if err := db.WithContext(ctx).Order("id").First(&it).Error; err != nil {
		b.Fatal(err)
	}

	return db, ctx, it.OwnerID
}

// update writes one row per parallel worker, so the only shared row between
// workers is the tailnets row read by the fence (no row-lock contention on
// the item itself).
func BenchmarkFenceWrite(b *testing.B) {
	for _, fenced := range []bool{false, true} {
		mode := "nofence"
		if fenced {
			mode = "fence"
		}

		b.Run(mode+"/update/serial", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, fenced)
			it := item{Name: "bench", OwnerID: ownerID}
			if err := db.WithContext(ctx).Create(&it).Error; err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				if err := db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).
					Update("name", fmt.Sprint(i)).Error; err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(mode+"/create/serial", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, fenced)
			b.ResetTimer()
			for b.Loop() {
				if err := db.WithContext(ctx).Create(&item{Name: "c", OwnerID: ownerID}).Error; err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(mode+"/tx10/serial", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, fenced)
			ids := make([]uint64, 10)
			for k := range ids {
				it := item{Name: "t", OwnerID: ownerID}
				if err := db.WithContext(ctx).Create(&it).Error; err != nil {
					b.Fatal(err)
				}
				ids[k] = it.ID
			}
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					for _, id := range ids {
						if err := tx.Model(&item{}).Where("id = ?", id).Update("name", fmt.Sprint(i)).Error; err != nil {
							return err
						}
					}

					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(mode+"/update/parallel-one-tailnet", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, fenced)
			if db.Dialector.Name() != "postgres" {
				b.Skip("SQLite serialises writers; hot-row contention is a PostgreSQL question")
			}
			var mu sync.Mutex
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				it := item{Name: "p", OwnerID: ownerID}
				mu.Lock()
				err := db.WithContext(ctx).Create(&it).Error
				mu.Unlock()
				if err != nil {
					b.Error(err)

					return
				}
				for i := 0; pb.Next(); i++ {
					if err := db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).
						Update("name", fmt.Sprint(i)).Error; err != nil {
						b.Error(err)

						return
					}
				}
			})
		})
	}
}

// BenchmarkFenceOwnerMoveUnderLoad measures the latency of the ownership
// move (UPDATE tailnets ... owner_epoch + 1) while writers of that tailnet
// hold FOR SHARE on the row. Each iteration is one move; the writers' fence
// follows the new epoch so they keep writing.
func BenchmarkFenceOwnerMoveUnderLoad(b *testing.B) {
	for _, writers := range []int{0, 8, 32} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			db, _, ownerID := benchDB(b, true)
			if db.Dialector.Name() != "postgres" {
				b.Skip("FOR SHARE is PostgreSQL only")
			}

			var (
				epoch atomic.Int64
				stop  atomic.Bool
				wrote atomic.Int64
				wg    sync.WaitGroup
			)
			epoch.Store(testEpoch)

			for w := range writers {
				it := item{Name: "w", OwnerID: ownerID}
				if err := db.WithContext(ctxFor(b, idA)).Create(&it).Error; err != nil {
					b.Fatal(err)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; !stop.Load(); i++ {
						ctx := fencedCtx(b, idA, testCluster, tailnet.Epoch(epoch.Load()))
						err := db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).
							Update("name", fmt.Sprintf("%d-%d", w, i)).Error
						if err == nil {
							wrote.Add(1)
						}
						// ErrFenced right after a move is the fence working.
					}
				}()
			}
			time.Sleep(200 * time.Millisecond) // writers running

			b.ResetTimer()
			start := time.Now()
			for b.Loop() {
				next := epoch.Load() + 1
				if err := db.Exec(`UPDATE tailnets SET owner_epoch = ? WHERE id = ?`, next, idA).Error; err != nil {
					b.Fatal(err)
				}
				epoch.Store(next)
			}
			elapsed := time.Since(start)
			b.StopTimer()
			stop.Store(true)
			wg.Wait()
			b.ReportMetric(float64(wrote.Load())/elapsed.Seconds(), "writes/s")
		})
	}
}
