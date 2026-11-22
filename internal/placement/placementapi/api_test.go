package placementapi

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
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
}

func newEnv(t *testing.T) *env {
	t.Helper()

	p, err := db.OpenPlatform(&types.Config{
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

	return &env{srv: srv, rootCAs: roots, opCA: opCA, reg: reg}
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
	code, _ := e.do(t, e.client(), call{method: http.MethodGet, path: "/v1/clusters", bearer: "hskey-api-TENANTKEY-secret"})
	assert.Equal(t, http.StatusForbidden, code, "tenant credential without certificate")

	code, _ = e.do(t, e.client(), call{method: http.MethodPost, path: "/v1/tailnets", key: "k", bearer: "tskey-api-x",
		body: map[string]any{"slug": "x", "class": "shared"}})
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
	_, err := e.client(rogue).Get(e.srv.URL + "/v1/clusters")
	require.Error(t, err, "certificate from an unpinned CA")

	// A server-auth certificate is not a client certificate.
	srvUsage := e.opCA.issue(t, "alice", x509.ExtKeyUsageServerAuth, alice)
	_, err = e.client(srvUsage).Get(e.srv.URL + "/v1/clusters")
	require.Error(t, err, "certificate without clientAuth usage")

	// Allowed operator.
	al := e.opCA.issue(t, "alice", x509.ExtKeyUsageClientAuth, alice)
	code, _ = e.do(t, e.client(al), list)
	assert.Equal(t, http.StatusOK, code)

	bot := e.opCA.issue(t, "bot", x509.ExtKeyUsageClientAuth, "", "ops-bot.mesh.internal")
	code, _ = e.do(t, e.client(bot), list)
	assert.Equal(t, http.StatusOK, code, "DNS SAN identity")

	// Allowed operator that also sends a tenant key: refused.
	code, _ = e.do(t, e.client(al), call{method: http.MethodGet, path: "/v1/clusters", bearer: "hskey-api-x"})
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

	id := tn["id"].(string)

	code, _ = e.do(t, c, call{method: http.MethodPost, path: "/v1/tailnets/" + id + "/activate", key: "a1", body: map[string]any{"expected_epoch": 1}})
	require.Equal(t, http.StatusOK, code)

	move := call{method: http.MethodPost, path: "/v1/tailnets/" + id + "/owner", key: "m1",
		body: map[string]any{"from_cluster": "c1", "from_epoch": 1, "to_cluster": "c2"}}
	code, moved := e.do(t, c, move)
	require.Equal(t, http.StatusOK, code, moved)
	assert.EqualValues(t, 2, moved["owner_epoch"])

	code, _ = e.do(t, c, move)
	require.Equal(t, http.StatusOK, code, "replay")

	stale := move
	stale.key = "m2"
	code, _ = e.do(t, c, stale)
	assert.Equal(t, http.StatusConflict, code, "stale epoch")

	code, got := e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/" + id})
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 2, got["owner_epoch"])
	assert.EqualValues(t, 0, got["devices"])

	code, q := e.do(t, c, call{method: http.MethodGet, path: "/v1/tailnets/" + id + "/quota"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, q["allowed"])
	assert.EqualValues(t, 5000, q["limit"])

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

	entries := a["entries"].([]any)
	outcomes := map[string]int{}

	for _, en := range entries {
		m := en.(map[string]any)
		assert.Equal(t, alice, m["actor"], "the certificate identity is the actor")
		outcomes[m["action"].(string)+"/"+m["outcome"].(string)]++
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
