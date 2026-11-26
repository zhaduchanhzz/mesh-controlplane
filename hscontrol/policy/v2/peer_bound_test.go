package v2

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"pgregory.net/rapid"
)

// P14f review R2: policy writes are held to the tailnet's peer bound.

// TestCountPeerEntriesMatchesBuild: the counting build gives the entries of
// the full build, and stops once past the limit.
func TestCountPeerEntriesMatchesBuild(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nodes := drawPeerMapNodes(rt)
		polB := drawPeerMapPolicy(rt)

		pm, err := NewPolicyManager(polB, scanUsers, nodes.ViewSlice(), WithEmptyPolicyDenies(rapid.Bool().Draw(rt, "emptyDenies")))
		if err != nil {
			rt.Skipf("policy rejected: %v", err)
		}

		var want int64
		for _, peers := range pm.BuildPeerMap(nodes.ViewSlice()) {
			want += int64(len(peers))
		}

		pm.mu.RLock()
		got := pm.countPeerEntriesLocked(nodes.ViewSlice(), want+1)

		var stopped int64
		if want > 0 {
			stopped = pm.countPeerEntriesLocked(nodes.ViewSlice(), want-1)
		}

		pm.mu.RUnlock()

		require.Equal(t, want, got, "count with room")

		if want > 0 {
			require.Greater(t, stopped, want-1, "count past the limit")
			require.LessOrEqual(t, stopped, want)
		}
	})
}

func TestValidatePolicyPeerEntriesBound(t *testing.T) {
	// Four users, one node each: allow-all gives 4*3 = 12 entries.
	users := scanUsers[:4]
	nodes := make(types.Nodes, 0, len(users))

	for i := range users {
		v4 := netip.AddrFrom4([4]byte{100, 64, 0, byte(i + 1)}) //nolint:gosec // test
		uid := users[i].ID
		nodes = append(nodes, &types.Node{
			ID: types.NodeID(i + 1), Hostname: fmt.Sprintf("n%d", i+1), //nolint:gosec // test
			IPv4: &v4, UserID: &uid, User: &users[i],
		})
	}

	allow := []byte(`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`)

	err := ValidatePolicy(allow, users, nodes.ViewSlice(), WithEmptyPolicyDenies(true), WithPeerEntriesBound(6, 0))
	require.ErrorIs(t, err, ErrPeerEntriesBound, "12 entries over a bound of 6")

	require.NoError(t, ValidatePolicy(allow, users, nodes.ViewSlice(), WithEmptyPolicyDenies(true), WithPeerEntriesBound(12, 0)),
		"exactly at the bound")
	require.NoError(t, ValidatePolicy(allow, users, nodes.ViewSlice(), WithEmptyPolicyDenies(true), WithPeerEntriesBound(6, 12)),
		"a tailnet already at 12 may keep 12")
	require.NoError(t, ValidatePolicy(allow, users, nodes.ViewSlice(), WithEmptyPolicyDenies(true)),
		"no bound")
	assert.NoError(t, ValidatePolicy([]byte(`{"grants":[]}`), users, nodes.ViewSlice(), WithEmptyPolicyDenies(true), WithPeerEntriesBound(1, 0)),
		"deny-all gives no entries")
}
