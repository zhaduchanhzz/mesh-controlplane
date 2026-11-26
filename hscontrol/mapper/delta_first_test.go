package mapper

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types/change"
	"tailscale.com/tailcfg"
)

// P14f F3 (ADR 0015 D2): with the incremental netmap on, a change sends each
// connected node only what differs from what it already holds.

const (
	deltaUsers        = 20
	deltaNodesPerUser = 10

	// drainQuiet is how long no frame may arrive before a step counts as
	// delivered.
	drainQuiet = 300 * time.Millisecond
)

// deltaFrames is what one node received for one change.
type deltaFrames struct {
	frames []*tailcfg.MapResponse
}

func (d deltaFrames) peersChanged() int {
	n := 0
	for _, f := range d.frames {
		n += len(f.PeersChanged)
	}

	return n
}

func (d deltaFrames) fullPeers() bool {
	return slices.ContainsFunc(d.frames, func(f *tailcfg.MapResponse) bool { return f.Peers != nil })
}

func (d deltaFrames) filters() bool {
	return slices.ContainsFunc(d.frames, func(f *tailcfg.MapResponse) bool { return f.PacketFilters != nil })
}

func (d deltaFrames) removed() []tailcfg.NodeID {
	var r []tailcfg.NodeID
	for _, f := range d.frames {
		r = append(r, f.PeersRemoved...)
	}

	slices.Sort(r)

	return r
}

func (d deltaFrames) profiles() []tailcfg.UserProfile {
	var p []tailcfg.UserProfile
	for _, f := range d.frames {
		p = append(p, f.UserProfiles...)
	}

	return p
}

func (d deltaFrames) bytes(t *testing.T) int {
	t.Helper()

	n := 0

	for _, f := range d.frames {
		b, err := json.Marshal(f)
		require.NoError(t, err)

		n += len(b)
	}

	return n
}

// setupDeltaTailnet connects every node of a 200-node tailnet under pol and
// drains the initial maps.
func setupDeltaTailnet(t *testing.T, incremental bool, pol string) (*TestData, func()) {
	t.Helper()

	td, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, deltaUsers, deltaNodesPerUser, largeBufferSize)
	td.Config.Tuning.IncrementalNetmap.Enabled = incremental

	_, err := td.State.SetPolicy([]byte(pol))
	require.NoError(t, err)

	for i := range td.Nodes {
		n := &td.Nodes[i]
		require.NoError(t, td.Batcher.AddNode(n.n.ID, n.ch, tailcfg.CapabilityVersion(100), nil))
	}

	drainAll(t, td)

	return td, cleanup
}

// drainAll collects what every node receives until no frame arrives for
// drainQuiet.
func drainAll(t *testing.T, td *TestData) map[types.NodeID]deltaFrames {
	t.Helper()

	got := make(map[types.NodeID]deltaFrames, len(td.Nodes))
	deadline := time.Now().Add(20 * time.Second)

	for {
		moved := false

		for i := range td.Nodes {
			n := &td.Nodes[i]

			for {
				select {
				case f := <-n.ch:
					d := got[n.n.ID]
					d.frames = append(d.frames, f)
					got[n.n.ID] = d
					moved = true

					continue
				default:
				}

				break
			}
		}

		if !moved {
			<-time.After(drainQuiet)

			pending := false
			for i := range td.Nodes {
				pending = pending || len(td.Nodes[i].ch) > 0
			}

			if !pending {
				return got
			}
		}

		require.True(t, time.Now().Before(deadline), "frames kept arriving")
	}
}

func userName(i int) string { return fmt.Sprintf("testuser-%d@", i) }

func groupPolicy(rules ...string) string {
	g1, g2 := make([]string, 0, deltaUsers/2), make([]string, 0, deltaUsers/2)
	for i := range deltaUsers {
		if i < deltaUsers/2 {
			g1 = append(g1, fmt.Sprintf("%q", userName(i)))
		} else {
			g2 = append(g2, fmt.Sprintf("%q", userName(i)))
		}
	}

	return fmt.Sprintf(`{
		"groups": {"group:g1": [%s], "group:g2": [%s]},
		"tagOwners": {"tag:x": [%q]},
		"acls": [%s]
	}`, strings.Join(g1, ","), strings.Join(g2, ","), userName(0), strings.Join(rules, ","))
}

const allowAllTagPolicy = `{
	"tagOwners": {"tag:x": ["testuser-0@"]},
	"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}`

func inGroup1(td *TestData, id types.NodeID) bool {
	for i := range td.Nodes {
		if td.Nodes[i].n.ID == id {
			return i < len(td.Nodes)/2
		}
	}

	return false
}

// (a) Tagging one node of an allow-all tailnet: every other node gets that
// one peer, not the whole list.
func TestDeltaFirstTagChangeSendsOnlyThatPeer(t *testing.T) {
	for _, incremental := range []bool{true, false} {
		t.Run(fmt.Sprintf("incremental=%v", incremental), func(t *testing.T) {
			td, cleanup := setupDeltaTailnet(t, incremental, allowAllTagPolicy)
			defer cleanup()

			tagged := td.Nodes[0].n.ID

			_, c, err := td.State.SetNodeTags(tagged, []string{"tag:x"})
			require.NoError(t, err)
			td.Batcher.AddWork(c)

			got := drainAll(t, td)

			total, maxPer, bytes := 0, 0, 0

			for i := range td.Nodes {
				id := td.Nodes[i].n.ID
				d := got[id]
				total += d.peersChanged()
				maxPer = max(maxPer, d.peersChanged())
				bytes += d.bytes(t)

				if id == tagged || !incremental {
					continue
				}

				assert.False(t, d.fullPeers(), "node %d: no full peer list", id)
				assert.LessOrEqual(t, d.peersChanged(), 1, "node %d: only the tagged peer", id)
				assert.Empty(t, d.removed(), "node %d: nothing removed", id)
			}

			t.Logf("METRIC tag-change incremental=%v recipients=%d peersChanged_total=%d max_per_node=%d bytes=%d",
				incremental, len(td.Nodes), total, maxPer, bytes)
		})
	}
}

