package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHost  = "boot.cp.example.test"
	urlA      = "https://c1.cp.example.test:8443"
	urlB      = "https://c2.cp.example.test:8443"
	secretA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secretB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keyActive = "a00000000001"
)

type fakeStore struct {
	recs map[string]Record
	err  error
}

func (f *fakeStore) LookupAuthKey(_ context.Context, keyID string) (Record, bool, error) {
	if f.err != nil {
		return Record{}, false, f.err
	}

	r, ok := f.recs[keyID]

	return r, ok, nil
}

func ptr[T any](v T) *T { return &v }

func storedHash(secret string) []byte { return []byte(hashPrefixSHA256 + ProofOf(secret)) }

// rec is an ACTIVE tailnet "acme" on an ACTIVE cluster c1 with a valid
// reusable key of secret.
func rec(secret string) Record {
	return Record{
		TailnetID: "tn-acme", Hash: storedHash(secret), Reusable: true,
		TailnetSlug: "acme", TailnetState: "ACTIVE",
		ClusterEndpoint: ptr(urlA), ClusterState: ptr("ACTIVE"),
	}
}

func fixture() *fakeStore {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	m := map[string]Record{keyActive: rec(secretA)}
	add := func(id string, f func(*Record)) {
		r := rec(secretA)
		f(&r)
		m[id] = r
	}

	add("a00000000002", func(r *Record) { r.Revoked = true })
	add("a00000000003", func(r *Record) { r.Expiration = &past })
	add("a00000000004", func(r *Record) { r.Reusable, r.Used = false, true })
	add("a00000000005", func(r *Record) { r.TailnetState = "SUSPENDED" })
	add("a00000000006", func(r *Record) { r.TailnetState = "PENDING" })
	add("a00000000007", func(r *Record) { r.TailnetState = "DELETING" })
	add("a00000000008", func(r *Record) { r.Deleted = true })
	add("a00000000009", func(r *Record) { r.MovingTo = ptr("c2") })
	add("a0000000000a", func(r *Record) { r.ClusterState = ptr("DISABLED") })
	add("a0000000000b", func(r *Record) { r.ClusterEndpoint, r.ClusterState = nil, nil })
	add("a0000000000c", func(r *Record) { r.Expiration = &future; r.ClusterState = ptr("DRAINING") })
	add("a0000000000d", func(r *Record) { r.Hash = []byte("$2a$10$legacybcrypthashvalue") })

	b := rec(secretB)
	b.TailnetID, b.TailnetSlug, b.ClusterEndpoint = "tn-beta", "beta", ptr(urlB)
	m["b00000000001"] = b

	return &fakeStore{recs: m}
}

func newTestHandler(store Store, cfg HandlerConfig) *Handler {
	if cfg.Hostname == "" {
		cfg.Hostname = testHost
	}

	return NewHandler(NewService(store), cfg)
}

func body(keyID, secret, tenant string) string {
	b, _ := json.Marshal(Request{
		Credential: Credential{Kind: CredentialKindAuthKeyProof, KeyID: keyID, Proof: ProofOf(secret)},
		Tenant:     tenant,
	})

	return string(b)
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(h http.Handler, method, path, payload string) response {
	req := httptest.NewRequest(method, "https://"+testHost+path, strings.NewReader(payload))
	req.Host = testHost
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	return response{rr.Code, rr.Header(), rr.Body.String()}
}

// TestBootstrapActiveReturnsOwnerURL (F06): a valid key of an ACTIVE
// tailnet on an ACTIVE or DRAINING cluster gets that cluster's URL and
// nothing else (no epoch, cluster id or tenant data).
func TestBootstrapActiveReturnsOwnerURL(t *testing.T) {
	h := newTestHandler(fixture(), DefaultHandlerConfig(""))

	for _, tc := range []struct{ key, secret, tenant, url string }{
		{keyActive, secretA, "", urlA},
		{keyActive, secretA, "acme", urlA},
		{"a0000000000c", secretA, "", urlA}, // draining cluster, future expiry
		{"b00000000001", secretB, "beta", urlB},
	} {
		r := do(h, http.MethodPost, PathBootstrap, body(tc.key, tc.secret, tc.tenant))
		require.Equal(t, http.StatusOK, r.status, tc.key)

		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.body), &got))
		assert.Equal(t, tc.url, got["control_url"])
		assert.Len(t, got, 2, "only control_url and issued_at: %v", got)
		assert.Equal(t, "no-store", r.header.Get("Cache-Control"))
	}
}

