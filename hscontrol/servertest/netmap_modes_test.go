package servertest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
)

// P14f review R4/R5: tests of changes that alter nothing a client holds run
// in both netmap modes and assert what each mode sends, counted on the
// server (no race with the client's reads).

// netmapModes are the two modes of tuning.incremental_netmap.
var netmapModes = []struct {
	name        string
	incremental bool
}{{"incremental", true}, {"kill-switch", false}}

func framesOf(t *testing.T, srv *servertest.TestServer, clients []*servertest.TestClient) []int64 {
	t.Helper()

	out := make([]int64, len(clients))
	for i, c := range clients {
		out[i] = srv.FramesDelivered(t, c)
	}

	return out
}

// assertNoopChangeFrames checks, after [servertest.TestServer.WaitMapsDelivered],
// the frames a change that alters nothing the clients hold produced: none
// with delta-first (ADR 0015 D2), one or more per client with the kill
// switch (the upstream policy response to every node).
func assertNoopChangeFrames(t *testing.T, srv *servertest.TestServer, clients []*servertest.TestClient, before []int64, what string) {
	t.Helper()

	for i, c := range clients {
		got := srv.FramesDelivered(t, c)
		if srv.IncrementalNetmap() {
			assert.Equal(t, before[i], got, "%s: delta-first sends %s no frame", what, c.Name)
		} else {
			assert.Greater(t, got, before[i], "%s: the kill switch sends %s the policy response", what, c.Name)
		}
	}
}
