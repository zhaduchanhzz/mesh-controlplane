package hscontrol

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/frontdoor"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"golang.org/x/sync/errgroup"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

var (
	errClusterOIDC = errors.New(
		"cluster mode serves auth-key registration only; OIDC/web registration per tailnet is P04b",
	)
	errClusterEmbeddedDERP = errors.New(
		"cluster mode does not run the embedded DERP server: one DERP server would relay for every " +
			"tailnet of the cluster, and per-tailnet DERP verify in cluster mode is P08b",
	)
	errClusterTailSQL = errors.New(
		"cluster mode refuses tailsql: it would expose the shared database of every tailnet (P08a N4)",
	)
	errClusterServerURL = errors.New(
		"cluster mode needs server_url with a DNS hostname: the listener answers only that host (P08, S11c)",
	)
	errClusterPolicyFile = errors.New(
		"cluster mode needs policy.mode=database: a policy file would apply to every tailnet",
	)
	errClusterTLSUnsupported = errors.New(
		"cluster mode supports tls.cert_path/tls.key_path or plain HTTP behind the front door, not ACME",
	)
)

// reconcileInterval is how often a cluster compares the tailnets it serves
// with the ones it owns (P10): new ACTIVE tailnets are loaded, runtimes whose
// tailnet moved, changed epoch or left ACTIVE are retired. Writes are fenced
// in the database regardless; this bounds how long a retired owner keeps
// answering map requests from memory.
const reconcileInterval = 2 * time.Second

// Cluster is one cluster process (ADR 0005, ADR 0007): one Noise key, one
// listener and one database pool, serving every ACTIVE tailnet the cluster
// owns. Each tailnet has its own runtime (a [Headscale] with its own State,
// batcher and GC on a tenant-scoped database handle). A Noise session is
// bound to exactly one runtime, resolved from the request; nothing is served
// before a tenant is resolved, and no tenant is a default.
type Cluster struct {
	cfg      *types.Config
	platform *db.Platform
	noiseKey *key.MachinePrivate
	// registry enforces the per-tailnet device quota on new enrollments
	// (P10): shared clusters 5000 per tailnet, dedicated clusters their
	// max_devices. The owner cluster must be registered.
	registry *placement.Registry

	// host is the normalized server_url hostname, the only Host (and, over
	// TLS, SNI) the public listener answers (P08, S11c).
	host string

	// front answers the tenant-free endpoints (/key, /version, /robots.txt)
	// and loads TLS settings; it has no State.
	front *Headscale

	mu       sync.RWMutex
	runtimes map[tailnet.ID]*tenantRuntime
	order    []tailnet.ID

	// Set by Serve: runtimes loaded later are started under them.
	serveCtx   context.Context //nolint:containedctx // lifetime of Serve
	serveGroup *errgroup.Group
}

// tenantRuntime is one loaded tailnet: its runtime, the ownership epoch it
// was loaded at, and how to stop it.
type tenantRuntime struct {
	id    tailnet.ID
	slug  string
	epoch uint64
	h     *Headscale

	// policyRev is the tailnets.policy_revision the runtime's policy matches;
	// only the reconcile loop reads or writes it after load.
	policyRev uint64

	cancel context.CancelFunc // stops its scheduled tasks
	stop   func()             // from startRuntime
}

