// Package endpointaudit_test holds P08a RED tests that touch process-global
// state (http.DefaultTransport) and therefore must run in their own test
// binary, away from the hscontrol package tests.
package endpointaudit_test

import (
	"bytes"
	"encoding/json"
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

// newRuntime runs NewHeadscale and turns a panic into an error.
func newRuntime(cfg *types.Config) (app *hscontrol.Headscale, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	return hscontrol.NewHeadscale(cfg)
}

// derpVerify is exactly what an embedded DERP server does to verify a
// client: POST a DERPAdmitClientRequest to its verify URL over
// http.DefaultClient.
func derpVerify(t *testing.T, nodeKey key.NodePublic) bool {
	t.Helper()

	body, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: nodeKey})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		derpServer.DerpVerifyScheme+"://verify", bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	var out tailcfg.DERPAdmitClientResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	return out.Allow
}

// TestP08aDERPVerifyTwoTailnetsOneProcess (S15, I11): with two tailnet
// runtimes in one process (the serve-tailnets POC, the only multi-tailnet
// mode on master), tailnet B's embedded DERP must not admit a node key of
// tailnet A.
//
// Expected today: RED. app.go registers the DERP verify transport on the
// process-global http.DefaultTransport under one fixed scheme
// ("headscale-derp-verify://verify"). The second runtime's NewHeadscale
// panics ("protocol already registered"), so serve-tailnets cannot run two
// tailnets with embedded DERP verify at all; and the one registered handler
// answers for every DERP in the process, so B's DERP would consult A's state.
//
// Once P05 lands (one runtime, many tailnets, shared PostgreSQL), the same
// assertion runs against a single runtime: a DERP server whose identity is
// scoped to tailnet B must refuse a node key of tailnet A. That variant needs
// a tailnet-scoped verify API that does not exist yet; see
// evidence/P08a-endpoint-inventory.md, section "Two-tenant verify after P05".
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

	assert.NoError(t, err,
		"RED S15/I11: tailnet B runtime with embedded DERP verify must start next to tailnet A in one process")

	// What B's embedded DERP asks when tailnet A's node connects to it.
	admitted := derpVerify(t, nodeA.NodeKey)
	assert.False(t, admitted,
		"RED S15/I11: the DERP verify path every runtime in the process shares admitted tailnet A's node key; "+
			"tailnet B's DERP would relay for a node that is not in tailnet B")
}
