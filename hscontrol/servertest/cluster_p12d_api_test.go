//go:build p05cluster

package servertest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsclient "tailscale.com/client/tailscale/v2"
	"tailscale.com/types/netmap"

	apiv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P12d (owner decision 2026-11-26, replacing P08b Q3 for policy and DNS):
// a tenant drives Tailscale's "Subnet routers" workflow through /api/v2
// with its own API key only. It writes its own policy (grants to a subnet,
// autoApprovers, tests) and DNS settings, and disables key expiry on its
// routers. Every write is scoped to the key's tailnet, fenced, bumps the
// tailnet's revision, is rate limited and audited; another tailnet's ID is
// 404 and the other tailnet gets no frame.

type apiFull struct {
	code int
	body string
	hdr  http.Header
}

func apiDo(t *testing.T, base, method, path, key, body string, hdr map[string]string) apiFull {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	require.NoError(t, err)

	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return apiFull{resp.StatusCode, string(b), resp.Header}
}

// auditLog collects the audit records of tenant writes (P12d).
type auditLog struct {
	mu   sync.Mutex
	recs map[string][]apiv2.TenantWrite
}

func captureAudit(t *testing.T, tc *servertest.TestCluster) *auditLog {
	t.Helper()

	l := &auditLog{recs: map[string][]apiv2.TenantWrite{}}
	tc.Cluster.SetTenantAuditHookForTest(func(id tailnet.ID, w apiv2.TenantWrite) {
		l.mu.Lock()
		defer l.mu.Unlock()

		l.recs[id.String()] = append(l.recs[id.String()], w)
	})
	t.Cleanup(func() { tc.Cluster.SetTenantAuditHookForTest(nil) })

	return l
}

func (l *auditLog) of(id string) []apiv2.TenantWrite {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.recs[id])
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// p12dAPI lifts the request and config-write buckets, so a test making
// many calls in a row measures behaviour, not rate limits (those have
// their own tests).
var p12dAPI = &types.TenantAPIConfig{
	GlobalRate: 10000, GlobalBurst: 10000,
	TailnetRate: 10000, TailnetBurst: 10000,
	KeyRate: 10000, KeyBurst: 10000,
	PreauthRate: 10000, PreauthBurst: 10000,
	ConfigWriteRate: 60000, ConfigWriteBurst: 1000,
}

// p12dMesh is twoTailnetMesh with p12dAPI.
func p12dMesh(t *testing.T) (*servertest.TestCluster, []*servertest.TestClient, []*servertest.TestClient) {
	t.Helper()

	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	tc.SetPolicy(t, tenantB, allowAllWithSSH)

	aClients := enroll(t, tc.Tenant(t, tenantA), "alice", "p12d-a1", "p12d-a2", "p12d-a3")
	bClients := enroll(t, tc.Tenant(t, tenantB), "bob", "p12d-b1", "p12d-b2")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, true)
		}
	}

	return tc, aClients, bClients
}

const (
	// Port 22 only, grants form: a1's filter shows the write landed.
	p12dPort22Policy = `{"grants":[{"src":["*"],"dst":["*"],"ip":["tcp:22"]}]}`
	// The same with a test that fails: refused, as Tailscale does.
	p12dFailingTestsPolicy = `{"grants":[{"src":["*"],"dst":["*"],"ip":["tcp:22"]}],
		"tests":[{"src":"alice@","accept":["alice@:80"]}]}`
)

