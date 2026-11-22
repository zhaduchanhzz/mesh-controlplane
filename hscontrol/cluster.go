package hscontrol

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
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
		"cluster mode does not run the embedded DERP server yet (per-tenant DERP verify is P08)",
	)
	errClusterPolicyFile = errors.New(
		"cluster mode needs policy.mode=database: a policy file would apply to every tailnet",
	)
	errClusterTLSUnsupported = errors.New(
		"cluster mode supports tls.cert_path/tls.key_path or plain HTTP behind the front door, not ACME",
	)
)

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

	// front answers the tenant-free endpoints (/key, /version, /robots.txt)
	// and loads TLS settings; it has no State.
	front *Headscale

	mu       sync.RWMutex
	runtimes map[tailnet.ID]*Headscale
	order    []tailnet.ID
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

	c := &Cluster{
		cfg:      cfg,
		platform: platform,
		noiseKey: noiseKey,
		front:    &Headscale{cfg: cfg, noisePrivateKey: noiseKey},
		runtimes: make(map[tailnet.ID]*Headscale),
	}

	err = c.loadOwnedTailnets()
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
	}

	return nil
}

func (c *Cluster) loadOwnedTailnets() error {
	tns, err := c.platform.OwnedTailnets(c.platform.ClusterID())
	if err != nil {
		return err
	}

	for _, tn := range tns {
		id, err := tailnet.ParseID(tn.ID)
		if err != nil {
			return fmt.Errorf("tailnet %q: %w", tn.ID, err)
		}

		tdb, err := c.platform.TenantDB(id)
		if err != nil {
			return fmt.Errorf("tailnet %s: %w", id, err)
		}

		rt, err := newHeadscale(c.cfg, c.noiseKey, "", func() (*state.State, error) {
			return state.NewStateWithDB(c.cfg, tdb)
		})
		if err != nil {
			_ = tdb.Close()

			return fmt.Errorf("tailnet %s runtime: %w", id, err)
		}

		c.runtimes[id] = rt
		c.order = append(c.order, id)

		log.Info().Str("tailnet", id.String()).Str("slug", tn.Slug).Msg("loaded tailnet runtime")
	}

	return nil
}

// Runtime returns the runtime of tailnet id, if this process serves it.
func (c *Cluster) Runtime(id tailnet.ID) (*Headscale, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rt, ok := c.runtimes[id]

	return rt, ok
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
		return c.runtimeFor(c.platform.TailnetForAuthKey(req.Auth.AuthKey))
	}

	id, err := c.platform.TailnetForNode(mk, req.NodeKey)
	if errors.Is(err, db.ErrUnknownTenant) && !req.OldNodeKey.IsZero() {
		// Node key rotation: the node is still stored under its old key.
		id, err = c.platform.TailnetForNode(mk, req.OldNodeKey)
	}

	return c.runtimeFor(id, err)
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

// HTTPHandler is the public handler of the cluster listener. It serves only
// the tenant-free and Noise endpoints: no admin API, debug or metrics
// (AGENTS rule 9; per-tailnet admin API is P08).
func (c *Cluster) HTTPHandler() http.Handler {
	r := chi.NewRouter()
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

// Serve starts every runtime and the cluster listener, and stops all of them
// when ctx is cancelled or a component fails.
func (c *Cluster) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errorGroup, ctx := errgroup.WithContext(ctx)

	var stops []func()

	defer func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}()

	for _, rt := range c.runtimesSnapshot() {
		stop, err := rt.startRuntime(ctx, errorGroup)
		stops = append(stops, stop)

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

	log.Info().
		Str("cluster", c.platform.ClusterID()).
		Int("tailnets", len(c.order)).
		Msgf("cluster listening on %s", ln.Addr())

	errorGroup.Go(func() error {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), types.HTTPShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil { //nolint:noinlineerr
			log.Error().Err(err).Msg("shutting down cluster http server")
		}

		if tlsBundle.Stop != nil {
			tlsBundle.Stop()
		}

		c.shutdownRuntimes()

		return nil
	})

	return errorGroup.Wait()
}

// shutdownRuntimes stops every runtime's background work and waits for its
// map streams, then closes the states and the shared pool.
func (c *Cluster) shutdownRuntimes() {
	for _, rt := range c.runtimesSnapshot() {
		rt.ephemeralGC.Close()

		if rt.challenges != nil {
			rt.challenges.Close()
		}

		if rt.mapBatcher != nil {
			rt.mapBatcher.Close()
		}

		rt.clientStreamsOpen.Wait()
	}

	c.closeRuntimes()

	err := c.platform.Close()
	if err != nil {
		log.Error().Err(err).Msg("closing platform database")
	}
}

func (c *Cluster) closeRuntimes() {
	for _, rt := range c.runtimesSnapshot() {
		err := rt.state.Close()
		if err != nil {
			log.Error().Err(err).Msg("closing tailnet state")
		}
	}
}
