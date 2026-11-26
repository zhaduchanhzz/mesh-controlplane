package lease

import (
	"errors"
	"fmt"
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
