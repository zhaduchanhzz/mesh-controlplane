package hscontrol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	apiv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/ratelimit"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tenantlabel"
)

// The public /api/v2 of a cluster (P08b, owner decision Q3). The API key of
// a request is the only thing that picks its tailnet: the platform looks
// the key up (credentials.tailnet_id), the request goes to the runtime
// serving that tailnet, and that runtime's v2 API authenticates the key
// again through its own scoped handle. {tailnet} accepts only "-" or the
// key's tailnet ID; a device of another tailnet is not in the runtime and is
// 404. Policy, DNS settings and device trust settings are refused (see
// apiv2.Backend.Tailnet); /api/v1 is not mounted.
//
// Every request is rate limited: a global bucket before the key lookup (an
// unauthenticated flood cannot exhaust the database), then the key's and
// the tailnet's buckets (tuning.tenant_api).

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
	keys     *ratelimit.Buckets
	tailnets *ratelimit.Buckets
}

func newClusterAPI(c *Cluster) *clusterAPI {
	ta := c.cfg.Tuning.TenantAPI

	return &clusterAPI{
		global:   ratelimit.NewBucket(float64(ta.GlobalRate), ta.GlobalBurst, time.Now()),
		keys:     ratelimit.NewBuckets(float64(ta.KeyRate), ta.KeyBurst),
		tailnets: ratelimit.NewBuckets(float64(ta.TailnetRate), ta.TailnetBurst),
	}
}

// runtimeAPI is the v2 API of one runtime, built on first use.
type runtimeAPI struct {
	once sync.Once
	h    http.Handler
}

// apiV2 returns rt's v2 API: the runtime's state, bound to its tailnet.
func (rt *tenantRuntime) apiV2() http.Handler {
	rt.api.once.Do(func() {
		mux, _ := apiv2.Handler(apiv2.Backend{
			State:     rt.h.state,
			Change:    rt.h.Change,
			Cfg:       rt.h.cfg,
			Connected: rt.h.isConnected,
			Tailnet:   rt.id.String(),
		})
		rt.api.h = mux
	})

	return rt.api.h
}

// writeAPIError writes the Tailscale error body the v2 API uses.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")

	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": msg, "status": status})
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

	if !c.api.global.Allow(now) {
		clusterAPIRefused.WithLabelValues("global_limit").Inc()
		writeAPIError(w, http.StatusTooManyRequests, "rate limited")

		return
	}

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

	id, keyID, err := c.platform.TailnetForAPIKey(token)
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

	label := tenantlabel.Label(id.String())

	// The key's bucket first: a key over its limit does not spend its
	// tailnet's tokens.
	if !c.api.keys.Allow(keyID, now) || !c.api.tailnets.Allow(id.String(), now) {
		tailnetAPIRequests.WithLabelValues(label, "limited").Inc()
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
	rt.apiV2().ServeHTTP(w, r)
}
