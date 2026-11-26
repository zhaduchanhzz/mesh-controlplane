package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"tailscale.com/types/key"
)

// P13e (ADR 0014 §1, §3): `cluster noise-key install` puts a new private
// key in place atomically (0600, validated, nothing half-written), and
// `cluster noise-key rotate` names its public key in the registry; `show`
// compares the two.

func writeKeyFile(t *testing.T, k key.MachinePrivate) string {
	t.Helper()

	text, err := k.MarshalText()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "next.key")
	require.NoError(t, os.WriteFile(path, text, 0o600))

	return path
}

func TestNoiseKeyInstall(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "noise_private.key")

	// --generate on a missing file.
	pub1, err := installNoiseKey(target, "", true)
	require.NoError(t, err)

	k1, err := config.ReadPrivateKey(target)
	require.NoError(t, err)
	assert.Equal(t, k1.Public().String(), pub1)

	st, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	// --from a file that is not a private key: refused, target unchanged.
	bad := filepath.Join(t.TempDir(), "bad.key")
	require.NoError(t, os.WriteFile(bad, []byte("mkey:"+strings.Repeat("ab", 32)), 0o600))

	_, err = installNoiseKey(target, bad, false)
	require.Error(t, err)

	same, err := config.ReadPrivateKey(target)
	require.NoError(t, err)
	assert.True(t, same.Equal(*k1), "a refused key leaves the installed one")

	// --from a valid key replaces it.
	k2 := key.NewMachine()

	pub2, err := installNoiseKey(target, writeKeyFile(t, k2), false)
	require.NoError(t, err)
	assert.Equal(t, k2.Public().String(), pub2)

	now, err := config.ReadPrivateKey(target)
	require.NoError(t, err)
	assert.True(t, now.Equal(k2))

	// Exactly one of --from and --generate; never the target itself.
	_, err = installNoiseKey(target, "", false)
	require.Error(t, err)
	_, err = installNoiseKey(target, bad, true)
	require.Error(t, err)
	_, err = installNoiseKey(target, target, false)
	require.Error(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file left behind")
}

func TestNoiseKeyRotateAndShow(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			p := p12Platform(t, backend, "c1")
			p12Register(t, p, "c1", placement.ClassShared, placement.ClusterActive)

			k1 := key.NewMachine()
			path := writeKeyFile(t, k1)

			st, err := noiseKeyStatus(p, path)
			require.NoError(t, err)
			assert.Equal(t, k1.Public().String(), st.FilePublicKey)
			assert.Empty(t, st.RegisteredPublicKey)
			assert.False(t, st.Match)

			// Default: the public key of the local key file.
			c, err := rotateNoiseKeyCLI(t.Context(), p, "c1", "", path)
			require.NoError(t, err)
			assert.Equal(t, k1.Public().String(), c.NoisePublicKey)

			st, err = noiseKeyStatus(p, path)
			require.NoError(t, err)
			assert.True(t, st.Match)

			// --public-key, validated.
			_, err = rotateNoiseKeyCLI(t.Context(), p, "c1", "mkey:nope", path)
			require.ErrorIs(t, err, placement.ErrInvalid)

			k2 := key.NewMachine().Public().String()
			c, err = rotateNoiseKeyCLI(t.Context(), p, "c1", k2, path)
			require.NoError(t, err)
			assert.Equal(t, k2, c.NoisePublicKey)

			st, err = noiseKeyStatus(p, path)
			require.NoError(t, err)
			assert.False(t, st.Match, "the local file is the old key now")

			// A missing key file is reported, not an error of show.
			st, err = noiseKeyStatus(p, filepath.Join(t.TempDir(), "missing.key"))
			require.NoError(t, err)
			assert.Empty(t, st.FilePublicKey)
			assert.NotEmpty(t, st.FileError)
		})
	}
}
