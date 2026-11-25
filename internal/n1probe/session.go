// Package n1probe drives the control protocol below what a Tailscale
// client sends, for the Gate N1 checks that an official client cannot
// express (P09, owner decisions Q3 and Q4):
//
//   - S11: tenant switches on one Noise session (SwitchTenant);
//   - S13: tenant-A load against enrollment, bootstrap and map streams
//     (Flood), while the harness measures tenant B.
//
// It is the non-test form of the servertest raw Noise session
// (hscontrol/servertest/cluster_client_p05.go): one Session is one Noise
// session on one TCP connection, never redialled, so a refusal on a bound
// session cannot be masked by a fresh, unbound one.
//
// Nothing here prints or returns a credential: results carry outcomes,
// counts and shortened keys only.
package n1probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/control/ts2021"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsdial"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// ErrSessionUsed reports a request after the session's one connection: the
// transport wanted to redial, which would open a new, unbound session.
var ErrSessionUsed = errors.New("n1probe: noise session already used its one connection")

// ErrNotURLPort reports a dial to a port other than the control URL's. The
// ts2021 transport also tries a plain-HTTP fallback on port 80 even for an
// https URL with an explicit port (the lab clusters are on 8443 and 8445,
// and port 80 of the VPS answers EOF); such a dial is refused without using
// the session's one connection.
var ErrNotURLPort = errors.New("n1probe: dial to a port other than the control URL's")

var errKeyFetch = errors.New("n1probe: GET /key")

// urlPort is u's port, or the scheme's default.
func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}

	if u.Scheme == "http" {
		return "80"
	}

	return "443"
}

// systemDial opens the session's one TCP connection, to the URL's port
// only: another port is refused (ErrNotURLPort) and does not count; a
// second connection is refused (ErrSessionUsed).
func (s *Session) systemDial(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port != s.port {
		return nil, fmt.Errorf("%w: %s (want %s)", ErrNotURLPort, addr, s.port)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.conns) > 0 {
		return nil, ErrSessionUsed
	}

	var d net.Dialer

	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	tc := &trackedConn{Conn: conn}
	s.conns = append(s.conns, tc)

	return tc, nil
}

// Session is one Noise session (one machine key, one TCP connection).
type Session struct {
	client *ts2021.Client
	dialer *tsdial.Dialer
	host   string
	port   string // the URL's port: the only one the session dials

	mu    sync.Mutex
	conns []*trackedConn
}

type trackedConn struct {
	net.Conn

	peerClosed atomic.Bool
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		c.peerClosed.Store(true)
	}

	return n, err
}

// Response is a response read in full (at most 1 MiB).
type Response struct {
	Status int
	Body   []byte
}

// OK reports a 2xx status.
func (r *Response) OK() bool { return r != nil && r.Status >= 200 && r.Status < 300 }

// NewSession prepares a session as machine mk to the cluster at serverURL
// (http or https, as the client's control URL) with Noise key serverKey. It
// dials on the first request. logf may be nil (silent).
func NewSession(serverURL string, serverKey key.MachinePublic, mk key.MachinePrivate, logf logger.Logf) (*Session, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("n1probe: url: %w", err)
	}

	if logf == nil {
		logf = logger.Discard
	}

	s := &Session{host: u.Hostname(), port: urlPort(u)}

	dialer := tsdial.NewDialer(netmon.NewStatic())
	dialer.SetSystemDialer(s.systemDial)

	client, err := ts2021.NewClient(ts2021.ClientOpts{
		ServerURL:    serverURL,
		PrivKey:      mk,
		ServerPubKey: serverKey,
		Dialer:       dialer,
		Logf:         logf,
	})
	if err != nil {
		_ = dialer.Close()

		return nil, fmt.Errorf("n1probe: ts2021: %w", err)
	}

	s.client, s.dialer = client, dialer

	return s, nil
}

// Close releases the session.
func (s *Session) Close() {
	_ = s.client.Close()
	_ = s.dialer.Close()
}

// Dials is how many TCP connections (Noise sessions) were opened.
func (s *Session) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.conns)
}

// PeerClosed reports whether the server closed the session's connection.
func (s *Session) PeerClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.conns) > 0 && s.conns[0].peerClosed.Load()
}

// WaitPeerClosed waits up to d for the server to close the connection.
func (s *Session) WaitPeerClosed(d time.Duration) bool {
	if s.PeerClosed() {
		return true
	}

	deadline := time.NewTimer(d)
	defer deadline.Stop()

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline.C:
			return s.PeerClosed()
		case <-tick.C:
			if s.PeerClosed() {
				return true
			}
		}
	}
}

