package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

// Per-tailnet settings (P06, ADR 0008 §2): one row per tailnet that has
// any, holding its DNS settings as validated JSON. A write bumps
// tailnets.config_revision in the same transaction; the cluster reconcile
// loop reloads a runtime whose revision moved, as with policy_revision.

const tailnetSettingsMigrationID = "202611241300-tailnet-settings"

// tailnetSettingsDDL is byte-identical in schema.sql and
// schema_postgres.sql (no dialect-specific types).
const tailnetSettingsDDL = `CREATE TABLE tailnet_settings(
  tailnet_id text PRIMARY KEY,
  dns_config text,
  CONSTRAINT fk_tailnet_settings_tailnet FOREIGN KEY(tailnet_id) REFERENCES tailnets(id)
);`

var errInvalidStoredSlugs = errors.New("tailnets with a slug that is not a DNS label")

func tailnetSettingsMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: tailnetSettingsMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				// The slug becomes the first label of the tailnet's base
				// domain (ADR 0008 §1). Refuse, never rewrite: renaming a
				// tailnet renames every MagicDNS name in it.
				var slugs []string

				err := tx.Raw(`SELECT slug FROM tailnets`).Scan(&slugs).Error
				if err != nil {
					return fmt.Errorf("reading tailnet slugs: %w", err)
				}

				bad := 0

				for _, s := range slugs {
					if types.ValidSlug(s) != nil {
						bad++
					}
				}

				if bad > 0 {
					return fmt.Errorf("%w: %d; rename them before upgrading", errInvalidStoredSlugs, bad)
				}

				return tx.Exec(tailnetSettingsDDL).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`DROP TABLE IF EXISTS tailnet_settings`).Error
			})
		},
	}
}

// SetTailnetDNS validates d for tailnet id (owned by this cluster, not
// deleted) under clusterBase and lim, stores it and bumps the tailnet's
// config_revision, in one transaction holding the ownership lock; nil
// clears the settings (the defaults). A refused write changes nothing.
func (p *Platform) SetTailnetDNS(id tailnet.ID, d *types.TenantDNS, clusterBase string, lim types.TenantDNSLimits) error {
	ctx, cancel := p.lookupContext()
	defer cancel()

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tenantscope.LockOwnership(tx, id.String())
		if err != nil {
			return err
		}

		var tn types.Tailnet

		res := tx.Where("id = ? AND owner_cluster_id = ? AND deleted_at IS NULL", id.String(), p.cluster).
			Limit(1).Find(&tn)
		if res.Error != nil {
			return res.Error
		}

		if res.RowsAffected != 1 {
			return fmt.Errorf("%w: tailnet %s", ErrTailnetNotOwned, id)
		}

		var stored sql.NullString

		if d != nil {
			err = d.Validate(tn.Slug, clusterBase, lim)
			if err != nil {
				return err
			}

			raw, err := json.Marshal(d)
			if err != nil {
				return err
			}

			stored = sql.NullString{String: string(raw), Valid: true}
		}

		err = tx.Exec(`INSERT INTO tailnet_settings (tailnet_id, dns_config) VALUES (?, ?)
			ON CONFLICT (tailnet_id) DO UPDATE SET dns_config = excluded.dns_config`, id.String(), stored).Error
		if err != nil {
			return err
		}

		return tx.Model(&types.Tailnet{}).Where("id = ?", id.String()).
			Updates(map[string]any{"config_revision": gorm.Expr("config_revision + 1"), colUpdatedAt: time.Now().UTC()}).Error
	})
	if err != nil {
		return fmt.Errorf("setting DNS of tailnet %s: %w", id, err)
	}

	return nil
}

// TailnetDNS returns tailnet id's stored DNS settings, nil for the
// defaults. A stored value that does not parse is an error, never the
// defaults; the caller validates it against the cluster's limits.
func (p *Platform) TailnetDNS(id tailnet.ID) (*types.TenantDNS, error) {
	ctx, cancel := p.lookupContext()
	defer cancel()

	var rows []sql.NullString

	err := p.db.WithContext(ctx).Raw(`SELECT dns_config FROM tailnet_settings WHERE tailnet_id = ?`, id.String()).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading DNS of tailnet %s: %w", id, err)
	}

	if len(rows) == 0 || !rows[0].Valid {
		return nil, nil //nolint:nilnil // no settings: the defaults
	}

	d, err := types.ParseTenantDNS([]byte(rows[0].String))
	if err != nil {
		return nil, fmt.Errorf("stored DNS of tailnet %s: %w", id, err)
	}

	return d, nil
}
