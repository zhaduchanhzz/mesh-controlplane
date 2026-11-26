package placementapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
)

// Test PKI, generated in memory per test run; no key is ever written.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T, name string) *ca {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &ca{cert: cert, key: key}
}

func (c *ca) issue(t *testing.T, cn string, usage x509.ExtKeyUsage, uri string, dns ...string) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: dns,
	}

	if uri != "" {
		u, err := url.Parse(uri)
		require.NoError(t, err)

		tmpl.URIs = []*url.URL{u}
	}

	if usage == x509.ExtKeyUsageServerAuth {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	require.NoError(t, err)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

const (
	alice   = "spiffe://mesh/operator/alice"
	mallory = "spiffe://mesh/operator/mallory"
)

type env struct {
	srv     *httptest.Server
	rootCAs *x509.CertPool
	opCA    *ca
	reg     *placement.Registry
	leases  *lease.Store
}

// live takes cluster id's lease as its running process would: placement
// only chooses clusters with a live lease (P12, owner decision Q2).
func (e *env) live(t *testing.T, id string) {
	t.Helper()

	_, err := e.leases.Acquire(context.Background(), id, "test-"+id, time.Hour)
	require.NoError(t, err)
}

func newEnv(t *testing.T) *env {
	t.Helper()

	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with setup applied to the authorizer (P13f: a
// revocation list).
func newEnvWith(t *testing.T, setup func(*Authorizer)) *env {
	t.Helper()

	p, err := db.MigrateAndOpenPlatform(&types.Config{
		Database: types.DatabaseConfig{
			Type:   types.DatabaseSqlite,
			Sqlite: types.SqliteConfig{Path: filepath.Join(t.TempDir(), "api.db")},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	reg, err := placement.New(p.RegistryDB(), placement.Config{})
	require.NoError(t, err)

	auth, err := NewAuthorizer([]string{alice, "ops-bot.mesh.internal"})
	require.NoError(t, err)

	if setup != nil {
		setup(auth)
	}

	serverCA := newCA(t, "server-ca")
	opCA := newCA(t, "operator-ca")

	pool := x509.NewCertPool()
	pool.AddCert(opCA.cert)

	tlsCfg, err := TLSConfig(serverCA.issue(t, "placement", x509.ExtKeyUsageServerAuth, ""), pool)
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(Handler(reg, auth))
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)

	roots := x509.NewCertPool()
	roots.AddCert(serverCA.cert)

	return &env{srv: srv, rootCAs: roots, opCA: opCA, reg: reg, leases: lease.NewStore(p.RegistryDB())}
}

func (e *env) client(certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: e.rootCAs, Certificates: certs, MinVersion: tls.VersionTLS13,
	}}}
}

type call struct {
	method, path, key, bearer string
	body                      any
}

// as is a checked type assertion on a decoded JSON value.
func as[T any](t *testing.T, v any) T {
	t.Helper()

	x, ok := v.(T)
	require.True(t, ok, "want %T, got %T", x, v)

	return x
}

// handshakeErr makes a GET that must fail before an HTTP response exists.
func handshakeErr(t *testing.T, c *http.Client, url string) error {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	return err
}

func (e *env) do(t *testing.T, c *http.Client, cl call) (int, map[string]any) {
	t.Helper()

	var rd io.Reader

	if cl.body != nil {
		b, err := json.Marshal(cl.body)
		require.NoError(t, err)

		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(t.Context(), cl.method, e.srv.URL+cl.path, rd)
	require.NoError(t, err)

	if cl.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if cl.key != "" {
		req.Header.Set("Idempotency-Key", cl.key)
	}

	if cl.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+cl.bearer)
	}

	resp, err := c.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var m map[string]any

	_ = json.Unmarshal(raw, &m)

	return resp.StatusCode, m
}

// TestOperatorAuth (RED, I08/S10): only an allowed operator certificate from
// the pinned CA gets in; a tenant credential is refused with 403 with or
// without a certificate; a certificate of another CA fails the handshake.
func TestOperatorAuth(t *testing.T) {
	e := newEnv(t)
	list := call{method: http.MethodGet, path: "/v1/clusters"}

	// Tenant API key / bearer, no certificate.
	code, _ := e.do(t, e.client(), call{method: http.MethodGet, path: "/v1/clusters", bearer: "hskey-api-TENANTKEY-secret"}) //nolint:gosec // G101: fake tenant credential; the test proves it is refused
	assert.Equal(t, http.StatusForbidden, code, "tenant credential without certificate")

	code, _ = e.do(t, e.client(), call{ //nolint:gosec // G101: fake tenant credential; the test proves it is refused
		method: http.MethodPost, path: "/v1/tailnets", key: "k", bearer: "tskey-api-x",
		body: map[string]any{"slug": "x", "class": "shared"},
	})
	assert.Equal(t, http.StatusForbidden, code)

	// No certificate, no credential.
	code, _ = e.do(t, e.client(), list)
	assert.Equal(t, http.StatusUnauthorized, code)

	code, _ = e.do(t, e.client(), call{method: http.MethodGet, path: "/v1/openapi.yaml"})
	assert.Equal(t, http.StatusUnauthorized, code, "the spec is behind the operator check too")

	// Same CA, identity not allowed.
	mal := e.opCA.issue(t, alice, x509.ExtKeyUsageClientAuth, mallory) // CN is ignored
	code, _ = e.do(t, e.client(mal), list)
	assert.Equal(t, http.StatusForbidden, code, "wrong identity")

	// Allowed identity in a certificate of another CA: handshake fails.
	rogue := newCA(t, "operator-ca").issue(t, "alice", x509.ExtKeyUsageClientAuth, alice)
	err := handshakeErr(t, e.client(rogue), e.srv.URL+"/v1/clusters")
	require.Error(t, err, "certificate from an unpinned CA")

	// A server-auth certificate is not a client certificate.
	srvUsage := e.opCA.issue(t, "alice", x509.ExtKeyUsageServerAuth, alice)
	err = handshakeErr(t, e.client(srvUsage), e.srv.URL+"/v1/clusters")
	require.Error(t, err, "certificate without clientAuth usage")

	// Allowed operator.
	al := e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice)
	code, _ = e.do(t, e.client(al), list)
	assert.Equal(t, http.StatusOK, code)

	bot := e.opCA.issue(t, "bot", x509.ExtKeyUsageClientAuth, "", "ops-bot.mesh.internal")
	code, _ = e.do(t, e.client(bot), list)
	assert.Equal(t, http.StatusOK, code, "DNS SAN identity")

	// Allowed operator that also sends a tenant key: refused.
	code, _ = e.do(t, e.client(al), call{method: http.MethodGet, path: "/v1/clusters", bearer: "hskey-api-x"}) //nolint:gosec // G101: fake tenant credential; the test proves it is refused
	assert.Equal(t, http.StatusForbidden, code)

	// Nothing was written by the refused calls.
	entries, err := e.reg.AuditLog(t.Context(), 100)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestOperatorFlow: create a cluster and a tailnet, idempotent retries,
// CAS, quota and audit through the API.
func TestOperatorFlow(t *testing.T) {
	e := newEnv(t)
	c := e.client(e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice))

	cluster := map[string]any{"id": "c1", "endpoint_url": "https://c1.cp.example.test:8443", "class": "shared", "state": "ACTIVE", "max_devices": 100}

	code, _ := e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", body: cluster})
	assert.Equal(t, http.StatusUnprocessableEntity, code, "Idempotency-Key is required")

	code, out := e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: "c1", body: cluster})
	require.Equal(t, http.StatusCreated, code, out)

	cluster2 := map[string]any{"id": "c2", "endpoint_url": "https://c2.cp.example.test:8443", "class": "shared", "state": "ACTIVE", "max_devices": 100}
	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: "c2", body: cluster2})
	require.Equal(t, http.StatusCreated, code)
	e.live(t, "c1")
	e.live(t, "c2")

	create := call{method: http.MethodPost, path: "/v1/tailnets", key: "t1", body: map[string]any{"slug": "acme", "class": "shared"}}
	code, tn := e.do(t, c, create)
	require.Equal(t, http.StatusCreated, code, tn)
	assert.Equal(t, "PENDING", tn["status"])
	assert.Equal(t, "c1", tn["owner_cluster_id"])

	code, again := e.do(t, c, create)
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, tn["id"], again["id"], "same key, same request: same result")

	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/tailnets", key: "t1", body: map[string]any{"slug": "other", "class": "shared"}})
	assert.Equal(t, http.StatusUnprocessableEntity, code, "same key, another request")

	id := as[string](t, tn["id"])

	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/tailnets/" + id + "/activate", key: "a1", body: map[string]any{"expected_epoch": 1}})
	require.Equal(t, http.StatusOK, code)

	move := call{
		method: http.MethodPost, path: "/v1/tailnets/" + id + "/owner", key: "m1",
		body: map[string]any{"from_cluster": "c1", "from_epoch": 1, "to_cluster": "c2"},
	}
	code, moved := e.do(t, c, move)
	require.Equal(t, http.StatusOK, code, moved)
	assert.EqualValues(t, 1, moved["owner_epoch"], "a move request (ADR 0010)")
	assert.Equal(t, "c2", moved["moving_to_cluster_id"])

	code, _ = e.do(t, c, move)
	require.Equal(t, http.StatusOK, code, "replay")

	stale := move
	stale.key = "m2"
	code, _ = e.do(t, c, stale)
	assert.Equal(t, http.StatusConflict, code, "a move is already pending")

	code, got := e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/" + id})
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 1, got["owner_epoch"])
	assert.Equal(t, "c2", got["moving_to_cluster_id"])
	assert.EqualValues(t, 0, got["devices"])

	// P12c C3: a tailnet with a move pending enrolls nothing (its devices
	// were counted against the target at the request).
	code, q := e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/" + id + "/quota"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, q["allowed"])
	assert.Contains(t, q["reason"], "moving")

	code, st := e.do(t, c, call{method: http.MethodPut, path: "/v1/clusters/c1/state", key: "s1", body: map[string]any{"state": "DRAINING", "expected_revision": 1}})
	require.Equal(t, http.StatusOK, code, st)
	assert.EqualValues(t, 2, st["revision"])

	code, _ = e.do(t, c, call{method: http.MethodPut, path: "/v1/clusters/c1/state", key: "s2", body: map[string]any{"state": "ACTIVE", "expected_revision": 1}})
	assert.Equal(t, http.StatusConflict, code, "stale revision")

	code, _ = e.do(t, c, call{method: http.MethodDelete, path: "/v1/clusters/c2", key: "d1"})
	assert.Equal(t, http.StatusConflict, code, "cluster in use")

	code, _ = e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/not-a-uuid/quota"})
	assert.Equal(t, http.StatusNotFound, code)

	code, a := e.do(t, c, call{method: http.MethodGet, path: "/v1/audit?limit=100"})
	require.Equal(t, http.StatusOK, code)

	entries := as[[]any](t, a["entries"])
	outcomes := map[string]int{}

	for _, en := range entries {
		m := as[map[string]any](t, en)
		assert.Equal(t, alice, m["actor"], "the certificate identity is the actor")
		outcomes[as[string](t, m["action"])+"/"+as[string](t, m["outcome"])]++
	}

	assert.Equal(t, 1, outcomes["CreateTailnet/ok"])
	assert.Equal(t, 1, outcomes["CreateTailnet/replay"])
	assert.Equal(t, 1, outcomes["CreateTailnet/error"])
	assert.Equal(t, 1, outcomes["MoveOwner/ok"])
	assert.Equal(t, 1, outcomes["MoveOwner/replay"])
	assert.Equal(t, 1, outcomes["MoveOwner/error"])
	assert.Equal(t, 1, outcomes["DeleteCluster/error"])
}

