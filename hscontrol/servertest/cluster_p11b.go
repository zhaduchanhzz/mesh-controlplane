//go:build p05cluster

package servertest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
)

// P11b: one cluster, several processes (active + standby) on one database
// and one Noise key, behind a front door that forwards each new TCP
// connection to the process whose /ready answers 200 (the lab's HAProxy
// with a /ready health check).

// ClusterProcess is one process of a [TestCluster].
type ClusterProcess struct {
	Cluster *hscontrol.Cluster
	// Addr is the process's own listener (not the cluster URL).
	Addr string
	// AdminSocket is the process's admin unix socket (P08b).
	AdminSocket string

	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

// frontDoor is a minimal L4 forwarder with a /ready health check.
type frontDoor struct {
	ln   net.Listener
	host string // Host header for health checks: the cluster hostname

	mu       sync.Mutex
	backends []string
}

func newFrontDoor(tb testing.TB, addr string) *frontDoor {
	tb.Helper()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		tb.Fatalf("servertest: front door: %v", err)
	}

	fd := &frontDoor{ln: ln}

	go fd.serve()

	tb.Cleanup(func() { _ = ln.Close() })

	return fd
}

func (fd *frontDoor) add(backend string) {
	fd.mu.Lock()
	fd.backends = append(fd.backends, backend)
	fd.mu.Unlock()
}

