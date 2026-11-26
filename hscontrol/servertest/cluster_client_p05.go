//go:build p05cluster

package servertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/control/controlclient"
	"tailscale.com/control/ts2021"
	"tailscale.com/health"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsdial"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
	"tailscale.com/types/persist"
	"tailscale.com/util/eventbus"
)

// ClusterClientOptions configures [TryNewClusterClient].
type ClusterClientOptions struct {
	// MachineKey is the device's machine key; zero means a new one. Two
	// clients given the same key are two profiles of one device.
	MachineKey key.MachinePrivate
	// AuthKey is the registration credential; "" registers without one.
	AuthKey string
	// Persist is the login to resume (another profile's, for a profile
	// switch back); zero means a new profile.
	Persist persist.Persist
	// Hostname defaults to the client name.
	Hostname string
	// SkipLogin returns the client before its first login, so the caller
	// can drive [TestClient.Direct] itself.
	SkipLogin bool
}

// NewClusterClient is [TryNewClusterClient] for a login that must succeed.
func NewClusterClient(tb testing.TB, srv *TestServer, name string, o ClusterClientOptions) *TestClient {
	tb.Helper()

	c, err := TryNewClusterClient(tb, srv, name, o)
	if err != nil {
		tb.Fatalf("servertest: NewClusterClient(%s): %v", name, err)
	}

	return c
}

// TryNewClusterClient is [TryNewClient] for a [TestCluster]: srv is a view
// from [TestCluster.Tenant] or [TestCluster.Front], the client dials the
// cluster's real loopback listener, mints no credential of its own, and can
// share a machine key with other clients. It returns the login error, and
// then a client that holds no node and does not poll.
func TryNewClusterClient(tb testing.TB, srv *TestServer, name string, o ClusterClientOptions) (*TestClient, error) {
	tb.Helper()

	mk := o.MachineKey
	if mk.IsZero() {
		mk = key.NewMachine()
	}

	hostname := o.Hostname
	if hostname == "" {
		hostname = name
	}

	bus := eventbus.New()
	tracker := health.NewTracker(bus)
	dialer := tsdial.NewDialer(netmon.NewStatic())
	dialer.SetBus(bus)
	dialer.SetSystemDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})

	tc := &TestClient{
		Name:       name,
		server:     srv,
		authKey:    o.AuthKey,
		machineKey: mk,
		discoKey:   key.NewDisco().Public(),
		updates:    make(chan *netmap.NetworkMap, 64),
		firstMap:   make(chan struct{}),
		bus:        bus,
		dialer:     dialer,
		tracker:    tracker,
	}
	tc.logf = func(format string, args ...any) { tb.Logf("[%s] %s", name, fmt.Sprintf(format, args...)) }

	hi := &tailcfg.Hostinfo{BackendLogID: "servertest-" + name, Hostname: hostname}

	direct, err := tc.newDirect(o.Persist, o.AuthKey, hi)
	if err != nil {
		tb.Fatalf("servertest: NewDirect(%s): %v", name, err)
	}

	tc.direct = direct

	tb.Cleanup(tc.cleanup)

	if o.SkipLogin {
		return tc, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = tc.tryLogin(ctx, controlclient.LoginDefault, nil)
	if err != nil {
		return tc, err
	}

	tc.startPoll(tb)

	select {
	case <-tc.firstMap:
	case <-tc.pollDone:
	case <-time.After(10 * time.Second):
		tb.Fatalf("servertest: NewClusterClient(%s): no map after 10s", name)
	}

	return tc, nil
}

