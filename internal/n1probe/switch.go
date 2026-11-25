package n1probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// SwitchConfig configures SwitchTenant. KeyA and KeyB are pre-auth keys of
// two tailnets served by the cluster at URL (reusable: each registers two or
// three nodes). They are only sent to the cluster, never reported.
type SwitchConfig struct {
	URL  string
	KeyA string
	KeyB string
	// CloseWait bounds the wait for the server to close a session after a
	// refused tenant switch (default 5s).
	CloseWait time.Duration
	Logf      logger.Logf
}

// CaseResult is one S11 case. Leak is set when the server answered a
// request it must refuse with 2xx, or any answer carried an identifier of
// the other tenant's nodes; Pass also needs the expected refusal and, for a
// tenant switch, the closed session.
type CaseResult struct {
	Case          string `json:"case"`
	Desc          string `json:"desc"`
	Status        int    `json:"status"`
	Error         string `json:"error,omitempty"`
	Refused       bool   `json:"refused"`
	SessionClosed bool   `json:"session_closed"`
	Dials         int    `json:"dials"`
	Leak          bool   `json:"leak"`
	LeakDetail    string `json:"leak_detail,omitempty"`
	Pass          bool   `json:"pass"`
	Note          string `json:"note,omitempty"`
}

// ErrSetup reports that the probe could not set up its nodes (a key was
// refused, the cluster unreachable): no case ran.
var ErrSetup = errors.New("n1probe: setup")

type probeNode struct {
	nk    key.NodePublic
	disco key.DiscoPublic
	host  string
}

