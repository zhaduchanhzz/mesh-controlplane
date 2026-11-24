package db

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"tailscale.com/tailcfg"
)

// P06 / ADR 0008: tailnet_settings and the per-tailnet DNS writes, on SQLite
// and PostgreSQL.

func TestCreateTailnetRefusesInvalidSlug(t *testing.T) {
	forEachPlatformBackend(t, func(t *testing.T, p *Platform) {
		for _, bad := range []string{"Upper", "under_score", "-dash", "dash-", "a.b"} {
			_, err := p.CreateTailnet(bad)
			require.Error(t, err, bad)
		}

		_, err := p.CreateTailnet("good-slug")
		require.NoError(t, err)
	})
}

func TestTailnetDNSRoundTripAndRevision(t *testing.T) {
	forEachPlatformBackend(t, func(t *testing.T, p *Platform) {
		tn, err := p.CreateTailnet("acme")
		require.NoError(t, err)

		id := tailnet.MustParseID(tn.ID)
		lim := types.DefaultTenantDNSLimits

		got, err := p.TailnetDNS(id)
		require.NoError(t, err)
		require.Nil(t, got, "no settings: defaults")

		want := &types.TenantDNS{
			Nameservers:  types.TenantNameservers{Global: []string{"9.9.9.9"}},
			ExtraRecords: []tailcfg.DNSRecord{{Name: "grafana.acme.mesh.test", Type: "A", Value: "100.64.0.9"}},
		}
		require.NoError(t, p.SetTailnetDNS(id, want, "mesh.test", lim))

		got, err = p.TailnetDNS(id)
		require.NoError(t, err)
		require.Equal(t, want, got)

		row, err := p.TailnetBySlug("acme")
		require.NoError(t, err)
		require.Equal(t, uint64(2), row.ConfigRevision, "a write bumps config_revision")

		// Refused: under another tailnet's domain. Nothing changes.
		bad := &types.TenantDNS{ExtraRecords: []tailcfg.DNSRecord{{Name: "x.other.mesh.test", Type: "A", Value: "100.64.0.9"}}}
		require.Error(t, p.SetTailnetDNS(id, bad, "mesh.test", lim))

		got, err = p.TailnetDNS(id)
		require.NoError(t, err)
		require.Equal(t, want, got)

		row, err = p.TailnetBySlug("acme")
		require.NoError(t, err)
		require.Equal(t, uint64(2), row.ConfigRevision, "a refused write does not bump")

		// Clear: back to defaults, revision bumped.
		require.NoError(t, p.SetTailnetDNS(id, nil, "mesh.test", lim))

		got, err = p.TailnetDNS(id)
		require.NoError(t, err)
		require.Nil(t, got)

		row, err = p.TailnetBySlug("acme")
		require.NoError(t, err)
		require.Equal(t, uint64(3), row.ConfigRevision)
	})
}

func TestTailnetDNSCorruptRowIsAnError(t *testing.T) {
	forEachPlatformBackend(t, func(t *testing.T, p *Platform) {
		tn, err := p.CreateTailnet("acme")
		require.NoError(t, err)

		require.NoError(t, p.db.Exec(`INSERT INTO tailnet_settings (tailnet_id, dns_config) VALUES (?, ?)`,
			tn.ID, `{"nameservers":`).Error)

		_, err = p.TailnetDNS(tailnet.MustParseID(tn.ID))
		require.Error(t, err, "a row that does not parse is an error, never defaults")
	})
}

func forEachPlatformBackend(t *testing.T, fn func(t *testing.T, p *Platform)) {
	t.Helper()

	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			fn(t, openPlatform(t, b.cfg(t, "", "")))
		})
	}
}
