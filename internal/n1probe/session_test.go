package n1probe

import (
	"context"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/key"
)

// Lab bug (N1 run at f934e9a): for https://host:8443 the transport also
// dialled port 80 (HTTP fallback, EOF at the VPS) and that dial used the
// session's one connection, so the real dial to 8443 was refused. Only the
// URL's port may be dialled, and a refused port must not use the budget.
func TestSessionDialsOnlyTheURLPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	s, err := NewSession("https://127.0.0.1:"+port, key.NewMachine().Public(), key.NewMachine(), nil)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	ctx := context.Background()

	for _, p := range []string{"80", "443"} {
		_, err = s.systemDial(ctx, "tcp", "127.0.0.1:"+p)
		require.ErrorIs(t, err, ErrNotURLPort, "port %s", p)
	}

	assert.Equal(t, 0, s.Dials(), "a refused port does not use the session's connection")

	c, err := s.systemDial(ctx, "tcp", "127.0.0.1:"+port)
	require.NoError(t, err, "the URL's (non-default) port is dialled")
	t.Cleanup(func() { _ = c.Close() })
	assert.Equal(t, 1, s.Dials())

	_, err = s.systemDial(ctx, "tcp", "127.0.0.1:"+port)
	require.ErrorIs(t, err, ErrSessionUsed, "never a second connection")
}

func TestURLPortDefaults(t *testing.T) {
	for in, want := range map[string]string{
		"https://c1.cp.gta5web.online:8443": "8443",
		"https://c2.cp.gta5web.online:8445": "8445",
		"https://example.com":               "443",
		"http://localhost:1234":             "1234",
		"http://localhost":                  "80",
	} {
		u, err := url.Parse(in)
		require.NoError(t, err)
		assert.Equal(t, want, urlPort(u), in)
	}
}