// NewCluster opens the platform database and builds a runtime for every
// ACTIVE tailnet owned by cfg.ClusterID. cfg is shared by the runtimes:
// per-tailnet DNS and IP settings come with the tailnet config revision
// (later ticket); policy, users, nodes and keys are per tailnet already.
func NewCluster(cfg *types.Config) (*Cluster, error) {
	err := config.Validate(cfg)
	if err != nil {
		return nil, err
	}

	err = validateClusterConfig(cfg)
	if err != nil {
		return nil, err
	}

	// A tenant without access rules is denied, not allow-all (AGENTS rule 8).
	cfg.PolicyEmptyDenies = true

	noiseKey, err := readOrCreatePrivateKey(cfg.AbsPath(cfg.Noise.PrivateKeyPath))
	if err != nil {
		return nil, fmt.Errorf("reading or creating Noise protocol private key: %w", err)
	}

	platform, err := db.OpenPlatform(cfg)
	if err != nil {
		return nil, fmt.Errorf("opening platform database: %w", err)
	}

	registry, err := placement.New(platform.RegistryDB(), placement.Config{})
	if err != nil {
		_ = platform.Close()

		return nil, fmt.Errorf("placement registry: %w", err)
	}

	host, err := clusterHost(cfg.ServerURL)
	if err != nil {
		return nil, err
	}

	c := &Cluster{
		host:     host,
		cfg:      cfg,
		platform: platform,
		noiseKey: noiseKey,
		registry: registry,
		front:    &Headscale{cfg: cfg, noisePrivateKey: noiseKey},
		runtimes: make(map[tailnet.ID]*tenantRuntime),
	}

	tns, err := platform.OwnedTailnets(platform.ClusterID())
	if err == nil {
		for _, tn := range tns {
			err = c.load(tn)
			if err != nil {
				break
			}
		}
	}

	if err != nil {
		c.closeRuntimes()
		_ = platform.Close()

		return nil, err
	}

	return c, nil
}

func validateClusterConfig(cfg *types.Config) error {
	switch {
	case cfg.ClusterID == "":
		return errors.New("cluster mode needs a cluster ID")
	case cfg.OIDC.Issuer != "":
		return errClusterOIDC
	case cfg.DERP.Server.Enabled:
		return errClusterEmbeddedDERP
	case cfg.Policy.Mode != types.PolicyModeDB:
		return errClusterPolicyFile
	case cfg.TLS.ACME.Hostname != "":
		return errClusterTLSUnsupported
	case cfg.TailSQL.Enabled:
		return errClusterTailSQL
	}

	if _, err := clusterHost(cfg.ServerURL); err != nil { //nolint:noinlineerr
		return err
	}

	return nil
}

// load builds the runtime of tn (not started) and registers it.
func (c *Cluster) load(tn types.Tailnet) error {
	id, err := tailnet.ParseID(tn.ID)
	if err != nil {
		return fmt.Errorf("tailnet %q: %w", tn.ID, err)
	}

	// TenantDB re-reads the row: owner, ACTIVE and the epoch the handle is
	// fenced on come from the same read.
	tdb, err := c.platform.TenantDB(id)
	if err != nil {
		return fmt.Errorf("tailnet %s: %w", id, err)
	}

	h, err := newHeadscale(c.cfg, c.noiseKey, "", func() (*state.State, error) {
		return state.NewStateWithDB(c.cfg, tdb)
	})
	if err != nil {
		_ = tdb.Close()

		return fmt.Errorf("tailnet %s runtime: %w", id, err)
	}

	rt := &tenantRuntime{
		id: id, slug: tn.Slug, epoch: tdb.Tailnet().OwnerEpoch, h: h,
		policyRev: tdb.Tailnet().PolicyRevision,
	}

	c.mu.Lock()
	c.runtimes[id] = rt
	c.order = append(c.order, id)
	c.mu.Unlock()

	log.Info().Str("tailnet", id.String()).Str("slug", tn.Slug).Uint64("epoch", rt.epoch).Msg("loaded tailnet runtime")

	return nil
}

// start runs rt's background work under the serve context.
func (c *Cluster) start(rt *tenantRuntime) error {
	ctx, cancel := context.WithCancel(c.serveCtx)
	rt.cancel = cancel

	stop, err := rt.h.startRuntime(ctx, c.serveGroup)
	rt.stop = stop

	return err
}

// Runtime returns the runtime of tailnet id, if this process serves it.
func (c *Cluster) Runtime(id tailnet.ID) (*Headscale, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rt, ok := c.runtimes[id]
	if !ok {
		return nil, false
	}

	return rt.h, true
}

