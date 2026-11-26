package placement

import (
	"context"
	"fmt"

	"gorm.io/gorm"
	"tailscale.com/types/key"
)

// ValidateNoisePublicKey checks that s is a cluster Noise public key as
// clients see it on /key and the cluster records it (P12, P13e, ADR 0014
// §3): "mkey:" and 64 hex digits, not the zero key. A malformed value in
// the registry would match no process's key, so every process of the
// cluster would refuse to serve.
func ValidateNoisePublicKey(s string) error {
	var k key.MachinePublic

	err := k.UnmarshalText([]byte(s))
	if err != nil || k.IsZero() {
		return fmt.Errorf("%w: noise_public_key must be a Noise public key (mkey: and 64 hex digits)", ErrInvalid)
	}

	return nil
}

// noiseKeyRotation is the audit detail of RotateNoiseKey: public keys only.
type noiseKeyRotation struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RotateNoiseKey names newKey as the Noise public key of cluster id (P13e,
// ADR 0014 §1-§2): validated, different from the registered key, revision
// + 1, audited with the previous and the new public key. The registry is
// read first for the audit detail; the update is a compare-and-swap on
// that value, so a concurrent change fails with ErrStaleRevision rather
// than be audited wrongly.
//
// Within one reconcile tick the cluster's active process, if it serves
// with another key, stops serving and gives the lease back (ADR 0014 §3);
// a process holding newKey's private key then takes over.
func (r *Registry) RotateNoiseKey(ctx context.Context, op Op, id, newKey string) (Cluster, error) {
	err := ValidateNoisePublicKey(newKey)
	if err != nil {
		return Cluster{}, err
	}

	cur, err := loadCluster(r.db.WithContext(ctx), id)
	if err != nil {
		return Cluster{}, err
	}

	if cur.NoisePublicKey == newKey {
		return Cluster{}, fmt.Errorf("%w: %s is already the registered Noise key of cluster %q", ErrInvalid, newKey, id)
	}

	detail := noiseKeyRotation{From: cur.NoisePublicKey, To: newKey}

	return mutate(ctx, r, op, "RotateNoiseKey", id, detail, func(tx *gorm.DB) (Cluster, string, error) {
		res := tx.Model(&Cluster{}).
			Where("id = ? AND COALESCE(noise_public_key, '') = ?", id, cur.NoisePublicKey).
			Updates(map[string]any{
				"noise_public_key": newKey,
				"revision":         gorm.Expr("revision + 1"),
				"updated_at":       r.now(),
			})
		if res.Error != nil {
			return Cluster{}, "", fmt.Errorf("rotating the Noise key: %w", res.Error)
		}

		if res.RowsAffected != 1 {
			return Cluster{}, "", fmt.Errorf("%w: the Noise key of cluster %q changed meanwhile; read it again", ErrStaleRevision, id)
		}

		c, err := loadCluster(tx, id)

		return c, "", err
	})
}
