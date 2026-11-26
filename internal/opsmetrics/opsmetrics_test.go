package opsmetrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

var errDown = errors.New("database down")

// P13f, AGENTS rule 9: the metrics listener of mesh-bootstrap and
// mesh-placement answers operators only (loopback or an explicit CIDR,
// never the Tailscale range), serves /metrics and /health only, and reads
// no forwarding header.
func TestOperatorsOnly(t *testing.T) {
	cidrs, err := ParseOperatorCIDRs("192.0.2.0/28, 2001:db8::/64")
	if err != nil {
		t.Fatal(err)
	}

	var down bool

	h := Handler(cidrs, func(context.Context) error {
		if down {
			return errDown
		}

		return nil
	})

	tests := []struct {
		peer, path string
		hdr        map[string]string
		want       int
	}{
		{"127.0.0.1:5000", "/metrics", nil, http.StatusOK},
		{"[::1]:5000", "/metrics", nil, http.StatusOK},
		{"192.0.2.5:5000", "/metrics", nil, http.StatusOK},
		{"[2001:db8::5]:5000", "/health", nil, http.StatusOK},
		{"[::ffff:192.0.2.5]:5000", "/metrics", nil, http.StatusOK},
		{"192.0.2.99:5000", "/metrics", nil, http.StatusForbidden},
		{"192.168.1.20:5000", "/metrics", nil, http.StatusForbidden},
		{"100.64.0.7:5000", "/metrics", nil, http.StatusForbidden},
		{"[fd7a:115c:a1e0::7]:5000", "/health", nil, http.StatusForbidden},
		{"203.0.113.9:5000", "/metrics", map[string]string{"X-Forwarded-For": "127.0.0.1"}, http.StatusForbidden},
		{"127.0.0.1:5000", "/debug/pprof/", nil, http.StatusNotFound},
		{"127.0.0.1:5000", "/v1/bootstrap", nil, http.StatusNotFound},
		{"garbage", "/metrics", nil, http.StatusForbidden},
	}

	for _, tt := range tests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
		req.RemoteAddr = tt.peer

		for k, v := range tt.hdr {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != tt.want {
			t.Errorf("%s %s: %d, want %d", tt.peer, tt.path, rec.Code, tt.want)
		}

		if tt.want != http.StatusOK && strings.Contains(rec.Body.String(), "# HELP") {
			t.Errorf("%s %s: metrics leaked in a refusal", tt.peer, tt.path)
		}
	}

	down = true
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "fail") {
		t.Errorf("/health with the database down: %d %q", rec.Code, rec.Body.String())
	}
}

func TestParseOperatorCIDRsRefusesTailscaleAndZero(t *testing.T) {
	for _, bad := range []string{"0.0.0.0/0", "::/0", "100.64.0.0/10", "100.100.0.0/16", "100.0.0.0/8", "fd7a:115c:a1e0::/64", "fd00::/8", "not-a-cidr"} {
		_, err := ParseOperatorCIDRs(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	got, err := ParseOperatorCIDRs("")
	if err != nil || len(got) != 0 {
		t.Errorf("empty list: %v %v", got, err)
	}

	got, err = ParseOperatorCIDRs("192.168.1.243/32")
	if err != nil || len(got) != 1 || got[0] != netip.MustParsePrefix("192.168.1.243/32") {
		t.Errorf("single CIDR: %v %v", got, err)
	}
}

func TestServeBindsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	addr, errc, err := Serve(ctx, "127.0.0.1:0", Handler(nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr.String()+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback scrape: %d", resp.StatusCode)
	}

	cancel()

	if err := <-errc; err != nil { //nolint:noinlineerr
		t.Fatalf("serve: %v", err)
	}
}
