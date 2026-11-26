package placement

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P09, S04 (06-tests-benchmarks.md:12), I04: tailnets created concurrently
// (CreateTailnet and CreateTailnetOn) while readers poll the registry: no
// reader ever sees a tailnet without its policy row, and a PENDING tailnet's
// policy is always the deny-all one. The tailnet row and its policy are one
// transaction, so on PostgreSQL (READ COMMITTED) a reader sees both or
// neither.
func TestS04TailnetNeverVisibleWithoutDenyPolicy(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			for _, id := range []string{"s1", "s2"} {
				addCluster(t, r, id, ClassShared, ClusterActive, 100000)
			}

			const (
				creators  = 6
				perWorker = 12
				readers   = 3
			)

			var (
				stop      atomic.Bool
				samples   atomic.Int64
				noPolicy  atomic.Int64
				notDeny   atomic.Int64
				readErrs  atomic.Int64
				wgReaders sync.WaitGroup
			)

			for range readers {
				wgReaders.Go(func() {
					for !stop.Load() {
						var n, m int64

						err := gdb.Raw(`SELECT count(*) FROM tailnets t WHERE t.deleted_at IS NULL AND NOT EXISTS (
							SELECT 1 FROM policies p WHERE p.tailnet_id = t.id AND p.deleted_at IS NULL)`).Scan(&n).Error
						if err == nil {
							err = gdb.Raw(`SELECT count(*) FROM tailnets t JOIN policies p ON p.tailnet_id = t.id
								WHERE t.status = 'PENDING' AND t.deleted_at IS NULL AND p.deleted_at IS NULL AND p.data <> ?`,
								DenyAllPolicy).Scan(&m).Error
						}

						if err != nil {
							readErrs.Add(1)

							continue
						}

						samples.Add(1)
						noPolicy.Add(n)
						notDeny.Add(m)
					}
				})
			}

			var (
				wg      sync.WaitGroup
				created atomic.Int64
				errs    = make(chan error, creators*perWorker)
			)

			for w := range creators {
				wg.Go(func() {
					for i := range perWorker {
						slug := fmt.Sprintf("s04-%d-%d", w, i)

						var err error
						if i%2 == 0 {
							_, err = r.CreateTailnet(ctx, testOp, slug, ClassShared)
						} else {
							_, err = r.CreateTailnetOn(ctx, testOp, slug, []string{"s1", "s2"}[w%2])
						}

						if err != nil {
							errs <- fmt.Errorf("%s: %w", slug, err)

							continue
						}

						created.Add(1)
					}
				})
			}

			wg.Wait()
			stop.Store(true)
			wgReaders.Wait()
			close(errs)

			for err := range errs {
				t.Error(err)
			}

			assert.EqualValues(t, creators*perWorker, created.Load())
			assert.Positive(t, samples.Load(), "the readers sampled while tailnets were created")
			assert.Zero(t, readErrs.Load(), "reader queries")
			assert.Zero(t, noPolicy.Load(), "a tailnet was visible without its policy")
			assert.Zero(t, notDeny.Load(), "a PENDING tailnet had a policy other than deny-all")

			var final int64
			require.NoError(t, gdb.Raw(`SELECT count(*) FROM tailnets t JOIN policies p ON p.tailnet_id = t.id
				WHERE t.slug LIKE 's04-%' AND p.data = ?`, DenyAllPolicy).Scan(&final).Error)
			assert.EqualValues(t, creators*perWorker, final, "every created tailnet has exactly its deny-all policy")
			t.Logf("S04 %s: %d tailnets, %d reader samples, 0 without policy", b.name, created.Load(), samples.Load())
		})
	}
}
