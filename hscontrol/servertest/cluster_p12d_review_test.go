//go:build p05cluster

package servertest_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/netmap"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P12d security review fixes (2026-11-26). Each finding is confined to the
// tenant's own data; these tests pin the fixed behaviour.

// reconcileTick is the cluster's reconcile interval (hscontrol
// reconcileInterval).
const reconcileTick = 2 * time.Second

var errInjectedInstall = errors.New("injected install failure: compiler internals")

// rawResult is an HTTP answer read without the test's goroutine (no
// require in goroutines).
type rawResult struct {
	code int
	body string
	err  error
}

func rawDo(base, method, path, key, body string) rawResult {
	req, err := http.NewRequest(method, base+path, strings.NewReader(body)) //nolint:noctx // bounded by the client timeout
	if err != nil {
		return rawResult{err: err}
	}

	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return rawResult{err: err}
	}

	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return rawResult{code: resp.StatusCode, body: string(b)}
}

// assertNoInternals checks that an error body carries no detail beyond
// its message: no data list, none of the given internals.
func assertNoInternals(t *testing.T, body string, internals ...string) {
	t.Helper()

	assert.NotContains(t, body, `"data"`, "no error details: %s", body)

	for _, s := range internals {
		if s != "" {
			assert.NotContains(t, body, s, "body leaks %q: %s", s, body)
		}
	}
}

// Finding 1 (MEDIUM): the reconcile loop read a runtime's DNS settings,
// then a tenant write stored and installed newer ones, then the reconcile
// installed what it had read, and never reloaded (its revision equalled
// the database). The read and the install are now one step under the
// same lock as tenant writes, and an already installed revision is not
// installed again: the runtime always ends with the newest settings.
func TestP12dReviewReconcileDNSRaceKeepsNewest(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI}, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, tc.Tenant(t, tenantA), "alice", "race-a1")[0]
	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID

	patched := make(chan rawResult, 1)

	var once sync.Once

	tc.Cluster.SetReconcileDNSHookForTest(func(id tailnet.ID) {
		if id != idA {
			return
		}

		once.Do(func() {
			// The tenant writes while the reconcile holds what it read.
			// The write either completes meanwhile (the race) or waits
			// for the reconcile to install first; give it the time to.
			go func() {
				patched <- rawDo(tc.URL, http.MethodPost, "/api/v2/tailnet/-/dns/nameservers", keyA, `{"dns":["2.2.2.2"]}`)
			}()

			select {
			case r := <-patched:
				patched <- r
			case <-time.After(2 * time.Second):
			}
		})
	})
	t.Cleanup(func() { tc.Cluster.SetReconcileDNSHookForTest(nil) })

	// The operator stores 1.1.1.1; the reconcile loads it and, in the
	// hook, the tenant stores 2.2.2.2.
	require.NoError(t, tc.SetTailnetDNS(t, tenantA, &types.TenantDNS{
		Nameservers: types.TenantNameservers{Global: []string{"1.1.1.1"}},
	}))

	var r rawResult
	select {
	case r = <-patched:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconcile never loaded the operator's settings")
	}

	require.NoError(t, r.err)
	require.Equal(t, http.StatusOK, r.code, r.body)

	stored, err := tc.TailnetDNS(t, tenantA)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, []string{"2.2.2.2"}, stored.Nameservers.Global, "stored: the tenant's write")

	// Let a stale install land, if any, and more reconcile ticks pass.
	time.Sleep(2*reconcileTick + 500*time.Millisecond) //nolint:forbidigo // waiting out reconcile ticks

	a1.WaitForCondition(t, "a1 ends with the newest nameserver", clusterWait, func(nm *netmap.NetworkMap) bool {
		rs := dnsResolvers(nm)

		return slices.Contains(rs, "2.2.2.2") && !slices.Contains(rs, "1.1.1.1")
	})

	time.Sleep(2*reconcileTick + 500*time.Millisecond) //nolint:forbidigo // the reconcile must not reinstall older settings

	rs := dnsResolvers(a1.Netmap())
	assert.Contains(t, rs, "2.2.2.2")
	assert.NotContains(t, rs, "1.1.1.1", "older settings never come back")
}

