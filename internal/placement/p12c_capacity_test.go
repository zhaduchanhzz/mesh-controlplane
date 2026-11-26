package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

// P12c C3 (review 2026-11-26): capacity and the dedicated rule count the
// tailnets moving INTO a cluster (moving_to_cluster_id = C), not only the
// ones it owns; enrollment and moves into one cluster serialise.

// tailnetOn creates slug ACTIVE on cluster with devices live devices.
func tailnetOn(t *testing.T, r *Registry, gdb *gorm.DB, slug, cluster string, devices int) types.Tailnet {
	t.Helper()

	tn, err := r.CreateTailnetOn(context.Background(), testOp, slug, cluster)
	require.NoError(t, err)
	tn, err = r.ActivateTailnet(context.Background(), testOp, tn.ID, tn.OwnerEpoch)
	require.NoError(t, err)
	addNodes(t, gdb, tn.ID, devices)

	return tn
}

// Two moves into an empty dedicated cluster: only the first is admitted;
// no tailnet may be created there either while that move is pending.
func TestMoveIntoDedicatedCountsIncoming(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "d1", ClassDedicated, ClusterActive, 100)

			a := tailnetOn(t, r, gdb, "a", "c1", 1)
			bt := tailnetOn(t, r, gdb, "b", "c1", 1)

			_, err := r.MoveOwner(ctx, testOp, a.ID, "c1", a.OwnerEpoch, "d1")
			require.NoError(t, err, "the first move into the empty dedicated cluster")

			_, err = r.MoveOwner(ctx, testOp, bt.ID, "c1", bt.OwnerEpoch, "d1")
			require.ErrorIs(t, err, ErrNoCapacity, "a second tenant must not be admitted into dedicated d1")
			assert.Empty(t, movingTo(t, gdb, bt.ID))

			_, err = r.CreateTailnetOn(ctx, testOp, "c", "d1")
			require.ErrorIs(t, err, ErrNoCapacity, "d1 is taken by the tailnet moving in")

			_, err = r.CreateTailnet(ctx, testOp, "c2", ClassDedicated)
			require.ErrorIs(t, err, ErrNoCapacity, "placement must not choose d1 either")

			// Withdrawn: d1 is free again.
			_, err = r.CancelMove(ctx, testOp, a.ID, a.OwnerEpoch)
			require.NoError(t, err)
			_, err = r.MoveOwner(ctx, testOp, bt.ID, "c1", bt.OwnerEpoch, "d1")
			require.NoError(t, err)
		})
	}
}

// Moves into a shared cluster count the devices already moving in; so do
// placement and the enrollment budget of the cluster's own tailnets.
func TestMoveCapacityCountsIncoming(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 10)

			a := tailnetOn(t, r, gdb, "a", "c1", 6)
			bt := tailnetOn(t, r, gdb, "b", "c1", 6)
			own := tailnetOn(t, r, gdb, "own", "c2", 4)

			// 4 owned + 6 moving in = 10 of 10.
			_, err := r.MoveOwner(ctx, testOp, a.ID, "c1", a.OwnerEpoch, "c2")
			require.NoError(t, err)

			_, err = r.MoveOwner(ctx, testOp, bt.ID, "c1", bt.OwnerEpoch, "c2")
			require.ErrorIs(t, err, ErrNoCapacity, "4 + 6 moving in + 6 > 10")
			assert.Empty(t, movingTo(t, gdb, bt.ID))

			// The cluster's own tailnet may not enroll into the room promised
			// to the tailnet moving in.
			q, err := r.EnrollmentAllowed(ctx, tailnet.MustParseID(own.ID))
			require.ErrorIs(t, err, ErrClusterFull, "4 owned + 6 moving in = 10 of 10")
			assert.False(t, q.Allowed)
			assert.EqualValues(t, 10, q.ClusterDevices)

			// Placement sees c2 full too: with c1 draining, nothing has room.
			_, err = r.UpdateClusterState(ctx, testOp, "c1", ClusterDraining, 0)
			require.NoError(t, err)
			_, err = r.CreateTailnet(ctx, testOp, "new", ClassShared)
			require.ErrorIs(t, err, ErrNoCapacity, "c2's room is taken by the tailnet moving in")
		})
	}
}

