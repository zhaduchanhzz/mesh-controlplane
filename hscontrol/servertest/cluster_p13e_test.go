//go:build p05cluster

package servertest_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"tailscale.com/types/key"
)

// P13e (ADR 0014 §3, owner D8): the active process re-checks the registered
// Noise key on every reconcile tick (2 s). After the operator rotates the
// key in the registry (an emergency rotation: the old key is considered
// leaked), a process still serving with the old key must stop serving and
// give the lease back within about one tick, instead of serving until its
// next takeover. It must not take the lease again while the registry names
// another key, never overwrites the registered key, and serves again once
// the registry names its key.
func TestP13eActiveStepsDownOnRegistryKeyChange(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "p13e-rot"})
	p := tc.Primary()
	tc.WaitReady(t, p, clusterWait)

	own := tc.NoiseKey(t).String()
	require.Equal(t, own, tc.RegisteredNoiseKey(t), "recorded at the first lease")

	epoch := tc.LeaseRow(t).Epoch
	rotated := key.NewMachine().Public().String()

	tc.SetRegisteredNoiseKey(t, rotated)
	start := time.Now()

	require.Eventually(t, func() bool { return tc.ProcessGet(t, p, "/ready") != http.StatusOK },
		6*time.Second, 25*time.Millisecond, "a process whose key is no longer the registered one must stop serving")
	t.Logf("stopped serving %s after the registry change", time.Since(start).Round(time.Millisecond))

	require.Eventually(t, func() bool { return tc.LeaseRow(t).Expired }, 2*time.Second, 25*time.Millisecond,
		"the lease is given back, not left to expire")
	t.Logf("lease given back %s after the registry change", time.Since(start).Round(time.Millisecond))

	assert.Never(t, func() bool { return tc.ProcessGet(t, p, "/ready") == http.StatusOK }, 3*time.Second, 100*time.Millisecond,
		"no serving while the registry names another key")
	assert.Equal(t, rotated, tc.RegisteredNoiseKey(t), "the registered key is never overwritten by the process")

	tc.SetRegisteredNoiseKey(t, own)
	tc.WaitReady(t, p, clusterWait)
	assert.Greater(t, tc.LeaseRow(t).Epoch, epoch, "serves again under a new lease")
}

// P13e (ADR 0014 §3): a cluster process never creates a Noise key on its
// own. The key file missing (a VM rebuilt, a wrong path) used to give the
// process a new key, which the registry then refused at the lease: safe,
// but a silent key change that is hard to diagnose.
func TestP13eMissingNoiseKeyRefused(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: "p13e-missing"})
	tc.WaitReady(t, tc.Primary(), clusterWait)

	missing := filepath.Join(t.TempDir(), "noise_private.key")

	err := tc.TryNewCluster(t, missing)
	require.Error(t, err, "a cluster process must not start without its Noise key file")
	assert.NoFileExists(t, missing, "no key is created")
	require.ErrorIs(t, err, hscontrol.ErrNoiseKeyMissing)
	t.Logf("refused: %v", err)

	// --init-noise-key creates a key only for a cluster whose registry
	// names none: this one has its key recorded.
	err = tc.TryNewCluster(t, missing, hscontrol.WithNoiseKeyInit())
	require.ErrorIs(t, err, hscontrol.ErrNoiseKeyMissing)
	assert.Contains(t, err.Error(), tc.RegisteredNoiseKey(t), "the error names the registered key")
	assert.NoFileExists(t, missing)

	// A key file that does not parse is an error, never replaced.
	bad := filepath.Join(t.TempDir(), "noise_private.key")
	require.NoError(t, os.WriteFile(bad, []byte("not a key\n"), 0o600))

	err = tc.TryNewCluster(t, bad, hscontrol.WithNoiseKeyInit())
	require.Error(t, err)

	b, err := os.ReadFile(bad)
	require.NoError(t, err)
	assert.Equal(t, "not a key\n", string(b))
}

// P13e (ADR 0014 §1): the planned rotation, in process. The new key is
// installed for a process that starts as standby; registering its public
// key in the registry (the CLI's RotateNoiseKey) makes the active, still on
// the old key, step down; the standby takes the lease and serves, and /key
// through the front door now answers the new key. Client recovery is
// measured in the lab drill, not here.
func TestP13ePlannedRotationSwitchover(t *testing.T) {
	tc := servertest.NewTestClusterWith(t, servertest.ClusterOptions{FrontDoor: true}, servertest.TailnetSeed{Slug: "p13e-plan"})
	a := tc.Primary()
	tc.WaitReady(t, a, clusterWait)

	oldKey := tc.NoiseKey(t)

	next := key.NewMachine()
	path := filepath.Join(t.TempDir(), "noise_private.key")
	require.NoError(t, hscontrol.WriteNoiseKeyFile(path, next))

	b := tc.StartProcessWithNoiseKey(t, path)
	assert.Never(t, func() bool { return tc.ProcessGet(t, b, "/ready") == http.StatusOK }, time.Second, 50*time.Millisecond,
		"standby while the active holds the lease")
	assert.Equal(t, oldKey, tc.NoiseKey(t))

	start := time.Now()

	_, err := tc.Registry(t).RotateNoiseKey(t.Context(), servertest.TestOperator, tc.ClusterID, next.Public().String())
	require.NoError(t, err)

	took := tc.WaitReady(t, b, clusterWait)
	t.Logf("process with the new key serving %s after the registry rotation", took.Round(time.Millisecond))

	assert.NotEqual(t, http.StatusOK, tc.ProcessGet(t, a, "/ready"), "the old-key process is standby")
	// NoiseKey uses the default transport: a kept-alive connection through
	// the front door (a TCP proxy) still reaches the old process.
	http.DefaultClient.CloseIdleConnections()
	assert.Equal(t, next.Public(), tc.NoiseKey(t), "/key answers the new key")
	assert.Less(t, time.Since(start), 10*time.Second)
}
