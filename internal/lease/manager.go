package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

var (
	errInvalidConfig = errors.New("invalid lease config")
	errNoIdentity    = errors.New("lease manager needs a cluster and a holder")
)

// Config times the lease. Defaults: [DefaultConfig].
type Config struct {
	// TTL is how long a lease lives in the database after each acquire or
	// renew, by the database clock.
	TTL time.Duration
	// RenewInterval is how often the active process renews.
	RenewInterval time.Duration
	// AcquireInterval is how often a standby tries to take the lease.
	AcquireInterval time.Duration
	// SafetyMargin is how long before the TTL the active process stops
	// serving on its own (monotonic clock).
	SafetyMargin time.Duration
}

// DefaultConfig: failover within about TTL + AcquireInterval; one missed
// renewal does not stop the active process.
var DefaultConfig = Config{
	TTL:             10 * time.Second,
	RenewInterval:   3 * time.Second,
	AcquireInterval: time.Second,
	SafetyMargin:    2 * time.Second,
}

// Validate checks that the active process renews, and stops serving, well
// before its lease can expire.
func (c Config) Validate() error {
	switch {
	case c.TTL < time.Second:
		return fmt.Errorf("%w: ttl %s is below 1s", errInvalidConfig, c.TTL)
	case c.SafetyMargin <= 0 || c.SafetyMargin*2 > c.TTL:
		return fmt.Errorf("%w: safety margin %s must be positive and at most half the ttl %s",
			errInvalidConfig, c.SafetyMargin, c.TTL)
	case c.RenewInterval <= 0 || c.RenewInterval >= c.TTL-c.SafetyMargin:
		return fmt.Errorf("%w: renew interval %s must be positive and below ttl - safety margin (%s)",
			errInvalidConfig, c.RenewInterval, c.TTL-c.SafetyMargin)
	case c.AcquireInterval <= 0:
		return fmt.Errorf("%w: acquire interval %s must be positive", errInvalidConfig, c.AcquireInterval)
	}

	return nil
}

// store is what the manager needs from [Store]; tests replace it.
type store interface {
	AcquireAbove(ctx context.Context, cluster, holder string, ttl time.Duration, floor uint64) (Lease, error)
	Renew(ctx context.Context, l Lease, ttl time.Duration) error
	Release(ctx context.Context, l Lease) error
}

// Manager keeps this process either standby (no lease) or active (holding
// the lease of its cluster). It is the only writer of its state.
type Manager struct {
	store   store
	cluster string
	holder  string
	cfg     Config

	mu       sync.Mutex
	cur      Lease
	active   bool
	deadline time.Time // monotonic: time.Now() values only
	// hwm is the highest epoch of this cluster's lease the process has seen
	// (held, or live under another holder); never lowered (ADR 0012 §3). It
	// is the floor of every acquire: an epoch at or below it is never
	// accepted (the database went back: restore, lossy promote).
	hwm uint64

	// regressions counts acquire attempts refused with ErrEpochRegression.
	regressions atomic.Int64

	// down asks the hold of the current lease to end ([Manager.StepDown]);
	// a new channel for every lease, so a request never outlives it.
	down chan string
}

// NewManager builds the manager of holder for cluster.
func NewManager(s *Store, cluster, holder string, cfg Config) (*Manager, error) {
	return newManager(s, cluster, holder, cfg)
}

func newManager(s store, cluster, holder string, cfg Config) (*Manager, error) {
	if cluster == "" || holder == "" {
		return nil, errNoIdentity
	}

	err := cfg.Validate()
	if err != nil {
		return nil, err
	}

	return &Manager{store: s, cluster: cluster, holder: holder, cfg: cfg}, nil
}

// Holder is this process's holder name.
func (m *Manager) Holder() string { return m.holder }

// HighWaterMark is the highest lease epoch this process has seen for its
// cluster (in memory: it does not survive a restart).
func (m *Manager) HighWaterMark() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.hwm
}

// Regressions counts the acquire attempts refused because the database
// would have granted an epoch at or below [Manager.HighWaterMark].
func (m *Manager) Regressions() int64 { return m.regressions.Load() }

