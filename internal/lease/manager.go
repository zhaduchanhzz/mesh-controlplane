package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	Acquire(ctx context.Context, cluster, holder string, ttl time.Duration) (Lease, error)
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
	m.mu.Unlock()
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
// it calls onAcquire (load and start serving); if that fails the lease is
// released and the process stays standby. When the lease ends (lost,
// self-fence deadline, shutdown) it calls onLose after [Manager.Current]
// already reports false. On shutdown the lease is released.
func (m *Manager) Run(ctx context.Context, onAcquire func(context.Context, Lease) error, onLose func(Lease, string)) {
	for {
		l, err := m.acquire(ctx)
		if err != nil {
			return // ctx done
		}

		err = onAcquire(ctx, l)
		if err != nil {
			log.Error().Err(err).Str("cluster", m.cluster).Uint64("lease_epoch", l.Epoch).
				Msg("cluster lease taken but serving failed to start; releasing")
			m.clear()
			onLose(l, "start failed")
			m.release(l) //nolint:contextcheck // the lease is given back after ctx ended; release bounds itself by the safety margin

			if !sleepCtx(ctx, m.cfg.AcquireInterval) {
				return
			}

			continue
		}

		reason := m.hold(ctx, l)
		m.clear()
		log.Warn().Str("cluster", m.cluster).Str("holder", m.holder).Uint64("lease_epoch", l.Epoch).
			Str("reason", reason).Msg("cluster lease ended: standby")
		onLose(l, reason)

		if ctx.Err() != nil {
			m.release(l) //nolint:contextcheck // the lease is given back after ctx ended; release bounds itself by the safety margin

			return
		}
	}
}

// acquire tries every AcquireInterval until it holds the lease.
func (m *Manager) acquire(ctx context.Context) (Lease, error) {
	for {
		start := time.Now()

		actx, cancel := context.WithTimeout(ctx, m.cfg.TTL-m.cfg.SafetyMargin)
		l, err := m.store.Acquire(actx, m.cluster, m.holder, m.cfg.TTL)

		cancel()

		switch {
		case err == nil:
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
		case errors.Is(err, ErrHeld):
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

// hold renews l until it is lost, the self-fence deadline passes or ctx
// ends, and says why.
func (m *Manager) hold(ctx context.Context, l Lease) string {
	m.mu.Lock()
	deadline := m.deadline
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
			return "shutdown"
		case <-fence.C:
			return "self-fence deadline passed"
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
				return "lease lost (expired or taken over)"
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
