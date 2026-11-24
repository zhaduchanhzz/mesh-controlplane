//go:build p05cluster

// P06 / ADR 0008: DNS per tailnet in one cluster process. Base domain
// <slug>.<cluster base>, DNS settings of one tailnet reach its own clients
// only, invalid settings are refused and a hand-corrupted row keeps the
// runtime out instead of falling back to defaults.

package servertest_test

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

func tailnetDomain(slug string) string { return servertest.ClusterTailnetDomain(slug) }

func resolverAddrs(nm *netmap.NetworkMap) []string {
	var out []string

	for _, r := range append(slices.Clone(nm.DNS.Resolvers), nm.DNS.FallbackResolvers...) {
		out = append(out, r.Addr)
	}

	return out
}

func recordNames(nm *netmap.NetworkMap) []string {
	var out []string
	for _, r := range nm.DNS.ExtraRecords {
		out = append(out, r.Name)
	}

	return out
}

// Same hostname in A and B: different MagicDNS names, each under its own
// tailnet's base domain, so certificate names cannot collide either.
func TestClusterP06DNSNamesPerTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a := enroll(t, tc.Tenant(t, tenantA), "alice", "phone", "laptop")
	b := enroll(t, tc.Tenant(t, tenantB), "bob", "phone", "laptop")

	for _, group := range [][]*servertest.TestClient{a, b} {
		for _, c := range group {
			c.WaitForPeers(t, 1, 15*time.Second)
		}
	}

	for slug, group := range map[string][]*servertest.TestClient{tenantA: a, tenantB: b} {
		domain := tailnetDomain(slug)
		nm := group[0].Netmap()

		require.Equal(t, "phone."+domain+".", nm.SelfNode.Name(), "self name in %s", slug)

		for _, p := range nm.Peers {
			require.Equal(t, "laptop."+domain+".", p.Name(), "peer name in %s", slug)
		}

		require.Contains(t, nm.DNS.Domains, domain, "%s gets its own MagicDNS domain", slug)
		require.NotContains(t, nm.DNS.Domains, servertest.ClusterBaseDomain, "not the cluster base")
	}
}

// Each tailnet gets its own nameservers, search domains and records; a
// change to A's reaches A's clients and sends B's nothing (I07).
func TestClusterP06DNSSettingsPerTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a := enroll(t, tc.Tenant(t, tenantA), "alice", "a1")[0]
	b := enroll(t, tc.Tenant(t, tenantB), "bob", "b1")[0]

	require.NoError(t, tc.SetTailnetDNS(t, tenantA, &types.TenantDNS{
		Nameservers:   types.TenantNameservers{Global: []string{"9.9.9.9"}},
		SearchDomains: []string{"a.corp.example"},
		ExtraRecords:  []tailcfg.DNSRecord{{Name: "grafana." + tailnetDomain(tenantA), Type: "A", Value: "100.64.0.50"}},
	}))
	require.NoError(t, tc.SetTailnetDNS(t, tenantB, &types.TenantDNS{
		Nameservers:   types.TenantNameservers{Global: []string{"149.112.112.112"}},
		SearchDomains: []string{"b.corp.example"},
	}))

	a.WaitForCondition(t, "A's DNS", 15*time.Second, func(nm *netmap.NetworkMap) bool {
		return slices.Contains(resolverAddrs(nm), "9.9.9.9") && slices.Contains(recordNames(nm), "grafana."+tailnetDomain(tenantA))
	})
	b.WaitForCondition(t, "B's DNS", 15*time.Second, func(nm *netmap.NetworkMap) bool {
		return slices.Contains(resolverAddrs(nm), "149.112.112.112")
	})

	na, nb := a.Netmap(), b.Netmap()
	require.NotContains(t, resolverAddrs(na), "149.112.112.112")
	require.NotContains(t, na.DNS.Domains, "b.corp.example")
	require.NotContains(t, resolverAddrs(nb), "9.9.9.9")
	require.NotContains(t, nb.DNS.Domains, "a.corp.example")
	require.Empty(t, recordNames(nb), "A's records never reach B")

	// I07: change A again; B gets no frame at all.
	before := b.UpdateCount()

	require.NoError(t, tc.SetTailnetDNS(t, tenantA, &types.TenantDNS{
		Nameservers: types.TenantNameservers{Global: []string{"1.1.1.1"}},
	}))
	a.WaitForCondition(t, "A's new DNS", 15*time.Second, func(nm *netmap.NetworkMap) bool {
		return slices.Contains(resolverAddrs(nm), "1.1.1.1") && len(recordNames(nm)) == 0
	})

	time.Sleep(3 * time.Second) // two reconcile ticks
	require.Equal(t, before, b.UpdateCount(), "A's DNS change sent B a frame")
}

// Invalid settings are refused and the stored value stays as it was.
func TestClusterP06DNSRefusesInvalid(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})

	good := &types.TenantDNS{SearchDomains: []string{"a.corp.example"}}
	require.NoError(t, tc.SetTailnetDNS(t, tenantA, good))

	for name, bad := range map[string]*types.TenantDNS{
		"record under B": {ExtraRecords: []tailcfg.DNSRecord{{Name: "phone." + tailnetDomain(tenantB), Type: "A", Value: "100.64.0.9"}}},
		"bad nameserver": {Nameservers: types.TenantNameservers{Global: []string{"resolver"}}},
		"too many records": {ExtraRecords: func() []tailcfg.DNSRecord {
			var rs []tailcfg.DNSRecord
			for i := range types.DefaultTenantDNSLimits.MaxExtraRecords + 1 {
				rs = append(rs, tailcfg.DNSRecord{Name: fmt.Sprintf("h%d.%s", i, tailnetDomain(tenantA)), Type: "A", Value: "100.64.0.9"})
			}

			return rs
		}()},
	} {
		require.Error(t, tc.SetTailnetDNS(t, tenantA, bad), name)
	}

	got, err := tc.TailnetDNS(t, tenantA)
	require.NoError(t, err)
	require.Equal(t, good, got, "a refused write leaves the stored settings unchanged")
}

// A stored row that does not validate (a hand edit) keeps the tailnet's
// runtime from loading: never served with default DNS.
func TestClusterP06DNSCorruptRowNotLoaded(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})

	st := tc.CreateTailnetLive(t, "tenant-c")
	tc.StoreRawTailnetDNSForTest(t, st.ID.String(), `{"nameservers":{"global":["resolver"]}}`)
	time.Sleep(3 * time.Second) // reconcile ticks
	require.False(t, tc.HasRuntime(t, "tenant-c"), "a tailnet with invalid stored DNS must not load")

	// Repaired: it loads.
	require.NoError(t, tc.SetTailnetDNS(t, "tenant-c", nil))
	tc.WaitRuntime(t, "tenant-c", true, 15*time.Second)
}

// Loading runtimes while others serve maps: with -race, a DNS config
// shared and rebuilt per runtime shows up here.
func TestClusterP06DNSHotLoadNoRace(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	srv := tc.Tenant(t, tenantA)

	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range 3 {
			slug := fmt.Sprintf("hot-%d", i)
			tc.CreateTailnetLive(t, slug)
			tc.WaitRuntime(t, slug, true, 15*time.Second)
		}
	})

	clients := enroll(t, srv, "alice", "r1", "r2", "r3", "r4")

	wg.Wait()

	for _, c := range clients {
		c.WaitForPeers(t, len(clients)-1, 20*time.Second)
		require.Contains(t, c.Netmap().DNS.Domains, tailnetDomain(tenantA))
	}
}