func TestP12dTenantPolicyWrite(t *testing.T) {
	tc, aClients, bClients := p12dMesh(t)
	audit := captureAudit(t, tc)
	idA, idB := tc.Tailnet(t, tenantA).ID.String(), tc.Tailnet(t, tenantB).ID.String()
	keyA, keyB := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantB)
	a1 := aClients[0]

	revA0, revB0 := tailnetRevisions(t, tc, idA), tailnetRevisions(t, tc, idB)
	polB0 := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyB, "", nil)
	require.Equal(t, http.StatusOK, polB0.code)

	before := settledFrames(t, bClients)

	g := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", nil)
	require.Equal(t, http.StatusOK, g.code, g.body)
	etag0 := g.hdr.Get("ETag")
	require.NotEmpty(t, etag0)

	// A's key naming B's tailnet: 404, like a tailnet that does not exist.
	for _, path := range []string{"/api/v2/tailnet/" + idB + "/acl", "/api/v2/tailnet/" + idB + "/acl/validate"} {
		r := apiDo(t, tc.URL, http.MethodPost, path, keyA, `{}`, nil)
		assert.Equal(t, http.StatusNotFound, r.code, "POST %s with A's key: %s", path, r.body)
	}

	// The key's own tailnet, by ID, with the current ETag: stored.
	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/"+idA+"/acl", keyA, p12dPort22Policy,
		map[string]string{"If-Match": etag0})
	require.Equal(t, http.StatusOK, r.code, r.body)
	etag1 := r.hdr.Get("ETag")
	assert.NotEqual(t, etag0, etag1)

	a1.WaitForCondition(t, "a1 gets the port-22-only filter", clusterWait, onlyPort22)

	revA1 := tailnetRevisions(t, tc, idA)
	assert.Equal(t, revA0[0]+1, revA1[0], "policy_revision of A bumped once")
	assert.Equal(t, revA0[1], revA1[1], "config_revision of A untouched")

	recs := audit.of(idA)
	require.Len(t, recs, 1, "one audit record per stored write")
	assert.Equal(t, "setACL", recs[0].Op)
	assert.NotZero(t, recs[0].KeyID)
	assert.Equal(t, revA1[0], recs[0].Revision)
	assert.Equal(t, sha256Hex(p12dPort22Policy), recs[0].SHA256)

	stored := func() string {
		t.Helper()

		g := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", map[string]string{"Accept": "application/hujson"})
		require.Equal(t, http.StatusOK, g.code)

		return g.body
	}

	// Refused writes: the stored and enforced policy and the revision stay.
	for _, c := range []struct {
		name string
		body string
		hdr  map[string]string
		code int
	}{
		{"stale ETag", `{"grants":[]}`, map[string]string{"If-Match": etag0}, http.StatusPreconditionFailed},
		{"does not parse", `{"grants": [`, nil, http.StatusBadRequest},
		{"unknown field", `{"grantz": []}`, nil, http.StatusBadRequest},
		{"failing tests", p12dFailingTestsPolicy, nil, http.StatusBadRequest},
		{"empty body", "", nil, http.StatusBadRequest},
		{"over max_policy_bytes", `{"grants":[],"x":"` + strings.Repeat("a", 300<<10) + `"}`, nil, http.StatusRequestEntityTooLarge},
	} {
		r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, c.body, c.hdr)
		assert.Equal(t, c.code, r.code, "%s: %s", c.name, r.body)
		assert.Equal(t, p12dPort22Policy, stored(), "%s: stored policy unchanged", c.name)
		assert.Equal(t, revA1, tailnetRevisions(t, tc, idA), "%s: no revision moved", c.name)
		assert.True(t, onlyPort22(a1.Netmap()), "%s: a1 keeps the port-22 filter", c.name)
	}

	assert.Len(t, audit.of(idA), 1, "refused writes are not audited as stored")

	// An empty policy denies at once (AGENTS rule 8).
	r = apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, `{}`, map[string]string{"If-Match": etag1})
	require.Equal(t, http.StatusOK, r.code, r.body)

	for _, c := range aClients {
		c.WaitForCondition(t, "deny-all after {}", clusterWait, func(nm *netmap.NetworkMap) bool {
			return len(nm.Peers) == 0 && nm.PacketFilterRules.Len() == 0 && len(nm.PacketFilter) == 0
		})
	}

	assert.Equal(t, `{}`, stored())
	assert.Equal(t, revA1[0]+1, tailnetRevisions(t, tc, idA)[0])

	// B saw nothing and nothing of B changed.
	assertQuiet(t, bClients, before, "A's policy writes")
	assert.Equal(t, revB0, tailnetRevisions(t, tc, idB), "B's revisions")

	polB1 := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyB, "", nil)
	assert.Equal(t, polB0.body, polB1.body, "B's policy")
	assert.Empty(t, audit.of(idB))

	for _, rec := range audit.of(idA) {
		assert.NotContains(t, fmt.Sprintf("%+v", rec), "grants", "the audit record never holds the policy")
	}
}

