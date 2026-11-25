package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUsageAndKeyErrors(t *testing.T) {
	var out, errb bytes.Buffer

	assert.Equal(t, 2, run(nil, strings.NewReader(""), &out, &errb))
	assert.Equal(t, 2, run([]string{"nope"}, strings.NewReader(""), &out, &errb))

	empty := filepath.Join(t.TempDir(), "k")
	assert.NoError(t, os.WriteFile(empty, nil, 0o600))
	assert.Equal(t, 2, run([]string{"flood", "--kind", "bootstrap", "--key", empty}, strings.NewReader(""), &out, &errb))
	assert.Equal(t, 2, run([]string{"switch-tenant", "--url", "http://x", "--key-a", "-", "--key-b", "-"}, strings.NewReader(""), &out, &errb))
}

// The key, read from stdin, appears in no output.
func TestFloodNeverPrintsKey(t *testing.T) {
	const key = "hskey-auth-id01-verysecretvalue42"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer

	code := run([]string{"flood", "--kind", "bootstrap", "--bootstrap-url", srv.URL, "--key", "-",
		"--rate", "20", "--duration", "200ms"}, strings.NewReader(key+"\n"), &out, &errb)
	assert.Equal(t, 0, code, errb.String())
	assert.Contains(t, out.String(), `"status_4xx"`)
	assert.NotContains(t, out.String()+errb.String(), "verysecretvalue42")
}