// reconcile loads the ACTIVE tailnets this cluster owns and does not serve
// yet, and retires runtimes whose tailnet is no longer owned by this cluster
// at the epoch they were loaded with (moved, epoch bumped, not ACTIVE,
// deleted). A failed listing changes nothing.
func (c *Cluster) reconcile() {
	owned, err := c.platform.OwnedTailnets(c.platform.ClusterID())
	if err != nil {
		log.Error().Err(err).Msg("reconcile: listing owned tailnets")

		return
	}

	want := make(map[string]types.Tailnet, len(owned))
	for _, tn := range owned {
		want[tn.ID] = tn
	}

	c.mu.RLock()
	var stale, policyChanged []*tenantRuntime
	loaded := make(map[string]bool, len(c.runtimes))
	for id, rt := range c.runtimes {
		loaded[id.String()] = true
		tn, ok := want[id.String()]
		switch {
		case !ok || tn.OwnerEpoch != rt.epoch:
			stale = append(stale, rt)
		case tn.PolicyRevision != rt.policyRev:
			policyChanged = append(policyChanged, rt)
		}
	}
	c.mu.RUnlock()

	for _, rt := range policyChanged {
		rev := want[rt.id.String()].PolicyRevision
		// The revision is recorded even when the new policy does not load:
		// the runtime keeps enforcing the previous one (never allow-all) and
		// the error is logged once, not on every tick.
		rt.policyRev = rev

		changes, err := rt.h.state.ReloadPolicy()
		if err != nil {
			log.Error().Err(err).Str("tailnet", rt.id.String()).Uint64("policy_revision", rev).
				Msg("reconcile: new policy rejected, keeping the previous one")

			continue
		}

		rt.h.Change(changes...)
		log.Info().Str("tailnet", rt.id.String()).Uint64("policy_revision", rev).Msg("reconcile: policy reloaded")
	}

	for _, rt := range stale {
		c.retire(rt, "not ACTIVE and owned by this cluster at the loaded epoch any more")
	}

	for _, tn := range owned {
		if loaded[tn.ID] && !containsRuntime(stale, tn.ID) {
			continue
		}

		err := c.load(tn)
		if err != nil {
			log.Error().Err(err).Str("tailnet", tn.ID).Msg("reconcile: loading tailnet")

			continue
		}

		id, _ := tailnet.ParseID(tn.ID)

		c.mu.RLock()
		rt := c.runtimes[id]
		c.mu.RUnlock()

		if err := c.start(rt); err != nil { //nolint:noinlineerr
			log.Error().Err(err).Str("tailnet", tn.ID).Msg("reconcile: starting tailnet")
			c.retire(rt, "failed to start")
		}
	}
}

func containsRuntime(rts []*tenantRuntime, id string) bool {
	for _, rt := range rts {
		if rt.id.String() == id {
			return true
		}
	}

	return false
}

// retire stops serving rt: it is removed from routing first (new sessions
// for its tailnet are refused), its bound Noise sessions are closed, its map
// streams end, and its background work stops. Its writes are already fenced
// in the database once ownership changed.
func (c *Cluster) retire(rt *tenantRuntime, reason string) {
	c.mu.Lock()
	if cur, ok := c.runtimes[rt.id]; !ok || cur != rt {
		c.mu.Unlock()

		return
	}

	delete(c.runtimes, rt.id)
	c.order = removeID(c.order, rt.id)
	c.mu.Unlock()

	log.Warn().Str("tailnet", rt.id.String()).Str("slug", rt.slug).Uint64("epoch", rt.epoch).
		Str("reason", reason).Msg("retiring tailnet runtime")

	c.stopRuntime(rt)
}

func (c *Cluster) stopRuntime(rt *tenantRuntime) {
	h := rt.h
	h.retired.Store(true)
	h.closeSessions()

	if rt.cancel != nil {
		rt.cancel()
	}

	h.ephemeralGC.Close()

	if h.challenges != nil {
		h.challenges.Close()
	}

	if h.mapBatcher != nil {
		h.mapBatcher.Close()
	}

	h.clientStreamsOpen.Wait()

	if rt.stop != nil {
		rt.stop()
	}

	err := h.state.Close()
	if err != nil {
		log.Error().Err(err).Str("tailnet", rt.id.String()).Msg("closing tailnet state")
	}
}