// A tailnet with a move pending enrolls no new device: its devices were
// counted against the target when the move was admitted.
func TestEnrollmentRefusedWhileMoving(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			addCluster(t, r, "c2", ClassShared, ClusterActive, 6)

			a := tailnetOn(t, r, gdb, "a", "c1", 6)

			_, err := r.EnrollmentAllowed(ctx, tailnet.MustParseID(a.ID))
			require.NoError(t, err, "not moving: c1 has room")

			_, err = r.MoveOwner(ctx, testOp, a.ID, "c1", a.OwnerEpoch, "c2")
			require.NoError(t, err, "6 of 6 on c2")

			q, err := r.EnrollmentAllowed(ctx, tailnet.MustParseID(a.ID))
			require.Error(t, err, "a 7th device would land on c2 at the hand-over")
			require.ErrorIs(t, err, ErrTailnetMoving)
			require.NotErrorIs(t, err, ErrQuotaExceeded, "transient: the client retries, not a quota refusal")
			assert.False(t, q.Allowed)
		})
	}
}

// PostgreSQL: a move into c2 waits for an enrollment into c2's own tailnet
// in flight (same cluster lock) and then counts its device: 4 + 1 enrolled
// + 6 moving > 10 is refused. Unserialised, the move read 4 + 6 = 10 and
// was admitted, then the enrollment committed an 11th device.
func TestMoveSerialisesWithEnrollmentIntoTarget(t *testing.T) {
	gdb := backends()[1].open(t)
	r := newRegistry(t, gdb, Config{})
	addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
	addCluster(t, r, "c2", ClassShared, ClusterActive, 10)

	a := tailnetOn(t, r, gdb, "a", "c1", 6)
	own := tailnetOn(t, r, gdb, "own", "c2", 4)

	held, release, enrolled := make(chan struct{}), make(chan struct{}), make(chan error, 1)

	go func() {
		enrolled <- gdb.Transaction(func(tx *gorm.DB) error {
			_, err := r.EnrollmentAllowedTx(tx, tailnet.MustParseID(own.ID))
			if err != nil {
				return err
			}

			now := time.Now().UTC()

			err = tx.Exec(`INSERT INTO nodes (tailnet_id, hostname, created_at, updated_at) VALUES (?, ?, ?, ?)`,
				own.ID, "in-flight", now, now).Error
			if err != nil {
				return err
			}

			close(held)
			<-release

			return nil
		})
	}()

	select {
	case <-held:
	case err := <-enrolled:
		t.Fatalf("enrollment: %v", err)
	}

	moved := make(chan error, 1)

	go func() {
		_, err := r.MoveOwner(context.Background(), testOp, a.ID, "c1", a.OwnerEpoch, "c2")
		moved <- err
	}()

	var (
		got      error
		returned bool
	)

	select {
	case got = <-moved:
		returned = true
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-enrolled)

	if returned {
		t.Logf("the move did not wait for the enrollment in flight: %v", got)
	} else {
		got = <-moved
	}

	assert.False(t, returned, "the move must wait for the enrollment into its target")
	require.ErrorIs(t, got, ErrNoCapacity, "4 + 1 enrolled + 6 moving in > 10")
	assert.Empty(t, movingTo(t, gdb, a.ID))
	assert.EqualValues(t, 5, count(t, gdb, `SELECT count(*) FROM nodes WHERE tailnet_id = ? AND deleted_at IS NULL`, own.ID))
}

// PostgreSQL: enrollment takes the tailnet's ownership lock (shared) before
// any cluster lock, the lock LockOwnership takes exclusive: an owner change
// in flight holds the enrollment back. This also checks that
// ownershipLockPrefix names tenantscope's lock.
func TestEnrollmentTakesOwnershipLockFirst(t *testing.T) {
	gdb := backends()[1].open(t)
	r := newRegistry(t, gdb, Config{})
	addCluster(t, r, "c1", ClassShared, ClusterActive, 100)

	a := tailnetOn(t, r, gdb, "a", "c1", 1)

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)

	go func() {
		done <- gdb.Transaction(func(tx *gorm.DB) error {
			err := tenantscope.LockOwnership(tx, a.ID)
			if err != nil {
				return err
			}

			close(locked)
			<-release

			return nil
		})
	}()

	<-locked

	enrolled := make(chan error, 1)

	go func() {
		_, err := r.EnrollmentAllowed(context.Background(), tailnet.MustParseID(a.ID))
		enrolled <- err
	}()

	select {
	case err := <-enrolled:
		t.Fatalf("enrollment did not wait for the ownership lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-enrolled)
}
