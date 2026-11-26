package db

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

func registerDedicated(t *testing.T, p *Platform, id string) {
	t.Helper()

	now := time.Now().UTC()
	require.NoError(t, p.db.Exec(`INSERT INTO clusters (id, endpoint_url, class, state, max_devices, revision, created_at, updated_at)
		VALUES (?, ?, 'dedicated', 'ACTIVE', 100, 1, ?, ?)`, id, "https://"+id+".example.test", now, now).Error)
}

// P12c C3 (review 2026-11-26): the flip itself re-checks the dedicated rule.
// A move into dedicated d1, which already owns a live tailnet, must never
// complete, whoever requested it (a request admitted before P12c, or a
// write by hand): neither the owner's hand-over nor the target's take-over.
func TestHandOverRefusesSecondTenantOnDedicated(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p1, l1 := leased(t, b, "c1", "h1")
			pd, _ := leased(t, b, "d1", "hd")
			registerDedicated(t, p1, "d1")

			platformTailnet(t, pd, "d1", types.TailnetActive, false) // d1's tenant
			id := platformTailnet(t, p1, "c1", types.TailnetActive, false)
			requestMove(t, p1, id, "d1")

			_, err := p1.HandOver(id, 1)
			require.ErrorIs(t, err, ErrHandoverStale, "d1 already has a tenant")

			got := row(t, p1, id)
			assert.Equal(t, "c1", got.OwnerClusterID)
			assert.EqualValues(t, 1, got.OwnerEpoch)

			require.NoError(t, p1.LeaseStore().Release(context.Background(), l1))
			require.ErrorIs(t, pd.TakeOver(id, "c1", 1), ErrHandoverStale, "nor taken over by d1")
			assert.Equal(t, "c1", row(t, pd, id).OwnerClusterID)

			// A deleted tenant does not count.
			require.NoError(t, pd.db.Exec(`UPDATE tailnets SET deleted_at = ? WHERE owner_cluster_id = 'd1'`, time.Now().UTC()).Error)
			require.NoError(t, pd.TakeOver(id, "c1", 1))
			assert.Equal(t, "d1", row(t, pd, id).OwnerClusterID)
		})
	}
}

// scheduledMove inserts a tailnet_moves row (P12b) in state.
func scheduledMove(t *testing.T, p *Platform, id tailnet.ID, from, to string, epoch int64, state string) string {
	t.Helper()

	mid := uuid.NewV4().String()
	now := time.Now().UTC()
	require.NoError(t, p.db.Exec(`INSERT INTO tailnet_moves (id, tailnet_id, from_cluster_id, to_cluster_id, from_epoch,
		scheduled_at, reason, state, error, requested_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, '', ?, ?, ?)`, mid, id.String(), from, to, epoch, now, state, now, now, now).Error)

	return mid
}

func moveState(t *testing.T, p *Platform, mid string) (string, *time.Time) {
	t.Helper()

	var rows []struct {
		State      string
		FinishedAt *time.Time
	}

	require.NoError(t, p.db.Raw(`SELECT state, finished_at FROM tailnet_moves WHERE id = ?`, mid).Scan(&rows).Error)
	require.Len(t, rows, 1)

	return rows[0].State, rows[0].FinishedAt
}

// P12c L7 (review 2026-11-26): the flip records the planned move it
// completes (REQUESTED, same from, epoch and to) HANDED_OVER in its own
// transaction, at the flip's time: the schedule never depends on what the
// scheduler later infers from the tailnet row.
func TestHandOverRecordsPlannedMove(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p1, l1 := leased(t, b, "c1", "h1")
			p2, _ := leased(t, b, "c2", "h2")

			id := platformTailnet(t, p1, "c1", types.TailnetActive, false)
			mid := scheduledMove(t, p1, id, "c1", "c2", 1, "REQUESTED")
			requestMove(t, p1, id, "c2")

			before := time.Now().UTC().Add(-time.Second)
			_, err := p1.HandOver(id, 1)
			require.NoError(t, err)

			state, fin := moveState(t, p1, mid)
			assert.Equal(t, "HANDED_OVER", state)
			require.NotNil(t, fin)
			assert.WithinRange(t, fin.UTC(), before, time.Now().UTC().Add(time.Second))

			// Take-over records it too; a move of another epoch is not touched.
			id2 := platformTailnet(t, p1, "c1", types.TailnetActive, false)
			other := scheduledMove(t, p1, id2, "c1", "c2", 7, "CANCELLED")
			mid2 := scheduledMove(t, p1, id2, "c1", "c2", 1, "REQUESTED")
			requestMove(t, p1, id2, "c2")
			require.NoError(t, p1.LeaseStore().Release(context.Background(), l1))
			require.NoError(t, p2.TakeOver(id2, "c1", 1))

			state, _ = moveState(t, p2, mid2)
			assert.Equal(t, "HANDED_OVER", state)
			state, _ = moveState(t, p2, other)
			assert.Equal(t, "CANCELLED", state, "another move of the tailnet is left alone")
		})
	}
}
