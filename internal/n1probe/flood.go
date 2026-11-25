package n1probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/bootstrap"
)

// Flood kinds (S13, owner decision Q4: tenant A's load must not stop B).
const (
	KindEnroll    = "enroll"    // a new machine key registering with the key, per op
	KindBootstrap = "bootstrap" // POST /v1/bootstrap with the key's proof, per op
	KindMapStream = "mapstream" // register a new machine, hold a map stream until the end
)

// FloodConfig configures Flood. Key is tenant A's pre-auth key (reusable);
// it is sent only to the cluster or bootstrap, never reported.
type FloodConfig struct {
	Kind         string
	URL          string // cluster URL (enroll, mapstream)
	BootstrapURL string // bootstrap URL (bootstrap)
	Tenant       string // optional tenant slug sent to bootstrap
	Key          string
	Rate         float64       // operations started per second
	Duration     time.Duration // stop starting operations after this
	MaxInflight  int           // concurrent operations (streams for mapstream); default 64
	Logf         logger.Logf
}

// FloodResult counts the operations of one run. Latency is the time to the
// answer (mapstream: to the first frame; status_2xx counts streams that
// delivered a frame); percentiles are over at most
// maxSamples answers.
type FloodResult struct {
	Kind       string  `json:"kind"`
	Started    int     `json:"started"`
	Skipped    int     `json:"skipped_inflight_cap"`
	S2xx       int     `json:"status_2xx"`
	S4xx       int     `json:"status_4xx"`
	S429       int     `json:"status_429"`
	S5xx       int     `json:"status_5xx"`
	Errors     int     `json:"errors"`
	P50ms      float64 `json:"p50_ms"`
	P90ms      float64 `json:"p90_ms"`
	P99ms      float64 `json:"p99_ms"`
	MaxMs      float64 `json:"max_ms"`
	ElapsedSec float64 `json:"elapsed_s"`
}

const maxSamples = 100_000

// op runs one operation: the HTTP status (0 on a transport error) and the
// latency to the answer.
type op func(ctx context.Context) (status int, latency time.Duration, err error)

// errUnknownKind reports a flood kind Flood does not know.
var errUnknownKind = errors.New("n1probe: unknown flood kind")

// Flood runs cfg.Kind at cfg.Rate for cfg.Duration with at most
// cfg.MaxInflight operations in flight, then waits for the operations in
// flight (map streams end at the deadline) and returns the counts.
func Flood(ctx context.Context, cfg FloodConfig) (FloodResult, error) {
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 64
	}

	if cfg.Rate <= 0 || cfg.Duration <= 0 {
		return FloodResult{}, fmt.Errorf("n1probe: rate and duration must be positive")
	}

	var (
		fn  op
		err error
	)

	end := time.Now().Add(cfg.Duration)

	switch cfg.Kind {
	case KindBootstrap:
		fn, err = bootstrapOp(cfg)
	case KindEnroll:
		fn, err = enrollOp(ctx, cfg)
	case KindMapStream:
		fn, err = mapStreamOp(ctx, cfg, end)
	default:
		err = fmt.Errorf("%w %q", errUnknownKind, cfg.Kind)
	}

	if err != nil {
		return FloodResult{}, err
	}

	res := run(ctx, fn, cfg.Rate, end, cfg.MaxInflight)
	res.Kind = cfg.Kind

	return res, nil
}

// run starts fn at rate until end, never more than maxInflight at once
// (an op that would exceed it is skipped and counted), then waits for the
// ops in flight.
func run(ctx context.Context, fn op, rate float64, end time.Time, maxInflight int) FloodResult {
	var (
		mu      sync.Mutex
		res     FloodResult
		samples []float64
		wg      sync.WaitGroup
	)

	start := time.Now()
	slots := make(chan struct{}, maxInflight)
	interval := time.Duration(float64(time.Second) / rate)

	if interval <= 0 {
		interval = time.Microsecond
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()

	runCtx, cancel := context.WithDeadline(ctx, end.Add(30*time.Second))
	defer cancel()

loop:
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
		}

		select {
		case slots <- struct{}{}:
		default:
			mu.Lock()
			res.Skipped++
			mu.Unlock()

			continue
		}

		mu.Lock()
		res.Started++
		mu.Unlock()

		wg.Go(func() {
			defer func() { <-slots }()

			status, lat, err := fn(runCtx)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err != nil && status == 0:
				res.Errors++
			case status == http.StatusTooManyRequests:
				res.S429++
			case status >= 200 && status < 300:
				res.S2xx++
			case status >= 400 && status < 500:
				res.S4xx++
			case status >= 500:
				res.S5xx++
			default:
				res.Errors++
			}

			if status != 0 && len(samples) < maxSamples {
				samples = append(samples, float64(lat.Microseconds())/1000)
			}
		})
	}

	wg.Wait()

	res.ElapsedSec = time.Since(start).Seconds()

	slices.Sort(samples)

	pct := func(p float64) float64 {
		if len(samples) == 0 {
			return 0
		}

		i := int(p * float64(len(samples)-1))

		return samples[i]
	}

	res.P50ms, res.P90ms, res.P99ms = pct(0.50), pct(0.90), pct(0.99)
	if len(samples) > 0 {
		res.MaxMs = samples[len(samples)-1]
	}

	return res
}

