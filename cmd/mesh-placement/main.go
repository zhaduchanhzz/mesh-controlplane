// Command mesh-placement serves the operator API of the placement registry
// (P10) over mTLS. It opens the shared database with the database section of
// a Headscale configuration file and never serves tenants. Serving never
// migrates (ADR 0013): the schema is created and migrated by the separate
// `mesh-placement migrate` step, which the operator runs before an upgrade.
//
// Usage:
//
//	mesh-placement migrate -config /etc/mesh/config.yaml [-timeout 10m]
//	mesh-placement epoch-fence -config /etc/mesh/config.yaml -reason "restore to T" \
//	  -operator alice [-external-hwm N]   # after a restore or forced promote (ADR 0012)
//	mesh-placement [serve] -config /etc/mesh/config.yaml \
//	  -tls-cert server.crt -tls-key server.key -client-ca operator-ca.crt \
//	  -operators spiffe://mesh/operator/alice[,...] [-listen 127.0.0.1:9443] \
//	  [-revoked-certs /etc/mesh/revoked-operators] \
//	  [-metrics-listen 192.168.1.241:9098 -metrics-operator-cidrs 192.168.1.243/32]
//	mesh-placement -openapi        # print the OpenAPI 3.1 document and exit
//
// Keys and the database password stay in files outside the repository; the
// command never logs them.
//
// P13f: -revoked-certs refuses revoked operator certificates (L5; fail
// closed on an unreadable file); -metrics-listen serves /metrics and /health
// to loopback and -metrics-operator-cidrs only (AGENTS rule 9).
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/dbstats"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/opsmetrics"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement/placementapi"
)

var (
	errFlagsRequired  = errors.New("-config, -tls-cert, -tls-key, -client-ca and -operators are required")
	errConfigRequired = errors.New("-config is required")
	errFenceFlags     = errors.New("-config, -reason and -operator are required")
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
		case "epoch-fence":
			return runEpochFence(os.Args[2:])
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}

	return runServe()
}

// runServe is `mesh-placement [serve]`: the operator API over mTLS, the
// move scheduler and, when asked, the operator metrics listener.
func runServe() error {
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
		metricsAddr = flag.String("metrics-listen", "", "operator metrics listener (/metrics, /health); empty: off. Never a public address")
		metricsNets = flag.String("metrics-operator-cidrs", "", "comma-separated CIDRs allowed on -metrics-listen besides loopback (never the Tailscale range)")
		revokedPath = flag.String("revoked-certs", "", "operator certificate revocation file (serial:<hex> or sha256:<hex> per line); unreadable or invalid: every request refused")
		revokedTick = flag.Duration("revoked-certs-reload", 10*time.Second, "how often -revoked-certs is read again")
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

	operatorNets, err := opsmetrics.ParseOperatorCIDRs(*metricsNets)
	if err != nil {
		return fmt.Errorf("-metrics-operator-cidrs: %w", err)
	}

	var revoked *placementapi.Revocations

	if *revokedPath != "" {
		revoked, err = placementapi.LoadRevocations(*revokedPath)
		if err != nil {
			return err
		}

		auth.WithRevocations(revoked)
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
		Handler:           countRequests(placementapi.Handler(reg, auth)),
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

	if revoked != nil {
		go revoked.Run(ctx, *revokedTick, log.Printf)
	}

	if sqlDB, err := p.RegistryDB().DB(); err == nil { //nolint:noinlineerr
		defer dbstats.Register("placement", sqlDB)()
	}

	errc := make(chan error, 2)

	if *metricsAddr != "" {
		maddr, merrc, err := opsmetrics.Serve(ctx, *metricsAddr, opsmetrics.Handler(operatorNets, p.Ping))
		if err != nil {
			return err
		}

		go func() {
			if err := <-merrc; err != nil { //nolint:noinlineerr
				errc <- fmt.Errorf("metrics listener: %w", err)
			}
		}()

		log.Printf("mesh-placement: operator metrics on %s", maddr)
	}

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

// runEpochFence is `mesh-placement epoch-fence` (ADR 0012 §1): after a
// restore or a promote that may have lost commits, and before PgBouncer lets
// the cluster processes back in, it raises every lease epoch and owner_epoch
// above the time-based floor and above every epoch present, in one
// transaction, and prints the result as JSON. Point -config at a direct
// connection to the primary (ADR 0006 §4). Running it again raises the
// epochs again (safe; every cluster reloads its tailnets once more).
func runEpochFence(args []string) error {
	fs := flag.NewFlagSet("epoch-fence", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Headscale configuration file (only database.* is used)")
	reason := fs.String("reason", "", "why the fence runs (recorded), e.g. the restore target")
	operator := fs.String("operator", "", "who runs it (recorded)")
	hwm := fs.Uint64("external-hwm", 0, "highest epoch recorded outside the database, if any")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")

	err := fs.Parse(args)
	if err != nil {
		return err
	}

	if *cfgPath == "" || *reason == "" || *operator == "" {
		return errFenceFlags
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

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := p.EpochFence(ctx, db.EpochFenceRequest{
		Reason: *reason, Operator: *operator, OperatorNow: time.Now(), ExternalHWM: *hwm,
	})
	if err != nil {
		return err
	}

	return json.NewEncoder(os.Stdout).Encode(res)
}

// apiRequests counts operator API requests by method and status (P13f), on
// the operator metrics listener.
var apiRequests = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "mesh_placement",
	Name:      "http_requests_total",
	Help:      "Operator API requests by method and HTTP status code.",
}, []string{"method", "code"})

type statusRecorder struct {
	http.ResponseWriter

	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}

	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}

	return s.ResponseWriter.Write(b)
}

func countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.code == 0 {
			rec.code = http.StatusOK
		}

		method := r.Method
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodHead:
		default:
			method = "other"
		}

		apiRequests.WithLabelValues(method, strconv.Itoa(rec.code)).Inc()
	})
}
