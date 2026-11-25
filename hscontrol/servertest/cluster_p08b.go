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
