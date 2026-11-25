package db

import (
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

// Platform administration (operator only, never reachable from a tenant
// request). Used by the `headscale cluster` CLI until the registry (P10).

// denyAllPolicy is the policy a new tailnet starts with: no grants, so a
// tenant runtime denies everything until its owner writes a policy
// (AGENTS rule 8).
const denyAllPolicy = `{"grants":[]}`

var (
	errEmptySlug          = errors.New("tailnet slug must not be empty")
	errInvalidStatus      = errors.New("invalid tailnet status")
	errEmptyTargetCluster = errors.New("target cluster must not be empty")
)

// CreateTailnet creates tailnet slug owned by this platform's cluster as
// PENDING, with the deny-all policy in the same transaction (AGENTS rule 8).
// It serves nothing until [Platform.SetTailnetStatus] makes it ACTIVE.
func (p *Platform) CreateTailnet(slug string) (types.Tailnet, error) {
	if slug == "" {
		return types.Tailnet{}, errEmptySlug
	}

	// The slug is the first label of the tailnet's MagicDNS domain (ADR
	// 0008 §1).
	slugErr := types.ValidSlug(slug)
	if slugErr != nil {
		return types.Tailnet{}, slugErr
	}

	now := time.Now().UTC()
	tn := types.Tailnet{
		ID:             uuid.NewV4().String(),
		Slug:           slug,
		Status:         types.TailnetPending,
		OwnerClusterID: p.cluster,
		OwnerEpoch:     1,
		PolicyRevision: 1,
		ConfigRevision: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Create(&tn).Error
		if err != nil {
			return fmt.Errorf("creating tailnet %q: %w", slug, err)
		}

		// Raw on purpose: the statement names its tailnet explicitly and
		// tenantscope has no scope for a tailnet that does not exist yet.
		err = tx.Exec(
			`INSERT INTO policies (tailnet_id, data, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			tn.ID, denyAllPolicy, now, now,
		).Error
		if err != nil {
			return fmt.Errorf("creating policy of tailnet %q: %w", slug, err)
		}

		return nil
	})
	if err != nil {
		return types.Tailnet{}, err
	}

	return tn, nil
}

// TailnetBySlug returns the live tailnet slug, whoever owns it.
func (p *Platform) TailnetBySlug(slug string) (types.Tailnet, error) {
	ctx, cancel := p.lookupContext()
	defer cancel()

	var tns []types.Tailnet

	err := p.db.WithContext(ctx).Where("slug = ? AND deleted_at IS NULL", slug).Limit(1).Find(&tns).Error
	if err != nil {
		return types.Tailnet{}, fmt.Errorf("loading tailnet %q: %w", slug, err)
	}

	if len(tns) == 0 {
		return types.Tailnet{}, fmt.Errorf("%w: %q", ErrUnknownTenant, slug)
	}

	return tns[0], nil
}

// ErrTailnetDeleting is returned when a status change would take a tailnet
// out of DELETING. DELETING is terminal (S18): a deleted tenant, its keys and
// its nodes must never be served again, so it cannot be reactivated, and its
// slug stays taken (a new tenant gets a new slug and a new ID).
var ErrTailnetDeleting = errors.New("tailnet is being deleted; DELETING is terminal")

// SetTailnetStatus changes the status of a tailnet this cluster owns. A
// running cluster process picks the change up within its reconcile interval.
// A tailnet in DELETING keeps that status (setting DELETING again is a no-op
// success); any other target fails with [ErrTailnetDeleting]. The check is in
// the UPDATE itself, so a concurrent reactivation cannot slip in.
func (p *Platform) SetTailnetStatus(id tailnet.ID, status types.TailnetStatus) error {
	switch status {
	case types.TailnetPending, types.TailnetActive, types.TailnetSuspended, types.TailnetDeleting:
	default:
		return fmt.Errorf("%w %q", errInvalidStatus, status)
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var affected int64

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tenantscope.LockOwnership(tx, id.String())
		if err != nil {
			return err
		}

		q := tx.Model(&types.Tailnet{}).
			Where("id = ? AND owner_cluster_id = ? AND deleted_at IS NULL", id.String(), p.cluster)
		if status != types.TailnetDeleting {
			q = q.Where("status <> ?", types.TailnetDeleting)
		}

		res := q.Updates(map[string]any{"status": status, colUpdatedAt: time.Now().UTC()})
		affected = res.RowsAffected

		return res.Error
	})
	if err != nil {
		return fmt.Errorf("updating tailnet %s: %w", id, err)
	}

	if affected == 1 {
		return nil
	}

	var cur types.Tailnet

	err = p.db.WithContext(ctx).
		Where("id = ? AND owner_cluster_id = ? AND deleted_at IS NULL", id.String(), p.cluster).
		Take(&cur).Error
	if err == nil && cur.Status == types.TailnetDeleting {
		return fmt.Errorf("%w: tailnet %s", ErrTailnetDeleting, id)
	}

	return fmt.Errorf("%w: tailnet %s", ErrTailnetNotOwned, id)
}

// ReassignTailnet hands tailnet id from this cluster to cluster to (which may
// be this cluster again) at fromEpoch+1, as a compare-and-swap on
// (owner_cluster_id, owner_epoch): it fails unless this cluster owns it at
// fromEpoch, so the epoch only grows and two callers cannot both win.
// Operator tool for the lab; the placement API (P10) does the same move.
func (p *Platform) ReassignTailnet(id tailnet.ID, fromEpoch uint64, to string) (uint64, error) {
	if to == "" {
		return 0, errEmptyTargetCluster
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var affected int64

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tenantscope.LockOwnership(tx, id.String())
		if err != nil {
			return err
		}

		res := tx.Model(&types.Tailnet{}).
			Where("id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND deleted_at IS NULL", id.String(), p.cluster, fromEpoch).
			Updates(map[string]any{
				"owner_cluster_id": to,
				"owner_epoch":      gorm.Expr("owner_epoch + 1"),
				colUpdatedAt:       time.Now().UTC(),
			})
		affected = res.RowsAffected

		return res.Error
	})
	if err != nil {
		return 0, fmt.Errorf("reassigning tailnet %s: %w", id, err)
	}

	if affected != 1 {
		return 0, fmt.Errorf("%w: tailnet %s is not owned by %s at epoch %d", ErrTailnetNotOwned, id, p.cluster, fromEpoch)
	}

	return fromEpoch + 1, nil
}

// BumpPolicyRevision records that the policy of tailnet id changed in the
// database. A running cluster reloads the policy of a runtime whose loaded
// revision differs (reconcile).
func (p *Platform) BumpPolicyRevision(id tailnet.ID) error {
	ctx, cancel := p.lookupContext()
	defer cancel()

	var affected int64

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tenantscope.LockOwnership(tx, id.String())
		if err != nil {
			return err
		}

		res := tx.Model(&types.Tailnet{}).
			Where("id = ? AND owner_cluster_id = ? AND deleted_at IS NULL", id.String(), p.cluster).
			Updates(map[string]any{"policy_revision": gorm.Expr("policy_revision + 1"), colUpdatedAt: time.Now().UTC()})
		affected = res.RowsAffected

		return res.Error
	})
	if err != nil {
		return fmt.Errorf("bumping policy revision of %s: %w", id, err)
	}

	if affected != 1 {
		return fmt.Errorf("%w: tailnet %s", ErrTailnetNotOwned, id)
	}

	return nil
}