// TestBootstrapRefusalsIndistinguishable (F06, I15, I08): every refusal a
// caller can provoke has the same status, headers and body, whether the key
// is unknown, wrong, revoked, expired, used, of a tailnet that is not
// ACTIVE or deleted, of a legacy hash, or does not match the tenant named.
func TestBootstrapRefusalsIndistinguishable(t *testing.T) {
	h := newTestHandler(fixture(), DefaultHandlerConfig(""))

	cases := map[string]string{
		"unknown key":               body("fffffffffff0", secretA, ""),
		"wrong proof":               body(keyActive, secretB, ""),
		"revoked":                   body("a00000000002", secretA, ""),
		"expired":                   body("a00000000003", secretA, ""),
		"single-use used":           body("a00000000004", secretA, ""),
		"suspended":                 body("a00000000005", secretA, ""),
		"pending":                   body("a00000000006", secretA, ""),
		"deleting":                  body("a00000000007", secretA, ""),
		"deleted":                   body("a00000000008", secretA, ""),
		"legacy hash":               body("a0000000000d", secretA, ""),
		"key of A names tenant B":   body(keyActive, secretA, "beta"),
		"key of B names tenant A":   body("b00000000001", secretB, "acme"),
		"unknown tenant named":      body(keyActive, secretA, "nosuch"),
		"moving without valid key":  body("a00000000009", secretB, ""),
		"disabled cluster bad key":  body("a0000000000a", secretB, ""),
		"unregistered cluster, bad": body("a0000000000b", secretB, ""),
	}

	var first *response

	for name, payload := range cases {
		r := do(h, http.MethodPost, PathBootstrap, payload)
		assert.Equal(t, http.StatusNotFound, r.status, name)
		assert.NotContains(t, r.body, "c1.cp", name)
		assert.NotContains(t, r.body, "c2.cp", name)

		if first == nil {
			first = &r

			continue
		}

		assert.Equal(t, first.body, r.body, name)
		assert.Equal(t, first.header, r.header, name)
	}

	// An unknown path is the same refusal.
	r := do(h, http.MethodGet, "/v1/tenants", "")
	assert.Equal(t, first.status, r.status)
	assert.Equal(t, first.body, r.body)
}

// TestBootstrapMovingAndUnavailableOnlyForCredentialed (F06): MOVING and an
// unusable owner cluster are told only to a caller with a valid key; the
// two 503 bodies differ from each other and from the refusal.
func TestBootstrapMovingAndUnavailableOnlyForCredentialed(t *testing.T) {
	h := newTestHandler(fixture(), DefaultHandlerConfig(""))

	moving := do(h, http.MethodPost, PathBootstrap, body("a00000000009", secretA, ""))
	assert.Equal(t, http.StatusServiceUnavailable, moving.status)
	assert.Equal(t, "30", moving.header.Get("Retry-After"))
	assert.Equal(t, string(bodyMoving), moving.body)

	for _, key := range []string{"a0000000000a", "a0000000000b"} {
		r := do(h, http.MethodPost, PathBootstrap, body(key, secretA, ""))
		assert.Equal(t, http.StatusServiceUnavailable, r.status, key)
		assert.Equal(t, string(bodyUnavailable), r.body, key)
		assert.NotContains(t, r.body, "c1.cp")
	}
}

// TestBootstrapStoreFailureIsUnavailable: a store error is a 503, never an
// answer, and never the refusal (the client retries).
func TestBootstrapStoreFailureIsUnavailable(t *testing.T) {
	h := newTestHandler(&fakeStore{err: errors.New("db down")}, DefaultHandlerConfig(""))
	r := do(h, http.MethodPost, PathBootstrap, body(keyActive, secretA, ""))
	assert.Equal(t, http.StatusServiceUnavailable, r.status)
	assert.Equal(t, string(bodyUnavailable), r.body)
}

// TestBootstrapMalformedNeverReachesStore: malformed requests are a uniform
// 400 decided without the store.
func TestBootstrapMalformedNeverReachesStore(t *testing.T) {
	h := newTestHandler(&fakeStore{err: errors.New("must not be called")}, DefaultHandlerConfig(""))

	for name, payload := range map[string]string{
		"not json":        "{",
		"empty":           "",
		"wrong kind":      `{"credential":{"kind":"authkey","key_id":"a00000000001","proof":"` + ProofOf(secretA) + `"}}`,
		"short key id":    `{"credential":{"kind":"authkey_proof","key_id":"a0","proof":"` + ProofOf(secretA) + `"}}`,
		"upper hex proof": `{"credential":{"kind":"authkey_proof","key_id":"a00000000001","proof":"` + strings.ToUpper(ProofOf(secretA)) + `"}}`,
		"unknown field":   `{"credential":{"kind":"authkey_proof","key_id":"a00000000001","proof":"` + ProofOf(secretA) + `"},"key":"x"}`,
		"bad tenant":      body(keyActive, secretA, "Bad_Slug"),
		"trailing data":   body(keyActive, secretA, "") + "{}",
		"huge":            `{"tenant":"` + strings.Repeat("a", 2*maxBody) + `"}`,
	} {
		r := do(h, http.MethodPost, PathBootstrap, payload)
		assert.Equal(t, http.StatusBadRequest, r.status, name)
		assert.Equal(t, string(bodyInvalid), r.body, name)
	}
}