// With no stored policy a tenant tailnet denies everything; the default a
// tenant reads must say so, never the upstream allow-all default.
func TestP12dTenantDefaultPolicyIsDeny(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicyViaDB(t, tenantA, "")
	keyA := tc.MintAPIKey(t, tenantA)

	r := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", map[string]string{"Accept": "application/hujson"})
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.NotContains(t, r.body, `"*:*"`, "never the allow-all default")
	assert.NotContains(t, r.body, `"accept"`)
	assert.Contains(t, r.body, `"grants"`)

	// The validate endpoint agrees that the default denies: it is a valid policy.
	v := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl/validate", keyA, r.body, nil)
	require.Equal(t, http.StatusOK, v.code, v.body)
	assert.NotContains(t, v.body, "message")

	// ts-default matches the untouched default.
	w := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, `{"grants":[]}`,
		map[string]string{"If-Match": `"ts-default"`})
	assert.Equal(t, http.StatusOK, w.code, w.body)
}

func TestP12dTenantConfigWriteRateLimit(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{
		TenantAPI: &types.TenantAPIConfig{
			GlobalRate: 1000, GlobalBurst: 1000,
			TailnetRate: 1000, TailnetBurst: 1000,
			KeyRate: 1000, KeyBurst: 1000,
			PreauthRate: 1000, PreauthBurst: 1000,
			ConfigWriteRate: 1, ConfigWriteBurst: 2,
		},
	}, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})

	idB := tc.Tailnet(t, tenantB).ID.String()
	keyA, keyA2, keyB := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantB)

	validate := func(key, tn string) apiFull {
		return apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/"+tn+"/acl/validate", key, `{"grants":[]}`, nil)
	}

	assert.Equal(t, http.StatusOK, validate(keyA, "-").code)
	assert.Equal(t, http.StatusOK,
		apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/dns/searchpaths", keyA, `{"searchPaths":["corp.example"]}`, nil).code)

	// A's config-write budget is spent, whichever key or endpoint.
	r := validate(keyA2, "-")
	assert.Equal(t, http.StatusTooManyRequests, r.code, r.body)
	assert.NotEmpty(t, r.hdr.Get("Retry-After"))
	assert.Equal(t, http.StatusTooManyRequests,
		apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, `{"grants":[]}`, nil).code)
	assert.Equal(t, http.StatusTooManyRequests,
		apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/dns/nameservers", keyA, `{"dns":["9.9.9.9"]}`, nil).code)

	// Reads and other writes are not config writes.
	assert.Equal(t, http.StatusOK, apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", nil).code)
	assert.Equal(t, http.StatusOK, apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/dns/searchpaths", keyA, "", nil).code)

	// Another tailnet's ID is still 404, never 429 (no existence oracle).
	assert.Equal(t, http.StatusNotFound, validate(keyA, idB).code)

	// B has its own budget.
	assert.Equal(t, http.StatusOK, validate(keyB, "-").code)
	assert.Equal(t, http.StatusOK, validate(keyB, "-").code)
}

// dnsResolvers is every resolver address a client was given (global or
// fallback).
func dnsResolvers(nm *netmap.NetworkMap) []string {
	var out []string

	for _, r := range append(slices.Clone(nm.DNS.Resolvers), nm.DNS.FallbackResolvers...) {
		out = append(out, r.Addr)
	}

	return out
}

func splitResolvers(nm *netmap.NetworkMap, domain string) []string {
	var out []string

	for d, rs := range nm.DNS.Routes {
		if strings.TrimSuffix(d, ".") != domain {
			continue
		}

		for _, r := range rs {
			out = append(out, r.Addr)
		}
	}

	return out
}

func TestP12dTenantDNS(t *testing.T) {
	tc, aClients, bClients := p12dMesh(t)
	audit := captureAudit(t, tc)
	idA, idB := tc.Tailnet(t, tenantA).ID.String(), tc.Tailnet(t, tenantB).ID.String()
	keyA := tc.MintAPIKey(t, tenantA)
	a1 := aClients[0]

	revA0, revB0 := tailnetRevisions(t, tc, idA), tailnetRevisions(t, tc, idB)
	before := settledFrames(t, bClients)

	call := func(method, path, body string) apiFull {
		t.Helper()

		return apiDo(t, tc.URL, method, "/api/v2/tailnet/-/dns/"+path, keyA, body, nil)
	}

	jsonEq := func(want string, r apiFull, msg string) {
		t.Helper()
		require.Equal(t, http.StatusOK, r.code, "%s: %s", msg, r.body)
		assert.JSONEq(t, want, r.body, msg)
	}

	// Defaults (ADR 0008 §2): MagicDNS on, nothing else.
	jsonEq(`{"dns":[]}`, call(http.MethodGet, "nameservers", ""), "GET nameservers")
	jsonEq(`{"searchPaths":[]}`, call(http.MethodGet, "searchpaths", ""), "GET searchpaths")
	jsonEq(`{}`, call(http.MethodGet, "split-dns", ""), "GET split-dns")
	jsonEq(`{"magicDNS":true}`, call(http.MethodGet, "preferences", ""), "GET preferences")

	// Nameservers.
	jsonEq(`{"dns":["9.9.9.9"],"magicDNS":true}`, call(http.MethodPost, "nameservers", `{"dns":["9.9.9.9"]}`), "POST nameservers")
	a1.WaitForCondition(t, "a1 gets the nameserver", clusterWait, func(nm *netmap.NetworkMap) bool {
		return slices.Contains(dnsResolvers(nm), "9.9.9.9")
	})
	jsonEq(`{"dns":["9.9.9.9"]}`, call(http.MethodGet, "nameservers", ""), "GET nameservers after POST")

	// Search paths.
	jsonEq(`{"searchPaths":["corp.example"]}`, call(http.MethodPost, "searchpaths", `{"searchPaths":["corp.example"]}`), "POST searchpaths")
	a1.WaitForCondition(t, "a1 gets the search path", clusterWait, func(nm *netmap.NetworkMap) bool {
		return slices.Contains(nm.DNS.Domains, "corp.example")
	})

	// Split DNS: PATCH merges, null removes, PUT replaces, {} clears.
	jsonEq(`{"corp.example":["192.168.77.53"],"lab.example":["10.0.0.53"]}`,
		call(http.MethodPatch, "split-dns", `{"corp.example":["192.168.77.53"],"lab.example":["10.0.0.53"]}`), "PATCH split-dns")
	jsonEq(`{"corp.example":["192.168.77.53"]}`, call(http.MethodPatch, "split-dns", `{"lab.example":null}`), "PATCH null removes")
	jsonEq(`{"x.example":["10.0.0.1"]}`, call(http.MethodPut, "split-dns", `{"x.example":["10.0.0.1"]}`), "PUT replaces")
	jsonEq(`{}`, call(http.MethodPut, "split-dns", `{}`), "PUT {} clears")
	jsonEq(`{"corp.example":["192.168.77.53"]}`, call(http.MethodPut, "split-dns", `{"corp.example":["192.168.77.53"]}`), "PUT corp")
	jsonEq(`{"corp.example":["192.168.77.53"]}`, call(http.MethodGet, "split-dns", ""), "GET split-dns")
	a1.WaitForCondition(t, "a1 resolves corp.example through the subnet's DNS server", clusterWait, func(nm *netmap.NetworkMap) bool {
		return slices.Contains(splitResolvers(nm, "corp.example"), "192.168.77.53")
	})

	// Preferences.
	jsonEq(`{"magicDNS":false}`, call(http.MethodPost, "preferences", `{"magicDNS":false}`), "MagicDNS off")
	a1.WaitForCondition(t, "a1 without MagicDNS", clusterWait, func(nm *netmap.NetworkMap) bool { return !nm.DNS.Proxied })
	jsonEq(`{"magicDNS":true}`, call(http.MethodPost, "preferences", `{"magicDNS":true}`), "MagicDNS on")
	a1.WaitForCondition(t, "a1 with MagicDNS", clusterWait, func(nm *netmap.NetworkMap) bool { return nm.DNS.Proxied })

	writes := 9
	revA1 := tailnetRevisions(t, tc, idA)
	assert.Equal(t, revA0[0], revA1[0], "policy_revision untouched")
	assert.Equal(t, revA0[1]+uint64(writes), revA1[1], "config_revision bumped once per write")

	recs := audit.of(idA)
	require.Len(t, recs, writes)
	assert.Equal(t, revA1[1], recs[len(recs)-1].Revision)

	for _, rec := range recs {
		assert.NotZero(t, rec.KeyID)
		assert.Len(t, rec.SHA256, 64)
		assert.NotContains(t, fmt.Sprintf("%+v", rec), "corp.example", "never the content")
	}

	// Refused (ADR 0008 §3): nothing stored, no revision, a1 keeps its DNS.
	inCluster := "x." + servertest.ClusterBaseDomain
	ownBase := servertest.ClusterTailnetDomain(tenantA)
	otherBase := servertest.ClusterTailnetDomain(tenantB)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "split-dns", `{"` + inCluster + `":["10.0.0.1"]}`},
		{http.MethodPatch, "split-dns", `{"` + ownBase + `":["10.0.0.1"]}`},
		{http.MethodPut, "split-dns", `{"` + otherBase + `":["10.0.0.1"]}`},
		{http.MethodPatch, "split-dns", `{"corp.example":["not-an-ip"]}`},
		{http.MethodPatch, "split-dns", `{"bad_domain!":["10.0.0.1"]}`},
		{http.MethodPatch, "split-dns", `["corp.example"]`},
		{http.MethodPost, "nameservers", `{"dns":["not-an-ip"]}`},
		{http.MethodPost, "nameservers", `{"dns":["http://dns.example/q"]}`},
		{http.MethodPost, "nameservers", `{"dns":"9.9.9.9"}`},
		{http.MethodPost, "nameservers", `{"dns":[` + strings.Repeat(`"9.9.9.9",`, 64) + `"9.9.9.9"]}`},
		{http.MethodPost, "searchpaths", `{"searchPaths":["bad_domain!"]}`},
		{http.MethodPost, "preferences", `{"magicDNS":"yes"}`},
	} {
		r := call(c.method, c.path, c.body)
		assert.Equal(t, http.StatusBadRequest, r.code, "%s %s %s: %s", c.method, c.path, c.body, r.body)
	}

	assert.Equal(t, revA1, tailnetRevisions(t, tc, idA), "refused writes move no revision")
	assert.Len(t, audit.of(idA), writes, "refused writes are not audited as stored")
	jsonEq(`{"corp.example":["192.168.77.53"]}`, call(http.MethodGet, "split-dns", ""), "split-dns after refusals")
	jsonEq(`{"dns":["9.9.9.9"]}`, call(http.MethodGet, "nameservers", ""), "nameservers after refusals")
	assert.Contains(t, splitResolvers(a1.Netmap(), "corp.example"), "192.168.77.53")

	// A's key naming B's tailnet: 404 for reads and writes.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "nameservers", ""},
		{http.MethodPost, "nameservers", `{"dns":["1.1.1.1"]}`},
		{http.MethodGet, "split-dns", ""},
		{http.MethodPatch, "split-dns", `{"corp.example":["10.0.0.1"]}`},
		{http.MethodPut, "split-dns", `{}`},
		{http.MethodPost, "searchpaths", `{"searchPaths":["b.example"]}`},
		{http.MethodPost, "preferences", `{"magicDNS":false}`},
	} {
		r := apiDo(t, tc.URL, c.method, "/api/v2/tailnet/"+idB+"/dns/"+c.path, keyA, c.body, nil)
		assert.Equal(t, http.StatusNotFound, r.code, "%s %s on B: %s", c.method, c.path, r.body)
	}

	// B: no frame, no revision, no settings.
	assertQuiet(t, bClients, before, "A's DNS writes")
	assert.Equal(t, revB0, tailnetRevisions(t, tc, idB))

	dnsB, err := tc.TailnetDNS(t, tenantB)
	require.NoError(t, err)
	assert.Nil(t, dnsB, "B keeps the defaults")

	for _, c := range bClients {
		nm := c.Netmap()
		assert.NotContains(t, dnsResolvers(nm), "9.9.9.9")
		assert.Empty(t, splitResolvers(nm, "corp.example"))
		assert.NotContains(t, nm.DNS.Domains, "corp.example")
	}

	// The operator sees what the tenant stored.
	dnsA, err := tc.TailnetDNS(t, tenantA)
	require.NoError(t, err)
	require.NotNil(t, dnsA)
	assert.Equal(t, []string{"9.9.9.9"}, dnsA.Nameservers.Global)
	assert.Equal(t, []string{"corp.example"}, dnsA.SearchDomains)
}

