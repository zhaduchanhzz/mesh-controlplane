package db

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P12d: a tenant's own policy and DNS writes go through its runtime's
// scoped and fenced handle, and bump the tailnet's revision in the same
// transaction, on SQLite and PostgreSQL. A write after the tailnet moved
// stores nothing and bumps nothing; the other tailnet never changes.

var errRefused = errors.New("refused")

func revisions(t *testing.T, p *Platform, id tailnet.ID) (uint64, uint64) {
	t.Helper()

	var tn types.Tailnet
	require.NoError(t, p.db.Where("id = ?", id.String()).Take(&tn).Error)

	return tn.PolicyRevision, tn.ConfigRevision
}

func TestTenantPolicyWriteBumpsRevisionFenced(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)
			a, bdb := tenantDB(t, p, idA), tenantDB(t, p, idB)

			_, err := bdb.SetPolicy(`{"grants":[]}`)
			require.NoError(t, err)

			polB, _ := revisions(t, p, idB)

			pol, rev, err := a.SetPolicyRevised(`{"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`)
			require.NoError(t, err)
			require.NotNil(t, pol)
			assert.Equal(t, uint64(2), rev, "policy_revision 1 -> 2")

			gotPol, gotCfg := revisions(t, p, idA)
			assert.Equal(t, uint64(2), gotPol)
			assert.Equal(t, uint64(1), gotCfg, "config_revision untouched")

			stored, err := a.GetPolicy()
			require.NoError(t, err)
			assert.Equal(t, pol.Data, stored.Data)

			// B: nothing of A's write.
			gotB, _ := revisions(t, p, idB)
			assert.Equal(t, polB, gotB)

			storedB, err := bdb.GetPolicy()
			require.NoError(t, err)
			assert.JSONEq(t, `{"grants":[]}`, storedB.Data)

			// A moves (newer epoch): the old handle stores nothing, bumps nothing.
			require.NoError(t, p.db.Model(&types.Tailnet{}).Where("id = ?", idA.String()).
				Updates(map[string]any{"owner_epoch": 2, "updated_at": time.Now().UTC()}).Error)

			_, _, err = a.SetPolicyRevised(`{}`)
			require.ErrorIs(t, err, tenantscope.ErrFenced)

			gotPol, _ = revisions(t, p, idA)
			assert.Equal(t, uint64(2), gotPol, "no bump after the move")

			fresh := tenantDB(t, p, idA)
			stored, err = fresh.GetPolicy()
			require.NoError(t, err)
			assert.Equal(t, pol.Data, stored.Data, "the policy of the fenced write was not stored")
		})
	}
}

func TestTenantDNSWriteBumpsRevisionFenced(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)
			a, bdb := tenantDB(t, p, idA), tenantDB(t, p, idB)

			got, err := a.TenantDNS()
			require.NoError(t, err)
			assert.Nil(t, got, "no settings: defaults")

			// Read-modify-write: the callback sees the stored value.
			set, rev, err := a.UpdateTenantDNS(func(cur *types.TenantDNS) (*types.TenantDNS, error) {
				assert.Nil(t, cur)

				return &types.TenantDNS{SearchDomains: []string{"corp.example"}}, nil
			})
			require.NoError(t, err)
			assert.Equal(t, uint64(2), rev)
			assert.Equal(t, []string{"corp.example"}, set.SearchDomains)

			_, rev, err = a.UpdateTenantDNS(func(cur *types.TenantDNS) (*types.TenantDNS, error) {
				require.NotNil(t, cur)
				assert.Equal(t, []string{"corp.example"}, cur.SearchDomains)

				next := *cur
				next.Nameservers.Split = map[string][]string{"corp.example": {"192.168.77.53"}}

				return &next, nil
			})
			require.NoError(t, err)
			assert.Equal(t, uint64(3), rev)

			got, err = a.TenantDNS()
			require.NoError(t, err)
			assert.Equal(t, []string{"corp.example"}, got.SearchDomains)
			assert.Equal(t, map[string][]string{"corp.example": {"192.168.77.53"}}, got.Nameservers.Split)

			// The operator read sees the same row.
			op, err := p.TailnetDNS(idA)
			require.NoError(t, err)
			assert.Equal(t, got, op)

			// A refusal from the callback changes nothing.
			_, _, err = a.UpdateTenantDNS(func(*types.TenantDNS) (*types.TenantDNS, error) { return nil, errRefused })
			require.ErrorIs(t, err, errRefused)

			_, cfgRev := revisions(t, p, idA)
			assert.Equal(t, uint64(3), cfgRev, "a refused write does not bump")

			// B is untouched.
			gotB, err := bdb.TenantDNS()
			require.NoError(t, err)
			assert.Nil(t, gotB)

			_, cfgB := revisions(t, p, idB)
			assert.Equal(t, uint64(1), cfgB)

			// nil clears to the defaults.
			_, rev, err = a.UpdateTenantDNS(func(*types.TenantDNS) (*types.TenantDNS, error) { return nil, nil }) //nolint:nilnil // nil settings: the defaults
			require.NoError(t, err)
			assert.Equal(t, uint64(4), rev)

			got, err = a.TenantDNS()
			require.NoError(t, err)
			assert.Nil(t, got)

			// After a move the old handle writes nothing.
			require.NoError(t, p.db.Model(&types.Tailnet{}).Where("id = ?", idA.String()).
				Updates(map[string]any{"owner_cluster_id": "cluster-2", "owner_epoch": 2, "updated_at": time.Now().UTC()}).Error)

			_, _, err = a.UpdateTenantDNS(func(*types.TenantDNS) (*types.TenantDNS, error) {
				return &types.TenantDNS{SearchDomains: []string{"late.example"}}, nil
			})
			require.ErrorIs(t, err, tenantscope.ErrFenced)

			_, cfgRev = revisions(t, p, idA)
			assert.Equal(t, uint64(4), cfgRev)

			op, err = p.TailnetDNS(idA)
			require.NoError(t, err)
			assert.Nil(t, op, "the fenced write stored nothing")
		})
	}
}

// A handle without a fence (no runtime) refuses: the revision bump needs
// the owner and epoch the write is fenced on.
func TestTenantConfigWriteNeedsFence(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			id := platformTailnet(t, p, "local", types.TailnetActive, false)
			a := tenantDB(t, p, id)

			s, err := tailnet.FromContext(a.DB.Statement.Context)
			require.NoError(t, err)

			ctx, err := tailnet.WithScope(t.Context(), s)
			require.NoError(t, err)

			unfenced := &HSDatabase{DB: a.DB.WithContext(ctx), cfg: a.cfg, tailnet: a.tailnet}

			_, _, err = unfenced.SetPolicyRevised(`{}`)
			require.ErrorIs(t, err, tenantscope.ErrNoFence)

			_, _, err = unfenced.UpdateTenantDNS(func(*types.TenantDNS) (*types.TenantDNS, error) { return nil, nil }) //nolint:nilnil // nil settings: the defaults
			require.ErrorIs(t, err, tenantscope.ErrNoFence)

			pol, cfg := revisions(t, p, id)
			assert.Equal(t, [2]uint64{1, 1}, [2]uint64{pol, cfg})
		})
	}
}
