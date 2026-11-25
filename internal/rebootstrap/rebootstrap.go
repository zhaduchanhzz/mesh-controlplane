// Package rebootstrap re-points the clients of a moved tailnet at its new
// owner cluster (P12b, owner decision Q5): it asks bootstrap for the control
// URL with the tailnet's pre-auth key (key id and SHA-256 proof only, never
// the secret), then runs a per-target command, e.g. `tailscale up
// --login-server <URL> --force-reauth --auth-key=file:...`, with bounded
// concurrency, and records how long each target took.
//
// The tool never puts the pre-auth key on a command line or in its output:
// commands get the control URL and the target name; the key reaches the
// client by a path the operator controls (a key file on the target, read by
// `--auth-key=file:`). Command output is kept only as a redacted tail.
package rebootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors of Resolve.
var (
	// ErrRefused: bootstrap does not give a URL for this key (unknown,
	// revoked, expired, used, wrong tenant: one answer for all).
	ErrRefused = errors.New("bootstrap refused the key")
	// ErrStillMoving: the tailnet was still being moved when the wait ended.
	ErrStillMoving = errors.New("tailnet still moving")
	// ErrBadKey: the key is not a pre-auth key (hskey-auth-<id>-<secret>).
	ErrBadKey = errors.New("not a pre-auth key")
	// ErrRateLimited: bootstrap kept answering 429 until the wait ended.
	ErrRateLimited = errors.New("bootstrap rate limited")
	// ErrUnexpected: bootstrap answered something this tool does not use.
	ErrUnexpected = errors.New("unexpected bootstrap answer")
)

var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Credential is the bootstrap credential of a pre-auth key: its identifier
// and the SHA-256 of its secret.
type Credential struct {
	KeyID string
	Proof string
}

// CredentialOf derives the bootstrap credential from a pre-auth key
// "hskey-auth-<12 hex id>-<secret>". The secret itself is not kept.
func CredentialOf(authKey string) (Credential, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(authKey), "hskey-auth-")
	if !ok {
		return Credential{}, ErrBadKey
	}

	id, secret, ok := strings.Cut(rest, "-")
	if !ok || !keyIDPattern.MatchString(id) || secret == "" {
		return Credential{}, ErrBadKey
	}

	sum := sha256.Sum256([]byte(secret))

	return Credential{KeyID: id, Proof: hex.EncodeToString(sum[:])}, nil
}

// Resolver asks bootstrap for a tailnet's control URL.
type Resolver struct {
	// BaseURL of the bootstrap service, e.g. https://boot.example:8444.
	BaseURL string
	// Tenant, optional: the slug the key must belong to.
	Tenant string
	Client *http.Client
	// MaxWait bounds the time spent waiting while the tailnet is MOVING
	// (503 + Retry-After) or bootstrap is briefly unavailable.
	MaxWait time.Duration
	// NotURL, when set, is the control URL before the move (the source
	// cluster): an answer equal to it means the move has not happened yet,
	// and Resolve keeps polling.
	NotURL string
	// Poll, when set, caps every wait (the server's Retry-After of 30 s is
	// meant for clients; the operator polls faster, within the per-tenant
	// rate limit).
	Poll time.Duration
	// Sleep is replaceable in tests.
	Sleep func(context.Context, time.Duration) error
	// Logf receives progress lines (status codes and waits, never the key).
	Logf func(format string, args ...any)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Resolve returns the control URL bootstrap gives for cred, waiting while
// the tailnet is moving (Retry-After, at most MaxWait in all).
func (r *Resolver) Resolve(ctx context.Context, cred Credential) (string, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	logf := r.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	body := map[string]any{"credential": map[string]string{
		"kind": "authkey_proof", "key_id": cred.KeyID, "proof": cred.Proof,
	}}
	if r.Tenant != "" {
		body["tenant"] = r.Tenant
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	endpoint := strings.TrimRight(r.BaseURL, "/") + "/v1/bootstrap"
	waited := time.Duration(0)

	for {
		code, ctrl, retry, err := r.post(ctx, client, endpoint, payload)
		if err != nil {
			return "", err
		}

		switch {
		case code == http.StatusOK && (r.NotURL == "" || strings.TrimRight(ctrl, "/") != strings.TrimRight(r.NotURL, "/")):
			return ctrl, nil
		case code == http.StatusOK:
			wait := r.Poll
			if wait <= 0 {
				wait = time.Second
			}

			if waited+wait > r.MaxWait {
				return "", fmt.Errorf("%w: still the old control URL after %s", ErrStillMoving, waited)
			}

			if err := sleep(ctx, wait); err != nil { //nolint:noinlineerr
				return "", err
			}

			waited += wait
		case code == http.StatusServiceUnavailable || code == http.StatusTooManyRequests:
			if retry <= 0 {
				retry = 5 * time.Second
			}

			if r.Poll > 0 && retry > r.Poll {
				retry = r.Poll
			}

			if waited+retry > r.MaxWait {
				if code == http.StatusServiceUnavailable {
					return "", fmt.Errorf("%w after %s (last status %d)", ErrStillMoving, waited, code)
				}

				return "", fmt.Errorf("%w after %s", ErrRateLimited, waited)
			}

			logf("bootstrap: status %d, retrying in %s", code, retry)

			if err := sleep(ctx, retry); err != nil { //nolint:noinlineerr
				return "", err
			}

			waited += retry
		case code == http.StatusNotFound:
			return "", ErrRefused
		default:
			return "", fmt.Errorf("%w: status %d", ErrUnexpected, code)
		}
	}
}

func (r *Resolver) post(ctx context.Context, client *http.Client, endpoint string, payload []byte) (int, string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, "", 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", 0, fmt.Errorf("bootstrap: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))

	var retry time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 { //nolint:noinlineerr
		retry = time.Duration(s) * time.Second
	}

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, "", retry, nil
	}

	var out struct {
		ControlURL string `json:"control_url"`
	}

	if err := json.Unmarshal(raw, &out); err != nil { //nolint:noinlineerr
		return 0, "", 0, fmt.Errorf("bootstrap: bad answer: %w", err)
	}

	u, err := url.Parse(out.ControlURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return 0, "", 0, fmt.Errorf("%w: no https control URL", ErrUnexpected)
	}

	return http.StatusOK, out.ControlURL, 0, nil
}