func removeID(ids []tailnet.ID, id tailnet.ID) []tailnet.ID {
	out := ids[:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}

	return out
}

// NoisePublicKey is the cluster's Noise key, shared by every tailnet.
func (c *Cluster) NoisePublicKey() key.MachinePublic {
	return c.noiseKey.Public()
}

func (c *Cluster) runtimeFor(id tailnet.ID, err error) (*Headscale, error) {
	if err != nil {
		return nil, err
	}

	rt, ok := c.Runtime(id)
	if !ok {
		// Known tailnet, but not served here (another cluster, not ACTIVE):
		// refused the same way as an unknown one.
		return nil, db.ErrUnknownTenant
	}

	return rt, nil
}

// runtimeForRegister implements sessionRouter.
func (c *Cluster) runtimeForRegister(req tailcfg.RegisterRequest, mk key.MachinePublic) (*Headscale, error) {
	if req.Auth != nil && req.Auth.AuthKey != "" {
		id, err := c.platform.TailnetForAuthKey(req.Auth.AuthKey)
		if err != nil {
			return nil, err
		}

		if !c.knownNode(mk, req) {
			err = c.checkQuota(id)
			if err != nil {
				return nil, err
			}
		}

		return c.runtimeFor(id, nil)
	}

	id, err := c.platform.TailnetForNode(mk, req.NodeKey)
	if errors.Is(err, db.ErrUnknownTenant) && !req.OldNodeKey.IsZero() {
		// Node key rotation: the node is still stored under its old key.
		id, err = c.platform.TailnetForNode(mk, req.OldNodeKey)
	}

	return c.runtimeFor(id, err)
}

// knownNode reports whether the registering device is already a node
// (re-login, key rotation): only new devices count against the quota.
func (c *Cluster) knownNode(mk key.MachinePublic, req tailcfg.RegisterRequest) bool {
	if _, err := c.platform.TailnetForNode(mk, req.NodeKey); err == nil { //nolint:noinlineerr
		return true
	}

	if req.OldNodeKey.IsZero() {
		return false
	}

	_, err := c.platform.TailnetForNode(mk, req.OldNodeKey)

	return err == nil
}

// checkQuota refuses a new device when tailnet id reached its limit, or when
// its limit cannot be determined (fail closed).
func (c *Cluster) checkQuota(id tailnet.ID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	q, err := c.registry.EnrollmentAllowed(ctx, id)
	if err != nil {
		if !errors.Is(err, placement.ErrQuotaExceeded) {
			log.Error().Err(err).Str("tailnet", id.String()).Msg("enrollment refused: quota cannot be checked")
		}

		return err
	}

	if q.Warn {
		log.Warn().Str("tailnet", id.String()).Int64("devices", q.Devices).Int64("limit", q.Limit).
			Msg("tailnet is close to its device limit")
	}

	return nil
}

// runtimeForNode implements sessionRouter.
func (c *Cluster) runtimeForNode(mk key.MachinePublic, nk key.NodePublic) (*Headscale, error) {
	return c.runtimeFor(c.platform.TailnetForNode(mk, nk))
}

func (c *Cluster) runtimesSnapshot() []*Headscale {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*Headscale, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.runtimes[id].h)
	}

	return out
}

func (c *Cluster) tenantSnapshot() []*tenantRuntime {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*tenantRuntime, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.runtimes[id])
	}

	return out
}

// NoiseUpgradeHandler serves /ts2021 with the cluster key; the session is
// routed to a runtime per request (ADR 0007).
func (c *Cluster) NoiseUpgradeHandler(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Upgrade") == "" {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	serveNoiseSession(w, req, *c.noiseKey, &noiseServer{
		router:    c,
		challenge: key.NewChallenge(),
	})
}

