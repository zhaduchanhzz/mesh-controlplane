package db

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// leased opens the platform of cluster on cfg and takes the cluster lease
// for holder, as a serving process does.
func leased(t *testing.T, b backend, cluster, holder string) (*Platform, lease.Lease) {
	t.Helper()

	p := openPlatform(t, b.cfg(t, "", cluster))
	p.RequireLease()

	l, err := p.LeaseStore().Acquire(context.Background(), cluster, holder, time.Hour)
	require.NoError(t, err)
	p.SetLease(l.Holder, l.Epoch)

	return p, l
}

func requestMove(t *testing.T, p *Platform, id tailnet.ID, to string) {
	t.Helper()
	require.NoError(t, p.db.Exec(`UPDATE tailnets SET moving_to_cluster_id = ? WHERE id = ?`, to, id.String()).Error)
}

func row(t *testing.T, p *Platform, id tailnet.ID) types.Tailnet {
	t.Helper()

	var tn types.Tailnet
	require.NoError(t, p.db.Where("id = ?", id.String()).Take(&tn).Error)

	return tn
}

func ids(tns []types.Tailnet) []string {
	out := make([]string, 0, len(tns))
	for _, tn := range tns {
		out = append(out, tn.ID)
	}

	return out
}

// ADR 0010: the owner flips a requested move only after it stops serving,
// and only while its process holds the cluster lease; a requested move takes
// the tailnet out of OwnedTailnets at once.
func TestHandOverCompletesRequestedMove(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p1, _ := leased(t, b, "c1", "h1")
			p2, _ := leased(t, b, "c2", "h2")
			id := platformTailnet(t, p1, "c1", types.TailnetActive, false)

			_, err := p1.HandOver(id, 1)
			require.ErrorIs(t, err, ErrHandoverStale, "no move requested")

			requestMove(t, p1, id, "c2")

			owned, err := p1.OwnedTailnets("c1")
			require.NoError(t, err)
			assert.NotContains(t, ids(owned), id.String(), "a tailnet being moved away is not loaded")

			pending, err := p1.PendingHandovers()
			require.NoError(t, err)
			assert.Equal(t, []string{id.String()}, ids(pending))

			orphans, err := p2.OrphanedIncoming()
			require.NoError(t, err)
			assert.Empty(t, orphans, "c1 is alive: c2 must wait for the hand-over")
			require.ErrorIs(t, p2.TakeOver(id, "c1", 1), ErrHandoverStale, "c1 still has a live lease")

			// Another process of c1 whose lease is not the live one.
			stale := openPlatform(t, b.cfg(t, "", "c1"))
			stale.SetLease("h-old", 99)
			_, err = stale.HandOver(id, 1)
			require.ErrorIs(t, err, ErrHandoverStale, "only the lease holder hands over")

			_, err = p1.HandOver(id, 2)
			require.ErrorIs(t, err, ErrHandoverStale, "wrong epoch")

			to, err := p1.HandOver(id, 1)
			require.NoError(t, err)
			assert.Equal(t, "c2", to)

			got := row(t, p1, id)
			assert.Equal(t, "c2", got.OwnerClusterID)
			assert.EqualValues(t, 2, got.OwnerEpoch)
			assert.Nil(t, got.MovingToClusterID)

			_, err = p1.HandOver(id, 1)
			require.ErrorIs(t, err, ErrHandoverStale, "done once")

			owned, err = p2.OwnedTailnets("c2")
			require.NoError(t, err)
			assert.Contains(t, ids(owned), id.String())
		})
	}
}

// ADR 0010 §4: with no live lease of the owner cluster, the target takes the
// tailnet over; a concurrent hand-over and take-over: exactly one wins.
func TestTakeOverWhenOwnerClusterIsDead(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p1, l1 := leased(t, b, "c1", "h1")
			p2, _ := leased(t, b, "c2", "h2")
			id := platformTailnet(t, p1, "c1", types.TailnetActive, false)
			requestMove(t, p1, id, "c2")

			require.NoError(t, p1.LeaseStore().Release(context.Background(), l1))

			_, err := p1.HandOver(id, 1)
			require.ErrorIs(t, err, ErrHandoverStale, "c1's process no longer holds its lease")

			orphans, err := p2.OrphanedIncoming()
			require.NoError(t, err)
			assert.Equal(t, []string{id.String()}, ids(orphans))

			// A third cluster cannot take it.
			p3, _ := leased(t, b, "c3", "h3")
			require.ErrorIs(t, p3.TakeOver(id, "c1", 1), ErrHandoverStale, "not the requested target")

			require.NoError(t, p2.TakeOver(id, "c1", 1))

			got := row(t, p2, id)
			assert.Equal(t, "c2", got.OwnerClusterID)
			assert.EqualValues(t, 2, got.OwnerEpoch)
			assert.Nil(t, got.MovingToClusterID)
			require.ErrorIs(t, p2.TakeOver(id, "c1", 1), ErrHandoverStale, "done once")

			// Concurrent completions of one move: exactly one CAS wins.
			id2 := platformTailnet(t, p1, "c1", types.TailnetActive, false)
			requestMove(t, p1, id2, "c2")

			const n = 8

			var wg sync.WaitGroup

			errs := make([]error, n)
			for i := range n {
				wg.Go(func() { errs[i] = p2.TakeOver(id2, "c1", 1) })
			}

			wg.Wait()

			ok := 0

			for _, err := range errs {
				if err == nil {
					ok++
				} else {
					require.ErrorIs(t, err, ErrHandoverStale)
				}
			}

			assert.Equal(t, 1, ok, "exactly one completes the move: %v", errs)
			assert.EqualValues(t, 2, row(t, p2, id2).OwnerEpoch)
		})
	}
}
