package placement

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

var ctx = context.Background()

// TestCreateTailnetPendingDenyOwner: a new tailnet is PENDING, owned by the
// chosen cluster at epoch 1, with the deny-all policy, in one transaction
// with its audit row (PLAN §5, AGENTS rule 8, I04).
func TestCreateTailnetPendingDenyOwner(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)

			tn, err := r.CreateTailnet(ctx, testOp, "acme", ClassShared)
			require.NoError(t, err)
			assert.Equal(t, types.TailnetPending, tn.Status)
			assert.Equal(t, "c1", tn.OwnerClusterID)
			assert.EqualValues(t, 1, tn.OwnerEpoch)

			var data string
			require.NoError(t, gdb.Raw(`SELECT data FROM policies WHERE tailnet_id = ?`, tn.ID).Scan(&data).Error)
			assert.JSONEq(t, `{"grants":[]}`, data)

			assert.EqualValues(t, 1, count(t, gdb,
				`SELECT count(*) FROM platform_audit WHERE action = 'CreateTailnet' AND target = ? AND outcome = 'ok' AND actor = ?`,
				tn.ID, testOp.Actor))

			_, err = r.CreateTailnet(ctx, testOp, "acme", ClassShared)
			require.ErrorIs(t, err, ErrConflict, "slug is unique")

			for _, bad := range []string{"", "Acme", "-x", "a b", "a/b"} {
				_, err = r.CreateTailnet(ctx, testOp, bad, ClassShared)
				require.ErrorIs(t, err, ErrInvalid, "slug %q", bad)
			}

			_, err = r.CreateTailnet(ctx, testOp, "other", Class("gold"))
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

// TestCreateTailnetFailureLeavesNothing (RED, I04): a failure at any step of
// CreateTailnet leaves no tailnet, no policy, no stored idempotent result and
// no successful audit; no tailnet ever exists without a deny policy or an
// owner.
func TestCreateTailnetFailureLeavesNothing(t *testing.T) {
	steps := []string{"place", "tailnet", "policy", "audit", "idempotency", "commit"}

	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)

			injected := errors.New("injected failure")

			for i, s := range steps {
				r.failAt = func(step string) error {
					if step == s {
						return injected
					}

					return nil
				}

				_, err := r.CreateTailnet(ctx, keyOp("k-"+s, "h"), slugN(i), ClassShared)
				require.ErrorIs(t, err, injected, "step %s", s)

				assert.Zero(t, count(t, gdb, `SELECT count(*) FROM tailnets WHERE slug = ?`, slugN(i)), "step %s: tailnet", s)
				assert.Zero(t, count(t, gdb, `SELECT count(*) FROM platform_idempotency WHERE idempotency_key = ?`, "k-"+s), "step %s: idempotency", s)
			}

			r.failAt = nil

			assert.Zero(t, count(t, gdb, `SELECT count(*) FROM policies`), "no orphan policy")
			assert.Zero(t, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'CreateTailnet' AND outcome = 'ok'`))
			assert.EqualValues(t, len(steps), count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'CreateTailnet' AND outcome = 'error'`),
				"each failed call is audited")

			// The same key works once the failure is gone.
			tn, err := r.CreateTailnet(ctx, keyOp("k-policy", "h"), "after", ClassShared)
			require.NoError(t, err)

			assertTailnetsComplete(t, r, tn.ID)
		})
	}
}

