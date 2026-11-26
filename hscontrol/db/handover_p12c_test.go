package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
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
