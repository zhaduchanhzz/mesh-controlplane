// Command mesh-bootstrap serves the public bootstrap endpoint (P11): a
// client proves its tenant with a pre-auth key proof and gets the URL of the
// cluster that owns the tenant. It only reads the shared database, opened
// with db.OpenReadOnly (no schema creation, no migration).
//
// P13f: behind the lab's front door (a separate HAProxy on the VPS) the
// listener accepts the PROXY protocol v2 header from -proxy-protocol-from
// only, so the per-address limit sees the client's address; an operator
// metrics listener (-metrics-listen) serves /metrics and /health to
// loopback and -metrics-operator-cidrs only.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/bootstrap"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/dbstats"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/opsmetrics"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/proxyproto"
)

var (
	errFlagsRequired = errors.New("-config, -tls-cert, -tls-key and -hostname are required")
	errNoPool        = errors.New("database pool unavailable")
)

// proxyRefused counts connections refused by the PROXY v2 listener.
var proxyRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "mesh_bootstrap",
	Name:      "proxy_refused_total",
	Help: "Connections closed by the PROXY v2 listener, by reason: missing_header, malformed, unsupported, " +
		"timeout (trusted front door), untrusted_header (a PROXY header from any other peer).",
}, []string{"reason"})

func main() {
	err := run()
	if err != nil {
		log.Fatalf("mesh-bootstrap: %v", err)
	}
}

func run() error {
	def := bootstrap.DefaultHandlerConfig("")

	var (
		cfgPath     = flag.String("config", "", "Headscale configuration file (only database.* is used)")
		listen      = flag.String("listen", "127.0.0.1:8444", "listen address")
		certFile    = flag.String("tls-cert", "", "server certificate (PEM)")
		keyFile     = flag.String("tls-key", "", "server private key (PEM)")
		hostname    = flag.String("hostname", "", "the public hostname served; other Host or SNI gets 421")
		globalRate  = flag.Float64("global-rate", def.GlobalRate, "requests per second over all callers")
		globalBurst = flag.Int("global-burst", def.GlobalBurst, "burst over all callers")
		tenantRate  = flag.Float64("tenant-rate", def.TenantRate, "answers per second per tenant (after verification)")
		tenantBurst = flag.Int("tenant-burst", def.TenantBurst, "burst per tenant")
		ipRate      = flag.Float64("ip-rate", -1, fmt.Sprintf("requests per second per client address (IPv6: per /64); "+
			"-1: %v with -proxy-protocol-from, 0 (off) without it (a TCP forwarder hides addresses); 0 disables", def.IPRate))
		ipBurst      = flag.Int("ip-burst", def.IPBurst, "burst per client address")
		proxyFrom    = flag.String("proxy-protocol-from", "", "comma-separated CIDRs of the front door that sends PROXY protocol v2 (e.g. 10.99.77.1/32); they must send it, any other peer must not; empty: off")
		proxyTimeout = flag.Duration("proxy-header-timeout", proxyproto.DefaultHeaderTimeout, "wait for the PROXY v2 header")
		metricsAddr  = flag.String("metrics-listen", "", "operator metrics listener (/metrics, /health); empty: off. Never a public address")
		metricsCIDRs = flag.String("metrics-operator-cidrs", "", "comma-separated CIDRs allowed on -metrics-listen besides loopback (never the Tailscale range)")
	)

	flag.Parse()

	if *cfgPath == "" || *certFile == "" || *keyFile == "" || *hostname == "" {
		return errFlagsRequired
	}

	trusted, err := proxyproto.ParseTrusted(*proxyFrom)
	if err != nil {
		return fmt.Errorf("-proxy-protocol-from: %w", err)
	}

	operators, err := opsmetrics.ParseOperatorCIDRs(*metricsCIDRs)
	if err != nil {
		return fmt.Errorf("-metrics-operator-cidrs: %w", err)
	}

	if *ipRate < 0 {
		*ipRate = 0
		if len(trusted) > 0 {
			*ipRate = def.IPRate
		}
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("loading server certificate: %w", err)
	}

	loaded, err := config.Load(*cfgPath, true, os.Environ())
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	ro, err := db.OpenReadOnly(loaded.Config)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}

	sqlDB, err := ro.DB()
	if err == nil {
		defer sqlDB.Close()
		defer dbstats.Register("bootstrap", sqlDB)()
	}

	handler := bootstrap.NewHandler(bootstrap.NewService(bootstrap.NewGormStore(ro)), bootstrap.HandlerConfig{
		Hostname:   *hostname,
		GlobalRate: *globalRate, GlobalBurst: *globalBurst,
		TenantRate: *tenantRate, TenantBurst: *tenantBurst,
		IPRate: *ipRate, IPBurst: *ipBurst,
	})

	srv := &http.Server{
		Addr:    *listen,
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}

	if len(trusted) > 0 {
		ln, err = proxyproto.NewListener(ln, proxyproto.Config{
			Trusted: trusted, HeaderTimeout: *proxyTimeout,
			Refused: func(reason string) { proxyRefused.WithLabelValues(reason).Inc() },
		})
		if err != nil {
			return err
		}
	}

	errc := make(chan error, 2)

	if *metricsAddr != "" {
		maddr, merrc, err := opsmetrics.Serve(ctx, *metricsAddr, opsmetrics.Handler(operators, func(ctx context.Context) error {
			if sqlDB == nil {
				return errNoPool
			}

			return sqlDB.PingContext(ctx)
		}))
		if err != nil {
			return err
		}

		go func() {
			if err := <-merrc; err != nil { //nolint:noinlineerr
				errc <- fmt.Errorf("metrics listener: %w", err)
			}
		}()

		log.Printf("mesh-bootstrap: operator metrics on %s", maddr)
	}

	go func() {
		log.Printf("mesh-bootstrap: serving %s on %s (PROXY v2 from %v, per-address limit %v/s)",
			*hostname, *listen, trusted, *ipRate)

		errc <- srv.ServeTLS(ln, "", "")
	}()

	select {
	case err = <-errc:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}
