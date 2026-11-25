package mapper

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P12b (owner Q1): a planned move reaches the client in the map response:
// DisplayMessages for clients that understand them (capver >= 117), Health
// for older ones; never the target cluster; once cleared, the message is
// deleted.
func TestMoveNoticeInMapResponse(t *testing.T) {
	st := &state.State{}
	m := &mapper{cfg: &types.Config{}, state: st}
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

	resp, err := m.NewMapResponseBuilder(1).WithCapabilityVersion(117).WithMoveNotice().Build()
	require.NoError(t, err)
	assert.Nil(t, resp.DisplayMessages, "nothing to say, nothing ever said")
	assert.Nil(t, resp.Health)

	require.True(t, st.SetMoveNotice(&types.MoveNotice{At: at, Reason: "rebalance"}))
	require.False(t, st.SetMoveNotice(&types.MoveNotice{At: at, Reason: "rebalance"}), "same notice: no change")

	resp, err = m.NewMapResponseBuilder(1).WithCapabilityVersion(117).WithMoveNotice().Build()
	require.NoError(t, err)
	dm := resp.DisplayMessages[MoveNoticeID]
	require.NotNil(t, dm)
	assert.Contains(t, dm.Title, "move")
	assert.Contains(t, dm.Text, "2026-11-27 10:00 UTC")
	assert.Contains(t, dm.Text, "rebalance")
	assert.Contains(t, dm.Text, "--force-reauth")
	assert.Nil(t, resp.Health, "either Health or DisplayMessages, not both")

	old, err := m.NewMapResponseBuilder(1).WithCapabilityVersion(116).WithMoveNotice().Build()
	require.NoError(t, err)
	require.Len(t, old.Health, 1)
	assert.True(t, strings.Contains(old.Health[0], "2026-11-27 10:00 UTC"))
	assert.Nil(t, old.DisplayMessages)

	require.True(t, st.SetMoveNotice(nil))

	resp, err = m.NewMapResponseBuilder(1).WithCapabilityVersion(117).WithMoveNotice().Build()
	require.NoError(t, err)
	v, ok := resp.DisplayMessages[MoveNoticeID]
	assert.True(t, ok, "the cleared notice is deleted on the client")
	assert.Nil(t, v)
	_ = tailcfg.DisplayMessageID("")
}
