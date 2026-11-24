package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
)

// P06 / ADR 0008: per-tailnet DNS. These are the pure parts: slug as a DNS
// label, the tailnet base domain, validation of what a tenant may set, and
// the runtime Config built from them.

var testLimits = TenantDNSLimits{MaxExtraRecords: 3, MaxNameservers: 2, MaxSearchDomains: 2, MaxConfigBytes: 4096}

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"a", "acme", "acme-prod", "t1", strings.Repeat("a", 63)} {
		require.NoError(t, ValidSlug(ok), ok)
	}

	for _, bad := range []string{"", "Acme", "acme_prod", "-acme", "acme-", "a.b", "a b", strings.Repeat("a", 64), "ácme"} {
		require.Error(t, ValidSlug(bad), bad)
	}
}

func TestTailnetBaseDomain(t *testing.T) {
	require.Equal(t, "acme.mesh.test", TailnetBaseDomain("acme", "mesh.test"))
	require.Equal(t, "acme.mesh.test", TailnetBaseDomain("acme", "mesh.test."))
}

func TestTenantDNSValidate(t *testing.T) {
	rec := func(name, typ, val string) tailcfg.DNSRecord { return tailcfg.DNSRecord{Name: name, Type: typ, Value: val} }

	for _, tc := range []struct {
		name string
		dns  TenantDNS
		ok   bool
	}{
		{"empty", TenantDNS{}, true},
		{"ip and doh nameservers", TenantDNS{Nameservers: TenantNameservers{Global: []string{"9.9.9.9", "https://dns.example/dns-query"}}}, true},
		{"split", TenantDNS{Nameservers: TenantNameservers{Split: map[string][]string{"corp.example": {"10.0.0.53"}}}}, true},
		{"own record", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("grafana.a.mesh.test", "A", "100.64.0.9")}}, true},
		{"record outside the cluster domain", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("db.corp.example", "AAAA", "fd7a:115c:a1e0::9")}}, true},
		{"search domain", TenantDNS{SearchDomains: []string{"corp.example"}}, true},

		{"record under another tailnet", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("phone.b.mesh.test", "A", "100.64.0.9")}}, false},
		{"record at the cluster base", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("mesh.test", "A", "100.64.0.9")}}, false},
		{"record at another tailnet's base", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("b.mesh.test", "A", "100.64.0.9")}}, false},
		{"record type", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("x.a.mesh.test", "TXT", "hi")}}, false},
		{"A with IPv6", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("x.a.mesh.test", "A", "fd7a::1")}}, false},
		{"record name", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{rec("bad_name.a.mesh.test", "A", "100.64.0.9")}}, false},
		{"nameserver not IP or https", TenantDNS{Nameservers: TenantNameservers{Global: []string{"resolver"}}}, false},
		{"nameserver http", TenantDNS{Nameservers: TenantNameservers{Global: []string{"http://dns.example/q"}}}, false},
		{"split domain", TenantDNS{Nameservers: TenantNameservers{Split: map[string][]string{"bad domain": {"10.0.0.53"}}}}, false},
		{"split under the cluster domain", TenantDNS{Nameservers: TenantNameservers{Split: map[string][]string{"b.mesh.test": {"10.0.0.53"}}}}, false},
		{"search domain invalid", TenantDNS{SearchDomains: []string{"not a domain"}}, false},

		{"too many records", TenantDNS{ExtraRecords: []tailcfg.DNSRecord{
			rec("a1.a.mesh.test", "A", "100.64.0.1"), rec("a2.a.mesh.test", "A", "100.64.0.2"),
			rec("a3.a.mesh.test", "A", "100.64.0.3"), rec("a4.a.mesh.test", "A", "100.64.0.4"),
		}}, false},
		{"too many nameservers (global + split)", TenantDNS{Nameservers: TenantNameservers{
			Global: []string{"9.9.9.9", "1.1.1.1"}, Split: map[string][]string{"corp.example": {"10.0.0.53"}},
		}}, false},
		{"too many search domains", TenantDNS{SearchDomains: []string{"a.example", "b.example", "c.example"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dns.Validate("a", "mesh.test", testLimits)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestParseTenantDNS(t *testing.T) {
	d, err := ParseTenantDNS([]byte(`{"magic_dns":false,"search_domains":["corp.example"]}`))
	require.NoError(t, err)
	require.NotNil(t, d.MagicDNS)
	require.False(t, *d.MagicDNS)

	_, err = ParseTenantDNS([]byte(`{"base_domain":"evil.example"}`))
	require.Error(t, err, "a tenant cannot set the base domain or any unknown key")

	_, err = ParseTenantDNS([]byte(`{"search_domains":`))
	require.Error(t, err)
}

func TestTenantDNSSizeLimit(t *testing.T) {
	big := TenantDNS{SearchDomains: []string{strings.Repeat("a", 60) + ".example"}}
	lim := testLimits
	raw, err := json.Marshal(big)
	require.NoError(t, err)

	lim.MaxConfigBytes = len(raw) - 1
	require.Error(t, big.Validate("a", "mesh.test", lim), "over max_config_bytes")
}

func clusterConfigForTest() *Config {
	c := &Config{}
	c.DNSConfig.BaseDomain = "mesh.test"
	c.DNSConfig.MagicDNS = true
	c.DNSConfig.Certificates.Enabled = true

	return c
}

func TestConfigForTailnet(t *testing.T) {
	base := clusterConfigForTest()

	no := false
	a := base.ForTailnet("a", &TenantDNS{
		Nameservers:   TenantNameservers{Global: []string{"9.9.9.9"}},
		SearchDomains: []string{"corp.example"},
		ExtraRecords:  []tailcfg.DNSRecord{{Name: "Grafana.a.mesh.test", Type: "A", Value: "100.64.0.9"}},
	})
	b := base.ForTailnet("b", &TenantDNS{MagicDNS: &no})
	c := base.ForTailnet("c", nil)

	require.NotSame(t, base, a)
	require.Equal(t, "mesh.test", base.DNSConfig.BaseDomain, "the cluster config is not changed")
	require.Equal(t, "a.mesh.test", a.DNSConfig.BaseDomain)
	require.Equal(t, "b.mesh.test", b.DNSConfig.BaseDomain)
	require.True(t, a.DNSConfig.Certificates.Enabled, "cluster-only settings carry over")

	ta := a.CloneTailcfgDNSConfig()
	require.Equal(t, []string{"a.mesh.test", "corp.example"}, ta.Domains)
	require.Len(t, ta.FallbackResolvers, 1)
	require.Equal(t, "grafana.a.mesh.test", ta.ExtraRecords[0].Name, "record names lowercased")

	require.False(t, b.CloneTailcfgDNSConfig().Proxied, "MagicDNS off for b")
	require.True(t, c.CloneTailcfgDNSConfig().Proxied, "no settings: MagicDNS on")
	require.Empty(t, c.CloneTailcfgDNSConfig().ExtraRecords)

	// Each runtime has its own live DNS: changing a's leaves b and the
	// cluster's alone.
	a.ReplaceTenantDNS("a", &TenantDNS{SearchDomains: []string{"other.example"}})
	require.Equal(t, []string{"a.mesh.test", "other.example"}, a.CloneTailcfgDNSConfig().Domains)
	require.NotContains(t, fmt.Sprint(b.CloneTailcfgDNSConfig().Domains), "other.example")
	require.NotContains(t, fmt.Sprint(base.CloneTailcfgDNSConfig().Domains), "other.example")
}
