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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/frontdoor"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/dbstats"
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
	errClusterNoID = errors.New("cluster mode needs a cluster ID")
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

	// healthPing replaces the database ping of /health in tests.
	healthPing func(context.Context) error
	// health is the last database ping of /health (P12c L1).
	health healthCache

	// loadHook, when set (tests, SetLoadHook), runs between building a
	// tailnet's runtime and starting it. Under mu.
	loadHook func(tailnet.ID)

	// epochs is the highest owner_epoch this process has seen per tailnet
	// (P13c, ADR 0012 §3): loaded at, or set by its own hand-over and
	// take-over. A tailnet whose owner_epoch is below it (the database went
	// back: restore, lossy promote) is retired and not loaded again until
	// the epoch fence raised owner_epoch.
	epochs ownerEpochs
}

// ownerEpochs is the per-tailnet owner_epoch high-water mark of a cluster
// process (memory only).
type ownerEpochs struct {
	mu     sync.Mutex
	seen   map[tailnet.ID]uint64
	logged map[tailnet.ID]uint64 // db epoch of the last regression logged
}

// note raises the high-water mark of id to epoch.
func (e *ownerEpochs) note(id tailnet.ID, epoch uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.seen == nil {
		e.seen = make(map[tailnet.ID]uint64)
	}

	e.seen[id] = max(e.seen[id], epoch)
}

// behind reports whether owner_epoch epoch of id is below the high-water
// mark (returned), and whether this regression is new (log it once).
func (e *ownerEpochs) behind(id tailnet.ID, epoch uint64) (uint64, bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	hwm := e.seen[id]
	if epoch >= hwm {
		return hwm, false, false
	}

	if e.logged == nil {
		e.logged = make(map[tailnet.ID]uint64)
	}

	first := e.logged[id] != epoch
	e.logged[id] = epoch

	return hwm, true, first
}

// errOwnerEpochRegression: a tailnet's owner_epoch is below one this
// process served or set it at.
var errOwnerEpochRegression = errors.New("owner_epoch went backwards in the database (restore or lossy promote): " +
	"not serving the tailnet until the epoch fence runs")

// ownerEpochRegressed logs (once per database epoch) and reports a tailnet
// whose owner_epoch went below this process's high-water mark.
func (c *Cluster) ownerEpochRegressed(id tailnet.ID, epoch uint64) error {
	hwm, back, first := c.epochs.behind(id, epoch)
	if !back {
		return nil
	}

	if first {
		log.Error().Str("tailnet", id.String()).Uint64("owner_epoch", epoch).Uint64("owner_epoch_high_water_mark", hwm).
			Msg("CRITICAL epoch regression: owner_epoch is below what this process saw (restore or lossy promote?); " +
				"the tailnet is not served until the epoch fence runs")
	}

	return fmt.Errorf("%w: tailnet %s at owner_epoch %d, seen %d", errOwnerEpochRegression, id, epoch, hwm)
}

// runLoadHook runs the test hook of the load-to-start window (P12c B5).
func (c *Cluster) runLoadHook(id tailnet.ID) {
	c.mu.RLock()
	hook := c.loadHook
	c.mu.RUnlock()

	if hook != nil {
		hook(id)
	}
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

	// P13f L9: a revision is recorded in policyRev/configRev only once its
	// reload succeeded. Until then *Pending is set (read by the metrics
	// collector) and reconcile retries every tick; *FailedRev is the
	// revision whose failure was logged at error level, so a failing
	// revision is logged once, not every tick. Only reconcile writes them.
	policyPending   atomic.Bool
	configPending   atomic.Bool
	policyFailedRev uint64
	configFailedRev uint64

	cancel context.CancelFunc // stops its scheduled tasks
	stop   func()             // from startRuntime

	// api is the runtime's public v2 API (P08b), built on first use.
	api runtimeAPI
}

