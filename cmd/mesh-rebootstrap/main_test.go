package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadTargetsRefusesShellSyntax(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t")

	require.NoError(t, os.WriteFile(p, []byte("# clients\nmc-client-a\nroot@10.0.0.5\n\nmc-client-a\n"), 0o600))
	got, err := readTargets(p)
	require.NoError(t, err)
	assert.Equal(t, []string{"mc-client-a", "root@10.0.0.5"}, got)

	for _, bad := range []string{"a;rm -rf /", "$(id)", "a b", "`x`"} {
		require.NoError(t, os.WriteFile(p, []byte(bad+"\n"), 0o600))
		_, err := readTargets(p)
		require.Error(t, err, bad)
	}

	require.NoError(t, os.WriteFile(p, []byte("# none\n"), 0o600))
	_, err = readTargets(p)
	require.Error(t, err)
}

func TestLoginPatternAndExpand(t *testing.T) {
	for _, ok := range []string{"https://c2.example:8443", "https://c2.example", "https://c2.example:8443/"} {
		assert.True(t, loginPattern.MatchString(ok), ok)
	}

	for _, bad := range []string{"http://c2.example", "https://c2.example/$(id)", "https://c2.example:8443;x", "https://"} {
		assert.False(t, loginPattern.MatchString(bad), bad)
	}

	assert.Equal(t, "ssh h tailscale up --login-server https://c2:8443 --force-reauth",
		expand("ssh {target} tailscale up --login-server {login_server} --force-reauth", "h", "https://c2:8443"))
}
