// Package endpointaudit_test holds P08a RED tests that touch process-global
// state (http.DefaultTransport) and therefore must run in their own test
// binary, away from the hscontrol package tests.
package endpointaudit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	derpServer "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/derp/server"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// tailnetRuntimeConfig is one tailnet runtime as serve-tailnets (P02 POC)
// starts it: its own DB, Noise key and embedded DERP with verify_clients on.
func tailnetRuntimeConfig(t *testing.T, name string, regionID tailcfg.DERPRegionID) *types.Config {
	t.Helper()

	dir := t.TempDir()
	v4 := netip.MustParsePrefix("100.64.0.0/10")
	v6 := netip.MustParsePrefix("fd7a:115c:a1e0::/48")

	cfg := config.Default()
	cfg.ServerURL = "https://" + name + ".lab.invalid"
	cfg.Noise.PrivateKeyPath = filepath.Join(dir, "noise.key")
	cfg.Database.Type = types.DatabaseSqlite
	cfg.Database.Sqlite.Path = filepath.Join(dir, "db.sqlite")
	cfg.Prefixes.V4 = &v4
	cfg.Prefixes.V6 = &v6
	cfg.Policy.Mode = types.PolicyModeDB
	cfg.PolicyEmptyDenies = true
	cfg.DNSConfig.MagicDNS = false
	cfg.DNSConfig.OverrideLocalDNS = false
	cfg.DNSConfig.BaseDomain = ""
	cfg.UnixSocket = filepath.Join(dir, "hs.sock")
	cfg.DERP.URLs = nil
	cfg.DERP.Server = types.DERPServerConfig{
		Enabled:                            true,
		VerifyClients:                      true,
		AutomaticallyAddEmbeddedDerpRegion: true,
		PrivateKeyPath:                     filepath.Join(dir, "derp.key"),
		RegionID:                           regionID,
		RegionCode:                         name,
		STUNAddr:                           "127.0.0.1:0",
	}

	return &cfg
}

var errPanic = errors.New("panic")

// newRuntime runs NewHeadscale and turns a panic into an error.
//
//nolint:nonamedreturns // recover can only replace the result through a named return
func newRuntime(cfg *types.Config) (app *hscontrol.Headscale, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errPanic, r)
		}
	}()

	return hscontrol.NewHeadscale(cfg)
}

// derpVerify is exactly what an embedded DERP server does to verify a
// client: POST a DERPAdmitClientRequest to its verify URL over
// http.DefaultClient. A transport error counts as "not admitted": the DERP
// server runs with fail-open off and refuses the client.
func derpVerify(t *testing.T, verifyURL string, nodeKey key.NodePublic) (bool, error) {
	t.Helper()

	body, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: nodeKey})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, verifyURL, bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}

	defer resp.Body.Close()

	var out tailcfg.DERPAdmitClientResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	return out.Allow, nil
}

// TestP08aDERPVerifyTwoTailnetsOneProcess (S15, I11): with two tailnet
// runtimes in one process, tailnet B's embedded DERP must not admit a node
// key of tailnet A.
//
// P08a (RED): app.go registered the DERP verify transport on the
// process-global http.DefaultTransport under one fixed URL
// ("headscale-derp-verify://verify"); the second runtime panicked and the
// first answered for every DERP in the process.
//
// P08 (GREEN): each runtime registers its own verify URL
// ([derpServer.RegisterVerifyHandler]) and its DERP server verifies against
// that URL only. Checks:
//   - both runtimes start in one process;
//   - B's verify URL refuses A's node (the isolation assertion);
//   - A's verify URL admits A's node (control: verify works at all);
//   - the old fixed URL routes to no runtime (refused, not admitted).
func TestP08aDERPVerifyTwoTailnetsOneProcess(t *testing.T) {
	appA, err := newRuntime(tailnetRuntimeConfig(t, "tailnet-a", 901))
	require.NoError(t, err, "tailnet A runtime")

	t.Cleanup(func() { _ = appA.GetState().Close() })

	user := appA.GetState().CreateUserForTest("a-user")
	nodeA := appA.GetState().CreateNodeForTest(user, "a-node")
	appA.GetState().PutNodeInStoreForTest(*nodeA)

	appB, err := newRuntime(tailnetRuntimeConfig(t, "tailnet-b", 902))
	if appB != nil {
		t.Cleanup(func() { _ = appB.GetState().Close() })
	}

	require.NoError(t, err,
		"S15/I11: tailnet B runtime with embedded DERP verify must start next to tailnet A in one process")

	urlA, urlB := appA.DERPVerifyURL(), appB.DERPVerifyURL()
	require.NotEmpty(t, urlA)
	require.NotEmpty(t, urlB)
	require.NotEqual(t, urlA, urlB, "each runtime must have its own DERP verify URL")

	// What B's embedded DERP asks when tailnet A's node connects to it.
	admitted, err := derpVerify(t, urlB, nodeA.NodeKey)
	require.NoError(t, err, "B's verify URL must be routed to runtime B")
	assert.False(t, admitted,
		"S15/I11: tailnet B's DERP admitted tailnet A's node key; it would relay for a node that is not in tailnet B")

	// Control: A's own DERP admits A's live node.
	admitted, err = derpVerify(t, urlA, nodeA.NodeKey)
	require.NoError(t, err)
	assert.True(t, admitted, "control: tailnet A's DERP must admit tailnet A's live node")

	// The old fixed URL must not reach any runtime.
	admitted, err = derpVerify(t, derpServer.DerpVerifyScheme+"://verify", nodeA.NodeKey)
	require.Error(t, err, "the old process-wide verify URL must not be routed to any runtime")
	assert.False(t, admitted)
}
