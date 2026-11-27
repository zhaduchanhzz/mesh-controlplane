// Package opsmetrics is the operator metrics listener of the platform
// binaries without a cluster config (mesh-bootstrap, mesh-placement; P13f).
// It follows the cluster's metrics_listen_addr rules (AGENTS rule 9, P08
// S14/I09):
//
//   - a separate listener, never the public one, off unless configured;
//   - a request is served only when its TCP peer is loopback or inside an
//     operator CIDR given explicitly (none by default); the Tailscale range
//     (100.64.0.0/10, fd7a:115c:a1e0::/48) is never an operator, since every
//     tenant node owns addresses there, and such CIDRs are refused at
//     startup; X-Forwarded-For and PROXY headers are never read;
//   - only GET /metrics and GET /health; no debug or profiling pages.
package opsmetrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	errTailscaleRange = errors.New("an operator CIDR may not overlap the Tailscale address space (100.64.0.0/10, fd7a:115c:a1e0::/48): tenant nodes own addresses there")
	errZeroPrefix     = errors.New("an operator CIDR of length 0 would admit every peer")
)

var (
	tailscaleCGNATRange = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleULARange   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// ParseOperatorCIDRs parses a comma-separated CIDR list ("" is none). A
// /0 or a range overlapping the Tailscale address space is refused.
func ParseOperatorCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix

	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}

		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("operator CIDR %q: %w", f, err)
		}

		p = p.Masked()

		switch {
		case p.Bits() == 0:
			return nil, fmt.Errorf("operator CIDR %s: %w", p, errZeroPrefix)
		case p.Overlaps(tailscaleCGNATRange) || p.Overlaps(tailscaleULARange):
			return nil, fmt.Errorf("operator CIDR %s: %w", p, errTailscaleRange)
		}

		out = append(out, p)
	}

	return out, nil
}

// IsOperatorIP reports whether a peer at ip may use an operator listener:
// loopback, or inside one of cidrs, and never in the Tailscale range.
func IsOperatorIP(ip netip.Addr, cidrs []netip.Prefix) bool {
	ip = ip.Unmap()

	if ip.IsLoopback() {
		return true
	}

	if tailscaleCGNATRange.Contains(ip) || tailscaleULARange.Contains(ip) {
		return false
	}

	for _, p := range cidrs {
		if p.Bits() > 0 && p.Contains(ip) {
			return true
		}
	}

	return false
}

// healthTimeout bounds one /health check.
const healthTimeout = 3 * time.Second

// Handler serves GET /metrics (the default Prometheus registry) and, when
// health is set, GET /health ({"status":"pass"} or 503 {"status":"fail"}),
// to operators only.
func Handler(cidrs []netip.Prefix, health func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())

	if health != nil {
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/health+json")
			w.Header().Set("Cache-Control", "no-store")

			ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
			defer cancel()

			if health(ctx) != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"fail"}`))

				return
			}

			_, _ = w.Write([]byte(`{"status":"pass"}`))
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")

		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !IsOperatorIP(ap.Addr(), cidrs) {
			http.Error(w, "forbidden", http.StatusForbidden)

			return
		}

		mux.ServeHTTP(w, r)
	})
}

// Serve listens on addr and serves h until ctx ends. It returns once the
// listener is bound (or failed to bind); errc receives the serve error.
func Serve(ctx context.Context, addr string, h http.Handler) (net.Addr, <-chan error, error) {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics listener %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	errc := make(chan error, 1)

	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		errc <- err
	}()

	go func() {
		<-ctx.Done()

		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_ = srv.Shutdown(sctx)
	}()

	return ln.Addr(), errc, nil
}
