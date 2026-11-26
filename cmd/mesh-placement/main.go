// Command mesh-placement serves the operator API of the placement registry
// (P10) over mTLS. It opens the shared database with the database section of
// a Headscale configuration file and never serves tenants. Serving never
// migrates (ADR 0013): the schema is created and migrated by the separate
// `mesh-placement migrate` step, which the operator runs before an upgrade.
//
// Usage:
//
//	mesh-placement migrate -config /etc/mesh/config.yaml [-timeout 10m]
//	mesh-placement [serve] -config /etc/mesh/config.yaml \
//	  -tls-cert server.crt -tls-key server.key -client-ca operator-ca.crt \
//	  -operators spiffe://mesh/operator/alice[,...] [-listen 127.0.0.1:9443]
//	mesh-placement -openapi        # print the OpenAPI 3.1 document and exit
//
// Keys and the database password stay in files outside the repository; the
// command never logs them.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement/placementapi"
)

var (
	errFlagsRequired  = errors.New("-config, -tls-cert, -tls-key, -client-ca and -operators are required")
	errConfigRequired = errors.New("-config is required")
	errNoClientCA     = errors.New("client CA file holds no certificate")
)

func main() {
	err := run()
	if err != nil {
		log.Fatalf("mesh-placement: %v", err)
	}
}

func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return runMigrate(os.Args[2:])
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}

	var (
		cfgPath     = flag.String("config", "", "Headscale configuration file (only database.* is used)")
		listen      = flag.String("listen", "127.0.0.1:9443", "listen address (keep it on a private network)")
		certFile    = flag.String("tls-cert", "", "server certificate (PEM)")
		keyFile     = flag.String("tls-key", "", "server private key (PEM)")
		clientCA    = flag.String("client-ca", "", "pinned operator client CA bundle (PEM)")
		operators   = flag.String("operators", "", "comma-separated allowed operator identities (URI or DNS SANs)")
		sharedLimit = flag.Int64("shared-tenant-limit", placement.DefaultSharedTenantLimit, "device limit of a tailnet on a shared cluster")
		openapi     = flag.Bool("openapi", false, "print the OpenAPI 3.1 document and exit")
		schedEvery  = flag.Duration("schedule-interval", 5*time.Second, "how often due tailnet moves are requested and their hand-over recorded (P12b); 0 disables the scheduler")
	)

	flag.Parse()

	if *openapi {
		spec, err := placementapi.Spec()
		if err != nil {
			return err
		}

		_, err = os.Stdout.Write(spec)

		return err
	}

	if *cfgPath == "" || *certFile == "" || *keyFile == "" || *clientCA == "" || *operators == "" {
		return errFlagsRequired
	}

	auth, err := placementapi.NewAuthorizer(strings.Split(*operators, ","))
	if err != nil {
		return err
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("loading server certificate: %w", err)
	}

	caPEM, err := os.ReadFile(*clientCA)
	if err != nil {
		return fmt.Errorf("reading client CA: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errNoClientCA
	}

	tlsCfg, err := placementapi.TLSConfig(cert, pool)
	if err != nil {
		return err
	}

	loaded, err := config.Load(*cfgPath, true, os.Environ())
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	p, err := db.OpenPlatform(loaded.Config)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer p.Close()

	reg, err := placement.New(p.RegistryDB(), placement.Config{SharedTenantLimit: *sharedLimit})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           placementapi.Handler(reg, auth),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *schedEvery > 0 {
		go placement.RunScheduler(ctx, reg, *schedEvery, log.Printf)
	}

	errc := make(chan error, 1)

	go func() {
		log.Printf("mesh-placement: serving the operator API on %s (mTLS)", *listen)

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

// runMigrate is `mesh-placement migrate` (ADR 0013 §2): it creates the
// schema of a fresh database or runs the pending migrations, in one
// transaction under a fixed advisory lock (two runs at once: one waits,
// then has nothing to do), and prints what it did as JSON. Point -config at
// a direct connection to the primary where possible (ADR 0006 §4).
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Headscale configuration file (only database.* is used)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for another migrate run and to migrate")

	err := fs.Parse(args)
	if err != nil {
		return err
	}

	if *cfgPath == "" {
		return errConfigRequired
	}

	loaded, err := config.Load(*cfgPath, true, os.Environ())
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := db.Migrate(ctx, loaded.Config)
	if err != nil {
		return fmt.Errorf("migrating the database: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(res)
}