// Finding 2 (LOW-MEDIUM): a policy stored and its revision bumped, but
// not installed afterwards, was answered with a bare 500, not audited,
// and the answer did not say the write was stored. The rule now: a refused
// write (4xx) stores nothing, so nothing can install it later; a stored
// write is audited, and when it could not be put in force at once the
// answer says it is stored (with its ETag), the previous policy stays
// enforced, and the cluster installs the stored one on its next reconcile.
func TestP12dReviewPolicyStoredNotInForce(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI}, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	audit := captureAudit(t, tc)

	a1 := enroll(t, tc.Tenant(t, tenantA), "alice", "nif-a1", "nif-a2")[0]
	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID.String()

	var fail atomic.Bool

	fail.Store(true)

	st := tc.Runtime(t, tenantA).GetState()
	st.SetPolicyInstallHookForTest(func() error {
		if fail.Swap(false) {
			return errInjectedInstall
		}

		return nil
	})
	t.Cleanup(func() { st.SetPolicyInstallHookForTest(nil) })

	rev0 := tailnetRevisions(t, tc, idA)

	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, p12dPort22Policy, nil)
	assert.Equal(t, http.StatusInternalServerError, r.code, r.body)
	assert.Contains(t, r.body, "stored", "the answer says the write is stored")
	assert.Contains(t, r.body, "not in force")
	assertNoInternals(t, r.body, "compiler internals")
	assert.Equal(t, `"`+sha256Hex(p12dPort22Policy)+`"`, r.hdr.Get("ETag"), "the stored policy's ETag")

	rev1 := tailnetRevisions(t, tc, idA)
	assert.Equal(t, rev0[0]+1, rev1[0], "stored: policy_revision bumped once")

	recs := audit.of(idA)
	require.Len(t, recs, 1, "the stored write is audited")
	assert.Equal(t, "setACL", recs[0].Op)
	assert.Equal(t, rev1[0], recs[0].Revision)
	assert.Equal(t, sha256Hex(p12dPort22Policy), recs[0].SHA256)
	assert.True(t, recs[0].NotInForce)

	g := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", map[string]string{"Accept": "application/hujson"})
	require.Equal(t, http.StatusOK, g.code)
	assert.Equal(t, p12dPort22Policy, g.body)

	// What is stored is what will be enforced: the reconcile installs it.
	a1.WaitForCondition(t, "the stored policy comes in force", clusterWait, onlyPort22)

	// A refused write (400) stores and bumps nothing: no reconcile can
	// ever install it.
	r = apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, p12dFailingTestsPolicy, nil)
	assert.Equal(t, http.StatusBadRequest, r.code, r.body)
	assert.Equal(t, rev1, tailnetRevisions(t, tc, idA))

	time.Sleep(reconcileTick + 500*time.Millisecond) //nolint:forbidigo // a reconcile tick passes
	assert.True(t, onlyPort22(a1.Netmap()), "the refused policy never comes in force")
	assert.Len(t, audit.of(idA), 1)
}

// slowLease renews rarely, so a lease ended in the database is not noticed
// by the process during the test: the write reaches the database fence.
var slowLease = &types.ClusterLeaseConfig{
	TTL: 10 * time.Minute, RenewInterval: 4 * time.Minute, AcquireInterval: 200 * time.Millisecond, SafetyMargin: time.Minute,
}