func randSuffix() string {
	var b [3]byte
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// SwitchTenant runs the S11 (a), (b) and (d) cases against the cluster:
//
//   - one machine key ("device") gets a node in tailnet A and one in B, as
//     S05 allows; another machine gets a second node in B;
//   - each case opens fresh Noise sessions and tries to use one session for
//     both tenants, or to map a node key that does not belong to the
//     session's machine, or to switch tenant with headers or a register.
//
// It returns one CaseResult per case; err is ErrSetup when no case ran.
func SwitchTenant(ctx context.Context, cfg SwitchConfig) ([]CaseResult, error) {
	if cfg.CloseWait <= 0 {
		cfg.CloseWait = 5 * time.Second
	}

	serverKey, err := FetchNoiseKey(ctx, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSetup, err)
	}

	sfx := randSuffix()
	device, other := key.NewMachine(), key.NewMachine()

	register := func(mk key.MachinePrivate, authKey, host string) (probeNode, error) {
		s, err := NewSession(cfg.URL, serverKey, mk, cfg.Logf)
		if err != nil {
			return probeNode{}, err
		}
		defer s.Close()

		n := probeNode{nk: key.NewNode().Public(), disco: key.NewDisco().Public(), host: host}

		resp, rr, err := s.Register(ctx, &tailcfg.RegisterRequest{
			NodeKey:  n.nk,
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: authKey},
			Hostinfo: &tailcfg.Hostinfo{Hostname: host},
		}, nil)
		if err != nil {
			return probeNode{}, err
		}

		if !resp.OK() || !rr.MachineAuthorized {
			return probeNode{}, fmt.Errorf("register %s: status %d, authorized %v", host, resp.Status, rr.MachineAuthorized)
		}

		return n, nil
	}

	a, err := register(device, cfg.KeyA, "n1probe-a-"+sfx)
	if err != nil {
		return nil, fmt.Errorf("%w: node in A: %w", ErrSetup, err)
	}

	b, err := register(device, cfg.KeyB, "n1probe-b-"+sfx)
	if err != nil {
		return nil, fmt.Errorf("%w: node in B: %w", ErrSetup, err)
	}

	z, err := register(other, cfg.KeyB, "n1probe-z-"+sfx)
	if err != nil {
		return nil, fmt.Errorf("%w: second node in B: %w", ErrSetup, err)
	}

	tokensB := []string{b.nk.String(), z.nk.String(), b.host, z.host}
	tokensA := []string{a.nk.String(), a.host}

	leakIn := func(body []byte, tokens []string) string {
		s := string(body)
		for _, t := range tokens {
			if strings.Contains(s, t) {
				return "answer carries " + short(t)
			}
		}

		return ""
	}

	newSess := func(mk key.MachinePrivate) (*Session, error) {
		return NewSession(cfg.URL, serverKey, mk, cfg.Logf)
	}

	var out []CaseResult

	// refusedSwitch completes r for a request that must be refused on a
	// session bound to the other tenant, and the session closed.
	refusedSwitch := func(r *CaseResult, s *Session, resp *Response, err error, foreign []string, needClose bool) {
		if err != nil {
			r.Error = err.Error()
			r.Refused = true
		} else {
			r.Status = resp.Status
			r.Refused = !resp.OK()

			if resp.OK() {
				r.Leak = true
				r.LeakDetail = "request for the other tenant answered " + fmt.Sprint(resp.Status)
			}

			if d := leakIn(resp.Body, foreign); d != "" {
				r.Leak, r.LeakDetail = true, d
			}
		}

		r.SessionClosed = s.WaitPeerClosed(cfg.CloseWait)
		r.Dials = s.Dials()
		r.Pass = !r.Leak && r.Refused && (!needClose || r.SessionClosed) && r.Dials <= 1
	}

	control := func(r *CaseResult, resp *Response, err error, what string) bool {
		if err != nil || !resp.OK() {
			r.Note = "positive control failed: " + what
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Status = resp.Status
			}

			return false
		}

		return true
	}

	run := func(id, desc string, fn func(r *CaseResult)) {
		r := CaseResult{Case: id, Desc: desc}
		fn(&r)
		out = append(out, r)
	}

	run("S11a-map-A-then-map-B", "map A's node, then on the same session map B's node of the same machine", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		resp, err := s.LiteMap(ctx, a.nk, a.disco)
		if !control(r, resp, err, "A's own node maps") {
			return
		}

		resp, err = s.FullMap(ctx, b.nk, b.disco, nil)
		refusedSwitch(r, s, resp, err, tokensB, true)
	})

	run("S11a-map-B-then-map-A", "map B's node, then on the same session map A's node of the same machine", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		resp, err := s.LiteMap(ctx, b.nk, b.disco)
		if !control(r, resp, err, "B's own node maps") {
			return
		}

		resp, err = s.FullMap(ctx, a.nk, a.disco, nil)
		refusedSwitch(r, s, resp, err, tokensA, true)
	})

	run("S11b-unknown-node-key", "map a node key that matches no node", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		resp, err := s.FullMap(ctx, key.NewNode().Public(), key.NewDisco().Public(), nil)
		refusedSwitch(r, s, resp, err, append(append([]string{}, tokensA...), tokensB...), false)
	})

	run("S11b-other-machine-node-key", "map B's node of another machine under this machine key", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		resp, err := s.FullMap(ctx, z.nk, z.disco, nil)
		refusedSwitch(r, s, resp, err, tokensB, false)
	})

	run("S11d-map-A-then-register-B-key", "map A's node, then register on the same session with B's key", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		resp, err := s.LiteMap(ctx, a.nk, a.disco)
		if !control(r, resp, err, "A's own node maps") {
			return
		}

		resp, rr, err := s.Register(ctx, &tailcfg.RegisterRequest{
			NodeKey:  key.NewNode().Public(),
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: cfg.KeyB},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "n1probe-rb-" + sfx},
		}, nil)
		if err == nil && resp.OK() && rr != nil && rr.MachineAuthorized {
			r.Status = resp.Status
			r.Leak, r.LeakDetail = true, "B's key registered on a session bound to A"
			r.SessionClosed = s.WaitPeerClosed(cfg.CloseWait)
			r.Dials = s.Dials()

			return
		}

		refusedSwitch(r, s, resp, err, tokensB, true)
	})

	run("S11d-tenant-headers", "map A's node with headers naming another tenant: A's own answer, nothing of B", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		hdr := http.Header{}
		for _, h := range []string{"X-Tailnet", "X-Tailnet-Id", "X-Headscale-Tailnet", "X-Tenant", "X-Forwarded-Host"} {
			hdr.Set(h, b.host)
		}

		resp, err := s.FullMap(ctx, a.nk, a.disco, hdr)
		if !control(r, resp, err, "A's own full map") {
			return
		}

		r.Status = resp.Status
		r.Dials = s.Dials()

		if d := leakIn(resp.Body, tokensB); d != "" {
			r.Leak, r.LeakDetail = true, d
		}

		hasSelf := strings.Contains(string(resp.Body), a.nk.String())
		if !hasSelf {
			r.Note = "A's map does not name A's node (not decoded: compressed or empty?)"
		}

		r.Pass = !r.Leak && hasSelf
	})

	run("S11a-register-A-then-map-B", "register this machine in A with A's key (a new node key), then map B's node of the same machine on the same session; last: it re-keys A's node", func(r *CaseResult) {
		s, err := newSess(device)
		if err != nil {
			r.Error = err.Error()

			return
		}
		defer s.Close()

		resp, rr, err := s.Register(ctx, &tailcfg.RegisterRequest{
			NodeKey:  key.NewNode().Public(),
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: cfg.KeyA},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "n1probe-ra-" + sfx},
		}, nil)
		if !control(r, resp, err, "A's key registers") || !rr.MachineAuthorized {
			if r.Note == "" {
				r.Note = "positive control failed: A's key not authorized"
			}

			return
		}

		resp, err = s.FullMap(ctx, b.nk, b.disco, nil)
		refusedSwitch(r, s, resp, err, tokensB, true)
	})

	return out, nil
}
