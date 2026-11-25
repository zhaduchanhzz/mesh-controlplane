package placement

import (
	"context"
	"fmt"
	"slices"
	"uuid"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// DenyAllPolicy is the policy every new tailnet starts with (AGENTS rule 8):
// no grants, so its runtime denies everything until the tenant writes one.
const DenyAllPolicy = `{"grants":[]}`

// TailnetInfo is a tailnet with its live device count.
type TailnetInfo struct {
	types.Tailnet

	Devices int64 `json:"devices"`
}

// CreateTailnet places a new tailnet of class and creates it, in ONE
// transaction (PLAN §5): choose the cluster (under lock), insert the tailnet
// PENDING with that owner at epoch 1, insert its deny-all policy, audit. Any
// failure rolls everything back: no tailnet ever exists without an owner or
// a deny policy (I04). It serves nothing until ActivateTailnet.
func (r *Registry) CreateTailnet(ctx context.Context, op Op, slug string, class Class) (types.Tailnet, error) {
	if !slugRE.MatchString(slug) {
		return types.Tailnet{}, fmt.Errorf("%w: slug %q", ErrInvalid, slug)
	}

	if !class.Valid() {
		return types.Tailnet{}, fmt.Errorf("%w: class %q", ErrInvalid, class)
	}

	detail := map[string]any{"slug": slug, "class": class}

	return mutate(ctx, r, op, "CreateTailnet", slug, detail, func(tx *gorm.DB) (types.Tailnet, string, error) {
		err := r.step("place")
		if err != nil {
			return types.Tailnet{}, "", err
		}

		c, err := place(tx, class, true)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		err = r.step("tailnet")
		if err != nil {
			return types.Tailnet{}, "", err
		}

		now := r.now()
		tn := types.Tailnet{
			ID:             uuid.NewV4().String(),
			Slug:           slug,
			Status:         types.TailnetPending,
			OwnerClusterID: c.ID,
			OwnerEpoch:     1,
			PolicyRevision: 1,
			ConfigRevision: 1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}

		err = tx.Create(&tn).Error
		if isDuplicate(err) {
			return types.Tailnet{}, "", fmt.Errorf("%w: slug %q", ErrConflict, slug)
		}

		if err != nil {
			return types.Tailnet{}, "", fmt.Errorf("creating tailnet: %w", err)
		}

		err = r.step("policy")
		if err != nil {
			return types.Tailnet{}, "", err
		}

		// Raw on purpose: policies is a tenant table and the statement names
		// its tailnet explicitly (tenantscope has no scope for a tailnet
		// being created), as db.Platform.CreateTailnet does.
		err = tx.Exec(
			`INSERT INTO policies (tailnet_id, data, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			tn.ID, DenyAllPolicy, now, now,
		).Error
		if err != nil {
			return types.Tailnet{}, "", fmt.Errorf("creating deny-all policy: %w", err)
		}

		return tn, tn.ID, nil
	})
}

// ActivateTailnet makes a PENDING or SUSPENDED tailnet ACTIVE, if its
// owner_epoch is still expectedEpoch and it has a policy row. A running
// cluster process picks the change up on restart (live reload is the
// runtime's part of P10).
func (r *Registry) ActivateTailnet(ctx context.Context, op Op, id string, expectedEpoch uint64) (types.Tailnet, error) {
	return r.setStatus(ctx, op, "ActivateTailnet", id, expectedEpoch, types.TailnetActive,
		[]types.TailnetStatus{types.TailnetPending, types.TailnetSuspended}, true)
}

// SuspendTailnet makes a PENDING or ACTIVE tailnet SUSPENDED, if its
// owner_epoch is still expectedEpoch.
func (r *Registry) SuspendTailnet(ctx context.Context, op Op, id string, expectedEpoch uint64) (types.Tailnet, error) {
	return r.setStatus(ctx, op, "SuspendTailnet", id, expectedEpoch, types.TailnetSuspended,
		[]types.TailnetStatus{types.TailnetPending, types.TailnetActive}, false)
}

func (r *Registry) setStatus(
	ctx context.Context, op Op, action, id string, expectedEpoch uint64,
	to types.TailnetStatus, from []types.TailnetStatus, needPolicy bool,
) (types.Tailnet, error) {
	if expectedEpoch == 0 {
		return types.Tailnet{}, fmt.Errorf("%w: expected epoch is required", ErrInvalid)
	}

	detail := map[string]any{"status": to, "expected_epoch": expectedEpoch}

	return mutate(ctx, r, op, action, id, detail, func(tx *gorm.DB) (types.Tailnet, string, error) {
		err := tenantscope.LockOwnership(tx, id)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		q := tx.Model(&types.Tailnet{}).
			Where("id = ? AND owner_epoch = ? AND status IN ? AND deleted_at IS NULL", id, expectedEpoch, from)
		if needPolicy {
			// Never serve a tailnet without its policy (no fallback to
			// upstream's allow-all on a missing policy).
			q = q.Where(`EXISTS (SELECT 1 FROM policies p WHERE p.tailnet_id = tailnets.id
				AND p.deleted_at IS NULL AND p.data IS NOT NULL AND p.data <> '')`)
		}

		res := q.Updates(map[string]any{"status": to, "updated_at": r.now()})
		if res.Error != nil {
			return types.Tailnet{}, "", fmt.Errorf("updating tailnet: %w", res.Error)
		}

		tn, err := loadTailnet(tx, id)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		if res.RowsAffected == 1 {
			return tn, "", nil
		}

		if tn.OwnerEpoch != expectedEpoch {
			return types.Tailnet{}, "", fmt.Errorf("%w: tailnet %s is at epoch %d", ErrStaleEpoch, id, tn.OwnerEpoch)
		}

		if !slices.Contains(from, tn.Status) {
			return types.Tailnet{}, "", fmt.Errorf("%w: tailnet %s is %s", ErrInvalidTransition, id, tn.Status)
		}

		return types.Tailnet{}, "", fmt.Errorf("%w: tailnet %s has no policy", ErrInvalidTransition, id)
	})
}

// GetTailnet returns live tailnet id with its live device count.
func (r *Registry) GetTailnet(ctx context.Context, id string) (TailnetInfo, error) {
	db := r.db.WithContext(ctx)

	tn, err := loadTailnet(db, id)
	if err != nil {
		return TailnetInfo{}, err
	}

	n, err := deviceCount(db, id)
	if err != nil {
		return TailnetInfo{}, err
	}

	return TailnetInfo{Tailnet: tn, Devices: n}, nil
}

// MoveOwner moves tailnet id from fromCluster at fromEpoch to toCluster.
//
// To another cluster it is a request (ADR 0010), a compare-and-swap that
// changes neither owner nor epoch:
//
//	UPDATE tailnets SET moving_to_cluster_id = to
//	 WHERE id = ? AND owner_cluster_id = from AND owner_epoch = fromEpoch
//	   AND moving_to_cluster_id IS NULL
//
// The owner cluster stops serving the tailnet, then hands it over (owner =
// to, owner_epoch + 1; db.Platform.HandOver), or the target takes it over
// if the owner cluster has no live lease; so two clusters never serve it at
// once. One move is pending at a time: a second request is ErrConflict; a
// stale owner or epoch is ErrStaleEpoch. CancelMove withdraws a request.
//
// To fromCluster itself it raises owner_epoch at once (one process retires
// and reloads the runtime).
//
// toCluster must be registered and ACTIVE; a dedicated one must own no
// other live tailnet. Moving a tenant's clients is P12b (re-bootstrap, new
// ControlURL, new login).
func (r *Registry) MoveOwner(ctx context.Context, op Op, id, fromCluster string, fromEpoch uint64, toCluster string) (types.Tailnet, error) {
	if fromCluster == "" || toCluster == "" || fromEpoch == 0 {
		return types.Tailnet{}, fmt.Errorf("%w: from cluster, from epoch and to cluster are required", ErrInvalid)
	}

	detail := map[string]any{"from_cluster": fromCluster, "from_epoch": fromEpoch, "to_cluster": toCluster}

	return mutate(ctx, r, op, "MoveOwner", id, detail, func(tx *gorm.DB) (types.Tailnet, string, error) {
		err := tenantscope.LockOwnership(tx, id)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		target, err := lockClusters(tx, "UPDATE", "id = ?", toCluster)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		if target[0].State != ClusterActive {
			return types.Tailnet{}, "", fmt.Errorf("%w: cluster %s is %s", ErrNoCapacity, toCluster, target[0].State)
		}

		if target[0].Class == ClassDedicated {
			var n int64

			err = tx.Raw(`SELECT count(*) FROM tailnets WHERE owner_cluster_id = ? AND id <> ? AND deleted_at IS NULL`,
				toCluster, id).Scan(&n).Error
			if err != nil {
				return types.Tailnet{}, "", fmt.Errorf("counting tailnets: %w", err)
			}

			if n > 0 {
				return types.Tailnet{}, "", fmt.Errorf("%w: dedicated cluster %s is taken", ErrNoCapacity, toCluster)
			}
		}

		var res *gorm.DB
		if toCluster == fromCluster {
			res = tx.Exec(`UPDATE tailnets SET owner_epoch = owner_epoch + 1, updated_at = ?
				WHERE id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND moving_to_cluster_id IS NULL AND deleted_at IS NULL`,
				r.now(), id, fromCluster, fromEpoch)
		} else {
			res = tx.Exec(`UPDATE tailnets SET moving_to_cluster_id = ?, updated_at = ?
				WHERE id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND moving_to_cluster_id IS NULL AND deleted_at IS NULL`,
				toCluster, r.now(), id, fromCluster, fromEpoch)
		}

		if res.Error != nil {
			return types.Tailnet{}, "", fmt.Errorf("moving tailnet: %w", res.Error)
		}

		tn, err := loadTailnet(tx, id)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		if res.RowsAffected != 1 {
			return types.Tailnet{}, "", moveRefused(tn, fromCluster, fromEpoch)
		}

		return tn, "", nil
	})
}

// moveRefused explains a MoveOwner compare-and-swap that changed nothing.
func moveRefused(tn types.Tailnet, fromCluster string, fromEpoch uint64) error {
	if tn.OwnerClusterID == fromCluster && tn.OwnerEpoch == fromEpoch && tn.MovingToClusterID != nil {
		return fmt.Errorf("%w: a move of tailnet %s to %s is pending", ErrConflict, tn.ID, *tn.MovingToClusterID)
	}

	return fmt.Errorf("%w: tailnet %s is owned by %s at epoch %d", ErrStaleEpoch, tn.ID, tn.OwnerClusterID, tn.OwnerEpoch)
}

// CancelMove withdraws the move of tailnet id requested at epoch (ADR 0010),
// while the owner has not handed it over yet: owner and epoch unchanged.
// No move pending is ErrConflict; another epoch is ErrStaleEpoch.
func (r *Registry) CancelMove(ctx context.Context, op Op, id string, epoch uint64) (types.Tailnet, error) {
	if epoch == 0 {
		return types.Tailnet{}, fmt.Errorf("%w: epoch is required", ErrInvalid)
	}

	return mutate(ctx, r, op, "CancelMove", id, map[string]any{"epoch": epoch}, func(tx *gorm.DB) (types.Tailnet, string, error) {
		err := tenantscope.LockOwnership(tx, id)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		res := tx.Exec(`UPDATE tailnets SET moving_to_cluster_id = NULL, updated_at = ?
			WHERE id = ? AND owner_epoch = ? AND moving_to_cluster_id IS NOT NULL AND deleted_at IS NULL`,
			r.now(), id, epoch)
		if res.Error != nil {
			return types.Tailnet{}, "", fmt.Errorf("cancelling move: %w", res.Error)
		}

		tn, err := loadTailnet(tx, id)
		if err != nil {
			return types.Tailnet{}, "", err
		}

		if res.RowsAffected != 1 {
			if tn.OwnerEpoch != epoch {
				return types.Tailnet{}, "", fmt.Errorf("%w: tailnet %s is at epoch %d", ErrStaleEpoch, id, tn.OwnerEpoch)
			}

			return types.Tailnet{}, "", fmt.Errorf("%w: no move of tailnet %s pending", ErrConflict, id)
		}

		return tn, "", nil
	})
}

func loadTailnet(db *gorm.DB, id string) (types.Tailnet, error) {
	var tns []types.Tailnet

	err := db.Where("id = ? AND deleted_at IS NULL", id).Limit(1).Find(&tns).Error
	if err != nil {
		return types.Tailnet{}, fmt.Errorf("loading tailnet: %w", err)
	}

	if len(tns) == 0 {
		return types.Tailnet{}, fmt.Errorf("%w: tailnet %s", ErrNotFound, id)
	}

	return tns[0], nil
}

// deviceCount counts the live devices of tailnet id. Raw on purpose: an
// operator count, not a tenant read; it returns a number only.
func deviceCount(db *gorm.DB, id string) (int64, error) {
	var n int64

	err := db.Raw(`SELECT count(*) FROM nodes WHERE tailnet_id = ? AND deleted_at IS NULL`, id).Scan(&n).Error
	if err != nil {
		return 0, fmt.Errorf("counting devices: %w", err)
	}

	return n, nil
}
