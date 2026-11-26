package hscontrol

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func counterValue(t *testing.T, c interface{ Write(m *dto.Metric) error }) float64 {
	t.Helper()

	var m dto.Metric
	require.NoError(t, c.Write(&m))

	return m.GetCounter().GetValue()
}

// P09, S17 (owner decision Q5): the P12 E2E saw one MapResponse counted by a
// process resumed after SIGSTOP past its lease. The servable() gate ran
// before the frame was marshalled and compressed, so a pause anywhere in
// that work let a frame out after the lease deadline. A lease lost between
// encoding and the socket write must stop the frame: nothing written,
// nothing counted, the last-frame times unchanged.
//
// Not parallel: mapWriteHook is package-wide.
func TestP09NoFrameWrittenWhenLeaseEndsBeforeWrite(t *testing.T) {
	app := createTestApp(t)

	user := app.state.CreateUserForTest("p09-frame-gate")
	pak, err := app.state.CreatePreAuthKey(user.TypedID(), false, false, nil, nil)
	require.NoError(t, err)

	nodeKey := key.NewNode()

	_, err = app.handleRegister(context.Background(), tailcfg.RegisterRequest{
		Auth:     &tailcfg.RegisterResponseAuth{AuthKey: pak.Key},
		NodeKey:  nodeKey.Public(),
		Hostinfo: &tailcfg.Hostinfo{Hostname: "p09-frame-gate"},
		Expiry:   time.Now().Add(24 * time.Hour),
	}, key.NewMachine().Public())
	require.NoError(t, err)

	node, ok := app.state.GetNodeByNodeKey(nodeKey.Public())
	require.True(t, ok)

	var held atomic.Bool
	held.Store(true)
	app.leaseGate = held.Load

	w := &recordingResponseWriter{}
	session := app.newMapSession(context.Background(), tailcfg.MapRequest{
		NodeKey: nodeKey.Public(),
		Stream:  true,
		Version: tailcfg.CapabilityVersion(100),
	}, w, node)

	sent := mapResponseSent.WithLabelValues("ok", "keepalive")
	refused := mapResponseNotServable.WithLabelValues("before_write")

	// Lease held: the frame is written and recorded.
	require.NoError(t, session.writeMap(&keepAlive))
	require.Equal(t, 1, w.writes)

	gate0, done0 := lastFrameTimes()
	require.NotZero(t, done0)
	require.LessOrEqual(t, gate0, done0)

	sent0, refused0 := counterValue(t, sent), counterValue(t, refused)

	// The lease ends after the first check, while the frame is encoded (in
	// the E2E: SIGSTOP there, SIGCONT past the deadline).
	mapWriteHook = func() { held.Store(false) }

	t.Cleanup(func() { mapWriteHook = nil })

	err = session.writeMap(&keepAlive)
	require.ErrorIs(t, err, errMapSessionStopped)

	assert.Equal(t, 1, w.writes, "no frame written after the lease ended")
	assert.InDelta(t, sent0, counterValue(t, sent), 0, "not counted as sent")
	assert.InDelta(t, refused0+1, counterValue(t, refused), 0, "counted as refused before the write")

	gate1, done1 := lastFrameTimes()
	assert.Equal(t, gate0, gate1, "last-frame times unchanged")
	assert.Equal(t, done0, done1)
}