// TestSpec: the OpenAPI document is produced from the Huma definitions.
func TestSpec(t *testing.T) {
	b, err := Spec()
	require.NoError(t, err)
	assert.Contains(t, string(b), "moveTailnetOwner")
	assert.Contains(t, string(b), "mutualTLS")

	_, err = Spec30()
	require.NoError(t, err)

	_, err = NewAuthorizer(nil)
	require.Error(t, err)
}

// P08b: tenant API keys through the operator API: minted once (a replay
// does not show the key again), listed without secrets, revoked; every call
// audited under the certificate identity.
func TestOperatorAPIKeys(t *testing.T) {
	e := newEnv(t)
	c := e.client(e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice))

	cluster := map[string]any{"id": "c1", "endpoint_url": "https://c1.cp.example.test:8443", "class": "shared", "state": "ACTIVE", "max_devices": 100}
	code, _ := e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: "c1", body: cluster})
	require.Equal(t, http.StatusCreated, code)
	e.live(t, "c1")

	code, tn := e.do(t, c, call{method: http.MethodPost, path: "/v1/tailnets", key: "t1", body: map[string]any{"slug": "acme", "class": "shared"}})
	require.Equal(t, http.StatusCreated, code)

	id := as[string](t, tn["id"])

	mint := call{method: http.MethodPost, path: "/v1/tailnets/" + id + "/apikeys", key: "k1", body: map[string]any{"expires_in_seconds": 3600}}

	code, _ = e.do(t, c, call{method: http.MethodPost, path: mint.path, body: mint.body})
	assert.Equal(t, http.StatusUnprocessableEntity, code, "Idempotency-Key is required")

	code, _ = e.do(t, c, call{method: http.MethodPost, path: mint.path, key: "k0", body: map[string]any{"expires_in_seconds": 400 * 24 * 3600}})
	assert.Equal(t, http.StatusUnprocessableEntity, code, "lifetime above 365 days")

	code, k := e.do(t, c, mint)
	require.Equal(t, http.StatusCreated, code, k)
	key, _ := k["key"].(string)
	require.NotEmpty(t, key)
	assert.Equal(t, id, k["tailnet_id"])
	assert.Nil(t, k["replayed"])

	code, again := e.do(t, c, mint)
	require.Equal(t, http.StatusCreated, code)
	assert.Nil(t, again["key"], "the key is shown once")
	assert.Equal(t, true, again["replayed"])
	assert.Equal(t, k["id"], again["id"])

	code, list := e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/" + id + "/apikeys"})
	require.Equal(t, http.StatusOK, code)
	require.Len(t, list["keys"], 1)
	b, err := json.Marshal(list)
	require.NoError(t, err)
	assert.NotContains(t, string(b), key[len(key)-10:], "listing shows no secret")

	keyID := int(as[float64](t, k["id"]))
	revoke := call{method: http.MethodPost, path: "/v1/tailnets/" + id + "/apikeys/" + strconv.Itoa(keyID) + "/revoke", key: "r1"}
	code, rev := e.do(t, c, revoke)
	require.Equal(t, http.StatusOK, code, rev)
	assert.NotNil(t, rev["revoked"])

	code, _ = e.do(t, c, call{method: http.MethodPost, path: revoke.path, key: "r2"})
	assert.Equal(t, http.StatusNotFound, code, "revoked twice")

	code, _ = e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/not-a-uuid/apikeys"})
	assert.Equal(t, http.StatusNotFound, code)

	code, a := e.do(t, c, call{method: http.MethodGet, path: "/v1/audit?limit=100"})
	require.Equal(t, http.StatusOK, code)

	outcomes := map[string]int{}

	for _, en := range as[[]any](t, a["entries"]) {
		m := as[map[string]any](t, en)
		outcomes[as[string](t, m["action"])+"/"+as[string](t, m["outcome"])]++
		ab, err := json.Marshal(m)
		require.NoError(t, err)
		assert.NotContains(t, string(ab), key[len(key)-10:], "audit shows no secret")
	}

	assert.Equal(t, 1, outcomes["CreateAPIKey/ok"])
	assert.Equal(t, 1, outcomes["CreateAPIKey/replay"])
	assert.Equal(t, 1, outcomes["RevokeAPIKey/ok"])
	assert.Equal(t, 1, outcomes["RevokeAPIKey/error"])
}