// assertTailnetsComplete: every tailnet has an owner and a policy, and want
// is among them.
func assertTailnetsComplete(t *testing.T, r *Registry, want string) {
	t.Helper()

	gdb := r.db
	assert.Zero(t, count(t, gdb, `SELECT count(*) FROM tailnets t WHERE t.owner_cluster_id = ''
		OR NOT EXISTS (SELECT 1 FROM policies p WHERE p.tailnet_id = t.id AND p.data IS NOT NULL)`))
	assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM tailnets WHERE id = ?`, want))
}

// TestPlacement: only ACTIVE clusters of the class with capacity are chosen,
// the least loaded first; a dedicated cluster takes one tailnet.
func TestPlacement(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			_, err := r.CreateTailnet(ctx, testOp, "none", ClassShared)
			require.ErrorIs(t, err, ErrNoCapacity, "no cluster at all")

			addCluster(t, r, "s1", ClassShared, ClusterActive, 10)
			addCluster(t, r, "s2", ClassShared, ClusterActive, 10)
			addCluster(t, r, "s3", ClassShared, ClusterDraining, 1000)
			addCluster(t, r, "s4", ClassShared, "", 1000) // DISABLED by default
			addCluster(t, r, "d1", ClassDedicated, ClusterActive, 10000)

			a, err := r.CreateTailnet(ctx, testOp, "a", ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s1", a.OwnerClusterID, "tie: lowest id")
			addNodes(t, gdb, a.ID, 6)

			c, err := r.PlaceTailnet(ctx, ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s2", c.ID, "least loaded")

			bt, err := r.CreateTailnet(ctx, testOp, "b", ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s2", bt.OwnerClusterID)
			addNodes(t, gdb, bt.ID, 10) // s2 full

			c, err = r.PlaceTailnet(ctx, ClassShared)
			require.NoError(t, err)
			assert.Equal(t, "s1", c.ID)

			addNodes(t, gdb, a.ID, 4) // s1 full
			_, err = r.CreateTailnet(ctx, testOp, "c", ClassShared)
			require.ErrorIs(t, err, ErrNoCapacity, "draining/disabled clusters are never chosen")

			d, err := r.CreateTailnet(ctx, testOp, "d", ClassDedicated)
			require.NoError(t, err)
			assert.Equal(t, "d1", d.OwnerClusterID)

			_, err = r.CreateTailnet(ctx, testOp, "e", ClassDedicated)
			require.ErrorIs(t, err, ErrNoCapacity, "one tailnet per dedicated cluster")

			list, err := r.ListClusters(ctx)
			require.NoError(t, err)
			require.Len(t, list, 5)

			byID := map[string]ClusterUsage{}
			for _, u := range list {
				byID[u.ID] = u
			}

			assert.EqualValues(t, 10, byID["s1"].Devices)
			assert.EqualValues(t, 1, byID["s1"].Tailnets)
			assert.EqualValues(t, 10, byID["s2"].Devices)
			assert.EqualValues(t, 1, byID["d1"].Tailnets)
			assert.Zero(t, byID["s3"].Devices)
		})
	}
}

// TestPlacementDedicatedConcurrent: concurrent CreateTailnet on dedicated
// clusters never puts two tailnets on one cluster.
func TestPlacementDedicatedConcurrent(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})

			for _, id := range []string{"d1", "d2", "d3"} {
				addCluster(t, r, id, ClassDedicated, ClusterActive, 10000)
			}

			const n = 8

			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				ok   int
				errs []error
			)

			for i := range n {
				wg.Go(func() {
					_, err := r.CreateTailnet(ctx, testOp, slugN(i), ClassDedicated)

					mu.Lock()
					defer mu.Unlock()

					if err == nil {
						ok++
					} else {
						errs = append(errs, err)
					}
				})
			}

			wg.Wait()

			assert.Equal(t, 3, ok)

			for _, err := range errs {
				require.ErrorIs(t, err, ErrNoCapacity)
			}

			assert.Zero(t, count(t, gdb, `SELECT count(*) FROM (SELECT owner_cluster_id FROM tailnets
				GROUP BY owner_cluster_id HAVING count(*) > 1) d`), "no dedicated cluster owns two tailnets")
		})
	}
}

// TestMoveOwnerStaleEpochRejected (RED, I12/S17): a CAS with an old epoch or
// a cluster that no longer owns the tailnet changes nothing. ADR 0010: the
// move is a request (moving_to); owner and epoch change at the hand-over.
func TestMoveOwnerStaleEpochRejected(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 100)

			tn := activeTailnet(t, r, "acme", ClassShared)

			moved, err := r.MoveOwner(ctx, testOp, tn.ID, "c1", 1, "c2")
			require.NoError(t, err)
			assert.Equal(t, "c1", moved.OwnerClusterID, "the owner changes at the hand-over")
			assert.EqualValues(t, 1, moved.OwnerEpoch)
			require.NotNil(t, moved.MovingToClusterID)
			assert.Equal(t, "c2", *moved.MovingToClusterID)

			for _, tc := range []struct {
				from  string
				epoch uint64
			}{{"c2", 1}, {"c1", 2}, {"c2", 2}} {
				_, err = r.MoveOwner(ctx, testOp, tn.ID, tc.from, tc.epoch, "c1")
				require.ErrorIs(t, err, ErrStaleEpoch, "from %s@%d", tc.from, tc.epoch)
			}

			_, err = r.MoveOwner(ctx, testOp, tn.ID, "c1", 1, "c2")
			require.ErrorIs(t, err, ErrConflict, "one move pending at a time")

			got, err := r.GetTailnet(ctx, tn.ID)
			require.NoError(t, err)
			assert.Equal(t, "c1", got.OwnerClusterID)
			assert.EqualValues(t, 1, got.OwnerEpoch)

			_, err = r.MoveOwner(ctx, testOp, tn.ID, "c1", 1, "nope")
			require.ErrorIs(t, err, ErrNotFound, "unregistered target")

			_, err = r.MoveOwner(ctx, testOp, "00000000-0000-4000-8000-000000000000", "c1", 1, "c2")
			require.ErrorIs(t, err, ErrNotFound)

			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'MoveOwner' AND outcome = 'ok'`))
			assert.EqualValues(t, 6, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE action = 'MoveOwner' AND outcome = 'error'`))
		})
	}
}

// TestMoveOwnerConcurrentExactlyOneWins (RED, I12): N operators CAS the same
// (owner, epoch) at once; exactly one request is recorded (ADR 0010), the
// others conflict; owner and epoch are unchanged until the hand-over.
func TestMoveOwnerConcurrentExactlyOneWins(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c3", ClassShared, ClusterActive, 100)

			tn := activeTailnet(t, r, "acme", ClassShared)

			const n = 24

			var (
				wg      sync.WaitGroup
				start   = make(chan struct{})
				mu      sync.Mutex
				winners []types.Tailnet
				others  []error
			)

			for i := range n {
				to := []string{"c2", "c3"}[i%2]

				wg.Go(func() {
					<-start

					got, err := r.MoveOwner(ctx, testOp, tn.ID, "c1", 1, to)

					mu.Lock()
					defer mu.Unlock()

					if err == nil {
						winners = append(winners, got)
					} else {
						others = append(others, err)
					}
				})
			}

			close(start)
			wg.Wait()

			require.Len(t, winners, 1, "exactly one CAS wins")

			for _, err := range others {
				require.ErrorIs(t, err, ErrConflict)
			}

			got, err := r.GetTailnet(ctx, tn.ID)
			require.NoError(t, err)
			assert.EqualValues(t, 1, got.OwnerEpoch)
			assert.Equal(t, "c1", got.OwnerClusterID)
			require.NotNil(t, got.MovingToClusterID)
			assert.Equal(t, *winners[0].MovingToClusterID, *got.MovingToClusterID)
		})
	}
}

// TestEpochNeverDecreases (RED, I12/I14): no API, UpdateClusterState
// included, and no raw UPDATE can lower owner_epoch or a cluster revision,
// or change the owner without raising the epoch.
func TestEpochNeverDecreases(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			c1 := addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 100)

			tn := activeTailnet(t, r, "acme", ClassShared)
			_, err := r.MoveOwner(ctx, testOp, tn.ID, "c1", 1, "c2")
			require.NoError(t, err)
			// The hand-over the owner cluster makes (ADR 0010).
			require.NoError(t, gdb.Exec(`UPDATE tailnets SET owner_cluster_id = 'c2', owner_epoch = 2,
				moving_to_cluster_id = NULL WHERE id = ?`, tn.ID).Error)

			// UpdateClusterState: revision grows, owner epochs are untouched.
			rev := c1.Revision

			for _, s := range []ClusterState{ClusterDraining, ClusterDisabled, ClusterActive} {
				c, err := r.UpdateClusterState(ctx, testOp, "c1", s, 0)
				require.NoError(t, err)
				assert.Greater(t, c.Revision, rev)
				rev = c.Revision
			}

			_, err = r.UpdateClusterState(ctx, testOp, "c1", ClusterDraining, rev-1)
			require.ErrorIs(t, err, ErrStaleRevision, "expected revision in the past")

			_, err = r.UpdateClusterState(ctx, testOp, "c1", ClusterState("GONE"), 0)
			require.ErrorIs(t, err, ErrInvalid)

			c, err := r.UpdateClusterState(ctx, testOp, "c1", ClusterDraining, rev)
			require.NoError(t, err)
			assert.Equal(t, rev+1, c.Revision)

			got, err := r.GetTailnet(ctx, tn.ID)
			require.NoError(t, err)
			assert.EqualValues(t, 2, got.OwnerEpoch, "cluster state never touches owner_epoch")

			// The database refuses any path that lowers them.
			require.Error(t, gdb.Exec(`UPDATE tailnets SET owner_epoch = 1 WHERE id = ?`, tn.ID).Error, "epoch decrease")
			require.Error(t, gdb.Exec(`UPDATE tailnets SET owner_cluster_id = 'c1' WHERE id = ?`, tn.ID).Error, "owner change without epoch")
			require.Error(t, gdb.Exec(`UPDATE tailnets SET owner_cluster_id = 'c1', owner_epoch = 2 WHERE id = ?`, tn.ID).Error, "owner change, same epoch")
			require.Error(t, gdb.Exec(`UPDATE clusters SET revision = revision - 1 WHERE id = 'c1'`).Error, "revision decrease")
			require.NoError(t, gdb.Exec(`UPDATE tailnets SET owner_epoch = owner_epoch + 1 WHERE id = ?`, tn.ID).Error, "increase allowed")

			got, err = r.GetTailnet(ctx, tn.ID)
			require.NoError(t, err)
			assert.EqualValues(t, 3, got.OwnerEpoch)
			assert.Equal(t, "c2", got.OwnerClusterID)

			// Updating cluster fields also only raises the revision.
			maxD := int64(200)
			c2, err := r.UpdateCluster(ctx, testOp, "c1", ClusterPatch{MaxDevices: &maxD})
			require.NoError(t, err)
			assert.Equal(t, rev+2, c2.Revision)
			assert.EqualValues(t, 200, c2.MaxDevices)
		})
	}
}

// TestQuotaSharedHardLimit (RED, I10): a tailnet at 5000 devices on a shared
// cluster may not enroll more; another tailnet on the same cluster is not
// affected; 80% warns; dedicated clusters use their max_devices; anything
// unknown fails closed.
func TestQuotaSharedHardLimit(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			require.EqualValues(t, 5000, r.Config().SharedTenantLimit)

			addCluster(t, r, "s1", ClassShared, ClusterActive, 100000)
			addCluster(t, r, "d1", ClassDedicated, ClusterActive, 7)

			full := activeTailnet(t, r, "full", ClassShared)
			addNodes(t, gdb, full.ID, 5000)

			neighbour := activeTailnet(t, r, "neighbour", ClassShared)
			addNodes(t, gdb, neighbour.ID, 10)

			warn := activeTailnet(t, r, "warn", ClassShared)
			addNodes(t, gdb, warn.ID, 3999)

			below := activeTailnet(t, r, "below", ClassShared)
			addNodes(t, gdb, below.ID, 3998)

			q, err := r.EnrollmentAllowed(ctx, tailnet.MustParseID(full.ID))
			require.ErrorIs(t, err, ErrQuotaExceeded)
			assert.False(t, q.Allowed)
			assert.EqualValues(t, 5000, q.Devices)
			assert.EqualValues(t, 5000, q.Limit)

			q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(neighbour.ID))
			require.NoError(t, err, "a neighbour on the same cluster is unaffected")
			assert.True(t, q.Allowed)
			assert.False(t, q.Warn)
			assert.EqualValues(t, 10, q.Devices)

			q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(warn.ID))
			require.NoError(t, err)
			assert.True(t, q.Allowed)
			assert.True(t, q.Warn, "the 4000th device reaches 80%%")

			q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(below.ID))
			require.NoError(t, err)
			assert.False(t, q.Warn)

			// Deleted devices do not count.
			require.NoError(t, gdb.Exec(`UPDATE nodes SET deleted_at = updated_at WHERE id IN
				(SELECT id FROM nodes WHERE tailnet_id = ? ORDER BY id LIMIT 1)`, full.ID).Error)
			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(full.ID))
			require.NoError(t, err)

			// Dedicated: the cluster's max_devices.
			ded := activeTailnet(t, r, "ded", ClassDedicated)
			addNodes(t, gdb, ded.ID, 6)
			q, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(ded.ID))
			require.NoError(t, err)
			assert.EqualValues(t, 7, q.Limit)
			addNodes(t, gdb, ded.ID, 1)
			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(ded.ID))
			require.ErrorIs(t, err, ErrQuotaExceeded)

			// Fail closed.
			pending, err := r.CreateTailnet(ctx, testOp, "pending", ClassShared)
			require.NoError(t, err)
			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(pending.ID))
			require.ErrorIs(t, err, ErrTailnetNotActive)

			legacy := rawTailnet(t, gdb, "local", types.TailnetActive)
			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(legacy))
			require.ErrorIs(t, err, ErrUnknownCluster)

			_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID("00000000-0000-4000-8000-000000000000"))
			require.ErrorIs(t, err, ErrNotFound)

			_, err = r.EnrollmentAllowed(ctx, tailnet.ID{})
			require.ErrorIs(t, err, ErrNotFound)

			// Inside a caller's transaction (the registration path).
			err = gdb.Transaction(func(tx *gorm.DB) error {
				_, err := r.EnrollmentAllowedTx(tx, tailnet.MustParseID(neighbour.ID))

				return err
			})
			require.NoError(t, err)
		})
	}
}

// TestQuotaConfigurable: the shared limit is configuration.
func TestQuotaConfigurable(t *testing.T) {
	gdb := backends()[0].open(t)
	r := newRegistry(t, gdb, Config{SharedTenantLimit: 3})
	addCluster(t, r, "s1", ClassShared, ClusterActive, 100)

	tn := activeTailnet(t, r, "acme", ClassShared)
	addNodes(t, gdb, tn.ID, 2)
	q, err := r.EnrollmentAllowed(ctx, tailnet.MustParseID(tn.ID))
	require.NoError(t, err)
	assert.True(t, q.Warn)
	addNodes(t, gdb, tn.ID, 1)
	_, err = r.EnrollmentAllowed(ctx, tailnet.MustParseID(tn.ID))
	require.ErrorIs(t, err, ErrQuotaExceeded)

	_, err = New(gdb, Config{WarnPercent: 101})
	require.ErrorIs(t, err, ErrInvalid)
}

// TestIdempotency: the same key and request returns the first result with no
// second effect; the same key with another request is refused; concurrent
// retries create one tailnet.
func TestIdempotency(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 100)

			first, err := r.CreateTailnet(ctx, keyOp("k1", "h1"), "acme", ClassShared)
			require.NoError(t, err)
			again, err := r.CreateTailnet(ctx, keyOp("k1", "h1"), "acme", ClassShared)
			require.NoError(t, err)
			assert.Equal(t, first.ID, again.ID)
			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM tailnets`))

			_, err = r.CreateTailnet(ctx, keyOp("k1", "h2"), "other", ClassShared)
			require.ErrorIs(t, err, ErrIdempotencyConflict)
			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM tailnets`))

			// Another operator's key space is separate.
			_, err = r.CreateTailnet(ctx, Op{Actor: "spiffe://mesh/operator/other", IdempotencyKey: "k1", RequestHash: "h3"}, "other", ClassShared)
			require.NoError(t, err)

			// A replayed CAS has no second effect.
			_, err = r.ActivateTailnet(ctx, testOp, first.ID, 1)
			require.NoError(t, err)
			m1, err := r.MoveOwner(ctx, keyOp("move", "hm"), first.ID, "c1", 1, "c2")
			require.NoError(t, err)
			m2, err := r.MoveOwner(ctx, keyOp("move", "hm"), first.ID, "c1", 1, "c2")
			require.NoError(t, err)
			assert.Equal(t, m1.OwnerEpoch, m2.OwnerEpoch)

			got, err := r.GetTailnet(ctx, first.ID)
			require.NoError(t, err)
			assert.EqualValues(t, 1, got.OwnerEpoch, "a move request; the hand-over raises the epoch")
			require.NotNil(t, got.MovingToClusterID)

			assert.EqualValues(t, 2, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE outcome = 'replay'`))
			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM platform_audit WHERE outcome = 'error' AND action = 'CreateTailnet'`))

			// Concurrent retries of one request.
			var wg sync.WaitGroup

			ids := make([]string, 8)
			errs := make([]error, 8)

			for i := range ids {
				wg.Go(func() {
					tn, err := r.CreateTailnet(ctx, keyOp("k-conc", "hc"), "conc", ClassShared)
					ids[i], errs[i] = tn.ID, err
				})
			}

			wg.Wait()

			for i := range ids {
				require.NoError(t, errs[i])
				assert.Equal(t, ids[0], ids[i])
			}

			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM tailnets WHERE slug = 'conc'`))
		})
	}
}

