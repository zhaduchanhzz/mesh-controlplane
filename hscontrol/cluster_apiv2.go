package hscontrol

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	apiv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/ratelimit"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tenantlabel"
)

// The public /api/v2 of a cluster (P08b, owner decision Q3). The API key of
// a request is the only thing that picks its tailnet: the platform looks
// the key up (credentials.tailnet_id), the request goes to the runtime
// serving that tailnet, and that runtime's v2 API authenticates the key
// again through its own scoped handle. {tailnet} accepts only "-" or the
// key's tailnet ID; a device of another tailnet is not in the runtime and is
// 404. The tailnet settings and the device trust settings are refused (see
// apiv2.Backend.Tailnet); the tailnet's own policy and DNS settings are
// written through its runtime (P12d), with a per-tailnet bucket of their
// own (tuning.tenant_api.config_write_rate); /api/v1 is not mounted.
//
// Every request is rate limited (tuning.tenant_api, P12c B4): a key this
// process has not authenticated recently spends the pre-authentication
// bucket before its database lookup (a flood of garbage keys cannot exhaust
// the database, and spends nothing else); then the key's and the tailnet's
// buckets; only a request they admit spends the global bucket. Neither a
// tenant over its limit nor unauthenticated keys can make another tenant's
// requests 429 (invariant I10); a key not seen for knownKeyTTL competes for
// the pre-authentication bucket like any unknown key.

var (
	clusterAPIRefused = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "headscale",
		Name:      "cluster_api_refused_total",
		Help:      "Public /api/v2 requests of a cluster refused before reaching a tailnet runtime, by reason.",
	}, []string{"reason"})
	tailnetAPIRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "headscale",
		Name:      "tailnet_api_requests_total",
		Help:      "Public /api/v2 requests with a valid API key, by tailnet and outcome (served, limited); tailnets past the label cap are summed under tailnet=\"other\".",
	}, []string{"tailnet", "outcome"})
)

// clusterAPI is the state of the public /api/v2 of a cluster process.
type clusterAPI struct {
	global   *ratelimit.Bucket
	preauth  *ratelimit.Bucket
	keys     *ratelimit.Buckets
	tailnets *ratelimit.Buckets
	known    knownKeys
	// writes is the per-tailnet bucket of policy and DNS writes (P12d),
	// spent by the runtime's v2 API after the request buckets admitted the
	// request and its {tailnet} was checked. Here, not in the runtime, so
	// a reload of the runtime does not refill it.
	writes *ratelimit.Buckets
}

func newClusterAPI(c *Cluster) *clusterAPI {
	ta := c.cfg.Tuning.TenantAPI
	now := time.Now()

	return &clusterAPI{
		global:   ratelimit.NewBucket(float64(ta.GlobalRate), ta.GlobalBurst, now),
		preauth:  ratelimit.NewBucket(float64(ta.PreauthRate), ta.PreauthBurst, now),
		keys:     ratelimit.NewBuckets(float64(ta.KeyRate), ta.KeyBurst),
		tailnets: ratelimit.NewBuckets(float64(ta.TailnetRate), ta.TailnetBurst),
		writes:   ratelimit.NewBuckets(float64(ta.ConfigWriteRate)/60, ta.ConfigWriteBurst),
	}
}

// knownKeyTTL is how long an API key that authenticated is routed without a
// new lookup (and without the pre-authentication bucket).
const knownKeyTTL = time.Minute

// maxKnownKeys bounds knownKeys; past it the set starts over. Only keys that
// authenticated enter it, so garbage cannot fill it.
const maxKnownKeys = 10_000

// knownKeys remembers, by SHA-256 of the whole token, the tailnet and key ID
// of API keys that authenticated recently. It only routes and picks the
// buckets to charge: the runtime authenticates the key again on every
// request through its tailnet-scoped handle (revocation and expiry are
// checked there), and a key's tailnet never changes.
type knownKeys struct {
	mu sync.Mutex
	m  map[[sha256.Size]byte]knownKey
}

type knownKey struct {
	tailnet tailnet.ID
	keyID   string
	until   time.Time
}

func (k *knownKeys) get(token string, now time.Time) (tailnet.ID, string, bool) {
	h := sha256.Sum256([]byte(token))

	k.mu.Lock()
	defer k.mu.Unlock()

	e, ok := k.m[h]
	if !ok || !now.Before(e.until) {
		return tailnet.ID{}, "", false
	}

	return e.tailnet, e.keyID, true
}

func (k *knownKeys) put(token string, id tailnet.ID, keyID string, now time.Time) {
	h := sha256.Sum256([]byte(token))

	k.mu.Lock()
	defer k.mu.Unlock()

	if k.m == nil || len(k.m) >= maxKnownKeys {
		k.m = make(map[[sha256.Size]byte]knownKey)
	}

	k.m[h] = knownKey{tailnet: id, keyID: keyID, until: now.Add(knownKeyTTL)}
}

// runtimeAPI is the v2 API of one runtime, built on first use.
type runtimeAPI struct {
	once sync.Once
	h    http.Handler
}

