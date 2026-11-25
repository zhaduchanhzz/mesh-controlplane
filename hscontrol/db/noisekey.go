package db

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrNoiseKeyMismatch reports that the Noise public key registered for this
// cluster (clusters.noise_public_key) is not the key this process serves
// with: the process must not serve (P12, owner decision Q6, fail closed).
var ErrNoiseKeyMismatch = errors.New("cluster Noise public key differs from the registered one")

// RecordNoiseKey checks this process's Noise public key pub against the
// cluster's registry row (P12, Q6): an empty key is recorded (write-once),
// an equal one is fine, a different one is ErrNoiseKeyMismatch. A cluster
// with no registry row is not checked (reported as R1/R5 by reconcile).
// Only while this process holds the cluster lease it recorded with
// SetLease.
func (p *Platform) RecordNoiseKey(pub string) error {
	holder, leaseEpoch, err := p.currentLease()
	if err != nil {
		return err
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var keys []string

		err := tx.Raw(`SELECT COALESCE(noise_public_key, '') FROM clusters WHERE id = ?`, p.cluster).Scan(&keys).Error
		if err != nil {
			return fmt.Errorf("reading the registered Noise key of %s: %w", p.cluster, err)
		}

		switch {
		case len(keys) == 0:
			return nil // not registered
		case keys[0] != "":
			if keys[0] != pub {
				return fmt.Errorf("%w: cluster %s", ErrNoiseKeyMismatch, p.cluster)
			}

			return nil
		}

		res := tx.Exec(`UPDATE clusters SET noise_public_key = ?, revision = revision + 1, updated_at = ?
			WHERE id = ? AND (noise_public_key IS NULL OR noise_public_key = '')
			  AND EXISTS (SELECT 1 FROM cluster_leases l
			    WHERE l.cluster_id = ? AND l.holder = ? AND l.epoch = ? AND l.expires_at > `+p.dbNow()+`)`,
			pub, time.Now().UTC(), p.cluster, p.cluster, holder, int64(leaseEpoch)) //nolint:gosec // epochs are positive int64 in the database
		if res.Error != nil {
			return fmt.Errorf("recording the Noise key of %s: %w", p.cluster, res.Error)
		}

		if res.RowsAffected != 1 {
			return fmt.Errorf("recording the Noise key of %s: this process does not hold the cluster lease", p.cluster) //nolint:err113
		}

		return nil
	})
}