// Post sends body as JSON to path on the session with extra headers.
func (s *Session) Post(ctx context.Context, path string, nodeKey key.NodePublic, body any, hdr http.Header) (*Response, error) {
	jbody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+s.host+path, bytes.NewReader(jbody))
	if err != nil {
		return nil, err
	}

	ts2021.AddLBHeader(req, nodeKey)
	req.Header.Set("Content-Type", "application/json")

	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	return &Response{Status: resp.StatusCode, Body: b}, err
}

// LiteMap sends a non-streaming endpoint update (Stream=false,
// OmitPeers=true): the smallest map request that binds a session.
func (s *Session) LiteMap(ctx context.Context, nodeKey key.NodePublic, disco key.DiscoPublic, endpoints ...string) (*Response, error) {
	req := tailcfg.MapRequest{
		Version:   tailcfg.CurrentCapabilityVersion,
		NodeKey:   nodeKey,
		DiscoKey:  disco,
		OmitPeers: true,
		Endpoints: addrPorts(endpoints),
	}

	return s.Post(ctx, "/machine/map", nodeKey, req, nil)
}

// FullMap opens a map stream for nodeKey with extra headers and returns its
// first frame (the full MapResponse, uncompressed: Compress is empty), then
// closes the stream. A refusal returns the status and its body.
func (s *Session) FullMap(ctx context.Context, nodeKey key.NodePublic, disco key.DiscoPublic, hdr http.Header) (*Response, error) {
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := s.StreamMap(sctx, nodeKey, disco, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

		return &Response{Status: resp.StatusCode, Body: b}, nil
	}

	var lenb [4]byte

	_, err = io.ReadFull(resp.Body, lenb[:])
	if err != nil {
		return &Response{Status: resp.StatusCode}, fmt.Errorf("n1probe: first map frame: %w", err)
	}

	n := min(int64(binary.LittleEndian.Uint32(lenb[:])), 1<<20)

	b, err := io.ReadAll(io.LimitReader(resp.Body, n))

	return &Response{Status: resp.StatusCode, Body: b}, err
}

// Register sends req to /machine/register with extra headers.
func (s *Session) Register(ctx context.Context, req *tailcfg.RegisterRequest, hdr http.Header) (*Response, *tailcfg.RegisterResponse, error) {
	if req.Version == 0 {
		req.Version = tailcfg.CurrentCapabilityVersion
	}

	resp, err := s.Post(ctx, "/machine/register", req.NodeKey, req, hdr)
	if err != nil {
		return resp, nil, err
	}

	var rr tailcfg.RegisterResponse
	if resp.OK() {
		_ = json.Unmarshal(resp.Body, &rr)
	}

	return resp, &rr, nil
}

func addrPorts(ss []string) []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(ss))
	for _, s := range ss {
		ap, err := netip.ParseAddrPort(s)
		if err == nil {
			out = append(out, ap)
		}
	}

	return out
}

// FetchNoiseKey reads the cluster's Noise public key from /key, as clients do.
func FetchNoiseKey(ctx context.Context, serverURL string) (key.MachinePublic, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		serverURL+"/key?v="+strconv.Itoa(int(tailcfg.CurrentCapabilityVersion)), nil)
	if err != nil {
		return key.MachinePublic{}, err
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return key.MachinePublic{}, fmt.Errorf("n1probe: GET /key: %w", err)
	}
	defer resp.Body.Close()

	var k tailcfg.OverTLSPublicKeyResponse

	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&k)
	if err != nil {
		return key.MachinePublic{}, fmt.Errorf("%w: status %d, decode %w", errKeyFetch, resp.StatusCode, err)
	}

	if k.PublicKey.IsZero() {
		return key.MachinePublic{}, fmt.Errorf("%w: status %d, no key", errKeyFetch, resp.StatusCode)
	}

	return k.PublicKey, nil
}

// short is the first 12 characters after the type prefix of a key string,
// enough to tell keys apart in a report, never a full key.
func short(s string) string {
	for i := range len(s) {
		if s[i] == ':' {
			s = s[i+1:]

			break
		}
	}

	if len(s) > 12 {
		return s[:12]
	}

	return s
}

// StreamMap opens a streaming map request (Stream=true) for nodeKey and
// returns the open response; the caller reads frames and closes the body.
func (s *Session) StreamMap(ctx context.Context, nodeKey key.NodePublic, disco key.DiscoPublic, hdr http.Header) (*http.Response, error) {
	jbody, err := json.Marshal(tailcfg.MapRequest{
		Version:  tailcfg.CurrentCapabilityVersion,
		NodeKey:  nodeKey,
		DiscoKey: disco,
		Stream:   true,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+s.host+"/machine/map", bytes.NewReader(jbody))
	if err != nil {
		return nil, err
	}

	ts2021.AddLBHeader(req, nodeKey)
	req.Header.Set("Content-Type", "application/json")

	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	return s.client.Do(req)
}
