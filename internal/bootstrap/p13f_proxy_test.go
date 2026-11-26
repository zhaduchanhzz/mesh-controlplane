package bootstrap

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/proxyproto"
)

// proxyV2 is a PROXY v2 header for a TCP/IPv4 client src.
func proxyV2(src netip.AddrPort) []byte {
	h := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A, 0x21, 0x11, 0, 12}
	s := src.Addr().As4()
	h = append(h, s[:]...)
	h = append(h, 192, 0, 2, 200)
	h = binary.BigEndian.AppendUint16(h, src.Port())

	return binary.BigEndian.AppendUint16(h, 8444)
}

// bootstrapBehind serves h over TLS on a PROXY v2 listener trusting
// trusted; the returned client sends header (nil: none) on each connection.
func bootstrapBehind(t *testing.T, h http.Handler, trusted string) (string, func(header []byte) *http.Client) {
	t.Helper()

	inner, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	pl, err := proxyproto.NewListener(inner, proxyproto.Config{Trusted: []netip.Prefix{netip.MustParsePrefix(trusted)}})
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = pl
	srv.StartTLS()
	t.Cleanup(srv.Close)

	addr := inner.Addr().String()

	client := func(header []byte) *http.Client {
		return &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				c, err := new(net.Dialer).DialContext(ctx, network, addr)
				if err == nil && header != nil {
					_, err = c.Write(header)
				}

				return c, err
			},
			//nolint:gosec // test server's self-signed certificate; SNI is what the handler checks
			TLSClientConfig: &tls.Config{ServerName: testHost, InsecureSkipVerify: true},
		}}
	}

	return "https://" + testHost + PathBootstrap, client
}

func post(t *testing.T, c *http.Client, url, payload string) (int, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(payload))
	require.NoError(t, err)

	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}

	_ = resp.Body.Close()

	return resp.StatusCode, nil
}

// P13f B7 end to end: behind the trusted front door each client is limited
// by the address in its PROXY v2 header, so a flood from one address
// leaves another address's tenant served.
func TestB7ProxyV2PerClientLimit(t *testing.T) {
	url, client := bootstrapBehind(t, newTestHandler(fixture(), DefaultHandlerConfig(testHost)), "127.0.0.1/32")

	flooder := client(proxyV2(netip.MustParseAddrPort("203.0.113.7:40000")))
	limited := 0

	for range 3 * DefaultIPBurst {
		code, err := post(t, flooder, url, body("fffffffffff0", secretA, ""))
		require.NoError(t, err)

		if code == http.StatusTooManyRequests {
			limited++
		}
	}

	assert.Positive(t, limited, "the flooding address is limited at its own bucket")

	code, err := post(t, client(proxyV2(netip.MustParseAddrPort("198.51.100.9:50000"))), url, body(keyActive, secretA, ""))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code, "another client address is served during the flood")
}

// P13f B7 spoofing: a peer that is not the trusted front door cannot pick
// the address it is limited as. A PROXY header from it closes the
// connection; a trusted front door without a header is refused too (every
// client would share the front door's bucket).
func TestB7ProxyV2Spoofing(t *testing.T) {
	h := newTestHandler(fixture(), DefaultHandlerConfig(testHost))

	url, client := bootstrapBehind(t, h, "192.0.2.0/24") // loopback is not the front door

	_, err := post(t, client(proxyV2(netip.MustParseAddrPort("198.51.100.9:1"))), url, body(keyActive, secretA, ""))
	require.Error(t, err, "a PROXY header from an untrusted peer was accepted")

	code, err := post(t, client(nil), url, body(keyActive, secretA, ""))
	require.NoError(t, err, "an untrusted peer without a header is served with its own address")
	assert.Equal(t, http.StatusOK, code)

	trustedURL, trustedClient := bootstrapBehind(t, h, "127.0.0.1/32")

	_, err = post(t, trustedClient(nil), trustedURL, body(keyActive, secretA, ""))
	require.Error(t, err, "the trusted front door must send a header")
}
