package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func registerCluster(t *testing.T, p *Platform, id, key string) {
	t.Helper()

	now := time.Now().UTC()

	var k any
	if key != "" {
		k = key
	}

	require.NoError(t, p.db.Exec(`INSERT INTO clusters (id, endpoint_url, class, state, noise_public_key, max_devices, revision, created_at, updated_at)
		VALUES (?, ?, 'shared', 'ACTIVE', ?, 100, 1, ?, ?)`, id, "https://"+id+".example.test", k, now, now).Error)
}

func registeredKey(t *testing.T, p *Platform, id string) string {
	t.Helper()

	var keys []*string
	require.NoError(t, p.db.Raw(`SELECT noise_public_key FROM clusters WHERE id = ?`, id).Scan(&keys).Error)
	require.Len(t, keys, 1)

	if keys[0] == nil {
		return ""
	}

	return *keys[0]
}

// P12, owner decision Q6: a cluster process records its Noise public key
// once when it takes the lease; later it must match, otherwise it does not
// serve. Only the lease holder writes it.
func TestRecordNoiseKey(t *testing.T) {
	const k1, k2 = "mkey:1111111111111111111111111111111111111111111111111111111111111111",
		"mkey:2222222222222222222222222222222222222222222222222222222222222222"

	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p, _ := leased(t, b, "c1", "h1")

			require.NoError(t, p.RecordNoiseKey(k1), "unregistered cluster: not checked")

			registerCluster(t, p, "c1", "")

			// Not the lease holder: nothing is written.
			other := openPlatform(t, b.cfg(t, "", "c1"))
			other.SetLease("h-old", 99)
			require.Error(t, other.RecordNoiseKey(k2))
			assert.Empty(t, registeredKey(t, p, "c1"))

			require.NoError(t, p.RecordNoiseKey(k1), "empty: recorded")
			assert.Equal(t, k1, registeredKey(t, p, "c1"))

			require.NoError(t, p.RecordNoiseKey(k1), "same key")

			err := p.RecordNoiseKey(k2)
			require.ErrorIs(t, err, ErrNoiseKeyMismatch)
			assert.Equal(t, k1, registeredKey(t, p, "c1"), "never overwritten")

			// A key registered by the operator beforehand is checked too.
			p2, _ := leased(t, b, "c2", "h2")
			registerCluster(t, p2, "c2", k2)
			require.ErrorIs(t, p2.RecordNoiseKey(k1), ErrNoiseKeyMismatch)
			require.NoError(t, p2.RecordNoiseKey(k2))
		})
	}
}
