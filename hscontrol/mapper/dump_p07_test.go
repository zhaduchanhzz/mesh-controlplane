package mapper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
)

// TestDebugDumpPerTailnet (P07): in cluster mode MapResponse dumps go under
// the tailnet's ID, so two tenants never share a dump directory; a single
// tailnet keeps the upstream layout <dir>/<node>.
func TestDebugDumpPerTailnet(t *testing.T) {
	dir := t.TempDir()
	old := debugDumpMapResponsePath
	debugDumpMapResponsePath = dir

	t.Cleanup(func() { debugDumpMapResponsePath = old })

	writeDebugMapResponse(&tailcfg.MapResponse{}, fullResponseDebug, "tailnet-a", 7)
	writeDebugMapResponse(&tailcfg.MapResponse{}, fullResponseDebug, "tailnet-b", 7)
	writeDebugMapResponse(&tailcfg.MapResponse{}, fullResponseDebug, "", 7)

	for _, sub := range []string{"tailnet-a/7", "tailnet-b/7", "7"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		require.NoError(t, err, sub)
		require.Len(t, entries, 1, sub)
	}

	require.Empty(t, (&mapper{cfg: &types.Config{}}).dumpSubdir(), "single tailnet: no tailnet level")
	require.Empty(t, (*mapper)(nil).dumpSubdir())
}
