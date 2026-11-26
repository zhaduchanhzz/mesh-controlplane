//go:build p05cluster

package servertest

import (
	"path/filepath"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P13 helpers.

// NewClusterOn builds, without starting it, a process of tc's cluster on
// the SQLite database at path (its own Noise key), and returns NewCluster's
// error: what `headscale cluster serve` does at start on that database.
func (tc *TestCluster) NewClusterOn(tb testing.TB, path string) error {
	tb.Helper()

	cfg := *tc.cfg
	cfg.Database.Type = types.DatabaseSqlite
	cfg.Database.Sqlite.Path = path
	cfg.Noise.PrivateKeyPath = filepath.Join(tb.TempDir(), "noise_private.key")

	_, err := hscontrol.NewCluster(&cfg)

	return err
}