// TestMutationsNeedActor: an unattributed mutation is refused before it
// writes anything (every mutation is audited).
func TestMutationsNeedActor(t *testing.T) {
	gdb := backends()[0].open(t)
	r := newRegistry(t, gdb, Config{})

	_, err := r.CreateCluster(ctx, Op{}, ClusterSpec{ID: "c1", EndpointURL: "https://c1", Class: ClassShared, MaxDevices: 1})
	require.ErrorIs(t, err, ErrInvalid)

	_, err = r.CreateCluster(ctx, Op{Actor: "a", IdempotencyKey: "k"}, ClusterSpec{ID: "c1", EndpointURL: "https://c1", Class: ClassShared, MaxDevices: 1})
	require.ErrorIs(t, err, ErrInvalid, "a key without a request hash")

	assert.Zero(t, count(t, gdb, `SELECT count(*) FROM clusters`))
}

// TestTailnetLifecycle: Activate/Suspend check the expected epoch and the
// current status.
func TestTailnetLifecycle(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)

			tn, err := r.CreateTailnet(ctx, testOp, "acme", ClassShared)
			require.NoError(t, err)

			_, err = r.ActivateTailnet(ctx, testOp, tn.ID, 2)
			require.ErrorIs(t, err, ErrStaleEpoch)

			got, err := r.ActivateTailnet(ctx, testOp, tn.ID, 1)
			require.NoError(t, err)
			assert.Equal(t, types.TailnetActive, got.Status)

			_, err = r.ActivateTailnet(ctx, testOp, tn.ID, 1)
			require.ErrorIs(t, err, ErrInvalidTransition)

			got, err = r.SuspendTailnet(ctx, testOp, tn.ID, 1)
			require.NoError(t, err)
			assert.Equal(t, types.TailnetSuspended, got.Status)

			got, err = r.ActivateTailnet(ctx, testOp, tn.ID, 1)
			require.NoError(t, err)
			assert.Equal(t, types.TailnetActive, got.Status)

			// A tailnet without a policy row is never activated.
			legacy := rawTailnet(t, gdb, "c1", types.TailnetPending)
			_, err = r.ActivateTailnet(ctx, testOp, legacy, 1)
			require.ErrorIs(t, err, ErrInvalidTransition)

			_, err = r.GetTailnet(ctx, "00000000-0000-4000-8000-000000000000")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