// Missing test (review) and finding 4: the owner and epoch of the tailnet
// still match, but the cluster lease is gone. Every tenant write is 503,
// stores nothing and bumps nothing, and the answer carries no platform
// internals (cluster, holder, epoch).
func TestP12dReviewWriteAfterLeaseLost(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI, Lease: slowLease},
		servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)
	audit := captureAudit(t, tc)

	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID.String()
	holder := tc.LeaseRow(t).Holder

	rev0 := tailnetRevisions(t, tc, idA)
	pol0 := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", map[string]string{"Accept": "application/hujson"})
	require.Equal(t, http.StatusOK, pol0.code)

	tc.ExpireLease(t)
	require.True(t, tc.Cluster.Ready(), "the process has not noticed: the database fence refuses")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v2/tailnet/-/acl", p12dPort22Policy},
		{http.MethodPost, "/api/v2/tailnet/-/dns/nameservers", `{"dns":["9.9.9.9"]}`},
		{http.MethodPost, "/api/v2/tailnet/-/dns/searchpaths", `{"searchPaths":["corp.example"]}`},
		{http.MethodPatch, "/api/v2/tailnet/-/dns/split-dns", `{"corp.example":["10.0.0.53"]}`},
		{http.MethodPut, "/api/v2/tailnet/-/dns/split-dns", `{}`},
		{http.MethodPost, "/api/v2/tailnet/-/dns/preferences", `{"magicDNS":false}`},
	} {
		r := apiDo(t, tc.URL, c.method, c.path, keyA, c.body, nil)
		assert.Equal(t, http.StatusServiceUnavailable, r.code, "%s %s: %s", c.method, c.path, r.body)
		assertNoInternals(t, r.body, "lease", "epoch", "fence", holder, tc.ClusterID, idA)
	}

	assert.Equal(t, rev0, tailnetRevisions(t, tc, idA), "nothing bumped")

	pol1 := apiDo(t, tc.URL, http.MethodGet, "/api/v2/tailnet/-/acl", keyA, "", map[string]string{"Accept": "application/hujson"})
	assert.Equal(t, pol0.body, pol1.body, "policy unchanged")

	dns, err := tc.TailnetDNS(t, tenantA)
	require.NoError(t, err)
	assert.Nil(t, dns, "DNS unchanged (defaults)")
	assert.Empty(t, audit.of(idA), "nothing stored, nothing audited")
}

// Finding 4: a 500 of the tenant API never echoes database or parser
// internals; a hand-broken stored value is the operator's problem, not a
// 400 about the tenant's input.
func TestP12dReviewNoInternalsIn500(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI}, servertest.TailnetSeed{Slug: tenantA})
	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID.String()

	tc.StoreRawTailnetDNSForTest(t, idA, `{"nameservers":{"global":"oops-not-a-list"}}`)
	rev0 := tailnetRevisions(t, tc, idA)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "nameservers", ""},
		{http.MethodGet, "split-dns", ""},
		{http.MethodPost, "nameservers", `{"dns":["9.9.9.9"]}`},
		{http.MethodPatch, "split-dns", `{"corp.example":["10.0.0.53"]}`},
	} {
		r := apiDo(t, tc.URL, c.method, "/api/v2/tailnet/-/dns/"+c.path, keyA, c.body, nil)
		assert.Equal(t, http.StatusInternalServerError, r.code, "%s %s: %s", c.method, c.path, r.body)
		assertNoInternals(t, r.body, "oops", "unmarshal", "json", "stored DNS", "invalid tailnet DNS")
	}

	assert.Equal(t, rev0, tailnetRevisions(t, tc, idA), "nothing stored")
}

// partialRequest sends the headers of a request declaring declared body
// bytes, and only sent of them, then waits for the answer: a server that
// waits for the body answers only after its body read timeout (5s).
func partialRequest(t *testing.T, base, method, path, key string, declared int, sent string) (int, time.Duration) {
	t.Helper()

	u, err := url.Parse(base)
	require.NoError(t, err)

	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", u.Host)
	require.NoError(t, err)

	defer conn.Close()

	start := time.Now()

	_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", method, path, u.Host, key, declared, sent)
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)

	_ = resp.Body.Close()

	return resp.StatusCode, time.Since(start)
}

// Finding 5: a policy body over max_policy_bytes is refused from its
// headers, before it is read; the config-write bucket is spent before the
// body is read or parsed.
func TestP12dReviewBodyRefusedBeforeRead(t *testing.T) {
	const limit = 4096

	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: &types.TenantAPIConfig{
		GlobalRate: 1000, GlobalBurst: 1000,
		TailnetRate: 1000, TailnetBurst: 1000,
		KeyRate: 1000, KeyBurst: 1000,
		PreauthRate: 1000, PreauthBurst: 1000,
		ConfigWriteRate: 1, ConfigWriteBurst: 1,
		MaxPolicyBytes: limit,
	}}, servertest.TailnetSeed{Slug: tenantA})
	keyA := tc.MintAPIKey(t, tenantA)

	// Declared over the limit: 413 at once, without waiting for the body.
	for _, path := range []string{"/api/v2/tailnet/-/acl", "/api/v2/tailnet/-/acl/validate"} {
		code, took := partialRequest(t, tc.URL, http.MethodPost, path, keyA, 2*limit, `{"grants":`)
		assert.Equal(t, http.StatusRequestEntityTooLarge, code, path)
		assert.Less(t, took, 2*time.Second, "%s: refused before the body was read", path)
	}

	// A body over the limit (Content-Length right) is refused too, and
	// spends nothing.
	big := `{"grants":[],"x":"` + strings.Repeat("a", 2*limit) + `"}`
	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, big, nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, r.code, r.body)

	// The only token: spent by a write.
	r = apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl/validate", keyA, `{"grants":[]}`, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)

	// The bucket is empty: 429 before the body is read.
	for _, path := range []string{"/api/v2/tailnet/-/acl", "/api/v2/tailnet/-/dns/nameservers"} {
		code, took := partialRequest(t, tc.URL, http.MethodPost, path, keyA, 100, `{"`)
		assert.Equal(t, http.StatusTooManyRequests, code, path)
		assert.Less(t, took, 2*time.Second, "%s: refused before the body was read", path)
	}
}