func (m *Manager) observe(epoch uint64) {
	m.mu.Lock()
	m.hwm = max(m.hwm, epoch)
	m.mu.Unlock()
}

// Current returns the lease this process may serve under: false when
// standby, and false as soon as the self-fence deadline has passed, even if
// the loop that would notice it has not run yet (paused process).
func (m *Manager) Current() (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.active || !time.Now().Before(m.deadline) {
		return Lease{}, false
	}

	return m.cur, true
}

func (m *Manager) set(l Lease, deadline time.Time) {
	m.mu.Lock()
	m.cur, m.active, m.deadline = l, true, deadline
	m.down = make(chan string, 1)
	m.mu.Unlock()
}

// StepDown ends the lease this process holds now, from outside the loop
// (P13e, ADR 0014 §3: the registry names another Noise key): Run reports
// it lost (onLose), releases it at once so that a standby takes over
// without waiting for the TTL, and waits a whole TTL before trying to take
// it again, so that a standby that may serve gets it first. It reports
// whether a lease was held; it never blocks.
func (m *Manager) StepDown(reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.active || m.down == nil {
		return false
	}

	select {
	case m.down <- reason:
	default: // already asked
	}

	return true
}

func (m *Manager) extend(deadline time.Time) {
	m.mu.Lock()
	m.deadline = deadline
	m.mu.Unlock()
}

func (m *Manager) clear() {
	m.mu.Lock()
	m.active = false
	m.mu.Unlock()
}

func (m *Manager) deadlineFrom(start time.Time) time.Time {
	return start.Add(m.cfg.TTL - m.cfg.SafetyMargin)
}

// Run alternates standby and active until ctx ends. When the lease is taken
// it starts renewing it at once and, meanwhile, calls onAcquire (load and
// start serving) with a context that ends with the lease; if onAcquire fails
// the lease is released and the process stays standby. When the lease ends
// (lost, self-fence deadline, shutdown, [Manager.StepDown]) [Manager.Current]
// reports false at once, and onLose is called after onAcquire returned. On
// shutdown and on a step-down the lease is released.
//
// Renewing during onAcquire (P12c B3): the self-fence deadline runs from the
// acquire, so a load longer than TTL - SafetyMargin used to self-fence the
// lease it had just taken, every time.
func (m *Manager) Run(ctx context.Context, onAcquire func(context.Context, Lease) error, onLose func(Lease, string)) {
	for {
		l, err := m.acquire(ctx)
		if err != nil {
			return // ctx done
		}

		hctx, stopHold := context.WithCancel(ctx)
		ended := make(chan holdEnd, 1)

		go func() {
			e := m.hold(hctx, l)
			m.clear()

			ended <- e
		}()

		err = onAcquire(hctx, l)
		if err != nil {
			stopHold()
			<-ended

			log.Error().Err(err).Str("cluster", m.cluster).Uint64("lease_epoch", l.Epoch).
				Msg("cluster lease taken but serving failed to start; releasing")
			onLose(l, "start failed")
			m.release(l) //nolint:contextcheck // the lease is given back after ctx ended; release bounds itself by the safety margin

			if !sleepCtx(ctx, m.cfg.AcquireInterval) {
				return
			}

			continue
		}

		end := <-ended

		stopHold()
		log.Warn().Str("cluster", m.cluster).Str("holder", m.holder).Uint64("lease_epoch", l.Epoch).
			Str("reason", end.reason).Msg("cluster lease ended: standby")
		onLose(l, end.reason)

		if ctx.Err() != nil {
			m.release(l) //nolint:contextcheck // the lease is given back after ctx ended; release bounds itself by the safety margin

			return
		}

		if end.steppedDown {
			// Given back at once, then a whole TTL before trying again: the
			// process that should serve takes it first.
			m.release(l) //nolint:contextcheck // release bounds itself by the safety margin

			if !sleepCtx(ctx, m.cfg.TTL) {
				return
			}
		}
	}
}

// holdEnd is why a hold ended; steppedDown when [Manager.StepDown] asked.
type holdEnd struct {
	reason      string
	steppedDown bool
}

