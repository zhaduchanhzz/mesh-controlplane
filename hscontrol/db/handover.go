package db

import (
	"errors"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// ADR 0010: a move to another cluster is requested (tailnets.
// moving_to_cluster_id, set by placement.Registry.MoveOwner) and completed
// by the cluster: the owner stops serving the tailnet, then flips the owner
// (HandOver); if the owner cluster has no live lease, the target flips it
// (TakeOver). The target loads the tailnet only once it owns it, so two
// clusters never hold a runtime of one tailnet at the same time.

// ErrHandoverStale reports a hand-over or take-over whose conditions no
// longer hold (no move requested, another owner or epoch, this process's
// lease not live, or the owner cluster still alive): nothing changed.
var ErrHandoverStale = errors.New("tailnet hand-over: move no longer pending at this owner and epoch, or lease not live")

// PendingHandovers lists the tailnets this cluster owns with a move to
// another cluster requested, in any status.
func (p *Platform) PendingHandovers() ([]types.Tailnet, error) {
	return nil, nil
}

// OrphanedIncoming lists the tailnets with a move to this cluster requested
// whose owner cluster has no live lease: nothing serves them.
func (p *Platform) OrphanedIncoming() ([]types.Tailnet, error) {
	return nil, nil
}

// HandOver completes the move of tailnet id, owned by this cluster at
// epoch: owner = moving_to, owner_epoch + 1, moving_to cleared. Only while
// this process holds the cluster lease it recorded with SetLease. It
// returns the new owner.
func (p *Platform) HandOver(id tailnet.ID, epoch uint64) (string, error) {
	return "", ErrHandoverStale
}

// TakeOver completes the move of tailnet id from cluster from at epoch to
// this cluster, when from has no live lease. Only while this process holds
// this cluster's lease.
func (p *Platform) TakeOver(id tailnet.ID, from string, epoch uint64) error {
	return ErrHandoverStale
}
