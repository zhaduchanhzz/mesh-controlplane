package hscontrol

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
)

// TestP06ClusterRefusesTenantDNSInConfig (ADR 0008 Q-c): DNS settings that
// are per tailnet in cluster mode must not be set in the cluster config,
// where they would silently apply to nobody or to everybody.
func TestP06ClusterRefusesTenantDNSInConfig(t *testing.T) {
	t.Parallel()

	base := func() *types.Config {
		return &types.Config{
			ClusterID: "c1",
			ServerURL: "https://c1.example.com:8443",
			Policy:    types.PolicyConfig{Mode: types.PolicyModeDB},
			DNSConfig: types.DNSConfig{BaseDomain: "mesh.test", MagicDNS: true},
		}
	}

	require.NoError(t, validateClusterConfig(base()), "control")

	for name, set := range map[string]func(*types.Config){
		"extra_records": func(c *types.Config) {
			c.DNSConfig.ExtraRecords = []tailcfg.DNSRecord{{Name: "x.example", Type: "A", Value: "100.64.0.1"}}
		},
		"extra_records_path": func(c *types.Config) { c.DNSConfig.ExtraRecordsPath = "/tmp/records.json" },
		"nameservers.global": func(c *types.Config) { c.DNSConfig.Nameservers.Global = []string{"9.9.9.9"} },
		"nameservers.split": func(c *types.Config) {
			c.DNSConfig.Nameservers.Split = map[string][]string{"corp.example": {"10.0.0.53"}}
		},
		"search_domains": func(c *types.Config) { c.DNSConfig.SearchDomains = []string{"corp.example"} },
		"no base_domain": func(c *types.Config) { c.DNSConfig.BaseDomain = "" },
	} {
		c := base()
		set(c)
		require.Error(t, validateClusterConfig(c), name)
	}
}