// Finding 6: disabling key expiry is audited and spends the tailnet's
// config-write bucket; another tailnet's device is 404 and spends nothing.
func TestP12dReviewDeviceKeyAuditedAndRateLimited(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: &types.TenantAPIConfig{
		GlobalRate: 1000, GlobalBurst: 1000,
		TailnetRate: 1000, TailnetBurst: 1000,
		KeyRate: 1000, KeyBurst: 1000,
		PreauthRate: 1000, PreauthBurst: 1000,
		ConfigWriteRate: 1, ConfigWriteBurst: 2,
	}}, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	audit := captureAudit(t, tc)

	a := tc.Tenant(t, tenantA)
	a1 := enroll(t, a, "alice", "dk-a1")[0]
	id1 := nodeIDOf(t, a, a1)
	keyA, keyB := tc.MintAPIKey(t, tenantA), tc.MintAPIKey(t, tenantB)
	idA, idB := tc.Tailnet(t, tenantA).ID.String(), tc.Tailnet(t, tenantB).ID.String()
	path := fmt.Sprintf("/api/v2/device/%d/key", id1)

	for range 3 {
		r := apiDo(t, tc.URL, http.MethodPost, path, keyB, `{"keyExpiryDisabled":true}`, nil)
		assert.Equal(t, http.StatusNotFound, r.code, "B's key on A's device: %s", r.body)
	}

	r := apiDo(t, tc.URL, http.MethodPost, path, keyA, `{"keyExpiryDisabled":true}`, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)
	r = apiDo(t, tc.URL, http.MethodPost, path, keyA, `{"keyExpiryDisabled":false}`, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)

	r = apiDo(t, tc.URL, http.MethodPost, path, keyA, `{"keyExpiryDisabled":true}`, nil)
	assert.Equal(t, http.StatusTooManyRequests, r.code, "A's config-write bucket is spent: %s", r.body)
	assert.NotEmpty(t, r.hdr.Get("Retry-After"))

	nv, ok := a.State().GetNodeByID(id1)
	require.True(t, ok)
	assert.False(t, nv.KeyExpiryDisabled(), "the refused write changed nothing")

	recs := audit.of(idA)
	require.Len(t, recs, 2, "one audit record per stored device key write")

	for i, want := range []bool{true, false} {
		assert.Equal(t, "setDeviceKey", recs[i].Op)
		assert.NotZero(t, recs[i].KeyID)
		assert.Equal(t, uint64(id1), recs[i].DeviceID)
		require.NotNil(t, recs[i].KeyExpiryDisabled)
		assert.Equal(t, want, *recs[i].KeyExpiryDisabled)
	}

	// B's 404s spent nothing of B's bucket.
	for range 2 {
		v := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl/validate", keyB, `{"grants":[]}`, nil)
		assert.Equal(t, http.StatusOK, v.code, v.body)
	}

	assert.Empty(t, audit.of(idB))
}

