//go:build p05cluster

package servertest

import (
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// SetTailnetDNS stores d as slug's DNS settings and bumps its config
// revision, as the operator CLI does (ADR 0008); nil restores the defaults.
// The running cluster reloads it.
func (tc *TestCluster) SetTailnetDNS(tb testing.TB, slug string, d *types.TenantDNS) error {
	tb.Helper()

	return tc.Platform(tb).SetTailnetDNS(tc.Tailnet(tb, slug).ID, d,
		tc.cfg.DNSConfig.BaseDomain, tc.cfg.Tuning.TenantDNS)
}

// TailnetDNS reads slug's stored DNS settings (nil: defaults).
func (tc *TestCluster) TailnetDNS(tb testing.TB, slug string) (*types.TenantDNS, error) {
	tb.Helper()

	return tc.Platform(tb).TailnetDNS(tc.Tailnet(tb, slug).ID)
}

// StoreRawTailnetDNSForTest writes raw as slug's stored DNS settings
// without validation, as a hand edit of the database would.
func (tc *TestCluster) StoreRawTailnetDNSForTest(tb testing.TB, id, raw string) {
	tb.Helper()

	err := tc.raw.Exec(`INSERT INTO tailnet_settings (tailnet_id, dns_config) VALUES (?, ?)
		ON CONFLICT (tailnet_id) DO UPDATE SET dns_config = excluded.dns_config`, id, raw).Error
	if err != nil {
		tb.Fatalf("servertest: raw tailnet_settings write: %v", err)
	}

	err = tc.raw.Exec(`UPDATE tailnets SET config_revision = config_revision + 1 WHERE id = ?`, id).Error
	if err != nil {
		tb.Fatalf("servertest: bump config revision: %v", err)
	}
}
