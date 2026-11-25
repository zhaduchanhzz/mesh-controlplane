// Command mesh-bootstrap serves the public bootstrap endpoint (P11): a
// client proves its tenant with a pre-auth key proof and gets the URL of the
// cluster that owns the tenant. It only reads the shared database, opened
// with db.OpenReadOnly (no schema creation, no migration).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/bootstrap"
)

var errFlagsRequired = errors.New("-config, -tls-cert, -tls-key and -hostname are required")

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
		ipRate      = flag.Float64("ip-rate", 0, "requests per second per client address; 0 disables (a TCP forwarder hides addresses)")
		ipBurst     = flag.Int("ip-burst", 20, "burst per client address")
	)

	flag.Parse()

	if *cfgPath == "" || *certFile == "" || *keyFile == "" || *hostname == "" {
		return errFlagsRequired
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

	errc := make(chan error, 1)

	go func() {
		log.Printf("mesh-bootstrap: serving %s on %s", *hostname, *listen)

		errc <- srv.ListenAndServeTLS("", "")
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
