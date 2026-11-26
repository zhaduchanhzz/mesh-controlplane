package placementapi

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func leafOf(t *testing.T, c tls.Certificate) *x509.Certificate {
	t.Helper()

	leaf, err := x509.ParseCertificate(c.Certificate[0])
	require.NoError(t, err)

	return leaf
}

// P13f L5 (review P10): an operator certificate could not be revoked. The
// revocation file names certificates by serial or SHA-256; a listed
// certificate is refused at once after the next reload, others keep
// working; a file that cannot be read or parsed refuses everyone (fail
// closed) until it is fixed.
func TestOperatorCertificateRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "revoked")
	require.NoError(t, os.WriteFile(path, []byte("# no revocations yet\n"), 0o600))

	rev, err := LoadRevocations(path)
	require.NoError(t, err)

	e := newEnvWith(t, func(a *Authorizer) { a.WithRevocations(rev) })
	list := call{method: http.MethodGet, path: "/v1/clusters"}

	al1 := e.opCA.issue(t, "alice-laptop", x509.ExtKeyUsageClientAuth, alice)
	al2 := e.opCA.issue(t, "alice-desktop", x509.ExtKeyUsageClientAuth, alice)
	bot := e.opCA.issue(t, "bot", x509.ExtKeyUsageClientAuth, "", "ops-bot.mesh.internal")

	for _, c := range []tls.Certificate{al1, al2, bot} {
		code, _ := e.do(t, e.client(c), list)
		require.Equal(t, http.StatusOK, code)
	}

	// Revoke al1 by serial (with ':' separators and leading zeros) and the
	// bot by fingerprint.
	sum := sha256.Sum256(bot.Certificate[0])
	serial := fmt.Sprintf("%040x", leafOf(t, al1).SerialNumber)

	pairs := make([]string, 0, len(serial)/2)
	for i := 0; i < len(serial); i += 2 {
		pairs = append(pairs, serial[i:i+2])
	}

	withColons := strings.Join(pairs, ":")

	require.NoError(t, os.WriteFile(path, []byte(
		"serial:"+withColons+"   # alice's lost laptop\n"+
			"SHA256:"+hex.EncodeToString(sum[:])+"\n"), 0o600))
	require.NoError(t, rev.Reload())

	code, _ := e.do(t, e.client(al1), list)
	assert.Equal(t, http.StatusForbidden, code, "revoked by serial")

	code, _ = e.do(t, e.client(bot), list)
	assert.Equal(t, http.StatusForbidden, code, "revoked by fingerprint")

	code, _ = e.do(t, e.client(al2), list)
	assert.Equal(t, http.StatusOK, code, "another certificate of the same identity still works")

	// An unparsable file: fail closed.
	require.NoError(t, os.WriteFile(path, []byte("serial:zz\n"), 0o600))
	require.Error(t, rev.Reload())

	code, _ = e.do(t, e.client(al2), list)
	assert.Equal(t, http.StatusServiceUnavailable, code, "a bad revocation file refuses everyone")

	// A missing file: fail closed too.
	require.NoError(t, os.Remove(path))
	require.Error(t, rev.Reload())

	code, _ = e.do(t, e.client(al2), list)
	assert.Equal(t, http.StatusServiceUnavailable, code)

	// Fixed: admitted again; the revoked ones stay out.
	require.NoError(t, os.WriteFile(path, []byte("serial:"+serial+"\n"), 0o600))
	require.NoError(t, rev.Reload())

	code, _ = e.do(t, e.client(al2), list)
	assert.Equal(t, http.StatusOK, code)

	code, _ = e.do(t, e.client(al1), list)
	assert.Equal(t, http.StatusForbidden, code)
}

func TestLoadRevocationsRefusesBadFile(t *testing.T) {
	dir := t.TempDir()

	for name, content := range map[string]string{
		"unknown kind": "fingerprint:abcd\n",
		"no kind":      "0a1b2c\n",
		"short sha256": "sha256:abcd\n",
		"zero serial":  "serial:00\n",
	} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		_, err := LoadRevocations(path)
		require.ErrorIs(t, err, ErrRevocationUnavailable, name)
	}

	_, err := LoadRevocations(filepath.Join(dir, "missing"))
	assert.ErrorIs(t, err, ErrRevocationUnavailable)
}