// acquire tries every AcquireInterval until it holds the lease.
func (m *Manager) acquire(ctx context.Context) (Lease, error) {
	for {
		start := time.Now()

		actx, cancel := context.WithTimeout(ctx, m.cfg.TTL-m.cfg.SafetyMargin)
		l, err := m.store.AcquireAbove(actx, m.cluster, m.holder, m.cfg.TTL, m.HighWaterMark())

		cancel()

		var held *HeldError

		switch {
		case err == nil:
			m.observe(l.Epoch)

			deadline := m.deadlineFrom(start)
			if time.Now().Before(deadline) {
				m.set(l, deadline)
				log.Info().Str("cluster", m.cluster).Str("holder", m.holder).Uint64("lease_epoch", l.Epoch).
					Msg("cluster lease acquired: active")

				return l, nil
			}
			// The acquire itself took too long to be safe: let it expire.
			log.Warn().Str("cluster", m.cluster).Uint64("lease_epoch", l.Epoch).
				Msg("cluster lease acquired too late to serve; letting it expire")
		case errors.As(err, &held):
			m.observe(held.Epoch)
		case errors.Is(err, ErrHeld):
		case errors.Is(err, ErrEpochRegression):
			// Fail closed: standby until the epoch fence raised the
			// database above what this process saw. Logged on the first
			// refusal and then every 30.
			if n := m.regressions.Add(1); n%30 == 1 {
				log.Error().Err(err).Str("cluster", m.cluster).Uint64("epoch_high_water_mark", m.HighWaterMark()).
					Int64("refusals", n).Msg("CRITICAL epoch regression: the database is behind what this process saw " +
					"(restore or lossy promote?); not taking the cluster lease until the epoch fence runs")
			}
		default:
			if ctx.Err() == nil {
				log.Error().Err(err).Str("cluster", m.cluster).Msg("cluster lease: acquire failed")
			}
		}

		if !sleepCtx(ctx, m.cfg.AcquireInterval) {
			return Lease{}, ctx.Err()
		}
	}
}

// hold renews l until it is lost, the self-fence deadline passes, ctx
// ends or StepDown asks, and says why.
func (m *Manager) hold(ctx context.Context, l Lease) holdEnd {
	m.mu.Lock()
	deadline, down := m.deadline, m.down
	m.mu.Unlock()

	fence := time.NewTimer(time.Until(deadline))
	defer fence.Stop()

	renew := time.NewTicker(m.cfg.RenewInterval)
	defer renew.Stop()

	type result struct {
		start time.Time
		err   error
	}

	done := make(chan result, 1)
	inFlight := false

	for {
		select {
		case <-ctx.Done():
			return holdEnd{reason: "shutdown"}
		case <-fence.C:
			return holdEnd{reason: "self-fence deadline passed"}
		case why := <-down:
			return holdEnd{reason: "stepped down: " + why, steppedDown: true}
		case <-renew.C:
			if inFlight {
				continue // a renewal stuck on the database: the deadline decides
			}

			inFlight = true
			start := time.Now()
			rctx, cancel := context.WithDeadline(ctx, deadline)

			go func() {
				defer cancel()

				done <- result{start: start, err: m.store.Renew(rctx, l, m.cfg.TTL)}
			}()
		case r := <-done:
			inFlight = false

			switch {
			case r.err == nil:
				deadline = m.deadlineFrom(r.start)
				m.extend(deadline)

				if !fence.Stop() {
					select {
					case <-fence.C:
					default:
					}
				}

				fence.Reset(time.Until(deadline))
			case errors.Is(r.err, ErrLost):
				return holdEnd{reason: "lease lost (expired or taken over)"}
			default:
				if ctx.Err() == nil {
					log.Error().Err(r.err).Str("cluster", m.cluster).Uint64("lease_epoch", l.Epoch).
						Msg("cluster lease: renew failed; serving until the self-fence deadline")
				}
			}
		}
	}
}

func (m *Manager) release(l Lease) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.SafetyMargin)
	defer cancel()

	err := m.store.Release(ctx, l)
	if err != nil {
		log.Warn().Err(err).Str("cluster", m.cluster).Msg("cluster lease: release failed; it expires by itself")
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
