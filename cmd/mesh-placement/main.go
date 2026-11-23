// Command mesh-placement serves the operator API of the placement registry
// (P10) over mTLS. It opens the shared database with the database section of
// a Headscale configuration file (running the v0.1 migrations like a cluster
// process does) and never serves tenants.
//
// Usage:
//
//	mesh-placement -config /etc/mesh/config.yaml \
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

func main() {
	err := run()
	if err != nil {
		log.Fatalf("mesh-placement: %v", err)
	}
}

func run() error {
	var (
		cfgPath     = flag.String("config", "", "Headscale configuration file (only database.* is used)")
		listen      = flag.String("listen", "127.0.0.1:9443", "listen address (keep it on a private network)")
		certFile    = flag.String("tls-cert", "", "server certificate (PEM)")
		keyFile     = flag.String("tls-key", "", "server private key (PEM)")
		clientCA    = flag.String("client-ca", "", "pinned operator client CA bundle (PEM)")
		operators   = flag.String("operators", "", "comma-separated allowed operator identities (URI or DNS SANs)")
		sharedLimit = flag.Int64("shared-tenant-limit", placement.DefaultSharedTenantLimit, "device limit of a tailnet on a shared cluster")
		openapi     = flag.Bool("openapi", false, "print the OpenAPI 3.1 document and exit")
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
		return errors.New("-config, -tls-cert, -tls-key, -client-ca and -operators are required")
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
		return errors.New("client CA file holds no certificate")
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
