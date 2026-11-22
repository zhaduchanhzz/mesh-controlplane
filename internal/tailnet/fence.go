package tailnet

import (
	"context"
	"errors"
	"fmt"
)

// ErrInvalidFence reports a Fence that does not name a cluster.
var ErrInvalidFence = errors.New("invalid ownership fence")

// Fence is the ownership a writer claims over its tailnet: the cluster it
// runs in and the owner_epoch it loaded the tailnet at (I12). Every write to
// tenant data is checked against the tailnets row inside the same
// transaction; a different owner or epoch rolls the write back. Placeholder
// for the cluster lease epoch: P11b.
type Fence struct {
	cluster string
	epoch   Epoch
}

// NewFence builds the fence of a runtime of cluster that loaded its tailnet
// at epoch.
func NewFence(cluster string, epoch Epoch) (Fence, error) {
	if cluster == "" {
		return Fence{}, fmt.Errorf("%w: missing cluster", ErrInvalidFence)
	}

	if epoch == 0 {
		return Fence{}, fmt.Errorf("%w: epoch must be at least 1", ErrInvalidFence)
	}

	return Fence{cluster: cluster, epoch: epoch}, nil
}

// Cluster is the cluster that claims ownership.
func (f Fence) Cluster() string { return f.cluster }

// Epoch is the owner_epoch the claim was made at.
func (f Fence) Epoch() Epoch { return f.epoch }

// Valid reports whether f was built by NewFence.
func (f Fence) Valid() bool { return f.cluster != "" && f.epoch != 0 }

type fenceKey struct{}

// WithFence returns ctx carrying f. An invalid fence is refused.
func WithFence(ctx context.Context, f Fence) (context.Context, error) {
	if !f.Valid() {
		return ctx, ErrInvalidFence
	}

	return context.WithValue(ctx, fenceKey{}, f), nil
}

// FenceFromContext returns the fence of ctx, if any.
func FenceFromContext(ctx context.Context) (Fence, bool) {
	if ctx == nil {
		return Fence{}, false
	}

	f, ok := ctx.Value(fenceKey{}).(Fence)

	return f, ok && f.Valid()
}