// NewCluster opens the platform database and builds a runtime for every
// ACTIVE tailnet owned by cfg.ClusterID. cfg is shared by the runtimes:
// per-tailnet DNS and IP settings come with the tailnet config revision
// (later ticket); policy, users, nodes and keys are per tailnet already.
//
// The cluster's Noise key file must exist (P13e, ADR 0014 §3): only
// [WithNoiseKeyInit] creates a missing one, and only for a cluster whose
// registry entry names no key yet.
func NewCluster(cfg *types.Config, opts ...ClusterOption) (*Cluster, error) {
	var o clusterOptions
	for _, opt := range opts {
		opt(&o)
	}

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

	keyPath := cfg.AbsPath(cfg.Noise.PrivateKeyPath)

	noiseKey, err := readClusterNoiseKey(keyPath, o.initNoiseKey)
	if err != nil {
		return nil, err
	}

	platform, err := db.OpenPlatform(cfg)
	if err != nil {
		return nil, fmt.Errorf("opening platform database: %w", err)
	}

	if noiseKey == nil {
		noiseKey, err = initClusterNoiseKey(platform, keyPath)
		if err != nil {
			_ = platform.Close()

			return nil, err
		}
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

	// P12c B2: a fenced transaction left open (process paused or cut off
	// before COMMIT) ends within half the self-fence deadline.
	tenantscope.SetTransactionBound(lc.TTL - lc.SafetyMargin)

	// ADR 0009: the secret of this cluster's external DERP servers (/verify).
	var verifySecret []byte
	if cfg.DERP.VerifySecretPath != "" {
		verifySecret, err = readVerifySecret(cfg.AbsPath(cfg.DERP.VerifySecretPath))
		if err != nil {
			_ = platform.Close()

			return nil, err
		}
	}

	c := &Cluster{
		host:        host,
		cfg:         cfg,
		platform:    platform,
		noiseKey:    noiseKey,
		registry:    registry,
		front:       &Headscale{cfg: cfg, noisePrivateKey: noiseKey, verifySecret: verifySecret},
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
		return errClusterNoID
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

// serveTailnet loads tn's runtime, starts it, and only then makes it
// routable (P12c B5): a request routed to a runtime that is built but not
// started (no map batcher yet) panicked, and a long poll left its node
// online. Until it is routable, its requests are refused like those of a
// tailnet this process does not serve. A runtime that fails to start is
// stopped, never routed. Callers hold leaseMu.
func (c *Cluster) serveTailnet(tn types.Tailnet, why string) {
	rt, err := c.load(tn)
	if err != nil {
		log.Error().Err(err).Str("tailnet", tn.ID).Msg(why + ": loading tailnet")

		return
	}

	c.runLoadHook(rt.id)

	err = c.start(rt)
	if err != nil {
		log.Error().Err(err).Str("tailnet", tn.ID).Msg(why + ": starting tailnet")

		rt.h.retired.Store(true)
		c.stopping.Go(func() { c.stopRuntime(rt) })

		return
	}

	c.mu.Lock()
	c.runtimes[rt.id] = rt
	c.order = append(c.order, rt.id)
	c.mu.Unlock()

	log.Info().Str("tailnet", rt.id.String()).Str("slug", rt.slug).Uint64("epoch", rt.epoch).Msg("serving tailnet runtime")
}

// load builds the runtime of tn: not started, not routable.
func (c *Cluster) load(tn types.Tailnet) (*tenantRuntime, error) {
	id, err := tailnet.ParseID(tn.ID)
	if err != nil {
		return nil, fmt.Errorf("tailnet %q: %w", tn.ID, err)
	}

	// TenantDB re-reads the row: owner, ACTIVE and the epoch the handle is
	// fenced on come from the same read.
	tdb, err := c.platform.TenantDB(id)
	if err != nil {
		return nil, fmt.Errorf("tailnet %s: %w", id, err)
	}

	// P13c: never at an owner_epoch below one this process saw (fail closed).
	err = c.ownerEpochRegressed(id, tdb.Tailnet().OwnerEpoch)
	if err != nil {
		_ = tdb.Close()

		return nil, err
	}

	c.epochs.note(id, tdb.Tailnet().OwnerEpoch)

	// The runtime's own config: the tailnet's DNS under <slug>.<base>, a
	// live DNS no other runtime shares (ADR 0008 §5). Stored settings that
	// do not parse or validate keep the tailnet out: never default DNS.
	dns, err := c.tailnetDNS(id, tdb.Tailnet().Slug)
	if err != nil {
		_ = tdb.Close()

		return nil, fmt.Errorf("tailnet %s: %w", id, err)
	}

	rcfg := c.cfg.ForTailnet(tdb.Tailnet().Slug, dns)

	h, err := newHeadscale(rcfg, c.noiseKey, "", func() (*state.State, error) {
		return state.NewStateWithDB(rcfg, tdb)
	})
	if err != nil {
		_ = tdb.Close()

		return nil, fmt.Errorf("tailnet %s runtime: %w", id, err)
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

	log.Info().Str("tailnet", id.String()).Str("slug", tn.Slug).Uint64("epoch", rt.epoch).Msg("loaded tailnet runtime")

	return rt, nil
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

// noiseKeyRotated re-checks, on every reconcile tick, the Noise key the
// registry names for this cluster (P13e, ADR 0014 §3, owner D8). Checked
// only when the lease was taken, a rotation in the registry (an emergency:
// the old key is considered leaked) left this process serving with the old
// key until its next takeover. On a mismatch it stops serving at once (not
// ready, no new tenant handle) and steps down: onLeaseLost retires every
// runtime, the lease is released for a process with the registered key,
// and this one stays standby. Any other error is logged only: writes are
// fenced in the database regardless. Callers hold leaseMu.
func (c *Cluster) noiseKeyRotated() bool {
	err := c.platform.RecordNoiseKey(c.noiseKey.Public().String()) //nolint:contextcheck // Platform bounds its own lookups (lookupContext)

	switch {
	case err == nil:
		return false
	case errors.Is(err, db.ErrNoiseKeyMismatch):
		log.Error().Err(err).Str("cluster", c.platform.ClusterID()).
			Msg("the registry names another Noise key for this cluster: stepping down")

		c.ready.Store(false)
		c.platform.ClearLease()
		c.lease.StepDown("the registry names another Noise key")

		return true
	default:
		log.Warn().Err(err).Msg("reconcile: checking the registered Noise key")

		return false
	}
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

	start := time.Now()
	defer func() { reconcileDuration.Observe(time.Since(start).Seconds()) }()

	if c.noiseKeyRotated() {
		return
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

	// P13f L9: a revision whose reload fails is not recorded as applied: the
	// runtime keeps its previous settings (never allow-all, never default
	// DNS), the failure is counted, and the next tick tries again, so a
	// transient failure (database read, a fix stored without a new
	// revision) does not leave the tailnet on stale settings until the next
	// revision. Each failing revision is logged once at error level.
	for _, rt := range configChanged {
		rev := want[rt.id.String()].ConfigRevision

		dns, err := c.tailnetDNS(rt.id, rt.slug)
		if err != nil {
			c.reloadFailed(rt, reloadDNS, rev, &rt.configFailedRev, &rt.configPending, err)

			continue
		}

		rt.configRev, rt.configFailedRev = rev, 0
		rt.configPending.Store(false)

		rt.h.cfg.ReplaceTenantDNS(rt.slug, dns)
		rt.h.Change(change.DNSConfig())
		log.Info().Str("tailnet", rt.id.String()).Uint64("config_revision", rev).Msg("reconcile: DNS settings reloaded")
	}

	for _, rt := range policyChanged {
		rev := want[rt.id.String()].PolicyRevision

		changes, err := rt.h.state.ReloadPolicy()
		if err != nil {
			c.reloadFailed(rt, reloadPolicy, rev, &rt.policyFailedRev, &rt.policyPending, err)

			continue
		}

		rt.policyRev, rt.policyFailedRev = rev, 0
		rt.policyPending.Store(false)

		rt.h.Change(changes...)
		log.Info().Str("tailnet", rt.id.String()).Uint64("policy_revision", rev).Msg("reconcile: policy reloaded")
	}

	for _, rt := range stale {
		reason := "not ACTIVE and owned by this cluster at the loaded epoch any more"
		if tn, ok := want[rt.id.String()]; ok && tn.OwnerEpoch < rt.epoch {
			reason = "owner_epoch went backwards (restore or lossy promote?)"
		}

		c.retire(rt, reason)
	}

	for _, tn := range owned {
		if loaded[tn.ID] && !containsRuntime(stale, tn.ID) {
			continue
		}

		// P13c: an owner_epoch below what this process saw is not loaded
		// (load refuses it too); checked here to skip the database reads.
		id, err := tailnet.ParseID(tn.ID)
		if err == nil && c.ownerEpochRegressed(id, tn.OwnerEpoch) != nil {
			continue
		}

		c.serveTailnet(tn, "reconcile")
	}

	c.applyMoveNotices()
}

// reloadFailed records a failed reload of kind (policy, dns) at revision
// rev of rt (P13f L9): counted, logged at error level once per revision,
// left pending so the next reconcile retries it.
func (c *Cluster) reloadFailed(rt *tenantRuntime, kind string, rev uint64, failedRev *uint64, pending *atomic.Bool, err error) {
	tailnetReloadFailures.WithLabelValues(tenantlabel.Label(rt.id.String()), kind).Inc()
	pending.Store(true)

	level := zerolog.DebugLevel

	if *failedRev != rev {
		*failedRev = rev
		level = zerolog.ErrorLevel
	}

	log.WithLevel(level).Err(err).Str("tailnet", rt.id.String()).Str("kind", kind).Uint64("revision", rev).
		Msg("reconcile: new tailnet settings rejected, keeping the previous ones; retrying every reconcile")
}

// applyMoveNotices gives each served tailnet the notice of its planned move,
// or clears it (P12b, owner decision Q1), and sends a full map to the
// tailnet's clients when it changed. Each runtime only sees its own
// tailnet's notice. A failed listing changes nothing.
func (c *Cluster) applyMoveNotices() {
	notices, err := c.platform.MoveNotices(c.platform.ClusterID())
	if err != nil {
		log.Error().Err(err).Msg("reconcile: listing planned moves")

		return
	}

	c.mu.RLock()

	rts := make([]*tenantRuntime, 0, len(c.runtimes))
	for _, rt := range c.runtimes {
		rts = append(rts, rt)
	}

	c.mu.RUnlock()

	for _, rt := range rts {
		var n *types.MoveNotice
		if v, ok := notices[rt.id.String()]; ok {
			n = &v
		}

		if !rt.h.state.SetMoveNotice(n) {
			continue
		}

		rt.h.Change(change.FullUpdate())

		if n != nil {
			log.Info().Str("tailnet", rt.id.String()).Time("move_at", n.At).Msg("reconcile: planned move notice set")
		} else {
			log.Info().Str("tailnet", rt.id.String()).Msg("reconcile: planned move notice cleared")
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
// in the database once ownership changed. c.retiring[rt.id] is closed once
// every runtime of the tailnet retired so far has stopped (P12c C1: a
// runtime loaded again and retired while an earlier one still drains).
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

	prev := c.retiring[rt.id]
	c.retiring[rt.id] = stopped
	c.mu.Unlock()

	log.Warn().Str("tailnet", rt.id.String()).Str("slug", rt.slug).Uint64("epoch", rt.epoch).
		Str("reason", reason).Msg("retiring tailnet runtime")

	// In the background: one slow request of this tailnet must not hold up
	// reconcile, and so every other tailnet of the cluster (P10 review B1).
	// New requests are already refused: rt is out of routing, and
	// stopRuntime marks it retired before anything else.
	rt.h.retired.Store(true)

	c.stopping.Go(func() {
		c.stopRuntime(rt)

		if prev != nil {
			<-prev // bounded by that runtime's drain timeout
		}

		close(stopped)

		c.mu.Lock()
		if c.retiring[rt.id] == stopped {
			delete(c.retiring, rt.id)
		}
		c.mu.Unlock()
	})
}

// handOverRequested gives away the tailnets this cluster owns with a move
// requested (ADR 0010): the runtime, if any, is retired; once it stopped
// serving (out of routing, sessions closed, map streams ended, requests
// drained or the drain timed out with the runtime refusing everything),
// the owner is flipped to the target. A failed flip is retried next tick.
//
// P12c C1: the flip runs only under leaseMu (reconcile holds it here), which
// every load of a runtime also holds, and only while the tailnet has no
// runtime in routing and none still stopping. A move cancelled and requested
// again at the same epoch (a cancel keeps the epoch) while an earlier drain
// was under way therefore never flips over a runtime loaded in between: that
// runtime is retired here first, like any served one.
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

		c.mu.RLock()
		rt := c.runtimes[id]
		c.mu.RUnlock()

		if rt != nil {
			c.retire(rt, "move to cluster "+to+" requested")
		}

		if c.awaitStopped(id, epoch, to) {
			continue
		}

		c.handOver(id, epoch, to)
	}
}

// awaitStopped reports whether a retired runtime of tailnet id is still
// stopping. If so, one waiter (per tailnet) tries the hand-over as soon as
// it stopped; reconcile retries it otherwise.
func (c *Cluster) awaitStopped(id tailnet.ID, epoch uint64, to string) bool {
	c.mu.Lock()

	stopped := c.retiring[id]
	if stopped == nil || isClosed(stopped) {
		c.mu.Unlock()

		return false
	}

	if c.handingOver[id] {
		c.mu.Unlock()

		return true
	}

	if c.handingOver == nil {
		c.handingOver = make(map[tailnet.ID]bool)
	}

	c.handingOver[id] = true
	c.mu.Unlock()

	c.stopping.Go(func() {
		<-stopped

		c.mu.Lock()
		delete(c.handingOver, id)
		c.mu.Unlock()

		c.tryHandOver(id, epoch, to)
	})

	return true
}

// tryHandOver flips right after a drain ended. It polls leaseMu with
// TryLock for at most one reconcile interval (the reconcile that retired the
// runtime usually still holds it) and gives up as soon as the process stops
// serving: a lease change or shutdown waits for c.stopping under leaseMu, so
// this goroutine must never block on it. A give-up leaves the flip to the
// next reconcile.
func (c *Cluster) tryHandOver(id tailnet.ID, epoch uint64, to string) {
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()

	giveUp := time.NewTimer(reconcileInterval)
	defer giveUp.Stop()

	for !c.leaseMu.TryLock() {
		if !c.ready.Load() {
			return
		}

		select {
		case <-poll.C:
		case <-giveUp.C:
			return
		}
	}
	defer c.leaseMu.Unlock()

	if !c.ready.Load() || !c.leaseHeld() {
		return
	}

	c.handOver(id, epoch, to)
}

// handOver flips the owner of tailnet id to the target of its requested
// move (db.Platform.HandOver). The caller holds leaseMu, so no runtime of
// id is loaded meanwhile; it does nothing while id has a runtime in routing
// or one still stopping (that one is handed over once it stopped).
func (c *Cluster) handOver(id tailnet.ID, epoch uint64, to string) {
	c.mu.RLock()
	_, serving := c.runtimes[id]
	stopped := c.retiring[id]
	c.mu.RUnlock()

	if serving || (stopped != nil && !isClosed(stopped)) {
		return
	}

	newOwner, err := c.platform.HandOver(id, epoch)
	if err != nil {
		log.Warn().Err(err).Str("tailnet", id.String()).Str("to", to).Uint64("epoch", epoch).
			Msg("tailnet hand-over not done; retried on the next reconcile")

		return
	}

	// This process set epoch+1: it never serves the tailnet below it again.
	c.epochs.note(id, epoch+1)

	log.Info().Str("tailnet", id.String()).Str("to", newOwner).Uint64("owner_epoch", epoch+1).
		Msg("tailnet handed over: no longer served here")
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
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

		c.epochs.note(id, tn.OwnerEpoch+1)

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

// healthCacheTTL is how long /health reuses a database ping (P12c L1).
const healthCacheTTL = time.Second

// healthPingTimeout bounds one database ping of /health.
const healthPingTimeout = 3 * time.Second

// healthCache is the last database ping of /health. Requests within
// healthCacheTTL of it reuse its result; a request that finds it stale
// pings, and concurrent ones wait for that ping instead of sending theirs.
type healthCache struct {
	mu  sync.Mutex
	at  time.Time
	err error
}

func (hc *healthCache) check(ctx context.Context, ping func(context.Context) error) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if !hc.at.IsZero() && time.Since(hc.at) < healthCacheTTL {
		return hc.err
	}

	// The ping outlives a client that hangs up: its result serves the next
	// requests.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthPingTimeout)
	defer cancel()

	hc.err = ping(pctx)
	hc.at = time.Now()

	return hc.err
}

// HealthHandler reports whether the shared database answers. The listener
// is public and unauthenticated, so the database is pinged at most once per
// healthCacheTTL however many requests arrive (P12c L1).
func (c *Cluster) HealthHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/health+json")

	ping := c.healthPing
	if ping == nil {
		ping = c.platform.Ping
	}

	err := c.health.check(req.Context(), ping)
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
// refuses /ts2021 anyway. The body names no lease epoch (P12c L2): the
// listener is public, and the epoch tells how often and when the cluster
// failed over; operators read it from cluster_leases or the logs.
func (c *Cluster) ReadyHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/health+json")
	w.Header().Set("Cache-Control", "no-store")

	if c.Ready() {
		_, _ = w.Write([]byte(`{"status":"ready"}`))

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
// as in reconcile; failing to list the tailnets gives the lease back. The
// lease is renewed meanwhile (P12c B3), however long this takes; ctx ends
// with the lease, and loading stops there.
func (c *Cluster) onLeaseAcquired(ctx context.Context, l lease.Lease) error {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()

	// Runtimes of an earlier lease of this process must be gone first.
	// onLeaseLost already waited (bounded) before the lease was taken
	// again; this only matters when that wait timed out.
	c.waitStopping("taking the cluster lease")

	c.platform.SetLease(l.Holder, l.Epoch)

	// P12, Q6: the registry names this cluster's Noise key; a process
	// serving with another key must not serve (fail closed: the lease is
	// given back, the process stays standby and retries).
	err := c.platform.RecordNoiseKey(c.noiseKey.Public().String()) //nolint:contextcheck // Platform bounds its own lookups (lookupContext)
	if err != nil {
		c.platform.ClearLease()

		return fmt.Errorf("cluster Noise key: %w", err)
	}

	owned, err := c.platform.OwnedTailnets(c.platform.ClusterID()) //nolint:contextcheck // Platform bounds its own lookups (lookupContext)
	if err != nil {
		c.platform.ClearLease()

		return fmt.Errorf("listing owned tailnets: %w", err)
	}

	for _, tn := range owned {
		if ctx.Err() != nil {
			log.Warn().Str("cluster", l.Cluster).Uint64("lease_epoch", l.Epoch).
				Msg("cluster lease ended while loading tailnets; stopping")

			return nil // onLeaseLost follows and retires what was loaded
		}

		c.serveTailnet(tn, "cluster lease") //nolint:contextcheck // Platform bounds its own lookups (lookupContext); runtimes live on the cluster's serve context, not this callback's
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

	// ADR 0009: client verification for this cluster's external DERP
	// servers (derp.verify_secret_path).
	r.Post("/verify", c.VerifyHandler)

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

	defer c.shutdownRuntimes() //nolint:contextcheck // shutdown runs after ctx is done and bounds itself

	// P13f: runtime counts and the shared pool's statistics on the
	// operator metrics listener while this process serves.
	defer registerCluster(c)()

	if sqlDB, err := c.platform.RegistryDB().DB(); err == nil { //nolint:noinlineerr
		defer dbstats.Register("platform", sqlDB)()
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

	// P08b: live operator administration, on the unix socket only.
	var adminSrv *http.Server

	if c.cfg.UnixSocket != "" {
		aln, err := listenAdminSocket(ctx, c.cfg)
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
				c.reconcile() //nolint:contextcheck // Platform bounds its own lookups (lookupContext)
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
