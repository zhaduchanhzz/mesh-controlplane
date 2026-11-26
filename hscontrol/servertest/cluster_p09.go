//go:build p05cluster

package servertest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// P09 (ADR 0009): the cluster's /verify, called by an external DERP server.

// VerifyResult is one /verify answer: the HTTP status, the raw body and,
// on 200, the decoded Allow.
type VerifyResult struct {
	Status int
	Body   string
	Allow  bool
}

// Verify posts a DERP admit request for nk to the cluster URL (through the
// front door when there is one) with secret as ?secret= (none when empty).
func (tc *TestCluster) Verify(tb testing.TB, secret string, nk key.NodePublic) VerifyResult {
	tb.Helper()

	return tc.verifyAt(tb, tc.URL, secret, nk)
}

// ProcessVerify is Verify straight to process p, bypassing the front door.
func (tc *TestCluster) ProcessVerify(tb testing.TB, p *ClusterProcess, secret string, nk key.NodePublic) VerifyResult {
	tb.Helper()

	return tc.verifyAt(tb, "http://"+p.Addr, secret, nk)
}

func (tc *TestCluster) verifyAt(tb testing.TB, base, secret string, nk key.NodePublic) VerifyResult {
	tb.Helper()

	body, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: nk})
	if err != nil {
		tb.Fatal(err)
	}

	u := base + "/verify"
	if secret != "" {
		u += "?secret=" + url.QueryEscape(secret)
	}

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body)) //nolint:noctx // bounded by client timeout
	if err != nil {
		tb.Fatal(err)
	}

	req.Host = tc.host
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		tb.Fatalf("servertest: POST /verify: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	out := VerifyResult{Status: resp.StatusCode, Body: string(raw)}

	if resp.StatusCode == http.StatusOK {
		var r tailcfg.DERPAdmitClientResponse
		if json.Unmarshal(raw, &r) == nil {
			out.Allow = r.Allow
		}
	}

	return out
}
