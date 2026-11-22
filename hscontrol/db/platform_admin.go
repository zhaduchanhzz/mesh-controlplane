package db

import (
	"errors"
	"fmt"
	"time"
	"uuid"

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

var errEmptySlug = errors.New("tailnet slug must not be empty")

// CreateTailnet creates tailnet slug owned by this platform's cluster as
// PENDING, with the deny-all policy in the same transaction (AGENTS rule 8).
// It serves nothing until [Platform.SetTailnetStatus] makes it ACTIVE.
func (p *Platform) CreateTailnet(slug string) (types.Tailnet, error) {
	if slug == "" {
		return types.Tailnet{}, errEmptySlug
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

// SetTailnetStatus changes the status of a tailnet this cluster owns. A
// running cluster process picks the change up on restart (live reload: P10).
func (p *Platform) SetTailnetStatus(id tailnet.ID, status types.TailnetStatus) error {
	switch status {
	case types.TailnetPending, types.TailnetActive, types.TailnetSuspended, types.TailnetDeleting:
	default:
		return fmt.Errorf("invalid tailnet status %q", status)
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	res := p.db.WithContext(ctx).Model(&types.Tailnet{}).
		Where("id = ? AND owner_cluster_id = ? AND deleted_at IS NULL", id.String(), p.cluster).
		Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return fmt.Errorf("updating tailnet %s: %w", id, res.Error)
	}

	if res.RowsAffected != 1 {
		return fmt.Errorf("%w: tailnet %s", ErrTailnetNotOwned, id)
	}

	return nil
}
