package placement

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P08b: tenant API keys are minted by the platform only, bound to one
// tailnet, shown once, audited, revocable; the platform lookup of a key
// returns its tailnet and nothing for a revoked, expired or tampered key.
func TestAPIKeyLifecycle(t *testing.T) {
	for _, b := range []struct {
		name string
		cfg  func(t *testing.T) *types.Config
	}{
		{"sqlite", func(t *testing.T) *types.Config {
			t.Helper()

			return &types.Config{
				Database: types.DatabaseConfig{
					Type:   types.DatabaseSqlite,
					Sqlite: types.SqliteConfig{Path: filepath.Join(t.TempDir(), "registry.db")},
				},
				Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
			}
		}},
		{"postgres", postgresTestConfig},
	} {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()

			p, err := db.OpenPlatform(b.cfg(t))
			require.NoError(t, err)
			t.Cleanup(func() { _ = p.Close() })

			gdb := p.RegistryDB()
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			tnA := activeTailnet(t, r, "acme", ClassShared)
			tnB := activeTailnet(t, r, "beta", ClassShared)

			// Lifetime bounds.
			for _, bad := range []time.Duration{0, -time.Hour, MaxAPIKeyLifetime + time.Hour} {
				_, _, err := r.CreateAPIKey(ctx, testOp, tnA.ID, bad, "")
				require.ErrorIs(t, err, ErrInvalid, "lifetime %s", bad)
			}

			_, _, err = r.CreateAPIKey(ctx, testOp, "00000000-0000-4000-8000-00000000dead", time.Hour, "")
			require.ErrorIs(t, err, ErrNotFound, "unknown tailnet")

			// Create with an idempotency key: the key is shown once.
			op := keyOp("mint-1", "h1")
			info, key, err := r.CreateAPIKey(ctx, op, tnA.ID, 24*time.Hour, "")
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(key, "hskey-api-"), "key format")
			assert.Equal(t, tnA.ID, info.TailnetID)
			assert.Contains(t, key, info.Prefix)

			again, key2, err := r.CreateAPIKey(ctx, op, tnA.ID, 24*time.Hour, "")
			require.NoError(t, err)
			assert.Empty(t, key2, "a replay does not show the key again")
			assert.Equal(t, info.ID, again.ID, "a replay returns the same key")
			assert.EqualValues(t, 1, count(t, gdb, `SELECT count(*) FROM credentials WHERE kind = 'api'`))

			_, _, err = r.CreateAPIKey(ctx, keyOp("mint-1", "other"), tnA.ID, 48*time.Hour, "")
			require.ErrorIs(t, err, ErrIdempotencyConflict)

			// Nothing secret is stored: no key, no secret in the audit or the
			// idempotency record.
			secret := key[strings.LastIndex(key, "-")+1:]
			for _, q := range []string{
				`SELECT count(*) FROM platform_audit WHERE detail LIKE ? OR target LIKE ?`,
				`SELECT count(*) FROM platform_idempotency WHERE response LIKE ? OR request_hash LIKE ?`,
			} {
				assert.Zero(t, count(t, gdb, q, "%"+secret+"%", "%"+secret+"%"), q)
			}

			assert.EqualValues(t, 1, count(t, gdb,
				`SELECT count(*) FROM platform_audit WHERE action = 'CreateAPIKey' AND target = ? AND outcome = 'ok'`,
				tnA.ID+"/apikey/"+strconv.FormatUint(info.ID, 10)))

			// The platform lookup resolves the key to its tailnet only.
			got, ident, err := p.TailnetForAPIKey(key)
			require.NoError(t, err)
			assert.Equal(t, tnA.ID, got.String())
			assert.Equal(t, info.Prefix, ident)

			for _, bad := range []string{
				"", "garbage", key + "x", key[:len(key)-1] + flip(key[len(key)-1]),
				strings.Replace(key, "hskey-api-", "hskey-auth-", 1),
				info.Prefix + "." + secret, // legacy format is not accepted
			} {
				_, _, err := p.TailnetForAPIKey(bad)
				require.ErrorIs(t, err, db.ErrUnknownTenant, "key %q", bad)
			}

			// Listing is per tailnet.
			keysA, err := r.ListAPIKeys(ctx, tnA.ID)
			require.NoError(t, err)
			require.Len(t, keysA, 1)

			keysB, err := r.ListAPIKeys(ctx, tnB.ID)
			require.NoError(t, err)
			assert.Empty(t, keysB)

			// Revoke: a key of A named under B is not found; under A it goes.
			_, err = r.RevokeAPIKey(ctx, testOp, tnB.ID, info.ID)
			require.ErrorIs(t, err, ErrNotFound, "A's key under B")

			_, _, err = p.TailnetForAPIKey(key)
			require.NoError(t, err, "still valid after the refused cross-tailnet revoke")

			rev, err := r.RevokeAPIKey(ctx, testOp, tnA.ID, info.ID)
			require.NoError(t, err)
			require.NotNil(t, rev.Revoked)

			_, _, err = p.TailnetForAPIKey(key)
			require.ErrorIs(t, err, db.ErrUnknownTenant, "revoked")

			_, err = r.RevokeAPIKey(ctx, testOp, tnA.ID, info.ID)
			require.ErrorIs(t, err, ErrNotFound, "revoking twice")

			// Expired keys do not resolve.
			_, key3, err := r.CreateAPIKey(ctx, testOp, tnB.ID, time.Hour, "")
			require.NoError(t, err)
			require.NoError(t, gdb.Exec(`UPDATE credentials SET expiration = ? WHERE kind = 'api' AND tailnet_id = ?`,
				time.Now().UTC().Add(-time.Minute), tnB.ID).Error)

			_, _, err = p.TailnetForAPIKey(key3)
			require.ErrorIs(t, err, db.ErrUnknownTenant, "expired")
		})
	}
}

func flip(c byte) string {
	if c == 'a' {
		return "b"
	}

	return "a"
}

// A key acts as a user of its own tailnet only.
func TestAPIKeyUserOfTailnet(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			gdb := b.open(t)
			r := newRegistry(t, gdb, Config{})
			addCluster(t, r, "c1", ClassShared, ClusterActive, 100)
			tnA := activeTailnet(t, r, "acme", ClassShared)
			tnB := activeTailnet(t, r, "beta", ClassShared)

			now := time.Now().UTC()
			ins := `INSERT INTO users (tailnet_id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`
			require.NoError(t, gdb.Exec(ins, tnA.ID, "alice", now, now).Error)
			require.NoError(t, gdb.Exec(ins, tnB.ID, "bob", now, now).Error)

			info, key, err := r.CreateAPIKey(ctx, testOp, tnA.ID, time.Hour, "alice")
			require.NoError(t, err)
			require.NotEmpty(t, key)
			assert.Equal(t, "alice", info.User)

			_, _, err = r.CreateAPIKey(ctx, testOp, tnA.ID, time.Hour, "bob")
			require.ErrorIs(t, err, ErrNotFound, "B's user named under A")

			_, _, err = r.CreateAPIKey(ctx, testOp, tnA.ID, time.Hour, "nobody")
			require.ErrorIs(t, err, ErrNotFound)

			keys, err := r.ListAPIKeys(ctx, tnA.ID)
			require.NoError(t, err)
			require.Len(t, keys, 1)
			assert.Equal(t, "alice", keys[0].User)
		})
	}
}
