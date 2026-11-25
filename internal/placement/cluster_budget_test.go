package placement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

// TestQuotaSharedClusterBudget (P10d): the tailnets of a shared cluster
// together cannot enroll past the cluster's max_devices, although each is far
// below its own 5000 limit. Before P10d, max_devices was only checked when a
// tailnet was placed. A dedicated cluster keeps its tailnet limit only.
func TestQuotaSharedClusterBudget(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "s1", ClassShared, ClusterActive, 10)
			addCluster(t, r, "d1", ClassDedicated, ClusterActive, 3)

			a := activeTailnet(t, r, "a", ClassShared)
			bt := activeTailnet(t, r, "b", ClassShared)
			ded := activeTailnet(t, r, "ded", ClassDedicated)

			addNodes(t, gdb, a.ID, 5)
			addNodes(t, gdb, bt.ID, 1)

			q, err := r.EnrollmentAllowed(ctx, tailnet.MustParseID(a.ID))
			require.NoError(t, err, "6 of 10 devices on the cluster")
			assert.True(t, q.Allowed)
			assert.EqualValues(t, 6, q.ClusterDevices)
			assert.EqualValues(t, 10, q.ClusterLimit)
			assert.False(t, q.Warn, "the tailnet itself is far from its limit")
			assert.False(t, q.ClusterWarn, "the 7th device is 70%% of the cluster")

			addNodes(t, gdb, bt.ID, 1) // 7 on the cluster
			q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(bt.ID))
			require.NoError(t, err)
			assert.True(t, q.ClusterWarn, "the 8th device reaches 80%% of the cluster")

			addNodes(t, gdb, a.ID, 3) // 10 on the cluster

			for _, id := range []string{a.ID, bt.ID} {
				q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(id))
				require.ErrorIs(t, err, ErrClusterFull)
				require.ErrorIs(t, err, ErrQuotaExceeded, "callers that refuse on a quota refuse on it")
				assert.False(t, q.Allowed)
				assert.EqualValues(t, 10, q.ClusterDevices)
			}

			// Deleted devices free the budget.
			require.NoError(t, gdb.Exec(`UPDATE nodes SET deleted_at = ? WHERE id IN
				(SELECT id FROM nodes WHERE tailnet_id = ? ORDER BY id LIMIT 1)`, time.Now().UTC(), a.ID).Error)
			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(a.ID))
			require.NoError(t, err, "a soft-deleted device does not count")

			// Dedicated: only the tailnet (= cluster) limit applies.
			addNodes(t, gdb, ded.ID, 2)
			q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(ded.ID))
			require.NoError(t, err)
			assert.Zero(t, q.ClusterLimit, "no separate cluster budget on a dedicated cluster")
			addNodes(t, gdb, ded.ID, 1)
			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(ded.ID))
			require.ErrorIs(t, err, ErrQuotaExceeded)
			require.NotErrorIs(t, err, ErrClusterFull)
		})
	}
}

// TestQuotaSharedClusterBudgetConcurrent (P10d): enrollments into two
// tailnets of one shared cluster, each checking the budget in the
// transaction that inserts its device, stop exactly at max_devices.
func TestQuotaSharedClusterBudgetConcurrent(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "s1", ClassShared, ClusterActive, 5)

			ids := []string{activeTailnet(t, r, "a", ClassShared).ID, activeTailnet(t, r, "b", ClassShared).ID}

			const attempts = 20

			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				accepted int
				refused  int
				other    []error
			)

			for i := range attempts {
				wg.Go(func() {
					id := ids[i%2]
					err := gdb.Transaction(func(tx *gorm.DB) error {
						_, err := r.EnrollmentAllowedTx(tx, tailnet.MustParseID(id))
						if err != nil {
							return err
						}

						now := time.Now().UTC()

						return tx.Exec(`INSERT INTO nodes (tailnet_id, hostname, created_at, updated_at) VALUES (?, ?, ?, ?)`,
							id, fmt.Sprintf("c-%d", i), now, now).Error
					})

					mu.Lock()
					defer mu.Unlock()

					switch {
					case err == nil:
						accepted++
					case errors.Is(err, ErrClusterFull):
						refused++
					default:
						other = append(other, err)
					}
				})
			}

			wg.Wait()
			require.Empty(t, other)
			assert.Equal(t, 5, accepted)
			assert.Equal(t, attempts-5, refused)
			assert.EqualValues(t, 5, count(t, gdb, `SELECT count(*) FROM nodes WHERE deleted_at IS NULL`))
		})
	}
}