// Tailscale recommends disabling key expiry on subnet routers; a tenant may
// do it on its own devices only. The other device trust settings stay with
// the operator.
func TestP12dTenantDeviceKeyExpiry(t *testing.T) {
	tc, aClients, bClients := p12dMesh(t)
	a := tc.Tenant(t, tenantA)
	keyA, keyB := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantB)
	id1 := nodeIDOf(t, a, aClients[0])
	path := fmt.Sprintf("/api/v2/device/%d/key", id1)

	before := settledFrames(t, bClients)

	r := apiDo(t, tc.URL, http.MethodPost, path, keyB, `{"keyExpiryDisabled":true}`, nil)
	assert.Equal(t, http.StatusNotFound, r.code, "B's key on A's device: %s", r.body)

	nv, ok := a.State().GetNodeByID(id1)
	require.True(t, ok)
	assert.False(t, nv.KeyExpiryDisabled(), "B's key changed nothing")

	r = apiDo(t, tc.URL, http.MethodPost, path, keyA, `{"keyExpiryDisabled":true}`, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)

	nv, ok = a.State().GetNodeByID(id1)
	require.True(t, ok)
	assert.True(t, nv.KeyExpiryDisabled())

	g := apiDo(t, tc.URL, http.MethodGet, fmt.Sprintf("/api/v2/device/%d", id1), keyA, "", nil)
	require.Equal(t, http.StatusOK, g.code)
	assert.Contains(t, g.body, `"keyExpiryDisabled":true`)

	r = apiDo(t, tc.URL, http.MethodPost, path, keyA, `{"keyExpiryDisabled":false}`, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)

	nv, _ = a.State().GetNodeByID(id1)
	assert.False(t, nv.KeyExpiryDisabled())

	// Still the operator's: authorize, client update, tailnet settings.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, fmt.Sprintf("/api/v2/device/%d/authorized", id1), `{"authorized":true}`},
		{http.MethodPost, fmt.Sprintf("/api/v2/device/%d/update-client", id1), ""},
		{http.MethodPatch, "/api/v2/tailnet/-/settings", `{"devicesApprovalOn":true}`},
	} {
		r := apiDo(t, tc.URL, c.method, c.path, keyA, c.body, nil)
		assert.Equal(t, http.StatusForbidden, r.code, "%s %s: %s", c.method, c.path, r.body)
	}

	assertQuiet(t, bClients, before, "A's device key writes")
}

