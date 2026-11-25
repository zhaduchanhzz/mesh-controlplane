package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/frontdoor"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"golang.org/x/sync/errgroup"
)

// serve-tailnets is the P02 proof of concept: several tailnets in ONE process,
// each a full runtime with its own config (server_url, database, Noise key)
// listening on loopback, behind a front door that picks the tailnet by Host.
// It is not the final multi-tenant design (see tickets/P02.md, ADR 0001).

var (
	serveTailnetsListen  string
	serveTailnetsTLSCert string
	serveTailnetsTLSKey  string
	serveTailnetsSpecs   []string
)

func init() {
	serveTailnetsCmd.Flags().StringVar(&serveTailnetsListen, "listen", "0.0.0.0:8443", "front door listen address")
	serveTailnetsCmd.Flags().StringVar(&serveTailnetsTLSCert, "tls-cert", "", "front door TLS certificate (PEM)")
	serveTailnetsCmd.Flags().StringVar(&serveTailnetsTLSKey, "tls-key", "", "front door TLS key (PEM)")
	serveTailnetsCmd.Flags().StringArrayVar(&serveTailnetsSpecs, "tailnet", nil,
		"id=<uuid>;hosts=<host>[,<host>...];config=<path> (repeat per tailnet)")
	rootCmd.AddCommand(serveTailnetsCmd)
}

var serveTailnetsCmd = &cobra.Command{
	Use:   "serve-tailnets",
	Short: "POC: serve several tailnets from one process, chosen by Host",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if len(serveTailnetsSpecs) < 1 {
			return errors.New("at least one --tailnet is required")
		}

		if serveTailnetsTLSCert == "" || serveTailnetsTLSKey == "" {
			return errors.New("--tls-cert and --tls-key are required: the front door always terminates TLS")
		}

		cert, err := tls.LoadX509KeyPair(serveTailnetsTLSCert, serveTailnetsTLSKey)
		if err != nil {
			return fmt.Errorf("loading front door certificate: %w", err)
		}

		var (
			apps     []*hscontrol.Headscale
			backends []frontdoor.Backend
		)

		for _, spec := range serveTailnetsSpecs {
			ts, err := parseTailnetSpec(spec)
			if err != nil {
				return err
			}

			loaded, err := config.Load(ts.config, true, os.Environ())
			if err != nil {
				return fmt.Errorf("tailnet %s: loading %s: %w", ts.id, ts.config, err)
			}

			cfg := loaded.Config

			// Every runtime here is a tenant tailnet: no policy, an empty
			// one, or one without acls/grants denies everything (P03a).
			cfg.PolicyEmptyDenies = true

			target, err := loopbackTarget(string(cfg.Addr))
			if err != nil {
				return fmt.Errorf("tailnet %s: %w", ts.id, err)
			}

			if cfg.TLS.CertPath != "" || cfg.TLS.ACME.Hostname != "" {
				return fmt.Errorf("tailnet %s: runtime must not terminate TLS; the front door does", ts.id)
			}

			app, err := hscontrol.NewHeadscale(cfg)
			if err != nil {
				return fmt.Errorf("tailnet %s: %w", ts.id, err)
			}

			apps = append(apps, app)
			backends = append(backends, frontdoor.Backend{Tailnet: ts.id, Hosts: ts.hosts, Target: target})

			log.Info().Str("tailnet", ts.id.String()).Strs("hosts", ts.hosts).Str("runtime", target.String()).
				Str("server_url", cfg.ServerURL).Msg("serve-tailnets: tailnet configured")
		}

		door, err := frontdoor.New(backends)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		defer stop()

		g, ctx := errgroup.WithContext(ctx)

		for _, app := range apps {
			g.Go(func() error { return app.Serve(ctx) })
		}

		srv := &http.Server{
			Addr:              serveTailnetsListen,
			Handler:           door,
			ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
				// The Tailscale control upgrade is HTTP/1.1.
				NextProtos: []string{"http/1.1"},
			},
		}

		g.Go(func() error {
			log.Info().Str("listen", serveTailnetsListen).Msg("serve-tailnets: front door listening (TLS)")

			err := srv.ListenAndServeTLS("", "")
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		})

		g.Go(func() error {
			<-ctx.Done()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			return srv.Shutdown(shutdownCtx) //nolint:contextcheck // parent is already cancelled
		})

		err = g.Wait()
		if err != nil && !errors.Is(err, context.Canceled) {
			return classifyServeError(err)
		}

		return nil
	},
}

type tailnetSpec struct {
	id     tailnet.ID
	hosts  []string
	config string
}

func parseTailnetSpec(s string) (tailnetSpec, error) {
	var ts tailnetSpec

	for part := range strings.SplitSeq(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return ts, fmt.Errorf("--tailnet %q: expected key=value", part)
		}

		switch strings.TrimSpace(k) {
		case "id":
			id, err := tailnet.ParseID(strings.TrimSpace(v))
			if err != nil {
				return ts, fmt.Errorf("--tailnet: %w", err)
			}

			ts.id = id
		case "hosts":
			for h := range strings.SplitSeq(v, ",") {
				if h = strings.TrimSpace(h); h != "" {
					ts.hosts = append(ts.hosts, h)
				}
			}
		case "config":
			ts.config = strings.TrimSpace(v)
		default:
			return ts, fmt.Errorf("--tailnet: unknown key %q", k)
		}
	}

	if ts.id.IsZero() || len(ts.hosts) == 0 || ts.config == "" {
		return ts, fmt.Errorf("--tailnet %q: id, hosts and config are required", s)
	}

	return ts, nil
}

// loopbackTarget only accepts a loopback listen address, so a runtime can
// never be reached around the front door.
func loopbackTarget(addr string) (*url.URL, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("listen_addr %q: %w", addr, err)
	}

	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("listen_addr %q must be a loopback address behind the front door", addr)
	}

	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}, nil
}