// TestBootstrapNoDebugOrMetrics (I09, S14, AGENTS rule 9): the public
// handler serves exactly POST /v1/bootstrap and GET /health; another Host
// or a TLS SNI that differs from Host gets 421.
func TestBootstrapNoDebugOrMetrics(t *testing.T) {
	h := newTestHandler(fixture(), DefaultHandlerConfig(""))

	for _, p := range []string{"/metrics", "/debug/pprof/", "/debug/vars", "/v1/openapi", "/v1/openapi.yaml", "/docs", "/"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			r := do(h, m, p, "")
			assert.Equal(t, http.StatusNotFound, r.status, m+" "+p)
			assert.Equal(t, string(bodyRefused), r.body, m+" "+p)
		}
	}

	assert.Equal(t, http.StatusNotFound, do(h, http.MethodGet, PathBootstrap, "").status, "GET bootstrap")
	assert.Equal(t, http.StatusNotFound, do(h, http.MethodPost, PathHealth, "").status, "POST health")
	assert.Equal(t, http.StatusOK, do(h, http.MethodGet, PathHealth, "").status)

	req := httptest.NewRequest(http.MethodPost, "https://c1.cp.example.test"+PathBootstrap, strings.NewReader(body(keyActive, secretA, "")))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusMisdirectedRequest, rr.Code, "another host")

	req = httptest.NewRequest(http.MethodPost, "https://"+testHost+PathBootstrap, strings.NewReader(body(keyActive, secretA, "")))
	req.Host = testHost
	req.TLS.ServerName = "c1.cp.example.test"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusMisdirectedRequest, rr.Code, "SNI differs from Host")
}

// TestBootstrapRateLimit (I10, S13): the global bucket bounds everyone; the
// per-tenant bucket is charged only after the credential verified, so wrong
// proofs naming tenant B's key never use up B's budget.
func TestBootstrapRateLimit(t *testing.T) {
	cfg := HandlerConfig{GlobalRate: 0, TenantRate: 0.0001, TenantBurst: 3}
	h := newTestHandler(fixture(), cfg)

	for range 50 {
		r := do(h, http.MethodPost, PathBootstrap, body("b00000000001", secretA, ""))
		require.Equal(t, http.StatusNotFound, r.status, "wrong proof for B's key")
	}

	for i := range 3 {
		require.Equal(t, http.StatusOK, do(h, http.MethodPost, PathBootstrap, body("b00000000001", secretB, "")).status, "B request %d", i)
	}

	r := do(h, http.MethodPost, PathBootstrap, body("b00000000001", secretB, ""))
	assert.Equal(t, http.StatusTooManyRequests, r.status, "B is now over its own budget")
	assert.Equal(t, "1", r.header.Get("Retry-After"))
	assert.Equal(t, http.StatusOK, do(h, http.MethodPost, PathBootstrap, body(keyActive, secretA, "")).status, "A is unaffected")

	g := newTestHandler(fixture(), HandlerConfig{GlobalRate: 0.0001, GlobalBurst: 2})
	assert.Equal(t, http.StatusOK, do(g, http.MethodPost, PathBootstrap, body(keyActive, secretA, "")).status)
	assert.Equal(t, http.StatusNotFound, do(g, http.MethodPost, PathBootstrap, body("fffffffffff0", secretA, "")).status)
	assert.Equal(t, http.StatusTooManyRequests, do(g, http.MethodPost, PathBootstrap, body(keyActive, secretA, "")).status)
}

// TestBootstrapNoSecretsInLogs (AGENTS rule 2): logs carry neither the
// proof, nor the key identifier, nor the tenant.
func TestBootstrapNoSecretsInLogs(t *testing.T) {
	var buf bytes.Buffer

	saved, savedLevel := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	t.Cleanup(func() {
		log.Logger = saved
		zerolog.SetGlobalLevel(savedLevel)
	})

	h := newTestHandler(fixture(), DefaultHandlerConfig(""))
	do(h, http.MethodPost, PathBootstrap, body(keyActive, secretA, "acme"))
	do(h, http.MethodPost, PathBootstrap, body("b00000000001", secretA, "beta"))

	out := buf.String()
	require.NotEmpty(t, out)

	for _, secret := range []string{ProofOf(secretA), ProofOf(secretB), secretA, keyActive, "b00000000001", "acme", "beta", urlA} {
		assert.NotContains(t, out, secret)
	}
}
