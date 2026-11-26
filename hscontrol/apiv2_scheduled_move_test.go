package hscontrol

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P12b (owner Q3): the tailnet's own API shows a planned move read-only:
// time and reason, never the target cluster; nothing when none is planned.
func TestAPIv2ScheduledMove(t *testing.T) {
	st := &state.State{}
	_, api := humatest.New(t, apiv2.Config())
	apiv2.Register(api, apiv2.Backend{State: st, Cfg: &types.Config{}})

	get := func() apiv2.ScheduledMove {
		t.Helper()

		resp := api.Get("/api/v2/tailnet/-/scheduled-move")
		require.Equalf(t, http.StatusOK, resp.Code, "body: %s", resp.Body)

		var m apiv2.ScheduledMove
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &m))

		return m
	}

	assert.Equal(t, apiv2.ScheduledMove{}, get(), "none planned")

	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	st.SetMoveNotice(&types.MoveNotice{At: at, Reason: "rebalance"})

	m := get()
	assert.True(t, m.Scheduled)
	require.NotNil(t, m.ScheduledAt)
	assert.True(t, m.ScheduledAt.Equal(at))
	assert.Equal(t, "rebalance", m.Reason)

	assert.Equal(t, http.StatusNotFound, api.Get("/api/v2/tailnet/other/scheduled-move").Code, "only its own tailnet")
	assert.Equal(t, http.StatusMethodNotAllowed, api.Post("/api/v2/tailnet/-/scheduled-move", map[string]any{}).Code, "read-only")

	st.SetMoveNotice(nil)
	assert.Equal(t, apiv2.ScheduledMove{}, get(), "cleared")
}
