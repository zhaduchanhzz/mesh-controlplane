package tenantscope

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
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

// Fence modes of the benchmarks: no fence; the P10 ownership fence; the P10
// fence plus the P11b cluster lease check (one more read, of cluster_leases,
// per write).
const (
	modeNoFence = "nofence"
	modeFence   = "fence"
	modeLease   = "fence+lease"
)

func benchDB(b *testing.B, mode string) (*gorm.DB, context.Context, uint64) {
	b.Helper()

	db := openDB(b)
	if mode == modeNoFence {
		withoutFence(b, db)
	}

	sqlDB, err := db.DB()
	if err == nil && db.Name() == "postgres" {
		sqlDB.SetMaxOpenConns(64)
		sqlDB.SetMaxIdleConns(64)
	}

	ctx := ctxFor(b, idA)

	if mode == modeLease {
		ddl := lease.SQLiteDDL
		if db.Name() == "postgres" {
			ddl = lease.PostgresDDL
		}

		err := db.Exec(ddl).Error
		if err != nil {
			b.Fatal(err)
		}

		l, err := lease.NewStore(db).Acquire(context.Background(), testCluster, "bench", time.Hour)
		if err != nil {
			b.Fatal(err)
		}

		ctx = leaseCtx(b, l.Holder, l.Epoch)
	}

	var it item

	err = db.WithContext(ctx).Order("id").First(&it).Error
	if err != nil {
		b.Fatal(err)
	}

	return db, ctx, it.OwnerID
}

// update writes one row per parallel worker, so the only shared row between
// workers is the tailnets row read by the fence (no row-lock contention on
// the item itself).
func BenchmarkFenceWrite(b *testing.B) {
	for _, mode := range []string{modeNoFence, modeFence, modeLease} {
		b.Run(mode+"/update/serial", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, mode)

			it := item{Name: "bench", OwnerID: ownerID}

			err := db.WithContext(ctx).Create(&it).Error
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()

			for i := 0; b.Loop(); i++ {
				err := db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).
					Update("name", strconv.Itoa(i)).Error
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(mode+"/create/serial", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, mode)
			b.ResetTimer()

			for b.Loop() {
				err := db.WithContext(ctx).Create(&item{Name: "c", OwnerID: ownerID}).Error
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(mode+"/tx10/serial", func(b *testing.B) {
			db, ctx, ownerID := benchDB(b, mode)

			ids := make([]uint64, 10)
			for k := range ids {
				it := item{Name: "t", OwnerID: ownerID}

				err := db.WithContext(ctx).Create(&it).Error
				if err != nil {
					b.Fatal(err)
				}

				ids[k] = it.ID
			}

			b.ResetTimer()

			for i := 0; b.Loop(); i++ {
				err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					for _, id := range ids {
						err := tx.Model(&item{}).Where("id = ?", id).Update("name", strconv.Itoa(i)).Error
						if err != nil {
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
			db, ctx, ownerID := benchDB(b, mode)
			if db.Name() != "postgres" {
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
					err := db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).
						Update("name", strconv.Itoa(i)).Error
					if err != nil {
						b.Error(err)

						return
					}
				}
			})
		})
	}
}

// writeLoad runs n writers updating their own row of tailnet A as fast as
// they can, each with the fence of the current epoch. stop ends them and
// returns the number of writes that committed.
func writeLoad(tb testing.TB, db *gorm.DB, ownerID uint64, n int, epoch *atomic.Uint64) func() int64 {
	tb.Helper()

	var (
		done  atomic.Bool
		wrote atomic.Int64
		wg    sync.WaitGroup
	)

	for w := range n {
		it := item{Name: "w", OwnerID: ownerID}

		err := db.WithContext(ctxFor(tb, idA)).Create(&it).Error
		if err != nil {
			tb.Fatal(err)
		}

		wg.Go(func() {
			for i := 0; !done.Load(); i++ {
				ctx := fencedCtx(tb, idA, testCluster, tailnet.Epoch(epoch.Load()))

				err := db.WithContext(ctx).Model(&item{}).Where("id = ?", it.ID).
					Update("name", fmt.Sprintf("%d-%d", w, i)).Error
				if err == nil {
					wrote.Add(1)
				}
				// ErrFenced right after a move is the fence working.
			}
		})
	}

	return func() int64 {
		done.Store(true)
		wg.Wait()

		return wrote.Load()
	}
}

// moveEpoch is an ownership change as production does it: LockOwnership,
// then the UPDATE of the tailnets row, in one transaction.
func moveEpoch(db *gorm.DB, next uint64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		err := LockOwnership(tx, idA)
		if err != nil {
			return err
		}

		return tx.Exec(`UPDATE tailnets SET owner_epoch = ? WHERE id = ?`, next, idA).Error
	})
}

// BenchmarkFenceOwnerMoveUnderLoad measures the latency of an ownership
// change while writers of that tailnet run back to back. Before P10c (plain
// UPDATE against FOR SHARE) one move took 26-136 s with 8 writers.
func BenchmarkFenceOwnerMoveUnderLoad(b *testing.B) {
	for _, writers := range []int{0, 8, 32} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			db, _, ownerID := benchDB(b, modeFence)
			if db.Name() != "postgres" {
				b.Skip("FOR SHARE is PostgreSQL only")
			}

			var epoch atomic.Uint64
			epoch.Store(testEpoch)
			stop := writeLoad(b, db, ownerID, writers, &epoch)

			time.Sleep(200 * time.Millisecond) //nolint:forbidigo // lets the writers reach steady load before timing

			start := time.Now()

			for b.Loop() {
				next := epoch.Load() + 1

				err := moveEpoch(db, next)
				if err != nil {
					b.Fatal(err)
				}

				epoch.Store(next)
			}

			elapsed := time.Since(start)

			b.StopTimer()
			b.ReportMetric(float64(stop())/elapsed.Seconds(), "writes/s")
		})
	}
}
