package lease

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrEpochRegression reports that the database would grant this process a
// lease epoch not above one it has already seen (ADR 0012 §3): the database
// went back in time (restore, promote that lost commits). The process does
// not take the lease until an epoch fence raised it.
var ErrEpochRegression = errors.New("cluster lease epoch went backwards in the database (restore or lossy promote): " +
	"not taking the lease until the epoch fence runs")

// HeldError is ErrHeld with the live lease's holder and epoch.
type HeldError struct {
	Holder string
	Epoch  uint64
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s (holder %s, epoch %d)", ErrHeld, e.Holder, e.Epoch)
}

// Is makes errors.Is(err, ErrHeld) hold.
func (e *HeldError) Is(target error) bool { return target == ErrHeld }

// AcquireAbove is Acquire that never grants an epoch at or below floor.
// RED stub (P13c): floor is ignored.
func (s *Store) AcquireAbove(ctx context.Context, cluster, holder string, ttl time.Duration, _ uint64) (Lease, error) {
	return s.Acquire(ctx, cluster, holder, ttl)
}

// HighWaterMark is the highest lease epoch this manager has seen. RED stub.
func (m *Manager) HighWaterMark() uint64 { return 0 }

// Regressions counts the acquire attempts refused with ErrEpochRegression.
// RED stub.
func (m *Manager) Regressions() int64 { return 0 }
