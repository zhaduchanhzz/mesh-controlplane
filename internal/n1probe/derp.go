package n1probe

import (
	"context"
	"errors"
	"time"

	"tailscale.com/derp/derphttp"
	"tailscale.com/net/netmon"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// DERPCheckConfig configures DERPCheck (S15, ADR 0009): connect to one
// DERP node as a client with a fresh random node key, as a real client
// would: dial IP:Port, TLS with SNI = Host, certificate verified against
// the system roots.
type DERPCheckConfig struct {
	Host    string // DERP node hostname (SNI, certificate name)
	IP      string // IPv4 to dial
	Port    int    // DERP TLS port
	Count   int    // attempts, each with a new node key (default 3)
	Timeout time.Duration
	Logf    logger.Logf

	// insecureForTests skips certificate verification (in-process tests
	// only; never set by the command).
	insecureForTests bool
}

// DERPAttempt is one connection attempt. Admitted is true iff the server
// sent a frame after the handshake: a DERP server sends its first frame
// (ServerInfo) only once the client is verified; a refused client sees the
// connection closed, or no frame, instead.
type DERPAttempt struct {
	Attempt   int     `json:"attempt"`
	Admitted  bool    `json:"admitted"`
	Error     string  `json:"error,omitempty"`
	ElapsedMs float64 `json:"elapsed_ms"`
}

var (
	errNoFrame       = errors.New("no frame from the server before the timeout")
	errDERPCheckArgs = errors.New("n1probe: derp-check needs --host, --ip and --port")
)

// DERPCheck runs cfg.Count attempts, each with a newly generated node key
// that no control server knows.
func DERPCheck(ctx context.Context, cfg DERPCheckConfig) ([]DERPAttempt, error) {
	if cfg.Host == "" || cfg.IP == "" || cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, errDERPCheckArgs
	}

	if cfg.Count <= 0 {
		cfg.Count = 3
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}

	if cfg.Logf == nil {
		cfg.Logf = logger.Discard
	}

	region := &tailcfg.DERPRegion{
		RegionID:   900,
		RegionCode: "n1probe",
		Nodes: []*tailcfg.DERPNode{{
			Name:             "n1probe",
			RegionID:         900,
			HostName:         cfg.Host,
			IPv4:             cfg.IP,
			IPv6:             "none",
			DERPPort:         cfg.Port,
			InsecureForTests: cfg.insecureForTests,
		}},
	}

	out := make([]DERPAttempt, 0, cfg.Count)

	for i := 1; i <= cfg.Count; i++ {
		out = append(out, derpAttempt(ctx, cfg, region, i))
	}

	return out, nil
}

func derpAttempt(ctx context.Context, cfg DERPCheckConfig, region *tailcfg.DERPRegion, n int) DERPAttempt {
	a := DERPAttempt{Attempt: n}
	t0 := time.Now()

	defer func() { a.ElapsedMs = float64(time.Since(t0).Microseconds()) / 1000 }()

	c := derphttp.NewRegionClient(key.NewNode(), cfg.Logf, netmon.NewStatic(), func() *tailcfg.DERPRegion { return region })
	defer c.Close()

	actx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	err := c.Connect(actx)
	if err != nil {
		a.Error = "connect: " + err.Error()

		return a
	}

	type recv struct{ err error }

	done := make(chan recv, 1)

	go func() {
		_, err := c.Recv()
		done <- recv{err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			a.Error = "recv: " + r.err.Error()

			return a
		}

		a.Admitted = true
	case <-actx.Done():
		_ = c.Close()
		a.Error = errNoFrame.Error()
	}

	return a
}