// PingResponseHandler completes a ping of whichever runtime issued it; ping
// IDs are unguessable and unique per runtime.
func (c *Cluster) PingResponseHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := req.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing ping ID", http.StatusBadRequest)
		return
	}

	for _, rt := range c.runtimesSnapshot() {
		if rt.state.CompletePing(id) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	http.Error(w, "unknown or expired ping", http.StatusNotFound)
}

// HealthHandler reports whether the shared database answers.
func (c *Cluster) HealthHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/health+json")

	err := c.platform.Ping(req.Context())
	if err != nil {
		log.Error().Err(err).Msg("health check failed")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"fail"}`))

		return
	}

	_, _ = w.Write([]byte(`{"status":"pass"}`))
}

// clusterRegistrationDisabled answers the web/OIDC registration routes: in
// cluster mode a registration without an auth key has no tenant (P04b).
func clusterRegistrationDisabled(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "interactive registration is not available on this server; use an auth key", http.StatusNotFound)
}

// clusterHost returns the normalized hostname of server_url. An IP literal or
// a missing host is refused: the listener selects nothing by IP.
func clusterHost(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: %q", errClusterServerURL, serverURL)
	}

	host, err := frontdoor.NormalizeHost(u.Host)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errClusterServerURL, err)
	}

	return host, nil
}

// hostGuard answers 421 Misdirected Request unless the request is for this
// cluster's hostname (P08, S11c): Host, normalized (lowercase, no port, no
// trailing dot), must equal the server_url host, and over TLS the SNI must
// equal it too. A raw-IP Host, a hostname of another cluster or tenant, or a
// connection whose SNI differs from Host gets nothing, not even /health.
func (c *Cluster) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, err := frontdoor.NormalizeHost(r.Host)
		if err != nil || host != c.host {
			misdirectedCluster(w, r, "host is not this cluster")
			return
		}

		if r.TLS != nil {
			sni, err := frontdoor.NormalizeHost(r.TLS.ServerName)
			if err != nil || sni != host {
				misdirectedCluster(w, r, "sni does not match host")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func misdirectedCluster(w http.ResponseWriter, r *http.Request, why string) {
	log.Debug().Str("host", r.Host).Str("path", r.URL.Path).Msg("cluster listener: refused: " + why)
	http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
}

// MetricsHandler is the cluster's private metrics listener
// (metrics_listen_addr): /metrics only, operators only ([operatorOnly]:
// loopback or metrics_operator_cidrs, never the Tailscale range). No debug
// pages: they would show state of every tailnet of the cluster.
func (c *Cluster) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())

	return securityHeaders(operatorOnly(c.cfg.MetricsOperatorCIDRs, mux))
}

// HTTPHandler is the public handler of the cluster listener. It serves only
// the tenant-free and Noise endpoints for the cluster's own hostname: no
// admin API, debug or metrics (AGENTS rule 9; per-tailnet admin API is
// P08b). Metrics are on [Cluster.MetricsHandler].
func (c *Cluster) HTTPHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(c.hostGuard)
	r.Use(middleware.RequestID)
	r.Use(middleware.RequestLogger(&zerologRequestLogger{}))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Get(ts2021UpgradePath, c.NoiseUpgradeHandler)
	r.Post(ts2021UpgradePath, c.NoiseUpgradeHandler)

	r.Get("/key", c.front.KeyHandler)
	r.Get("/health", c.HealthHandler)
	r.Get("/version", c.front.VersionHandler)
	r.Get("/robots.txt", c.front.RobotsHandler)
	r.Head("/machine/ping-response", c.PingResponseHandler)

	r.Get("/register/{auth_id}", clusterRegistrationDisabled)
	r.Get("/auth/{auth_id}", clusterRegistrationDisabled)

	r.Get("/favicon.ico", FaviconHandler)
	r.Get("/", BlankHandler)

	return r
}

// Serve starts every runtime and the cluster listener, keeps the served
// tailnets in line with ownership, and stops everything when ctx is
// cancelled or a component fails.
func (c *Cluster) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errorGroup, ctx := errgroup.WithContext(ctx)
	c.serveCtx, c.serveGroup = ctx, errorGroup

	defer c.shutdownRuntimes()

	for _, rt := range c.tenantSnapshot() {
		err := c.start(rt)
		if err != nil {
			return fmt.Errorf("starting tailnet runtime: %w", err)
		}
	}

	tlsBundle, err := c.front.getTLSSettings(ctx)
	if err != nil {
		return fmt.Errorf("configuring TLS settings: %w", err)
	}

	srv := &http.Server{
		Addr:        string(c.cfg.Addr),
		Handler:     c.HTTPHandler(),
		ReadTimeout: types.HTTPTimeout,
		// Long polls override this per request.
		WriteTimeout: types.HTTPTimeout,
	}

	var ln net.Listener
	if tlsBundle.Config != nil {
		srv.TLSConfig = tlsBundle.Config
		ln, err = tls.Listen("tcp", string(c.cfg.Addr), tlsBundle.Config)
	} else {
		ln, err = new(net.ListenConfig).Listen(ctx, "tcp", string(c.cfg.Addr))
	}

	if err != nil {
		return &types.ListenerBindError{Listener: "cluster HTTP", YAMLKey: "listen_addr", Addr: string(c.cfg.Addr), Err: err}
	}

	errorGroup.Go(serveHTTP(srv, ln))

	var metricsSrv *http.Server

	if c.cfg.MetricsAddr != "" {
		mln, err := new(net.ListenConfig).Listen(ctx, "tcp", string(c.cfg.MetricsAddr))
		if err != nil {
			return &types.ListenerBindError{
				Listener: "cluster metrics", YAMLKey: "metrics_listen_addr",
				Addr: string(c.cfg.MetricsAddr), Err: err,
			}
		}

		metricsSrv = &http.Server{
			Addr:         string(c.cfg.MetricsAddr),
			Handler:      c.MetricsHandler(),
			ReadTimeout:  types.HTTPTimeout,
			WriteTimeout: types.HTTPTimeout,
		}

		errorGroup.Go(serveHTTP(metricsSrv, mln))
		log.Info().Msgf("cluster metrics (operators only) on %s", mln.Addr())
	}

	c.mu.RLock()
	n := len(c.order)
	c.mu.RUnlock()

	log.Info().
		Str("cluster", c.platform.ClusterID()).
		Int("tailnets", n).
		Msgf("cluster listening on %s", ln.Addr())

	errorGroup.Go(func() error {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				c.reconcile()
			}
		}
	})

	errorGroup.Go(func() error {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), types.HTTPShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil { //nolint:noinlineerr
			log.Error().Err(err).Msg("shutting down cluster http server")
		}

		if metricsSrv != nil {
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil { //nolint:noinlineerr
				log.Error().Err(err).Msg("shutting down cluster metrics server")
			}
		}

		if tlsBundle.Stop != nil {
			tlsBundle.Stop()
		}

		return nil
	})

	return errorGroup.Wait()
}

// shutdownRuntimes stops every runtime, then closes the shared pool.
func (c *Cluster) shutdownRuntimes() {
	c.mu.Lock()
	rts := make([]*tenantRuntime, 0, len(c.order))
	for _, id := range c.order {
		rts = append(rts, c.runtimes[id])
	}

	c.runtimes = make(map[tailnet.ID]*tenantRuntime)
	c.order = nil
	c.mu.Unlock()

	for _, rt := range rts {
		c.stopRuntime(rt)
	}

	err := c.platform.Close()
	if err != nil {
		log.Error().Err(err).Msg("closing platform database")
	}
}

// closeRuntimes closes the states of runtimes that never started.
func (c *Cluster) closeRuntimes() {
	for _, rt := range c.runtimesSnapshot() {
		err := rt.state.Close()
		if err != nil {
			log.Error().Err(err).Msg("closing tailnet state")
		}
	}
}
