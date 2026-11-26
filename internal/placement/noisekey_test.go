package placement

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/key"
)

// TestRotateNoiseKey (P13e, ADR 0014 §3): the registry's rotation names the
// new Noise public key of a cluster, with format check, revision + 1 and an
// audit row naming the old and the new public key (never a private key);
// the same key again, a malformed key and an unknown cluster are refused.
func TestRotateNoiseKey(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			r := newRegistry(t, b.open(t), Config{})
			ctx := context.Background()
			c := addCluster(t, r, "c1", ClassShared, ClusterActive, 100)

			k1 := key.NewMachine().Public().String()
			k2 := key.NewMachine().Public().String()

			_, err := r.RotateNoiseKey(ctx, testOp, "c1", "mkey:c1")
			require.ErrorIs(t, err, ErrInvalid, "malformed key")

			got, err := r.RotateNoiseKey(ctx, testOp, "c1", k1)
			require.NoError(t, err)
			assert.Equal(t, k1, got.NoisePublicKey)
			assert.Equal(t, c.Revision+1, got.Revision)

			_, err = r.RotateNoiseKey(ctx, testOp, "c1", k1)
			require.ErrorIs(t, err, ErrInvalid, "rotating to the registered key is a mistake, not a no-op")

			got, err = r.RotateNoiseKey(ctx, testOp, "c1", k2)
			require.NoError(t, err)
			assert.Equal(t, k2, got.NoisePublicKey)

			_, err = r.RotateNoiseKey(ctx, testOp, "nope", k1)
			require.ErrorIs(t, err, ErrNotFound)

			entries, err := r.AuditLog(ctx, 100)
			require.NoError(t, err)

			var rotations []AuditEntry

			for _, a := range entries {
				assert.NotContains(t, a.Detail, "privkey:", "no private key in the audit log")

				if a.Action == "RotateNoiseKey" && a.Outcome == "ok" {
					rotations = append(rotations, a)
				}
			}

			require.Len(t, rotations, 2)

			for _, a := range rotations {
				assert.Equal(t, "c1", a.Target)
				assert.Equal(t, testOp.Actor, a.Actor)
			}

			details := rotations[0].Detail + rotations[1].Detail
			assert.True(t, strings.Contains(details, k1) && strings.Contains(details, k2), "old and new keys audited: %s", details)
		})
	}
}