// TestDeleteCluster: a cluster referenced by a tailnet is never deleted.
func TestDeleteCluster(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterDisabled, 100)

			_, err := r.CreateTailnet(ctx, testOp, "acme", ClassShared)
			require.NoError(t, err)

			require.ErrorIs(t, r.DeleteCluster(ctx, testOp, "c1"), ErrClusterInUse)
			require.NoError(t, r.DeleteCluster(ctx, testOp, "c2"))
			require.ErrorIs(t, r.DeleteCluster(ctx, testOp, "c2"), ErrNotFound)

			_, err = r.CreateCluster(ctx, testOp, ClusterSpec{ID: "c3", EndpointURL: "https://c1.cp.example.test:8443", Class: ClassShared, MaxDevices: 1})
			require.ErrorIs(t, err, ErrConflict, "endpoint is unique")

			_, err = r.CreateCluster(ctx, testOp, ClusterSpec{ID: "c1", EndpointURL: "https://x", Class: ClassShared, MaxDevices: 1})
			require.ErrorIs(t, err, ErrConflict)

			for _, bad := range []ClusterSpec{
				{ID: "", EndpointURL: "https://y", Class: ClassShared, MaxDevices: 1},
				{ID: "c4", EndpointURL: "", Class: ClassShared, MaxDevices: 1},
				{ID: "c4", EndpointURL: "http://y", Class: ClassShared, MaxDevices: 1},
				{ID: "c4", EndpointURL: "https://y", Class: "gold", MaxDevices: 1},
				{ID: "c4", EndpointURL: "https://y", Class: ClassShared, MaxDevices: 0},
			} {
				_, err = r.CreateCluster(ctx, testOp, bad)
				require.ErrorIs(t, err, ErrInvalid, "%+v", bad)
			}
		})
	}
}