// Finding 7: search paths stay outside the cluster's MagicDNS domain
// (except the tailnet's own subtree); a split domain may not be a parent
// of the cluster's domain; split domains count toward a limit even mapped
// to no nameserver; nameservers are never loopback, link-local or
// unspecified. Stored settings that break only these newer rules still
// load (with a warning), as lowered limits do (ADR 0008 §3).
func TestP12dReviewDNSRules(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI}, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	a1 := enroll(t, tc.Tenant(t, tenantA), "alice", "rules-a1")[0]
	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID.String()

	parent := servertest.ClusterBaseDomain[strings.Index(servertest.ClusterBaseDomain, ".")+1:]

	splitMany := map[string][]string{}
	for i := range types.DefaultTenantDNSLimits.MaxSplitDomains + 1 {
		splitMany[fmt.Sprintf("d%d.example", i)] = []string{}
	}

	many := "{"
	for d := range splitMany {
		many += `"` + d + `":[],`
	}

	many = strings.TrimSuffix(many, ",") + "}"

	rev0 := tailnetRevisions(t, tc, idA)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "searchpaths", `{"searchPaths":["` + servertest.ClusterBaseDomain + `"]}`},
		{http.MethodPost, "searchpaths", `{"searchPaths":["x.` + servertest.ClusterBaseDomain + `"]}`},
		{http.MethodPost, "searchpaths", `{"searchPaths":["` + servertest.ClusterTailnetDomain(tenantB) + `"]}`},
		{http.MethodPut, "split-dns", `{"` + parent + `":["10.0.0.53"]}`},
		{http.MethodPut, "split-dns", many},
		{http.MethodPost, "nameservers", `{"dns":["127.0.0.1"]}`},
		{http.MethodPost, "nameservers", `{"dns":["::1"]}`},
		{http.MethodPost, "nameservers", `{"dns":["169.254.169.254"]}`},
		{http.MethodPost, "nameservers", `{"dns":["fe80::1"]}`},
		{http.MethodPost, "nameservers", `{"dns":["0.0.0.0"]}`},
		{http.MethodPost, "nameservers", `{"dns":["https://127.0.0.1/dns-query"]}`},
		{http.MethodPatch, "split-dns", `{"corp.example":["127.0.0.53"]}`},
	} {
		r := apiDo(t, tc.URL, c.method, "/api/v2/tailnet/-/dns/"+c.path, keyA, c.body, nil)
		assert.Equal(t, http.StatusBadRequest, r.code, "%s %s %.80s: %s", c.method, c.path, c.body, r.body)
	}

	assert.Equal(t, rev0, tailnetRevisions(t, tc, idA), "refused writes move no revision")

	// The tailnet's own subtree is a fine search path.
	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/dns/searchpaths", keyA,
		`{"searchPaths":["svc.`+servertest.ClusterTailnetDomain(tenantA)+`"]}`, nil)
	assert.Equal(t, http.StatusOK, r.code, r.body)

	// Stored before these rules (here by hand): still loaded.
	tc.StoreRawTailnetDNSForTest(t, idA, `{"nameservers":{"global":["127.0.0.1"]}}`)
	a1.WaitForCondition(t, "stored settings that break only the newer rules still load", clusterWait,
		func(nm *netmap.NetworkMap) bool { return slices.Contains(dnsResolvers(nm), "127.0.0.1") })
}

// Finding 8: a policy write that stored and installed a revision is not
// installed again by a reconcile that saw the new revision before the
// write recorded it; re-storing the policy in force sends nothing.
func TestP12dReviewReconcileSkipsObservedPolicy(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{TenantAPI: p12dAPI}, servertest.TailnetSeed{Slug: tenantA})
	tc.SetPolicy(t, tenantA, allowAll)

	clients := enroll(t, tc.Tenant(t, tenantA), "alice", "obs-a1", "obs-a2")
	for _, c := range clients {
		waitTenantMesh(t, c, clients, true)
	}

	keyA := tc.MintAPIKey(t, tenantA)
	idA := tc.Tailnet(t, tenantA).ID.String()

	var once sync.Once

	st := tc.Runtime(t, tenantA).GetState()
	st.SetPolicyInstallHookForTest(func() error {
		// Stored, not installed yet: a reconcile tick sees the new
		// revision now.
		once.Do(func() { time.Sleep(reconcileTick + time.Second) }) //nolint:forbidigo // a reconcile tick passes

		return nil
	})
	t.Cleanup(func() { st.SetPolicyInstallHookForTest(nil) })

	rev0 := tailnetRevisions(t, tc, idA)
	before := settledFrames(t, clients)

	r := apiDo(t, tc.URL, http.MethodPost, "/api/v2/tailnet/-/acl", keyA, allowAll, nil)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Equal(t, rev0[0]+1, tailnetRevisions(t, tc, idA)[0])

	assertQuiet(t, clients, before, "re-storing the policy in force")
}