// The Subnet routers workflow end to end in one tailnet, with the API key
// only: the tenant's policy grants the subnet and auto-approves it for
// tag:router, a tagged auth key from the API enrolls two routers, both
// routes (IPv4 and IPv6) are approved without an approval call, and the
// route fails over to the second router when the first goes away. B
// advertises the same prefix and gets nothing of A, and A's autoApprovers
// approve nothing in B.
func TestP12dSubnetRouterAutoApproveHA(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI},
		servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	keyA, keyB := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantB)
	route4 := netip.MustParsePrefix("192.168.77.0/24")
	route6 := netip.MustParsePrefix("fd00:77::/64")

	a1 := enroll(t, a, "alice", "p12d-a1")[0]

	policy := `{
		"tagOwners": {"tag:router": ["alice@"]},
		"grants": [
			{"src": ["*"], "dst": ["*"], "ip": ["*"]},
			{"src": ["autogroup:member"], "dst": ["192.168.77.0/24", "fd00:77::/64"], "ip": ["*"]}
		],
		"autoApprovers": {"routes": {"192.168.77.0/24": ["tag:router"], "fd00:77::/64": ["tag:router"]}},
		"tests": [{"src": "alice@", "accept": ["192.168.77.2:7777"]}]
	}`
	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, policy, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)

	r = apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/keys", keyA,
		`{"capabilities":{"devices":{"create":{"reusable":true,"ephemeral":false,"preauthorized":true,"tags":["tag:router"]}}}}`, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)

	var created struct {
		Key string `json:"key"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &created))

	r1 := servertest.NewClusterClient(t, a, "p12d-router-1", servertest.ClusterClientOptions{AuthKey: created.Key})
	r2 := servertest.NewClusterClient(t, a, "p12d-router-2", servertest.ClusterClientOptions{AuthKey: created.Key})
	routes := []netip.Prefix{route4, route6}

	advertiseRoutes(t, r1, routes)
	advertiseRoutes(t, r2, routes)

	// Both routers' routes are approved by the policy: no approval call.
	for _, rc := range []*servertest.TestClient{r1, r2} {
		id := nodeIDOf(t, a, rc)
		require.Eventually(t, func() bool {
			g := apiDo(t, tc.URL, http.MethodGet, fmt.Sprintf("/api/v2/device/%d/routes", id), keyA, "", nil)

			var dr apiv2.DeviceRoutes
			if g.code != http.StatusOK || json.Unmarshal([]byte(g.body), &dr) != nil {
				return false
			}

			return slices.Contains(dr.Enabled, route4.String()) && slices.Contains(dr.Enabled, route6.String())
		}, clusterWait, 100*time.Millisecond, "%s's routes auto-approved", rc.Name)

		// B's key cannot read or approve A's router.
		assert.Equal(t, http.StatusNotFound,
			apiDo(t, tc.URL, http.MethodGet, fmt.Sprintf("/api/v2/device/%d/routes", id), keyB, "", nil).code)
		assert.Equal(t, http.StatusNotFound,
			apiDo(t, tc.URL, http.MethodPost, fmt.Sprintf("/api/v2/device/%d/routes", id), keyB, `{"routes":[]}`, nil).code)
	}

	// Disable key expiry on the routers, as Tailscale recommends.
	for _, rc := range []*servertest.TestClient{r1, r2} {
		id := nodeIDOf(t, a, rc)
		g := apiDo(t, tc.URL, http.MethodPost, fmt.Sprintf("/api/v2/device/%d/key", id), keyA, `{"keyExpiryDisabled":true}`, nil)
		assert.Equal(t, http.StatusOK, g.code, g.body)
	}

	primaryOf := func(nm *netmap.NetworkMap) string {
		for _, p := range nm.Peers {
			if slices.Contains(p.PrimaryRoutes().AsSlice(), route4) && slices.Contains(p.PrimaryRoutes().AsSlice(), route6) {
				return p.Hostinfo().Hostname()
			}
		}

		return ""
	}

	var primary string

	a1.WaitForCondition(t, "a1 gets both routes through one router", clusterWait, func(nm *netmap.NetworkMap) bool {
		primary = primaryOf(nm)

		return primary != ""
	})

	// HA: the primary goes away, the other router takes the routes.
	gone, other := r1, r2
	if primary == r2.Name {
		gone, other = r2, r1
	}

	gone.Disconnect(t)
	a1.WaitForCondition(t, "a1 fails over to the other router", 60*time.Second, func(nm *netmap.NetworkMap) bool {
		return primaryOf(nm) == other.Name
	})

	// B advertises the same prefix: A's autoApprovers approve nothing in B,
	// and B's client never sees A's routers.
	bClients := enroll(t, b, "bob", "p12d-b1", "p12d-b-router")
	advertiseRoutes(t, bClients[1], routes)

	idBR := nodeIDOf(t, b, bClients[1])
	assert.Never(t, func() bool {
		nv, ok := b.State().GetNodeByID(idBR)

		return ok && nv.ApprovedRoutes().Len() > 0
	}, 2*time.Second, 100*time.Millisecond, "B's router is not approved by A's policy")

	for _, p := range bClients[0].Netmap().Peers {
		assert.NotContains(t, []string{r1.Name, r2.Name, a1.Name}, p.Hostinfo().Hostname())
	}
}

// Policy and DNS writes need the process that holds the cluster lease and
// a tailnet it serves: a standby answers 503, a tailnet owned by another
// cluster 404.
func TestP12dTenantWritesStandbyAndForeign(t *testing.T) {
	tc := newHACluster(t)
	key := tc.MintAPIKey(t, tenantA)
	standby := tc.StartProcess(t)
	hdr := http.Header{"Authorization": {"Bearer " + key}}

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v2/tailnet/-/acl"},
		{http.MethodPost, "/api/v2/tailnet/-/dns/nameservers"},
		{http.MethodPatch, "/api/v2/tailnet/-/dns/split-dns"},
		{http.MethodPost, "/api/v2/device/1/key"},
	} {
		assert.Equal(t, http.StatusServiceUnavailable, tc.ProcessDo(t, standby, c.method, c.path, hdr), "standby %s %s", c.method, c.path)
	}

	foreign := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA},
		servertest.TailnetSeed{Slug: "foreign", Owner: "some-other-cluster"})
	keyF := foreign.MintAPIKey(t, "foreign")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v2/tailnet/-/acl", `{"grants":[]}`},
		{http.MethodPost, "/api/v2/tailnet/-/dns/nameservers", `{"dns":["9.9.9.9"]}`},
		{http.MethodPut, "/api/v2/tailnet/-/dns/split-dns", `{}`},
	} {
		r := apiDo(t, foreign.URL, c.method, c.path, keyF, c.body, nil)
		assert.Equal(t, http.StatusNotFound, r.code, "not served %s %s: %s", c.method, c.path, r.body)
	}
}

// The official Tailscale Go SDK drives a tenant's policy and DNS
// unchanged.
func TestP12dTenantGoSDK(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI}, servertest.TailnetSeed{Slug: tenantA})
	keyA := tc.MintAPIKey(t, tenantA)
	ctx := t.Context()
	client := goClient(t, tc.URL, keyA)

	pf := client.PolicyFile()
	raw, err := pf.Raw(ctx)
	require.NoError(t, err)
	require.NoError(t, pf.Set(ctx, p12dPort22Policy, raw.ETag))

	got, err := pf.Raw(ctx)
	require.NoError(t, err)
	assert.Equal(t, p12dPort22Policy, got.HuJSON)
	require.Error(t, pf.Set(ctx, `{"grants":[]}`, raw.ETag), "stale ETag refused")

	dns := client.DNS()
	require.NoError(t, dns.SetNameservers(ctx, []string{"9.9.9.9", "1.1.1.1"}))
	ns, err := dns.Nameservers(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"9.9.9.9", "1.1.1.1"}, ns)

	require.NoError(t, dns.SetSearchPaths(ctx, []string{"corp.example"}))
	sp, err := dns.SearchPaths(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"corp.example"}, sp)

	require.NoError(t, dns.SetSplitDNS(ctx, map[string][]string{"corp.example": {"192.168.77.53"}, "lab.example": {"10.0.0.53"}}))
	split, err := dns.UpdateSplitDNS(ctx, map[string][]string{"lab.example": nil})
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"corp.example": {"192.168.77.53"}}, map[string][]string(split))

	split, err = dns.SplitDNS(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"corp.example": {"192.168.77.53"}}, map[string][]string(split))

	require.NoError(t, dns.SetPreferences(ctx, tsclient.DNSPreferences{MagicDNS: false}))
	prefs, err := dns.Preferences(ctx)
	require.NoError(t, err)
	assert.False(t, prefs.MagicDNS)

	stored, err := tc.TailnetDNS(t, tenantA)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, []string{"9.9.9.9", "1.1.1.1"}, stored.Nameservers.Global)
	assert.Equal(t, map[string][]string{"corp.example": {"192.168.77.53"}}, stored.Nameservers.Split)
	require.NotNil(t, stored.MagicDNS)
	assert.False(t, *stored.MagicDNS)
}