func bootstrapOp(cfg FloodConfig) (op, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(cfg.Key), "hskey-auth-")
	if !ok {
		return nil, fmt.Errorf("n1probe: bootstrap flood needs a pre-auth key (hskey-auth-…)")
	}

	id, secret, ok := strings.Cut(rest, "-")
	if !ok || cfg.BootstrapURL == "" {
		return nil, fmt.Errorf("n1probe: bootstrap flood needs a pre-auth key and --bootstrap-url")
	}

	req := map[string]any{"credential": map[string]string{
		"kind": bootstrap.CredentialKindAuthKeyProof, "key_id": id, "proof": bootstrap.ProofOf(secret),
	}}
	if cfg.Tenant != "" {
		req["tenant"] = cfg.Tenant
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	u := strings.TrimRight(cfg.BootstrapURL, "/") + "/v1/bootstrap"

	return func(ctx context.Context) (int, time.Duration, error) {
		t0 := time.Now()

		r, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return 0, 0, err
		}

		r.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(r)
		if err != nil {
			return 0, time.Since(t0), err
		}

		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()

		return resp.StatusCode, time.Since(t0), nil
	}, nil
}

func clusterKey(ctx context.Context, cfg FloodConfig) (key.MachinePublic, error) {
	if cfg.URL == "" || cfg.Key == "" {
		return key.MachinePublic{}, fmt.Errorf("n1probe: %s flood needs --url and a key", cfg.Kind)
	}

	return FetchNoiseKey(ctx, cfg.URL)
}

// enrollOp registers a new machine (new Noise session) with the key per op.
func enrollOp(ctx context.Context, cfg FloodConfig) (op, error) {
	serverKey, err := clusterKey(ctx, cfg)
	if err != nil {
		return nil, err
	}

	authKey := strings.TrimSpace(cfg.Key)

	return func(ctx context.Context) (int, time.Duration, error) {
		t0 := time.Now()

		s, err := NewSession(cfg.URL, serverKey, key.NewMachine(), cfg.Logf)
		if err != nil {
			return 0, 0, err
		}
		defer s.Close()

		resp, _, err := s.Register(ctx, &tailcfg.RegisterRequest{
			NodeKey:  key.NewNode().Public(),
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: authKey},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "n1flood-" + randSuffix()},
		}, nil)
		if err != nil {
			if resp != nil {
				return resp.Status, time.Since(t0), err
			}

			return 0, time.Since(t0), err
		}

		return resp.Status, time.Since(t0), nil
	}, nil
}

// mapStreamOp registers a new machine and holds a map stream open until
// end: the status is the stream's, the latency the time to the first frame.
func mapStreamOp(ctx context.Context, cfg FloodConfig, end time.Time) (op, error) {
	serverKey, err := clusterKey(ctx, cfg)
	if err != nil {
		return nil, err
	}

	authKey := strings.TrimSpace(cfg.Key)

	return func(ctx context.Context) (int, time.Duration, error) {
		s, err := NewSession(cfg.URL, serverKey, key.NewMachine(), cfg.Logf)
		if err != nil {
			return 0, 0, err
		}
		defer s.Close()

		nk, disco := key.NewNode().Public(), key.NewDisco().Public()

		resp, rr, err := s.Register(ctx, &tailcfg.RegisterRequest{
			NodeKey:  nk,
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: authKey},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "n1stream-" + randSuffix()},
		}, nil)
		if err != nil {
			if resp != nil {
				return resp.Status, 0, err
			}

			return 0, 0, err
		}

		if !resp.OK() || !rr.MachineAuthorized {
			return resp.Status, 0, nil
		}

		sctx, cancel := context.WithDeadline(ctx, end)
		defer cancel()

		t0 := time.Now()

		stream, err := s.StreamMap(sctx, nk, disco, nil)
		if err != nil {
			return 0, time.Since(t0), err
		}
		defer stream.Body.Close()

		if stream.StatusCode != http.StatusOK {
			return stream.StatusCode, time.Since(t0), nil
		}

		// The first frame (4-byte length + body) proves the stream serves;
		// then hold it, discarding frames, until the deadline.
		var hdr [4]byte

		_, err = io.ReadFull(stream.Body, hdr[:])
		lat := time.Since(t0)

		if err != nil {
			return stream.StatusCode, lat, fmt.Errorf("first frame: %w", err)
		}

		_, _ = io.Copy(io.Discard, stream.Body)

		return http.StatusOK, lat, nil
	}, nil
}
