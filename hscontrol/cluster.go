package hscontrol

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
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
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tenantlabel"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
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
	// ADR 0008: DNS is per tailnet in cluster mode. The cluster sets the
	// base domain every tailnet's lives under; settings a tailnet owns in
	// the config file would apply to nobody or to everybody.
	errClusterTenantDNS = errors.New(
		"cluster mode keeps dns.extra_records, dns.extra_records_path, dns.nameservers and " +
			"dns.search_domains per tailnet (`cluster tailnet dns`); remove them from the config")
	errClusterNoBaseDomain = errors.New(
		"cluster mode needs dns.base_domain: each tailnet's MagicDNS domain is <slug>.<base_domain>")

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

// runtimeDrainTimeout bounds how long stopping a retired runtime waits for
// its in-flight Noise requests (map long-polls included, which retire ends).
// Past it the runtime's State is left open and the stop is abandoned with an
// error log: a leaked runtime is safer than handlers on a closed State, and
// its writes are fenced in the database (P10 review B1).
var runtimeDrainTimeout = 30 * time.Second

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
	// retiring holds, per tailnet, a channel closed once its retired
	// runtime stopped serving (ADR 0010: a hand-over waits for it);
	// handingOver marks tailnets whose hand-over is under way. Under mu.
	retiring    map[tailnet.ID]chan struct{}
	handingOver map[tailnet.ID]bool

	// Set by Serve: runtimes loaded later are started under them.
	serveCtx   context.Context //nolint:containedctx // lifetime of Serve
	serveGroup *errgroup.Group

	// stopping tracks runtimes being stopped in the background by retire
	// (P10 review B1): reconcile never waits for one, shutdown waits for all.
	stopping sync.WaitGroup

	// P11b: the cluster lease elects the one process of the cluster that
	// serves (active); the others are standby. leaseMu serializes taking the
	// lease (load every owned tailnet), losing it (retire them all) and
	// reconcile, so no runtime is loaded outside a held lease. ready is set
	// once the tailnets are loaded and cleared first when the lease ends.
	lease   *lease.Manager
	leaseMu sync.Mutex
	ready   atomic.Bool

	// api is the public /api/v2 (P08b): rate limits by key and tailnet.
	api *clusterAPI
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
	// configRev is the tailnets.config_revision the runtime's DNS matches
	// (ADR 0008); same rules as policyRev.
	configRev uint64

	cancel context.CancelFunc // stops its scheduled tasks
	stop   func()             // from startRuntime

	// api is the runtime's public v2 API (P08b), built on first use.
	api runtimeAPI
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

	// P11b: tenant handles exist only under the cluster lease; tailnets are
	// loaded when this process takes it (Serve), never here.
	platform.RequireLease()

	lc := cfg.Tuning.ClusterLease

	mgr, err := lease.NewManager(platform.LeaseStore(), platform.ClusterID(), leaseHolderName(), lease.Config{
		TTL: lc.TTL, RenewInterval: lc.RenewInterval, AcquireInterval: lc.AcquireInterval, SafetyMargin: lc.SafetyMargin,
	})
	if err != nil {
		_ = platform.Close()

		return nil, fmt.Errorf("cluster lease: %w", err)
	}

	c := &Cluster{
		host:        host,
		cfg:         cfg,
		platform:    platform,
		noiseKey:    noiseKey,
		registry:    registry,
		front:       &Headscale{cfg: cfg, noisePrivateKey: noiseKey},
		runtimes:    make(map[tailnet.ID]*tenantRuntime),
		retiring:    make(map[tailnet.ID]chan struct{}),
		handingOver: make(map[tailnet.ID]bool),
		lease:       mgr,
	}
	c.api = newClusterAPI(c)

	// P08b: per-tailnet metric labels are capped (owner decision Q5).
	tenantlabel.SetLimit(cfg.Tuning.TailnetMetricLabelLimit)

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
	case len(cfg.DNSConfig.ExtraRecords) > 0, cfg.DNSConfig.ExtraRecordsPath != "",
		len(cfg.DNSConfig.Nameservers.Global) > 0, len(cfg.DNSConfig.Nameservers.Split) > 0,
		len(cfg.DNSConfig.SearchDomains) > 0:
		return errClusterTenantDNS
	case cfg.DNSConfig.BaseDomain == "":
		return errClusterNoBaseDomain
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

	// The runtime's own config: the tailnet's DNS under <slug>.<base>, a
	// live DNS no other runtime shares (ADR 0008 §5). Stored settings that
	// do not parse or validate keep the tailnet out: never default DNS.
	dns, err := c.tailnetDNS(id, tdb.Tailnet().Slug)
	if err != nil {
		_ = tdb.Close()

		return fmt.Errorf("tailnet %s: %w", id, err)
	}

	rcfg := c.cfg.ForTailnet(tdb.Tailnet().Slug, dns)

	h, err := newHeadscale(rcfg, c.noiseKey, "", func() (*state.State, error) {
		return state.NewStateWithDB(rcfg, tdb)
	})
	if err != nil {
		_ = tdb.Close()

		return fmt.Errorf("tailnet %s runtime: %w", id, err)
	}

	// The device quota is enforced in the node insert transaction (P10
	// review A3); the earlier check in runtimeForRegister only refuses
	// early.
	h.state.SetEnrollGate(func(tx *gorm.DB) error {
		q, err := c.registry.EnrollmentAllowedTx(tx, id)
		if err == nil && q.Warn {
			log.Warn().Str("tailnet", id.String()).Int64("devices", q.Devices).Int64("limit", q.Limit).
				Msg("tailnet is close to its device limit")
		}

		if err == nil && q.ClusterWarn {
			log.Warn().Str("cluster", q.ClusterID).Int64("devices", q.ClusterDevices).Int64("limit", q.ClusterLimit).
				Msg("cluster is close to its device limit; place new tailnets elsewhere")
		}

		return err
	})

	h.leaseGate = c.leaseHeld

	rt := &tenantRuntime{
		id: id, slug: tn.Slug, epoch: tdb.Tailnet().OwnerEpoch, h: h,
		policyRev: tdb.Tailnet().PolicyRevision,
		configRev: tdb.Tailnet().ConfigRevision,
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
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()

	if !c.ready.Load() || !c.leaseHeld() {
		return // standby, or the lease is ending: onLeaseLost retires
	}

	// ADR 0010: first give away the tailnets with a move requested (they
	// are not in OwnedTailnets any more) and take in those whose owner
	// cluster is dead, so the listing below loads them in this tick.
	c.handOverRequested()
	c.takeOverOrphans()

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
	var stale, policyChanged, configChanged []*tenantRuntime
	loaded := make(map[string]bool, len(c.runtimes))
	for id, rt := range c.runtimes {
		loaded[id.String()] = true
		tn, ok := want[id.String()]
		switch {
		case !ok || tn.OwnerEpoch != rt.epoch:
			stale = append(stale, rt)
		default:
			if tn.PolicyRevision != rt.policyRev {
				policyChanged = append(policyChanged, rt)
			}

			if tn.ConfigRevision != rt.configRev {
				configChanged = append(configChanged, rt)
			}
		}
	}
	c.mu.RUnlock()

	for _, rt := range configChanged {
		rev := want[rt.id.String()].ConfigRevision
		// As for policy: recorded even when the new settings do not load,
		// so the runtime keeps serving its previous DNS and the error is
		// logged once.
		rt.configRev = rev

		dns, err := c.tailnetDNS(rt.id, rt.slug)
		if err != nil {
			log.Error().Err(err).Str("tailnet", rt.id.String()).Uint64("config_revision", rev).
				Msg("reconcile: new DNS settings rejected, keeping the previous ones")

			continue
		}

		rt.h.cfg.ReplaceTenantDNS(rt.slug, dns)
		rt.h.Change(change.DNSConfig())
		log.Info().Str("tailnet", rt.id.String()).Uint64("config_revision", rev).Msg("reconcile: DNS settings reloaded")
	}

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

// tailnetDNS reads and checks tailnet id's stored DNS settings (ADR 0008):
// nil for the defaults; an error when they do not parse or break a rule of
// form. Settings over the size limits (lowered after they were stored)
// still load, with a warning; the next write must fit.
func (c *Cluster) tailnetDNS(id tailnet.ID, slug string) (*types.TenantDNS, error) {
	dns, err := c.platform.TailnetDNS(id)
	if err != nil {
		return nil, err
	}

	if dns == nil {
		return nil, nil //nolint:nilnil // the defaults
	}

	err = dns.Validate(slug, c.cfg.DNSConfig.BaseDomain, unlimitedTenantDNS)
	if err != nil {
		return nil, fmt.Errorf("stored DNS settings: %w", err)
	}

	err = dns.Validate(slug, c.cfg.DNSConfig.BaseDomain, c.cfg.Tuning.TenantDNS)
	if err != nil {
		log.Warn().Str("tailnet", id.String()).Msg("stored DNS settings exceed tuning.tenant_dns; " +
			"they are served, the next write must fit")
	}

	return dns, nil
}

// unlimitedTenantDNS checks form only.
var unlimitedTenantDNS = types.TenantDNSLimits{
	MaxExtraRecords: math.MaxInt, MaxNameservers: math.MaxInt, MaxSearchDomains: math.MaxInt, MaxConfigBytes: math.MaxInt,
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

	stopped := make(chan struct{})
	if c.retiring == nil {
		c.retiring = make(map[tailnet.ID]chan struct{})
	}

	c.retiring[rt.id] = stopped
	c.mu.Unlock()

	log.Warn().Str("tailnet", rt.id.String()).Str("slug", rt.slug).Uint64("epoch", rt.epoch).
		Str("reason", reason).Msg("retiring tailnet runtime")

	// In the background: one slow request of this tailnet must not hold up
	// reconcile, and so every other tailnet of the cluster (P10 review B1).
	// New requests are already refused: rt is out of routing, and
	// stopRuntime marks it retired before anything else.
	rt.h.retired.Store(true)

	c.stopping.Add(1)

	go func() {
		defer c.stopping.Done()

		c.stopRuntime(rt)
		close(stopped)

		c.mu.Lock()
		if c.retiring[rt.id] == stopped {
			delete(c.retiring, rt.id)
		}
		c.mu.Unlock()
	}()
}

// handOverRequested gives away the tailnets this cluster owns with a move
// requested (ADR 0010): the runtime, if any, is retired; once it stopped
// serving (out of routing, sessions closed, map streams ended, requests
// drained or the drain timed out with the runtime refusing everything),
// the owner is flipped to the target. A failed flip is retried next tick.
func (c *Cluster) handOverRequested() {
	moving, err := c.platform.PendingHandovers()
	if err != nil {
		log.Error().Err(err).Msg("reconcile: listing hand-overs")

		return
	}

	for _, tn := range moving {
		id, err := tailnet.ParseID(tn.ID)
		if err != nil || tn.MovingToClusterID == nil {
			continue
		}

		to, epoch := *tn.MovingToClusterID, tn.OwnerEpoch

		c.mu.Lock()
		if c.handingOver[id] {
			c.mu.Unlock()

			continue
		}

		if c.handingOver == nil {
			c.handingOver = make(map[tailnet.ID]bool)
		}

		c.handingOver[id] = true
		rt := c.runtimes[id]
		c.mu.Unlock()

		if rt != nil {
			c.retire(rt, "move to cluster "+to+" requested")
		}

		c.mu.RLock()
		stopped := c.retiring[id]
		c.mu.RUnlock()

		c.stopping.Add(1)

		go func() {
			defer c.stopping.Done()

			if stopped != nil {
				<-stopped
			}

			c.handOver(id, epoch, to)
		}()
	}
}

func (c *Cluster) handOver(id tailnet.ID, epoch uint64, to string) {
	defer func() {
		c.mu.Lock()
		delete(c.handingOver, id)
		c.mu.Unlock()
	}()

	newOwner, err := c.platform.HandOver(id, epoch)
	if err != nil {
		log.Warn().Err(err).Str("tailnet", id.String()).Str("to", to).Uint64("epoch", epoch).
			Msg("tailnet hand-over not done; retried on the next reconcile")

		return
	}

	log.Info().Str("tailnet", id.String()).Str("to", newOwner).Uint64("owner_epoch", epoch+1).
		Msg("tailnet handed over: no longer served here")
}

// takeOverOrphans completes the moves to this cluster whose owner cluster
// has no live lease: no process of it serves them (ADR 0010 §4).
func (c *Cluster) takeOverOrphans() {
	orphans, err := c.platform.OrphanedIncoming()
	if err != nil {
		log.Error().Err(err).Msg("reconcile: listing orphaned moves")

		return
	}

	for _, tn := range orphans {
		id, err := tailnet.ParseID(tn.ID)
		if err != nil {
			continue
		}

		err = c.platform.TakeOver(id, tn.OwnerClusterID, tn.OwnerEpoch)
		if err != nil {
			log.Warn().Err(err).Str("tailnet", tn.ID).Str("from", tn.OwnerClusterID).Msg("tailnet take-over not done")

			continue
		}

		log.Warn().Str("tailnet", tn.ID).Str("from", tn.OwnerClusterID).Uint64("owner_epoch", tn.OwnerEpoch+1).
			Msg("tailnet taken over from a cluster with no live lease")
	}
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

	// Admitted Noise requests finish first: a handler must not run on a
	// closed State (P10 review B1). Map streams end because their sessions
	// were closed above.
	if !h.inflight.drain(runtimeDrainTimeout) {
		log.Error().Str("tailnet", rt.id.String()).Int("in_flight", h.inflight.inFlight()).
			Dur("timeout", runtimeDrainTimeout).
			Msg("retired tailnet runtime still has requests in flight; leaving its state open")

		return
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

		// Served here first (P10 review A4): a tailnet of another cluster or
		// not ACTIVE gets the generic refusal and costs no quota query.
		rt, err := c.runtimeFor(id, nil)
		if err != nil {
			return nil, err
		}

		if !c.knownNode(id, mk, req) {
			err = c.checkQuota(id)
			if err != nil {
				return nil, err
			}
		}

		return rt, nil
	}

	id, err := c.platform.TailnetForNode(mk, req.NodeKey)
	if errors.Is(err, db.ErrUnknownTenant) && !req.OldNodeKey.IsZero() {
		// Node key rotation: the node is still stored under its old key.
		id, err = c.platform.TailnetForNode(mk, req.OldNodeKey)
	}

	return c.runtimeFor(id, err)
}

// knownNode reports whether the registering device is already a node of
// tailnet id (re-login, key rotation): only new devices count against the
// quota. A node of another tailnet is a new device for id (P10 review A2: a
// node of A naming its A key as OldNodeKey must not skip B's quota).
func (c *Cluster) knownNode(id tailnet.ID, mk key.MachinePublic, req tailcfg.RegisterRequest) bool {
	if got, err := c.platform.TailnetForNode(mk, req.NodeKey); err == nil && got == id { //nolint:noinlineerr
		return true
	}

	if req.OldNodeKey.IsZero() {
		return false
	}

	got, err := c.platform.TailnetForNode(mk, req.OldNodeKey)

	return err == nil && got == id
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

	// P11b: a standby process (or one whose lease is ending) opens no
	// session, whatever routed the client here.
	if !c.Ready() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "standby", http.StatusServiceUnavailable)

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

// ReadyHandler (P11b) answers 200 only on the process that holds the cluster
// lease and has loaded its tailnets; standby answers 503. The front door (L4
// forwarder health check) sends clients to the ready process; the standby
// refuses /ts2021 anyway.
func (c *Cluster) ReadyHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/health+json")
	w.Header().Set("Cache-Control", "no-store")

	if l, ok := c.lease.Current(); ok && c.ready.Load() {
		_, _ = fmt.Fprintf(w, `{"status":"ready","lease_epoch":%d}`, l.Epoch)

		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"status":"standby"}`))
}

// leaseHeld is the lease gate of every runtime of this process.
func (c *Cluster) leaseHeld() bool {
	_, ok := c.lease.Current()

	return ok
}

// LeaseHolder is this process's name in cluster_leases.
func (c *Cluster) LeaseHolder() string { return c.lease.Holder() }

// Ready reports whether this process serves: it holds the cluster lease and
// has loaded its tailnets.
func (c *Cluster) Ready() bool { return c.ready.Load() && c.leaseHeld() }

// onLeaseAcquired loads and starts every ACTIVE tailnet the cluster owns,
// under the new lease. A tailnet that fails to load is logged and left out,
// as in reconcile; failing to list the tailnets gives the lease back.
func (c *Cluster) onLeaseAcquired(_ context.Context, l lease.Lease) error {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()

	// Runtimes of an earlier lease of this process must be gone first.
	c.waitStopping("taking the cluster lease")

	c.platform.SetLease(l.Holder, l.Epoch)

	// P12, Q6: the registry names this cluster's Noise key; a process
	// serving with another key must not serve (fail closed: the lease is
	// given back, the process stays standby and retries).
	err := c.platform.RecordNoiseKey(c.noiseKey.Public().String())
	if err != nil {
		c.platform.ClearLease()

		return fmt.Errorf("cluster Noise key: %w", err)
	}

	owned, err := c.platform.OwnedTailnets(c.platform.ClusterID())
	if err != nil {
		c.platform.ClearLease()

		return fmt.Errorf("listing owned tailnets: %w", err)
	}

	for _, tn := range owned {
		err := c.load(tn)
		if err != nil {
			log.Error().Err(err).Str("tailnet", tn.ID).Msg("cluster lease: loading tailnet")

			continue
		}

		id, _ := tailnet.ParseID(tn.ID)

		c.mu.RLock()
		rt := c.runtimes[id]
		c.mu.RUnlock()

		if err := c.start(rt); err != nil { //nolint:noinlineerr
			log.Error().Err(err).Str("tailnet", tn.ID).Msg("cluster lease: starting tailnet")
			c.retire(rt, "failed to start")
		}
	}

	c.ready.Store(true)

	c.mu.RLock()
	n := len(c.order)
	c.mu.RUnlock()

	log.Info().Str("cluster", l.Cluster).Str("holder", l.Holder).Uint64("lease_epoch", l.Epoch).
		Int("tailnets", n).Msg("cluster active: serving")

	return nil
}

// onLeaseLost stops serving: every runtime is retired (sessions closed, map
// streams ended) and no tenant handle is opened until the next lease. Writes
// still in flight roll back in the database (lease fence).
func (c *Cluster) onLeaseLost(l lease.Lease, reason string) {
	c.ready.Store(false)

	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()

	c.platform.ClearLease()

	for _, rt := range c.tenantSnapshot() {
		c.retire(rt, "cluster lease ended: "+reason)
	}

	log.Warn().Str("cluster", l.Cluster).Str("holder", l.Holder).Uint64("lease_epoch", l.Epoch).
		Str("reason", reason).Msg("cluster standby: not serving")

	c.waitStopping("losing the cluster lease")
}

// waitStopping waits, bounded, for retired runtimes to finish stopping.
func (c *Cluster) waitStopping(why string) {
	done := make(chan struct{})

	go func() {
		c.stopping.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(runtimeDrainTimeout + 5*time.Second):
		log.Error().Str("why", why).Msg("retired tailnet runtimes still stopping; going on")
	}
}

// leaseHolderName names this process in cluster_leases: host name plus a
// random suffix, new on every start, so a restarted process never mistakes
// an earlier incarnation's lease for its own.
func leaseHolderName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	var b [4]byte

	_, _ = rand.Read(b[:])

	return host + "-" + hex.EncodeToString(b[:])
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
	r.Get("/ready", c.ReadyHandler)
	r.Get("/version", c.front.VersionHandler)
	r.Get("/robots.txt", c.front.RobotsHandler)
	r.Head("/machine/ping-response", c.PingResponseHandler)

	// P08b: the tenant API, keyed by a platform-issued API key bound to
	// one tailnet. No /api/v1.
	r.Handle("/api/v2/*", http.HandlerFunc(c.APIv2Handler))

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

	// P08b: live operator administration, on the unix socket only.
	var adminSrv *http.Server

	if c.cfg.UnixSocket != "" {
		aln, err := listenAdminSocket(c.cfg)
		if err != nil {
			return err
		}

		adminSrv = &http.Server{Handler: c.AdminHandler(), ReadTimeout: types.HTTPTimeout}

		errorGroup.Go(serveHTTP(adminSrv, aln))
		log.Info().Msgf("cluster admin API (operators only) on unix socket %s", c.cfg.UnixSocket)
	}

	log.Info().
		Str("cluster", c.platform.ClusterID()).
		Str("holder", c.lease.Holder()).
		Msgf("cluster listening on %s (standby until it holds the cluster lease)", ln.Addr())

	// P11b: active while this process holds the cluster lease, standby
	// otherwise. On shutdown the lease is released so a standby takes over
	// without waiting for the TTL.
	errorGroup.Go(func() error {
		c.lease.Run(ctx, c.onLeaseAcquired, c.onLeaseLost)

		return nil
	})

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

		if adminSrv != nil {
			if err := adminSrv.Shutdown(shutdownCtx); err != nil { //nolint:noinlineerr
				log.Error().Err(err).Msg("shutting down cluster admin socket")
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

	// Runtimes retired earlier may still be stopping in the background.
	c.stopping.Wait()

	err := c.platform.Close()
	if err != nil {
		log.Error().Err(err).Msg("closing platform database")
	}
}

// BaseDomain is the MagicDNS domain of h's tailnet: <slug>.<cluster base>
// in cluster mode (ADR 0008), dns.base_domain otherwise.
func (h *Headscale) BaseDomain() string {
	return h.cfg.DNSConfig.BaseDomain
}
