//go:build p05cluster

package servertest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
)

// Admin calls the live admin API (P08b) on the unix socket of process p
// (nil: the first process) and returns the status and the body.
func (tc *TestCluster) Admin(tb testing.TB, p *ClusterProcess, method, path string, body any) (int, []byte) {
	tb.Helper()

	if p == nil {
		p = tc.procs[0]
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return new(net.Dialer).DialContext(ctx, "unix", p.AdminSocket)
			},
		},
	}
	defer client.CloseIdleConnections()

	var rd io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			tb.Fatalf("servertest: admin body: %v", err)
		}

		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(tb.Context(), method, "http://admin"+path, rd)
	if err != nil {
		tb.Fatalf("servertest: admin request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		tb.Fatalf("servertest: admin %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, out
}

// MintAPIKey mints an API key for tailnet slug through the platform
// registry, as the operator CLI and API do (P08b).
func (tc *TestCluster) MintAPIKey(tb testing.TB, slug string) string {
	tb.Helper()

	return tc.MintUserAPIKey(tb, slug, "")
}

// MintUserAPIKey is [TestCluster.MintAPIKey] for a key acting as user of the
// tailnet.
func (tc *TestCluster) MintUserAPIKey(tb testing.TB, slug, user string) string {
	tb.Helper()

	reg, err := placement.New(tc.Platform(tb).RegistryDB(), placement.Config{})
	if err != nil {
		tb.Fatalf("servertest: registry: %v", err)
	}

	_, key, err := reg.CreateAPIKey(tb.Context(), placement.Op{Actor: "servertest"}, tc.Tailnet(tb, slug).ID.String(), time.Hour, user)
	if err != nil {
		tb.Fatalf("servertest: CreateAPIKey: %v", err)
	}

	return key
}