func (fd *frontDoor) ready(backend string) bool {
	req, err := http.NewRequest(http.MethodGet, "http://"+backend+"/ready", nil) //nolint:noctx // bounded by client timeout
	if err != nil {
		return false
	}

	req.Host = fd.host

	resp, err := (&http.Client{Timeout: 500 * time.Millisecond}).Do(req)
	if err != nil {
		return false
	}

	_ = resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

func (fd *frontDoor) serve() {
	for {
		conn, err := fd.ln.Accept()
		if err != nil {
			return
		}

		go fd.forward(conn)
	}
}

func (fd *frontDoor) forward(conn net.Conn) {
	defer conn.Close()

	fd.mu.Lock()
	backends := append([]string(nil), fd.backends...)
	fd.mu.Unlock()

	for _, b := range backends {
		if !fd.ready(b) {
			continue
		}

		up, err := net.DialTimeout("tcp", b, time.Second)
		if err != nil {
			continue
		}

		defer up.Close()

		done := make(chan struct{}, 2)

		go func() { _, _ = io.Copy(up, conn); done <- struct{}{} }()
		go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()

		<-done

		return
	}
	// No ready backend: the client sees the connection close and retries.
}

// startProcess runs one more process of the cluster on its own listener.
func (tc *TestCluster) startProcess(tb testing.TB, addr string) *ClusterProcess {
	tb.Helper()

	cfg := *tc.cfg
	cfg.Addr = cfgListenAddr(addr)
	// P08b: each process has its own admin socket (they would share a
	// host's path only if they shared the host).
	if len(tc.procs) > 0 && cfg.UnixSocket != "" {
		cfg.UnixSocket = fmt.Sprintf("%s.%d", cfg.UnixSocket, len(tc.procs))
	}

	// The first process of a test cluster creates its Noise key, as
	// `cluster serve --init-noise-key` does (P13e); later ones read it.
	c, err := hscontrol.NewCluster(&cfg, hscontrol.WithNoiseKeyInit())
	if err != nil {
		tb.Fatalf("servertest: NewCluster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &ClusterProcess{Cluster: c, Addr: addr, AdminSocket: cfg.UnixSocket, cancel: cancel, done: make(chan error, 1)}

	go func() { p.done <- c.Serve(ctx) }()

	tb.Cleanup(func() { tc.StopProcess(tb, p) })

	if tc.front != nil {
		tc.front.add(addr)
	}

	tc.procs = append(tc.procs, p)

	return p
}

// StartProcess starts another process of this cluster (same database, same
// Noise key, own listener) behind the front door and waits until it
// listens. It is standby while another process holds the lease.
func (tc *TestCluster) StartProcess(tb testing.TB) *ClusterProcess {
	tb.Helper()

	p := tc.startProcess(tb, freeLoopbackAddr(tb))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code := tc.ProcessGet(tb, p, "/health"); code == http.StatusOK {
			return p
		}

		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // polling a listener
	}

	tb.Fatalf("servertest: process at %s not listening after 10s", p.Addr)

	return nil
}

// Primary is the process NewTestClusterWith started.
func (tc *TestCluster) Primary() *ClusterProcess { return tc.procs[0] }

// StopProcess stops p gracefully (it releases its lease) and waits.
func (tc *TestCluster) StopProcess(tb testing.TB, p *ClusterProcess) {
	tb.Helper()

	p.once.Do(func() {
		p.cancel()

		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			tb.Errorf("servertest: process %s did not stop 30s after cancel", p.Addr)
		}
	})
}

// Use makes p the process behind tc.Cluster (Tenant, Runtime, HasRuntime).
func (tc *TestCluster) Use(p *ClusterProcess) { tc.Cluster = p.Cluster }

// ProcessGet sends GET path straight to p (not through the front door),
// with the cluster hostname as Host; it returns the status code, 0 on error.
func (tc *TestCluster) ProcessGet(tb testing.TB, p *ClusterProcess, path string) int {
	tb.Helper()

	return tc.processDo(tb, p, http.MethodGet, path, nil)
}

// ProcessDo sends method path straight to p (not through the front door),
// with the cluster hostname as Host, hdr and no body; it returns the status
// code, 0 on error.
func (tc *TestCluster) ProcessDo(tb testing.TB, p *ClusterProcess, method, path string, hdr http.Header) int {
	tb.Helper()

	return tc.processDo(tb, p, method, path, hdr)
}

func (tc *TestCluster) processDo(tb testing.TB, p *ClusterProcess, method, path string, hdr http.Header) int {
	tb.Helper()

	req, err := http.NewRequest(method, "http://"+p.Addr+path, nil) //nolint:noctx // bounded by client timeout
	if err != nil {
		tb.Fatal(err)
	}

	req.Host = tc.host

	for k, v := range hdr {
		req.Header[k] = v
	}

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// ProcessNoiseUpgrade asks p directly for a Noise upgrade and returns the
// status (a standby must refuse before any handshake).
func (tc *TestCluster) ProcessNoiseUpgrade(tb testing.TB, p *ClusterProcess) int {
	tb.Helper()

	return tc.processDo(tb, p, http.MethodPost, "/ts2021", http.Header{
		"Upgrade":    {"tailscale-control-protocol"},
		"Connection": {"upgrade"},
	})
}

// WaitReady waits until p serves (holds the lease, tailnets loaded).
func (tc *TestCluster) WaitReady(tb testing.TB, p *ClusterProcess, timeout time.Duration) time.Duration {
	tb.Helper()

	start := time.Now()
	for time.Since(start) < timeout {
		if tc.ProcessGet(tb, p, "/ready") == http.StatusOK {
			return time.Since(start)
		}

		time.Sleep(25 * time.Millisecond) //nolint:forbidigo // polling a listener
	}

	tb.Fatalf("servertest: process %s not ready after %s", p.Addr, timeout)

	return 0
}

// LeaseRow reads the cluster's lease row.
func (tc *TestCluster) LeaseRow(tb testing.TB) lease.Row {
	tb.Helper()

	row, ok, err := lease.NewStore(tc.raw).Get(context.Background(), tc.ClusterID)
	if err != nil || !ok {
		tb.Fatalf("servertest: lease row: ok=%v err=%v", ok, err)
	}

	return row
}

// ExpireLease ends the current lease in the database, as if the active
// process had been unable to renew for a whole TTL (paused, partitioned).
func (tc *TestCluster) ExpireLease(tb testing.TB) {
	tb.Helper()

	err := tc.raw.Exec(`UPDATE cluster_leases SET expires_at = `+lease.SQLiteNow+` WHERE cluster_id = ?`, tc.ClusterID).Error
	if err != nil {
		tb.Fatalf("servertest: expire lease: %v", err)
	}
}