// P12 step 2 (Q4): the reconcile report is an operator read: no certificate
// or a tenant credential is refused; it lists anomalies by id and count
// only, and reading it changes nothing (no audit entry).
func TestOperatorReconcile(t *testing.T) {
	e := newEnv(t)
	c := e.client(e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice))
	get := call{method: http.MethodGet, path: "/v1/reconcile"}

	code, _ := e.do(t, e.client(), get)
	assert.Equal(t, http.StatusUnauthorized, code, "no certificate")

	code, _ = e.do(t, e.client(), call{method: http.MethodGet, path: "/v1/reconcile", bearer: "hskey-api-TENANTKEY-secret"}) //nolint:gosec // G101: fake tenant credential; the test proves it is refused
	assert.Equal(t, http.StatusForbidden, code, "tenant credential")

	code, _ = e.do(t, e.client(e.opCA.issue(t, alice, x509.ExtKeyUsageClientAuth, mallory)), get)
	assert.Equal(t, http.StatusForbidden, code, "identity not allowed")

	cluster := map[string]any{"id": "c1", "endpoint_url": "https://c1.cp.example.test:8443", "class": "shared", "state": "ACTIVE", "max_devices": 100}
	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: "c1", body: cluster})
	require.Equal(t, http.StatusCreated, code)

	findings := func() []any {
		t.Helper()

		code, rep := e.do(t, c, get)
		require.Equal(t, http.StatusOK, code, rep)
		require.Contains(t, rep, "generated_at")

		f, ok := rep["findings"].([]any)
		require.True(t, ok, "findings is a list: %v", rep["findings"])

		return f
	}

	// Registered ACTIVE, no process yet: R5 only.
	assert.Equal(t, []any{map[string]any{"code": "R5_CLUSTER_ACTIVE_NO_LIVE_LEASE", "cluster_id": "c1"}}, findings())

	// Its process runs, without a registered Noise key: R6 only.
	e.live(t, "c1")
	assert.Equal(t, []any{map[string]any{"code": "R6_CLUSTER_NO_NOISE_KEY", "cluster_id": "c1"}}, findings())

	code, _ = e.do(t, c, call{method: http.MethodPatch, path: "/v1/clusters/c1", key: "k1", body: map[string]any{"noise_public_key": p13eKey}})
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, findings(), "clean")

	entries, err := e.reg.AuditLog(t.Context(), 100)
	require.NoError(t, err)

	for _, a := range entries {
		assert.NotContains(t, a.Action, "econcile", "a read is not audited as a mutation")
	}
}

