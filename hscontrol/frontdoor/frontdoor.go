// Package frontdoor picks the tailnet of an incoming control-plane request and
// hands the connection to that tailnet's runtime. It is the P02 proof of
// concept for "one process, one Noise identity per tailnet": every tailnet is
// reached through its own hostname, and each hostname maps to exactly one
// backend that owns that tailnet's Noise key.
//
// Security rules (03-isolation-threat-model.md I01, tickets/P02.md):
//
//   - The tailnet comes only from the normalised Host header; there is no
//     default tailnet. Unknown hosts and IP literals get 421 before any
//     protocol upgrade.
//   - Over TLS the SNI must name the same host. Port 80 has no SNI; there the
//     binding is enforced by the Noise IK handshake, because a client only
//     knows the key that tailnet's /key endpoint served.
//   - Client-supplied tailnet hints (X-Tailnet-ID and friends) are dropped.
//   - A request is proxied to one backend only; keys are never tried in turn.
package frontdoor

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// ErrBadHost reports a Host or SNI value that cannot name a tailnet.
var ErrBadHost = errors.New("host cannot name a tailnet")

// errConfig prefixes every backend configuration error New returns.
var errConfig = errors.New("frontdoor")

// clientTailnetHeaders are hints a client might send to pick a tailnet. They
// are never trusted and are removed before proxying.
var clientTailnetHeaders = []string{"X-Tailnet-ID", "X-Tailnet", "X-Mesh-Tailnet", "X-Tenant-ID"}

// Backend is one tailnet runtime behind the front door.
type Backend struct {
	Tailnet tailnet.ID
	// Hosts are the hostnames (any case, optional port) that select this tailnet.
	Hosts []string
	// Target is the runtime's loopback URL, e.g. http://127.0.0.1:18441.
	Target *url.URL
}

type route struct {
	tailnet tailnet.ID
	proxy   *httputil.ReverseProxy
}

// Door routes requests by Host to exactly one Backend.
type Door struct {
	byHost map[string]route
}

// New builds a Door. Every host must be a valid DNS name and belong to one
// tailnet only.
func New(backends []Backend) (*Door, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("%w: no backends", errConfig)
	}

	d := &Door{byHost: make(map[string]route)}

	for i, b := range backends {
		if b.Tailnet.IsZero() {
			return nil, fmt.Errorf("%w: backend %d has no tailnet", errConfig, i)
		}

		if b.Target == nil || b.Target.Host == "" {
			return nil, fmt.Errorf("%w: backend %s has no target", errConfig, b.Tailnet)
		}

		if len(b.Hosts) == 0 {
			return nil, fmt.Errorf("%w: backend %s has no hosts", errConfig, b.Tailnet)
		}

		r := route{tailnet: b.Tailnet, proxy: newProxy(b.Target)}

		for _, h := range b.Hosts {
			host, err := NormalizeHost(h)
			if err != nil {
				return nil, fmt.Errorf("frontdoor: backend %s: %w", b.Tailnet, err)
			}

			if prev, dup := d.byHost[host]; dup {
				return nil, fmt.Errorf("%w: host %q mapped to both %s and %s", errConfig, host, prev.tailnet, b.Tailnet)
			}

			d.byHost[host] = r
		}
	}

	return d, nil
}

func newProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Keep the client's Host so the runtime builds correct URLs.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		// Map polls stream; never buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn().Err(err).Str("host", r.Host).Msg("frontdoor: backend unavailable")
			http.Error(w, "tailnet unavailable", http.StatusBadGateway)
		},
	}
}

// ServeHTTP implements http.Handler.
func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, err := NormalizeHost(r.Host)
	if err != nil {
		misdirected(w, r, "bad host")
		return
	}

	if r.TLS != nil {
		sni, err := NormalizeHost(r.TLS.ServerName)
		if err != nil || sni != host {
			misdirected(w, r, "sni does not match host")
			return
		}
	}

	rt, ok := d.byHost[host]
	if !ok {
		misdirected(w, r, "unknown tailnet host")
		return
	}

	for _, h := range clientTailnetHeaders {
		r.Header.Del(h)
	}

	rt.proxy.ServeHTTP(w, r)
}

func misdirected(w http.ResponseWriter, r *http.Request, why string) {
	log.Debug().Str("host", r.Host).Str("path", r.URL.Path).Msg("frontdoor: refused: " + why)
	http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
}

// NormalizeHost lowercases a Host/SNI value, drops a valid port and one
// trailing dot, and rejects anything that is not a DNS hostname (including IP
// literals), so a tailnet is only ever selected by name.
func NormalizeHost(h string) (string, error) {
	if h == "" {
		return "", fmt.Errorf("%w: empty", ErrBadHost)
	}

	host := h
	if strings.HasPrefix(host, "[") {
		return "", fmt.Errorf("%w: IP literal %q", ErrBadHost, h)
	}

	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		port := host[i+1:]

		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 || strings.ContainsRune(host[:i], ':') {
			return "", fmt.Errorf("%w: bad port in %q", ErrBadHost, h)
		}

		host = host[:i]
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || len(host) > 253 {
		return "", fmt.Errorf("%w: %q", ErrBadHost, h)
	}

	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("%w: IP literal %q", ErrBadHost, h)
	}

	for label := range strings.SplitSeq(host, ".") {
		if !validLabel(label) {
			return "", fmt.Errorf("%w: %q", ErrBadHost, h)
		}
	}

	return host, nil
}

func validLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}

	for i := range len(l) {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}

	return true
}
