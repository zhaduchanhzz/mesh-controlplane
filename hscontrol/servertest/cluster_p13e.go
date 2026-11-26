//go:build p05cluster

package servertest

import (
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
)

// P13e (ADR 0014): rotating the cluster's Noise key.

// TryNewCluster builds, without starting it, one more process of tc's
// cluster whose Noise key file is keyPath, and returns NewCluster's error.
// A process that was built is not started (its pool stays open).
func (tc *TestCluster) TryNewCluster(tb testing.TB, keyPath string, opts ...hscontrol.ClusterOption) error {
	tb.Helper()

	cfg := *tc.cfg
	cfg.Noise.PrivateKeyPath = keyPath

	_, err := hscontrol.NewCluster(&cfg, opts...)

	return err
}