// P12b (owner Q2): the move schedule through the operator API: schedule
// (Idempotency-Key required, replay gives the same move), get, list,
// cancel; operator-only like the rest.
func TestOperatorMoves(t *testing.T) {
	e := newEnv(t)
	c := e.client(e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice))

	for _, id := range []string{"c1", "c2"} {
		cl := map[string]any{"id": id, "endpoint_url": "https://" + id + ".cp.example.test:8443", "class": "shared", "state": "ACTIVE", "max_devices": 100}
		code, _ := e.do(t, c, call{method: http.MethodPost, path: "/v1/clusters", key: id, body: cl})
		require.Equal(t, http.StatusCreated, code)
		e.live(t, id)
	}

	code, tn := e.do(t, c, call{method: http.MethodPost, path: "/v1/tailnets", key: "t1", body: map[string]any{"slug": "acme", "class": "shared"}})
	require.Equal(t, http.StatusCreated, code, tn)
	id, _ := tn["id"].(string)
	owner, _ := tn["owner_cluster_id"].(string)

	require.NotEmpty(t, id)
	require.NotEmpty(t, owner)
	target := map[string]string{"c1": "c2", "c2": "c1"}[owner]

	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/tailnets/" + id + "/activate", key: "a1", body: map[string]any{"expected_epoch": 1}})
	require.Equal(t, http.StatusOK, code)

	when := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	sched := call{
		method: http.MethodPost, path: "/v1/tailnets/" + id + "/moves", key: "s1",
		body: map[string]any{"to_cluster": target, "scheduled_at": when, "reason": "rebalance"},
	}

	code, _ = e.do(t, e.client(), call{method: sched.method, path: sched.path, key: "s0", body: sched.body})
	assert.Equal(t, http.StatusUnauthorized, code, "no certificate")

	code, _ = e.do(t, c, call{method: sched.method, path: sched.path, body: sched.body})
	assert.Equal(t, http.StatusUnprocessableEntity, code, "Idempotency-Key is required")

	code, m := e.do(t, c, sched)
	require.Equal(t, http.StatusCreated, code, m)
	assert.Equal(t, "SCHEDULED", m["state"])
	assert.Equal(t, owner, m["from_cluster_id"])
	assert.Equal(t, target, m["to_cluster_id"])
	moveID, _ := m["id"].(string)
	require.NotEmpty(t, moveID)

	code, again := e.do(t, c, sched)
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, moveID, again["id"], "replay")

	code, _ = e.do(t, c, call{method: http.MethodPost, path: sched.path, key: "s2", body: sched.body})
	assert.Equal(t, http.StatusConflict, code, "one open move per tailnet")

	code, got := e.do(t, c, call{method: http.MethodGet, path: "/v1/moves/" + moveID})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, moveID, got["id"])

	code, list := e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/" + id + "/moves"})
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, list["moves"], 1)

	code, cancelled := e.do(t, c, call{method: http.MethodPost, path: "/v1/moves/" + moveID + "/cancel", key: "x1"})
	require.Equal(t, http.StatusOK, code, cancelled)
	assert.Equal(t, "CANCELLED", cancelled["state"])

	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/moves/" + moveID + "/cancel", key: "x2"})
	assert.Equal(t, http.StatusConflict, code)

	code, _ = e.do(t, c, call{method: http.MethodGet, path: "/v1/moves/nope"})
	assert.Equal(t, http.StatusNotFound, code)
}