// Runner runs the command of one target with the control URL; it returns
// the command's combined output and its error.
type Runner func(ctx context.Context, target, loginServer string) ([]byte, error)

// Result is the outcome of one target.
type Result struct {
	Target string `json:"target"`
	// StartMS and EndMS are milliseconds since the start of the run.
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	// OutputTail is the redacted end of the command's output.
	OutputTail string `json:"output_tail,omitempty"`
}

// Summary is the measured run (owner decision Q7: t50/t90/t100).
type Summary struct {
	LoginServer string `json:"login_server"`
	// StartedAt is when the first command started (the targets' times are
	// relative to it).
	StartedAt   time.Time `json:"started_at"`
	Targets     int       `json:"targets"`
	Concurrency int       `json:"concurrency"`
	OK          int       `json:"ok"`
	Failed      int       `json:"failed"`
	// T50MS, T90MS, T100MS: time from the start of the run until 50 / 90 /
	// 100 % of all targets had re-logged; -1 when that share never did.
	T50MS   int64    `json:"t50_ms"`
	T90MS   int64    `json:"t90_ms"`
	T100MS  int64    `json:"t100_ms"`
	Results []Result `json:"results"`
}

// maxTail bounds the output kept per target.
const maxTail = 1024

var secretPattern = regexp.MustCompile(`(hskey-[a-z]+-|tskey-[a-z]+-)[A-Za-z0-9_-]+`)

// Redact removes key material from s.
func Redact(s string) string {
	return secretPattern.ReplaceAllString(s, "${1}[REDACTED]")
}

func tail(b []byte) string {
	if len(b) > maxTail {
		b = b[len(b)-maxTail:]
	}

	return Redact(string(b))
}

// Run runs run for every target, at most concurrency at a time, each
// bounded by perTarget, and measures when each one finished.
func Run(ctx context.Context, loginServer string, targets []string, concurrency int, perTarget time.Duration, run Runner) Summary {
	if concurrency < 1 {
		concurrency = 1
	}

	start := time.Now()
	results := make([]Result, len(targets))
	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup

	for i, target := range targets {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			res := Result{Target: target, StartMS: time.Since(start).Milliseconds()}

			tctx, cancel := context.WithTimeout(ctx, perTarget)
			out, err := run(tctx, target, loginServer)

			cancel()

			res.EndMS = time.Since(start).Milliseconds()
			res.OK = err == nil
			res.OutputTail = tail(out)

			if err != nil {
				res.Error = Redact(err.Error())
			}

			results[i] = res
		})
	}

	wg.Wait()

	sum := Summarize(loginServer, concurrency, results)
	sum.StartedAt = start.UTC()

	return sum
}

// Summarize computes the counts and the t50/t90/t100 of results: the
// smallest time by which that share of all targets (failed ones count as
// never) had finished successfully.
func Summarize(loginServer string, concurrency int, results []Result) Summary {
	s := Summary{LoginServer: loginServer, Targets: len(results), Concurrency: concurrency, Results: results}

	var done []int64

	for _, r := range results {
		if r.OK {
			s.OK++

			done = append(done, r.EndMS)
		} else {
			s.Failed++
		}
	}

	slices.Sort(done)

	at := func(pct int) int64 {
		if len(results) == 0 {
			return -1
		}

		need := max((len(results)*pct+99)/100, 1)

		if need > len(done) {
			return -1
		}

		return done[need-1]
	}

	s.T50MS, s.T90MS, s.T100MS = at(50), at(90), at(100)

	return s
}
