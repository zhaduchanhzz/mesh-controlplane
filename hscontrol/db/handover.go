package db

import (
	"errors"
	"fmt"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
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

// dbNow is the database clock of p's dialect (as cluster_leases uses).
func (p *Platform) dbNow() string {
	if p.db.Name() == dialectPostgres {
		return lease.PostgresNow
	}

	return lease.SQLiteNow
}

// PendingHandovers lists the tailnets this cluster owns with a move to
// another cluster requested, in any status.
func (p *Platform) PendingHandovers() ([]types.Tailnet, error) {
	ctx, cancel := p.lookupContext()
	defer cancel()

	var tns []types.Tailnet

	err := p.db.WithContext(ctx).
		Where("owner_cluster_id = ? AND moving_to_cluster_id IS NOT NULL AND deleted_at IS NULL", p.cluster).
		Order("id").Find(&tns).Error
	if err != nil {
		return nil, fmt.Errorf("listing hand-overs of cluster %s: %w", p.cluster, err)
	}

	return tns, nil
}

// OrphanedIncoming lists the tailnets with a move to this cluster requested
// whose owner cluster has no live lease: nothing serves them.
func (p *Platform) OrphanedIncoming() ([]types.Tailnet, error) {
	ctx, cancel := p.lookupContext()
	defer cancel()

	var tns []types.Tailnet

	err := p.db.WithContext(ctx).
		Where(`moving_to_cluster_id = ? AND deleted_at IS NULL AND NOT EXISTS (
			SELECT 1 FROM cluster_leases l WHERE l.cluster_id = tailnets.owner_cluster_id AND l.expires_at > `+p.dbNow()+`)`,
			p.cluster).
		Order("id").Find(&tns).Error
	if err != nil {
		return nil, fmt.Errorf("listing orphaned moves to cluster %s: %w", p.cluster, err)
	}

	return tns, nil
}

// HandOver completes the move of tailnet id, owned by this cluster at
// epoch: owner = moving_to, owner_epoch + 1, moving_to cleared. Only while
// this process holds the cluster lease it recorded with SetLease. It
// returns the new owner. The caller must have stopped serving the tailnet.
// The target must not be a dedicated cluster that already owns another live
// tailnet (P12c C3: re-checked here, whatever admitted the request).
func (p *Platform) HandOver(id tailnet.ID, epoch uint64) (string, error) {
	var to string

	err := p.flip(id, func(tx *gorm.DB, now time.Time, holder string, leaseEpoch uint64) (int64, error) {
		var targets []string

		err := tx.Raw(`SELECT moving_to_cluster_id FROM tailnets
			WHERE id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND moving_to_cluster_id IS NOT NULL AND deleted_at IS NULL`,
			id.String(), p.cluster, int64(epoch)).Scan(&targets).Error //nolint:gosec // see below
		if err != nil || len(targets) != 1 {
			return 0, err
		}

		to = targets[0]

		err = p.lockTargetCluster(tx, to)
		if err != nil {
			return 0, err
		}

		res := tx.Exec(`UPDATE tailnets SET owner_cluster_id = moving_to_cluster_id, owner_epoch = owner_epoch + 1,
				moving_to_cluster_id = NULL, updated_at = ?
			WHERE id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND moving_to_cluster_id = ? AND deleted_at IS NULL
			  AND EXISTS (SELECT 1 FROM cluster_leases l
			    WHERE l.cluster_id = ? AND l.holder = ? AND l.epoch = ? AND l.expires_at > `+p.dbNow()+`)`+dedicatedFree,
			now, id.String(), p.cluster, int64(epoch), to, p.cluster, holder, int64(leaseEpoch), //nolint:gosec // epochs are positive int64 in the database
			to, id.String())
		if res.Error != nil || res.RowsAffected != 1 {
			return res.RowsAffected, res.Error
		}

		return 1, recordHandedOver(tx, id, p.cluster, epoch, to, now)
	})
	if err != nil {
		return "", err
	}

	return to, nil
}

// TakeOver completes the move of tailnet id from cluster from at epoch to
// this cluster, when from has no live lease (no process of it serves the
// tailnet). Only while this process holds this cluster's lease, and, as
// HandOver, not into a dedicated cluster that owns another live tailnet.
func (p *Platform) TakeOver(id tailnet.ID, from string, epoch uint64) error {
	return p.flip(id, func(tx *gorm.DB, now time.Time, holder string, leaseEpoch uint64) (int64, error) {
		err := p.lockTargetCluster(tx, p.cluster)
		if err != nil {
			return 0, err
		}

		dbNow := p.dbNow()
		res := tx.Exec(`UPDATE tailnets SET owner_cluster_id = moving_to_cluster_id, owner_epoch = owner_epoch + 1,
				moving_to_cluster_id = NULL, updated_at = ?
			WHERE id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND moving_to_cluster_id = ? AND deleted_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM cluster_leases o WHERE o.cluster_id = ? AND o.expires_at > `+dbNow+`)
			  AND EXISTS (SELECT 1 FROM cluster_leases l
			    WHERE l.cluster_id = ? AND l.holder = ? AND l.epoch = ? AND l.expires_at > `+dbNow+`)`+dedicatedFree,
			now, id.String(), from, int64(epoch), p.cluster, from, p.cluster, holder, int64(leaseEpoch), //nolint:gosec // see HandOver
			p.cluster, id.String())
		if res.Error != nil || res.RowsAffected != 1 {
			return res.RowsAffected, res.Error
		}

		return 1, recordHandedOver(tx, id, from, epoch, p.cluster, now)
	})
}

// recordHandedOver closes, in the flip's transaction, the planned move
// (P12b tailnet_moves) whose request this flip completes: REQUESTED with the
// same source, epoch and target becomes HANDED_OVER at the flip's time
// (P12c L7: the schedule records what happened, not what the scheduler later
// infers from the tailnet row). A move made without a schedule has no row.
// Explicit SQL: tailnet_moves is a platform table, not a tenant scope.
func recordHandedOver(tx *gorm.DB, id tailnet.ID, from string, epoch uint64, to string, now time.Time) error {
	err := tx.Exec(`UPDATE tailnet_moves SET state = 'HANDED_OVER', finished_at = ?, updated_at = ?
		WHERE tailnet_id = ? AND state = 'REQUESTED' AND from_cluster_id = ? AND from_epoch = ? AND to_cluster_id = ?`,
		now, now, id.String(), from, int64(epoch), to).Error //nolint:gosec // epochs are positive int64 in the database
	if err != nil {
		return fmt.Errorf("recording the planned move: %w", err)
	}

	return nil
}

// dedicatedFree ends the WHERE of a flip (args: target cluster, tailnet id):
// a dedicated target must own no other live tailnet (ADR 0003: one tenant).
// Placement admits at most one move into a dedicated cluster (P12c C3);
// this is the last check, at the flip, under the target's row lock.
const dedicatedFree = `
			  AND NOT EXISTS (SELECT 1 FROM clusters c JOIN tailnets o ON o.owner_cluster_id = c.id
			    WHERE c.id = ? AND c.class = 'dedicated' AND o.id <> ? AND o.deleted_at IS NULL)`

// lockTargetCluster locks the clusters row of the flip's target on
// PostgreSQL (FOR UPDATE, as placement's CreateTailnet and MoveOwner do), so
// a flip and anything placement adds to that cluster serialise. Lock order:
// tailnet ownership (flip), then cluster row. An unregistered target has no
// row and nothing to lock.
func (p *Platform) lockTargetCluster(tx *gorm.DB, cluster string) error {
	if p.db.Name() != dialectPostgres {
		return nil
	}

	var ids []string

	err := tx.Raw(`SELECT id FROM clusters WHERE id = ? FOR UPDATE`, cluster).Scan(&ids).Error
	if err != nil {
		return fmt.Errorf("locking cluster %s: %w", cluster, err)
	}

	return nil
}

// flip runs one ownership change of tailnet id under LockOwnership with this
// process's lease; exactly one row must change, else ErrHandoverStale.
func (p *Platform) flip(id tailnet.ID, update func(tx *gorm.DB, now time.Time, holder string, leaseEpoch uint64) (int64, error)) error {
	holder, leaseEpoch, err := p.currentLease()
	if err != nil {
		return err
	}

	if holder == "" {
		return fmt.Errorf("%w: tailnet %s: this process holds no cluster lease", ErrHandoverStale, id)
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var n int64

	err = p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tenantscope.LockOwnership(tx, id.String())
		if err != nil {
			return err
		}

		n, err = update(tx, time.Now().UTC(), holder, leaseEpoch)

		return err
	})
	if err != nil {
		return fmt.Errorf("hand-over of tailnet %s: %w", id, err)
	}

	if n != 1 {
		return fmt.Errorf("%w: tailnet %s", ErrHandoverStale, id)
	}

	return nil
}