// RawNoiseSession is one Noise session (one machine key, one TCP connection)
// to a [TestCluster], driven request by request, for tests that need what a
// [controlclient.Direct] never sends: two tenants on one session (I01),
// spoofed headers (S10/S11d), a register without credentials.
//
// It never dials twice: once the server closes the connection, later
// requests fail instead of opening a new, unbound session.
type RawNoiseSession struct {
	client *ts2021.Client
	host   string

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

var errSessionUsed = errors.New("servertest: raw noise session already used its one connection")

// RawResponse is a response read in full.
type RawResponse struct {
	Status int
	Body   []byte
}

// OK reports a 2xx status.
func (r *RawResponse) OK() bool { return r != nil && r.Status >= 200 && r.Status < 300 }

// NewRawNoiseSession prepares a session as machine mk to the cluster at
// serverURL with Noise key serverKey. It dials on the first request.
func NewRawNoiseSession(tb testing.TB, serverURL string, serverKey key.MachinePublic, mk key.MachinePrivate) *RawNoiseSession {
	tb.Helper()

	u, err := url.Parse(serverURL)
	if err != nil {
		tb.Fatalf("servertest: raw noise URL: %v", err)
	}

	s := &RawNoiseSession{host: u.Hostname()}

	dialer := tsdial.NewDialer(netmon.NewStatic())
	dialer.SetSystemDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		// One RawNoiseSession is one Noise session: a request the transport
		// would retry on a fresh connection fails instead, so a refusal on
		// the bound session is never masked by a new, unbound one.
		if len(s.conns) > 0 {
			return nil, errSessionUsed
		}

		var d net.Dialer

		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		tc := &trackedConn{Conn: conn}
		s.conns = append(s.conns, tc)

		return tc, nil
	})

	client, err := ts2021.NewClient(ts2021.ClientOpts{
		ServerURL:    serverURL,
		PrivKey:      mk,
		ServerPubKey: serverKey,
		Dialer:       dialer,
		Logf:         tb.Logf,
	})
	if err != nil {
		tb.Fatalf("servertest: ts2021.NewClient: %v", err)
	}

	s.client = client

	tb.Cleanup(func() {
		_ = client.Close()
		_ = dialer.Close()
	})

	return s
}

// Close closes the session's connection, as a client going away does.
func (s *RawNoiseSession) Close() error {
	return s.client.Close()
}

// Dials is how many TCP connections (Noise sessions) the session has opened.
func (s *RawNoiseSession) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.conns)
}

// PeerClosed reports whether reading the i-th connection ended: the server
// closed it (the client side never closes it before cleanup).
func (s *RawNoiseSession) PeerClosed(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return i < len(s.conns) && s.conns[i].peerClosed.Load()
}

// Post sends body as JSON to path with extra headers on the session.
func (s *RawNoiseSession) Post(ctx context.Context, path string, nodeKey key.NodePublic, body any, hdr http.Header) (*RawResponse, error) {
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
	if err != nil {
		return &RawResponse{Status: resp.StatusCode, Body: b}, err
	}

	return &RawResponse{Status: resp.StatusCode, Body: b}, nil
}

// LiteMap sends a non-streaming endpoint update (Stream=false,
// OmitPeers=true) for nodeKey: the server applies endpoints and answers 200
// with no body. It is the smallest map request that binds a session.
func (s *RawNoiseSession) LiteMap(
	ctx context.Context,
	nodeKey key.NodePublic,
	disco key.DiscoPublic,
	endpoints ...string,
) (*RawResponse, error) {
	req := tailcfg.MapRequest{
		Version:   tailcfg.CurrentCapabilityVersion,
		NodeKey:   nodeKey,
		DiscoKey:  disco,
		Stream:    false,
		OmitPeers: true,
		Endpoints: parseAddrPorts(endpoints),
	}

	return s.Post(ctx, "/machine/map", nodeKey, req, nil)
}

// Register sends req to /machine/register with extra headers.
func (s *RawNoiseSession) Register(ctx context.Context, req *tailcfg.RegisterRequest, hdr http.Header) (*RawResponse, *tailcfg.RegisterResponse, error) {
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

func parseAddrPorts(ss []string) []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddrPort(s))
	}

	return out
}