// (b) A policy edit that changes only group 1's own filter: group 2 gets
// nothing, group 1 gets its filter and no peer list. Then dropping the
// g1->g2 rule: each side gets the other side as removals, and no peers.
func TestDeltaFirstPolicyEditSendsOnlyWhatChanged(t *testing.T) {
	for _, incremental := range []bool{true, false} {
		t.Run(fmt.Sprintf("incremental=%v", incremental), func(t *testing.T) {
			g1g1 := `{"action": "accept", "src": ["group:g1"], "dst": ["group:g1:*"]}`
			g1g1ssh := `{"action": "accept", "src": ["group:g1"], "dst": ["group:g1:22"]}`
			g2g2 := `{"action": "accept", "src": ["group:g2"], "dst": ["group:g2:*"]}`
			g1g2 := `{"action": "accept", "src": ["group:g1"], "dst": ["group:g2:*"]}`

			td, cleanup := setupDeltaTailnet(t, incremental, groupPolicy(g1g1, g2g2, g1g2))
			defer cleanup()

			// Step 1: only group 1's own filter changes.
			_, err := td.State.SetPolicy([]byte(groupPolicy(g1g1ssh, g2g2, g1g2)))
			require.NoError(t, err)
			td.Batcher.AddWork(change.PolicyChange())

			got := drainAll(t, td)

			total, bytes := 0, 0

			for i := range td.Nodes {
				id := td.Nodes[i].n.ID
				d := got[id]
				total += d.peersChanged()
				bytes += d.bytes(t)

				if !incremental {
					continue
				}

				assert.Zero(t, d.peersChanged(), "node %d: peers did not change", id)
				assert.False(t, d.fullPeers(), "node %d: no full peer list", id)
				assert.Empty(t, d.removed(), "node %d: nothing removed", id)

				if inGroup1(td, id) {
					assert.True(t, d.filters(), "node %d (g1): its filter changed", id)
				} else {
					assert.False(t, d.filters(), "node %d (g2): its filter did not change", id)
					assert.Empty(t, d.frames, "node %d (g2): nothing to send", id)
				}
			}

			t.Logf("METRIC policy-edit incremental=%v peersChanged_total=%d bytes=%d", incremental, total, bytes)

			// Step 2: g1 and g2 stop seeing each other.
			_, err = td.State.SetPolicy([]byte(groupPolicy(g1g1ssh, g2g2)))
			require.NoError(t, err)
			td.Batcher.AddWork(change.PolicyChange())

			got = drainAll(t, td)

			var g1IDs, g2IDs []tailcfg.NodeID

			for i := range td.Nodes {
				if i < len(td.Nodes)/2 {
					g1IDs = append(g1IDs, td.Nodes[i].n.ID.NodeID())
				} else {
					g2IDs = append(g2IDs, td.Nodes[i].n.ID.NodeID())
				}
			}

			total, bytes = 0, 0

			for i := range td.Nodes {
				id := td.Nodes[i].n.ID
				d := got[id]
				total += d.peersChanged()
				bytes += d.bytes(t)

				// Removal is required in both modes (isolation).
				if inGroup1(td, id) {
					assert.Equal(t, g2IDs, d.removed(), "node %d (g1): all of g2 removed", id)
				} else {
					assert.Equal(t, g1IDs, d.removed(), "node %d (g2): all of g1 removed", id)
				}

				if incremental {
					assert.Zero(t, d.peersChanged(), "node %d: remaining peers did not change", id)
				}
			}

			t.Logf("METRIC policy-revoke incremental=%v peersChanged_total=%d bytes=%d", incremental, total, bytes)
		})
	}
}

// (c) A user's profile changes: no node gets a full map, and the nodes that
// see that user's nodes get the new profile.
func TestDeltaFirstUserProfileChangeNoFullMap(t *testing.T) {
	for _, incremental := range []bool{true, false} {
		t.Run(fmt.Sprintf("incremental=%v", incremental), func(t *testing.T) {
			td, cleanup := setupDeltaTailnet(t, incremental, allowAllTagPolicy)
			defer cleanup()

			user := td.Users[3]

			_, c, err := td.State.UpdateUser(types.UserID(user.ID), func(u *types.User) error {
				u.DisplayName = "Renamed Three"
				return nil
			})
			require.NoError(t, err)
			require.False(t, c.IsEmpty())

			if incremental {
				assert.False(t, c.IsFull(), "user change must not be a full update")
			}

			td.Batcher.AddWork(c)

			got := drainAll(t, td)

			total, bytes, full := 0, 0, 0

			for i := range td.Nodes {
				id := td.Nodes[i].n.ID
				d := got[id]
				total += d.peersChanged()
				bytes += d.bytes(t)

				if d.fullPeers() {
					full++
				}

				found := slices.ContainsFunc(d.profiles(), func(p tailcfg.UserProfile) bool {
					return p.DisplayName == "Renamed Three"
				})
				assert.True(t, found, "node %d: new profile delivered", id)

				if incremental {
					assert.False(t, d.fullPeers(), "node %d: no full map", id)
					assert.Zero(t, d.peersChanged(), "node %d: no peer changed", id)
				}
			}

			t.Logf("METRIC user-profile incremental=%v full_maps=%d peersChanged_total=%d bytes=%d",
				incremental, full, total, bytes)
		})
	}
}
