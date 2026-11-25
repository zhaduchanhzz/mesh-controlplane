package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/frontdoor"
)

// Public routes. Nothing else is served: no debug, metrics, docs or OpenAPI
// on the public listener (AGENTS rule 9).
const (
	PathBootstrap = "/v1/bootstrap"
	PathHealth    = "/health"
)

// maxBody bounds the request body.
const maxBody = 4 << 10

// Fixed response bodies. The refusal is byte-identical for every reason
// (I15); its headers do not depend on the reason either.
var (
	bodyRefused     = []byte(`{"error":"not available"}` + "\n")
	bodyInvalid     = []byte(`{"error":"bad request"}` + "\n")
	bodyMoving      = []byte(`{"error":"tenant is moving; retry later"}` + "\n")
	bodyUnavailable = []byte(`{"error":"temporarily unavailable"}` + "\n")
	bodyLimited     = []byte(`{"error":"too many requests"}` + "\n")
	bodyMisdirected = []byte(`{"error":"misdirected request"}` + "\n")
)

// HandlerConfig configures the public handler.
type HandlerConfig struct {
	// Hostname is the only Host (and TLS SNI) served; others get 421.
	Hostname string
	// GlobalRate/GlobalBurst bound all requests (per second).
	GlobalRate  float64
	GlobalBurst int
	// TenantRate/TenantBurst bound the answers per tailnet, charged only
	// after the credential is verified, so a caller cannot drain another
	// tenant's budget with a wrong proof.
	TenantRate  float64
	TenantBurst int
	// IPRate/IPBurst bound requests per client address; 0 disables it (a
	// TCP forwarder in front hides client addresses).
	IPRate  float64
	IPBurst int
}

// DefaultHandlerConfig returns conservative limits for hostname.
func DefaultHandlerConfig(hostname string) HandlerConfig {
	return HandlerConfig{
		Hostname:   hostname,
		GlobalRate: 200, GlobalBurst: 400,
		TenantRate: 20, TenantBurst: 100,
	}
}

// Handler is the public HTTP handler of the bootstrap service.
type Handler struct {
	svc     *Service
	cfg     HandlerConfig
	global  *bucket
	tenants *buckets
	ips     *buckets
	now     func() time.Time
}

// NewHandler returns the public handler.
func NewHandler(svc *Service, cfg HandlerConfig) *Handler {
	if host, err := frontdoor.NormalizeHost(cfg.Hostname); err == nil {
		cfg.Hostname = host
	}

	h := &Handler{svc: svc, cfg: cfg, now: time.Now}
	h.global = newBucket(cfg.GlobalRate, cfg.GlobalBurst, h.now())
	h.tenants = newBuckets(cfg.TenantRate, cfg.TenantBurst)

	if cfg.IPRate > 0 {
		h.ips = newBuckets(cfg.IPRate, cfg.IPBurst)
	}

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, err := frontdoor.NormalizeHost(r.Host)
	if err != nil || host != h.cfg.Hostname {
		write(w, http.StatusMisdirectedRequest, bodyMisdirected, 0)

		return
	}

	if r.TLS != nil {
		sni, err := frontdoor.NormalizeHost(r.TLS.ServerName)
		if err != nil || sni != host {
			write(w, http.StatusMisdirectedRequest, bodyMisdirected, 0)

			return
		}
	}

	switch {
	case r.URL.Path == PathHealth && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	case r.URL.Path == PathBootstrap && r.Method == http.MethodPost:
		h.bootstrap(w, r)
	default:
		write(w, http.StatusNotFound, bodyRefused, 0)
	}
}

func (h *Handler) bootstrap(w http.ResponseWriter, r *http.Request) {
	now := h.now()

	if h.ips != nil && !h.ips.allow(clientIP(r), now) {
		h.log("limited_ip", "")
		write(w, http.StatusTooManyRequests, bodyLimited, 1)

		return
	}

	if !h.global.allow(now) {
		h.log("limited_global", "")
		write(w, http.StatusTooManyRequests, bodyLimited, 1)

		return
	}

	var req Request

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	err := dec.Decode(&req)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}

	if err != nil {
		h.log(OutcomeInvalid.String(), "")
		write(w, http.StatusBadRequest, bodyInvalid, 0)

		return
	}

	res, err := h.svc.Resolve(r.Context(), req)
	if err != nil {
		log.Error().Err(err).Str("key", keyTag(req.Credential.KeyID)).Msg("bootstrap: lookup failed")
	}

	if res.TailnetID != "" && !h.tenants.allow(res.TailnetID, now) {
		h.log("limited_tenant", req.Credential.KeyID)
		write(w, http.StatusTooManyRequests, bodyLimited, 1)

		return
	}

	h.log(res.Outcome.String(), req.Credential.KeyID)

	switch res.Outcome {
	case OutcomeOK:
		body, _ := json.Marshal(struct {
			ControlURL string    `json:"control_url"`
			IssuedAt   time.Time `json:"issued_at"`
		}{res.ControlURL, now.UTC().Truncate(time.Second)})
		write(w, http.StatusOK, append(body, '\n'), 0)
	case OutcomeInvalid:
		write(w, http.StatusBadRequest, bodyInvalid, 0)
	case OutcomeMoving:
		write(w, http.StatusServiceUnavailable, bodyMoving, 30)
	case OutcomeUnavailable:
		write(w, http.StatusServiceUnavailable, bodyUnavailable, 10)
	default:
		write(w, http.StatusNotFound, bodyRefused, 0)
	}
}

// log records the outcome with a short hash of the key identifier, never
// the identifier itself, the proof or the tenant.
func (h *Handler) log(outcome, keyID string) {
	ev := log.Info().Str("outcome", outcome)
	if keyID != "" {
		ev = ev.Str("key", keyTag(keyID))
	}

	ev.Msg("bootstrap")
}

func keyTag(keyID string) string {
	sum := sha256.Sum256([]byte("bootstrap/" + keyID))

	return hex.EncodeToString(sum[:4])
}

func write(w http.ResponseWriter, status int, body []byte, retryAfter int) {
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Length", strconv.Itoa(len(body)))

	if retryAfter > 0 {
		hd.Set("Retry-After", strconv.Itoa(retryAfter))
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// bucket is a token bucket.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(rate float64, burst int, now time.Time) *bucket {
	return &bucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now}
}

func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.rate <= 0 {
		return true
	}

	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}

	b.last = now

	if b.tokens < 1 {
		return false
	}

	b.tokens--

	return true
}

// buckets is a set of buckets by key, bounded: past maxKeys it starts over
// (losing at most one burst of history), so a key flood cannot grow memory.
type buckets struct {
	mu    sync.Mutex
	rate  float64
	burst int
	m     map[string]*bucket
}

const maxKeys = 100_000

func newBuckets(rate float64, burst int) *buckets {
	return &buckets{rate: rate, burst: burst, m: make(map[string]*bucket)}
}

func (s *buckets) allow(key string, now time.Time) bool {
	s.mu.Lock()

	b, ok := s.m[key]
	if !ok {
		if len(s.m) >= maxKeys {
			s.m = make(map[string]*bucket)
		}

		b = newBucket(s.rate, s.burst, now)
		s.m[key] = b
	}

	s.mu.Unlock()

	return b.allow(now)
}
