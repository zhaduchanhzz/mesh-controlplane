package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A tenant's own policy and DNS writes through its runtime's handle (P12d,
// owner decision 2026-11-26). Each write is one transaction on the handle:
//
//  1. tenantscope.LockOwnership (the first statement of a transaction that
//     updates a tailnets row);
//  2. the tailnet's revision is bumped, only while the row still names the
//     owner and epoch the handle is fenced on, ACTIVE and not deleted;
//  3. the tenant row is written through the tenantscope plugin, whose
//     fence also checks the cluster lease in the same transaction.
//
// So the stored value and its revision commit together or not at all, and
// a process that lost the tailnet (moved, newer epoch, lease gone) stores
// nothing and bumps nothing. The lock order is the plugin's: tailnet
// advisory lock, tailnets row, cluster advisory lock.

// Revision columns of the tailnets row a tenant write may bump.
const (
	colPolicyRevision = "policy_revision"
	colConfigRevision = "config_revision"
)

// bumpTailnetRevision increments column col of the tailnet tx is scoped to,
// fenced on tx's ownership fence, and returns the new value.
func bumpTailnetRevision(tx *gorm.DB, col string) (uint64, error) {
	s, err := tailnet.FromContext(tx.Statement.Context)
	if err != nil {
		return 0, err
	}

	f, ok := tailnet.FenceFromContext(tx.Statement.Context)
	if !ok {
		return 0, tenantscope.ErrNoFence
	}

	id := s.ID().String()

	err = tenantscope.LockOwnership(tx, id)
	if err != nil {
		return 0, err
	}

	res := tx.Model(&types.Tailnet{}).
		Where("id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND status = ? AND deleted_at IS NULL",
			id, f.Cluster(), uint64(f.Epoch()), types.TailnetActive).
		Updates(map[string]any{col: gorm.Expr(col + " + 1"), colUpdatedAt: time.Now().UTC()})
	if res.Error != nil {
		return 0, fmt.Errorf("bumping %s of tailnet %s: %w", col, id, res.Error)
	}

	if res.RowsAffected != 1 {
		return 0, fmt.Errorf("%w: tailnet %s is not owned by %q at epoch %d, or not ACTIVE",
			tenantscope.ErrFenced, id, f.Cluster(), f.Epoch())
	}

	var tn types.Tailnet

	err = tx.Where("id = ?", id).Take(&tn).Error
	if err != nil {
		return 0, fmt.Errorf("reading tailnet %s: %w", id, err)
	}

	if col == colPolicyRevision {
		return tn.PolicyRevision, nil
	}

	return tn.ConfigRevision, nil
}

// Fenced reports whether writes on this handle are fenced on a tailnet's
// ownership (a cluster runtime or an operator handle), so a policy write
// must bump the tailnet's policy_revision.
func (hsdb *HSDatabase) Fenced() bool {
	_, ok := tailnet.FenceFromContext(hsdb.DB.Statement.Context)

	return ok
}

// SetPolicyRevised is [HSDatabase.SetPolicy] that also bumps the tailnet's
// policy_revision in the same transaction (see above), returning the new
// revision. It needs a fenced handle.
func (hsdb *HSDatabase) SetPolicyRevised(policy string) (*types.Policy, uint64, error) {
	type out struct {
		p   *types.Policy
		rev uint64
	}

	o, err := Write(hsdb.DB, func(tx *gorm.DB) (out, error) {
		rev, err := bumpTailnetRevision(tx, colPolicyRevision)
		if err != nil {
			return out{}, err
		}

		p := types.Policy{Data: policy}

		err = tx.Clauses(clause.Returning{}).Create(&p).Error
		if err != nil {
			return out{}, err
		}

		// As SetPolicy: only the newest row is read.
		err = tx.Unscoped().Where("id < ?", p.ID).Delete(&types.Policy{}).Error
		if err != nil {
			return out{}, err
		}

		return out{&p, rev}, nil
	})
	if err != nil {
		return nil, 0, err
	}

	return o.p, o.rev, nil
}

// tenantDNSRow reads the stored settings of the tailnet tx is scoped to
// (nil for the defaults) and whether a row exists. A stored value that does
// not parse is an error, never the defaults.
func tenantDNSRow(tx *gorm.DB) (*types.TenantDNS, bool, error) {
	var rows []types.TailnetSettings

	err := tx.Limit(1).Find(&rows).Error
	if err != nil {
		return nil, false, fmt.Errorf("reading DNS settings: %w", err)
	}

	if len(rows) == 0 {
		return nil, false, nil
	}

	if !rows[0].DNSConfig.Valid {
		return nil, true, nil
	}

	d, err := types.ParseTenantDNS([]byte(rows[0].DNSConfig.String))
	if err != nil {
		return nil, true, fmt.Errorf("stored DNS settings: %w", err)
	}

	return d, true, nil
}

// TenantDNS returns the stored DNS settings of the handle's tailnet, nil
// for the defaults.
func (hsdb *HSDatabase) TenantDNS() (*types.TenantDNS, error) {
	d, _, err := tenantDNSRow(hsdb.DB)

	return d, err
}

// UpdateTenantDNS replaces the DNS settings of the handle's tailnet by what
// fn returns for the stored ones (nil: the defaults), and bumps its
// config_revision, in one transaction (see above); the new settings and
// revision are returned. An error from fn changes nothing. fn validates.
func (hsdb *HSDatabase) UpdateTenantDNS(
	fn func(*types.TenantDNS) (*types.TenantDNS, error),
) (*types.TenantDNS, uint64, error) {
	type out struct {
		d   *types.TenantDNS
		rev uint64
	}

	o, err := Write(hsdb.DB, func(tx *gorm.DB) (out, error) {
		// The bump first: under the ownership lock, the read below sees
		// every write that committed before this one.
		rev, err := bumpTailnetRevision(tx, colConfigRevision)
		if err != nil {
			return out{}, err
		}

		cur, exists, err := tenantDNSRow(tx)
		if err != nil {
			return out{}, err
		}

		next, err := fn(cur)
		if err != nil {
			return out{}, err
		}

		var stored sql.NullString

		if next != nil {
			raw, err := json.Marshal(next)
			if err != nil {
				return out{}, err
			}

			stored = sql.NullString{String: string(raw), Valid: true}
		}

		s, err := tailnet.FromContext(tx.Statement.Context)
		if err != nil {
			return out{}, err
		}

		if exists {
			err = tx.Model(&types.TailnetSettings{}).Where("tailnet_id = ?", s.ID().String()).
				Update("dns_config", stored).Error
		} else {
			err = tx.Create(&types.TailnetSettings{DNSConfig: stored}).Error
		}

		if err != nil {
			return out{}, fmt.Errorf("storing DNS settings: %w", err)
		}

		return out{next, rev}, nil
	})
	if err != nil {
		return nil, 0, err
	}

	return o.d, o.rev, nil
}