// apiV2 returns rt's v2 API: the runtime's state, bound to its tailnet.
func (rt *tenantRuntime) apiV2(c *Cluster) http.Handler {
	rt.api.once.Do(func() {
		id := rt.id
		mux, _ := apiv2.Handler(apiv2.Backend{
			State:     rt.h.state,
			Change:    rt.h.Change,
			Cfg:       rt.h.cfg,
			Connected: rt.h.isConnected,
			Tailnet:   id.String(),
			Tenant: &apiv2.Tenant{
				Slug:              rt.slug,
				ClusterBaseDomain: c.cfg.DNSConfig.BaseDomain,
				AllowConfigWrite: func() bool {
					return c.api.writes.Allow(id.String(), time.Now())
				},
				Audit: func(w apiv2.TenantWrite) { auditTenantWrite(id, w) },
			},
		})
		rt.api.h = mux
	})

	return rt.api.h
}

// auditTenantWrite logs a tenant's stored policy or DNS write (P12d): the
// tailnet, the operation, the API key's ID, the new revision and the
// SHA-256 of what was stored; never the content.
func auditTenantWrite(id tailnet.ID, w apiv2.TenantWrite) {
	log.Info().Str("tailnet", id.String()).Str("op", w.Op).Uint64("api_key_id", w.KeyID).
		Uint64("revision", w.Revision).Str("sha256", w.SHA256).Msg("tenant API: configuration stored")

	if fn := tenantAuditHook.Load(); fn != nil {
		(*fn)(id, w)
	}
}

// writeAPIError writes the Tailscale error body the v2 API uses.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")

	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}

	w.WriteHeader(status)

	err := json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
		Status  int    `json:"status"`
	}{msg, status})
	if err != nil {
		log.Debug().Err(err).Msg("cluster API: writing error response")
	}
}

// apiToken extracts the API key of a request: Bearer, or HTTP Basic with the
// key as the username (what the Tailscale SDK sends).
func apiToken(header string) (string, bool) {
	if token, ok := strings.CutPrefix(header, "Bearer "); ok {
		return token, token != ""
	}

	if encoded, ok := strings.CutPrefix(header, "Basic "); ok {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", false
		}

		user, _, _ := strings.Cut(string(raw), ":")

		return user, user != ""
	}

	return "", false
}

// APIv2Handler serves the public /api/v2 of the cluster.
func (c *Cluster) APIv2Handler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	if !c.ready.Load() || !c.leaseHeld() {
		clusterAPIRefused.WithLabelValues("standby").Inc()
		writeAPIError(w, http.StatusServiceUnavailable, "not ready")

		return
	}

	token, ok := apiToken(r.Header.Get("Authorization"))
	if !ok {
		clusterAPIRefused.WithLabelValues("unauthenticated").Inc()
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")

		return
	}

	id, keyID, ok := c.api.known.get(token, now)
	if !ok {
		if !c.api.preauth.Allow(now) {
			clusterAPIRefused.WithLabelValues("preauth_limit").Inc()
			writeAPIError(w, http.StatusTooManyRequests, "rate limited")

			return
		}

		var err error

		id, keyID, err = c.platform.TailnetForAPIKey(token) //nolint:contextcheck // Platform bounds its own lookups (lookupContext)
		if err != nil {
			if errors.Is(err, db.ErrUnknownTenant) {
				clusterAPIRefused.WithLabelValues("unauthenticated").Inc()
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")

				return
			}

			log.Error().Err(err).Msg("cluster api: API key lookup failed")
			clusterAPIRefused.WithLabelValues("lookup_error").Inc()
			writeAPIError(w, http.StatusServiceUnavailable, "temporarily unavailable")

			return
		}

		c.api.known.put(token, id, keyID, now)
	}

	label := tenantlabel.Label(id.String())

	// The key's bucket first: a key over its limit does not spend its
	// tailnet's tokens; a request either refuses does not spend the global
	// bucket.
	if !c.api.keys.Allow(keyID, now) || !c.api.tailnets.Allow(id.String(), now) {
		tailnetAPIRequests.WithLabelValues(label, "limited").Inc()
		writeAPIError(w, http.StatusTooManyRequests, "rate limited")

		return
	}

	if !c.api.global.Allow(now) {
		clusterAPIRefused.WithLabelValues("global_limit").Inc()
		writeAPIError(w, http.StatusTooManyRequests, "rate limited")

		return
	}

	c.mu.RLock()
	rt, ok := c.runtimes[id]
	c.mu.RUnlock()

	if !ok || !rt.h.servable() {
		// Owned by another cluster, not ACTIVE, or retiring: like a tailnet
		// that does not exist.
		clusterAPIRefused.WithLabelValues("not_served").Inc()
		writeAPIError(w, http.StatusNotFound, "tailnet not found")

		return
	}

	tailnetAPIRequests.WithLabelValues(label, "served").Inc()

	// The runtime's router matches the full /api/v2/... path with a
	// routing context of its own.
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, nil))
	rt.apiV2(c).ServeHTTP(w, r)
}

// tenantAuditHook, when set, also receives every audit record of a
// tenant's policy or DNS write (tests).
var tenantAuditHook atomic.Pointer[func(tailnet.ID, apiv2.TenantWrite)]

// SetTenantAuditHookForTest makes fn receive every audit record of a
// tenant's policy or DNS write (P12d), in addition to the log line; nil
// removes it. For tests.
func (c *Cluster) SetTenantAuditHookForTest(fn func(tailnet.ID, apiv2.TenantWrite)) {
	if fn == nil {
		tenantAuditHook.Store(nil)

		return
	}

	tenantAuditHook.Store(&fn)
}
