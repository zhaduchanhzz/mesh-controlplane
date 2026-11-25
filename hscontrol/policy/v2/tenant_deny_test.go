package v2

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/types/views"
)

// P03a: with [WithEmptyPolicyDenies] a policy without access rules denies
// everything (no filter rules, no peers, no SSH). Without it the upstream
// semantics hold: no acls and no grants allow everything.

const tenantSSHAllPolicy = `{
	"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
	"ssh": [{"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["root"]}]
}`

func tenantDenyFixture() (types.Users, types.Nodes) {
	users := types.Users{
		{ID: 1, Name: "alice", Email: "alice@example.com"},
		{ID: 2, Name: "bob", Email: "bob@example.com"},
	}

	a1 := node("a1", "100.64.0.1", "fd7a:115c:a1e0::1", users[0])
	a1.ID = 1
	a2 := node("a2", "100.64.0.2", "fd7a:115c:a1e0::2", users[0])
	a2.ID = 2
	b1 := node("b1", "100.64.0.3", "fd7a:115c:a1e0::3", users[1])
	b1.ID = 3

	return users, types.Nodes{a1, a2, b1}
}

func writeEmptyPolicyFile(t *testing.T) []byte {
	t.Helper()

	p := filepath.Join(t.TempDir(), "policy.hujson")
	require.NoError(t, os.WriteFile(p, nil, 0o600))

	b, err := os.ReadFile(p)
	require.NoError(t, err)

	return b
}

// roundTrip parses b and marshals the result again, as a store or API that
// re-encodes the policy would. The omitempty on acls/grants turns
// {"grants":[]} into a document without grants.
func roundTrip(t *testing.T, b []byte) []byte {
	t.Helper()

	pol, err := unmarshalPolicy(b)
	require.NoError(t, err)

	out, err := json.Marshal(pol)
	require.NoError(t, err)

	return out
}

type tenantDenyInput struct {
	name string
	pol  func(t *testing.T) []byte
	// upstreamAllows is what the input means with the switch off: true for
	// allow-all, false for the explicit empty rule list that denies.
	upstreamAllows bool
}

func tenantDenyInputs() []tenantDenyInput {
	lit := func(s string) func(*testing.T) []byte {
		return func(*testing.T) []byte { return []byte(s) }
	}
	rt := func(s string) func(*testing.T) []byte {
		return func(t *testing.T) []byte {
			t.Helper()

			return roundTrip(t, []byte(s))
		}
	}

	return []tenantDenyInput{
		{"nil", func(*testing.T) []byte { return nil }, true},
		{"empty-bytes", lit(""), true},
		{"empty-file", writeEmptyPolicyFile, true},
		{"empty-object", lit("{}"), true},
		{"json-null", lit("null"), true},
		{"comment-only-object", lit("// nothing yet\n{}"), true},
		{"empty-grants", lit(`{"grants":[]}`), false},
		{"empty-acls", lit(`{"acls":[]}`), false},
		{"empty-acls-and-grants", lit(`{"acls":[],"grants":[]}`), false},
		// Round trips: the omitempty trap. With the switch off, the
		// re-encoded {"grants":[]} has become allow-all.
		{"roundtrip-empty-object", rt("{}"), true},
		{"roundtrip-empty-grants", rt(`{"grants":[]}`), true},
		{"roundtrip-empty-acls", rt(`{"acls":[]}`), true},
		{"roundtrip-json-null", rt("null"), true},
		{"roundtrip-twice-empty-grants", func(t *testing.T) []byte {
			t.Helper()

			return roundTrip(t, roundTrip(t, []byte(`{"grants":[]}`)))
		}, true},
	}
}

func peerCount(m map[types.NodeID][]types.NodeID) int {
	n := 0
	for _, peers := range m {
		n += len(peers)
	}

	return n
}

func assertDeniesEverything(t *testing.T, pm *PolicyManager, nodes types.Nodes) {
	t.Helper()

	filter, matchers := pm.Filter()
	assert.Empty(t, filter, "global filter must have no rules")
	assert.Empty(t, matchers, "no matchers")

	peers := pm.BuildPeerMap(nodes.ViewSlice())
	assert.Zero(t, peerCount(peers), "no peers, not even same-user devices: %v", peers)

	for _, n := range nodes {
		nv := n.View()

		rules, err := pm.FilterForNode(nv)
		require.NoError(t, err)
		assert.Empty(t, rules, "%s: packet filter must be empty", n.Hostname)

		ms, err := pm.MatchersForNode(nv)
		require.NoError(t, err)
		assert.Empty(t, ms, "%s: no route matchers", n.Hostname)

		ssh, err := pm.SSHPolicy("https://ctrl.example", nv)
		require.NoError(t, err)
		require.NotNil(t, ssh, "%s: SSH policy must be non-nil (nil keeps old rules)", n.Hostname)
		assert.Empty(t, ssh.Rules, "%s: no SSH rules", n.Hostname)

		assert.Empty(t, pm.NodeCapMap(n.ID), "%s: no policy capabilities", n.Hostname)
	}
}

func assertAllowsEverything(t *testing.T, pm *PolicyManager, nodes types.Nodes) {
	t.Helper()

	filter, _ := pm.Filter()
	assert.NotEmpty(t, filter, "upstream: no rules means allow-all")

	peers := pm.BuildPeerMap(nodes.ViewSlice())
	for _, n := range nodes {
		assert.Len(t, peers[n.ID], len(nodes)-1, "%s sees every other node", n.Hostname)
	}
}

// assertUpstreamExplicitDeny is the upstream meaning of {"grants":[]}:
// no rules, but a user's own devices still list each other as peers.
func assertUpstreamExplicitDeny(t *testing.T, pm *PolicyManager, nodes types.Nodes) {
	t.Helper()

	filter, _ := pm.Filter()
	assert.Empty(t, filter)

	peers := pm.BuildPeerMap(nodes.ViewSlice())
	assert.Equal(t, []types.NodeID{2}, peers[1], "same-user peers stay upstream")
	assert.Empty(t, peers[3])
}

func TestTenantEmptyPolicyDenies_NewPolicyManager(t *testing.T) {
	t.Parallel()

	for _, in := range tenantDenyInputs() {
		for _, on := range []bool{true, false} {
			name := in.name + "/switch-off"
			if on {
				name = in.name + "/switch-on"
			}

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				users, nodes := tenantDenyFixture()

				pm, err := NewPolicyManager(in.pol(t), users, nodes.ViewSlice(), WithEmptyPolicyDenies(on))
				require.NoError(t, err)

				switch {
				case on:
					assertDeniesEverything(t, pm, nodes)
				case in.upstreamAllows:
					assertAllowsEverything(t, pm, nodes)
				default:
					assertUpstreamExplicitDeny(t, pm, nodes)
				}
			})
		}
	}
}

// TestTenantEmptyPolicyDenies_SetPolicy installs each input over an
// allow-all policy, as a reload or API write does.
func TestTenantEmptyPolicyDenies_SetPolicy(t *testing.T) {
	t.Parallel()

	for _, in := range tenantDenyInputs() {
		t.Run(in.name, func(t *testing.T) {
			t.Parallel()

			users, nodes := tenantDenyFixture()

			pm, err := NewPolicyManager([]byte(tenantSSHAllPolicy), users, nodes.ViewSlice(), WithEmptyPolicyDenies(true))
			require.NoError(t, err)

			filter, _ := pm.Filter()
			require.NotEmpty(t, filter, "precondition: explicit allow policy")

			b := in.pol(t)

			changed, err := pm.SetPolicy(b)
			require.NoError(t, err)
			assert.True(t, changed)

			assertDeniesEverything(t, pm, nodes)
		})
	}
}

// Upstream refuses a zero-length SetPolicy; with the switch off it still does.
func TestTenantEmptyPolicyDenies_SetPolicyEmptyUpstream(t *testing.T) {
	t.Parallel()

	users, nodes := tenantDenyFixture()

	pm, err := NewPolicyManager([]byte(`{"grants":[]}`), users, nodes.ViewSlice())
	require.NoError(t, err)

	_, err = pm.SetPolicy(nil)
	require.ErrorIs(t, err, ErrPolicyEmpty)
	assertUpstreamExplicitDeny(t, pm, nodes)
}

// An explicit tenant policy still allows what it says, so the switch
// only changes the meaning of "no access rules".
func TestTenantEmptyPolicyDenies_ExplicitPolicyStillApplies(t *testing.T) {
	t.Parallel()

	users, nodes := tenantDenyFixture()

	pm, err := NewPolicyManager([]byte(tenantSSHAllPolicy), users, nodes.ViewSlice(), WithEmptyPolicyDenies(true))
	require.NoError(t, err)

	assertAllowsEverything(t, pm, nodes)

	ssh, err := pm.SSHPolicy("https://ctrl.example", nodes[0].View())
	require.NoError(t, err)
	assert.NotEmpty(t, ssh.Rules)
}

// SSH rules without any access rule: the tenant filter denies, so SSH is
// withheld too rather than compiled from the ssh block.
func TestTenantEmptyPolicyDenies_SSHWithoutAccessRules(t *testing.T) {
	t.Parallel()

	users, nodes := tenantDenyFixture()
	pol := `{"ssh": [{"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["root"]}]}`

	pm, err := NewPolicyManager([]byte(pol), users, nodes.ViewSlice(), WithEmptyPolicyDenies(true))
	require.NoError(t, err)
	assertDeniesEverything(t, pm, nodes)

	_, ok := pm.SSHCheckParams(1, 2, "root")
	assert.False(t, ok)

	// Upstream: no acls/grants allows all and the ssh block applies.
	up, err := NewPolicyManager([]byte(pol), users, nodes.ViewSlice())
	require.NoError(t, err)

	ssh, err := up.SSHPolicy("https://ctrl.example", nodes[0].View())
	require.NoError(t, err)
	assert.NotEmpty(t, ssh.Rules)
}

// A parse error never falls back to allow-all, in either mode: the
// constructor fails (startup aborts) and SetPolicy keeps what was there.
func TestTenantEmptyPolicyDenies_ParseErrorNeverAllows(t *testing.T) {
	t.Parallel()

	bad := [][]byte{
		[]byte(`{"acls":[`),
		[]byte(`{"grants":[{"src":["nobody-such@"],"dst":["*"],"ip":["*"]}]`),
		[]byte(`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}],"unknownKey":1}`),
		[]byte(`not json at all`),
	}

	for _, on := range []bool{true, false} {
		users, nodes := tenantDenyFixture()

		for _, b := range bad {
			_, err := NewPolicyManager(b, users, nodes.ViewSlice(), WithEmptyPolicyDenies(on))
			require.Error(t, err, "switch=%v: %q must not load", on, b)
		}

		pm, err := NewPolicyManager([]byte(`{"grants":[]}`), users, nodes.ViewSlice(), WithEmptyPolicyDenies(on))
		require.NoError(t, err)

		before, _ := pm.Filter()

		for _, b := range bad {
			_, err := pm.SetPolicy(b)
			require.Error(t, err, "switch=%v: %q must be refused", on, b)

			after, _ := pm.Filter()
			assert.Equal(t, before, after, "switch=%v: a refused write keeps the old filter", on)
			assert.Empty(t, after)
		}
	}
}

// ValidatePolicy is the write boundary: it refuses empty input in both
// modes, and with the switch on it evaluates `tests` against deny-all.
func TestTenantEmptyPolicyDenies_ValidatePolicy(t *testing.T) {
	t.Parallel()

	users, nodes := tenantDenyFixture()
	ns := nodes.ViewSlice()

	for _, on := range []bool{true, false} {
		require.ErrorIs(t, ValidatePolicy(nil, users, ns, WithEmptyPolicyDenies(on)), ErrPolicyEmpty)
		require.ErrorIs(t, ValidatePolicy([]byte{}, users, ns, WithEmptyPolicyDenies(on)), ErrPolicyEmpty)
	}

	pol := []byte(`{"tests":[{"src":"alice@example.com","accept":["bob@example.com:22"]}]}`)
	require.NoError(t, ValidatePolicy(pol, users, ns), "upstream: no rules allow the test")
	require.Error(t, ValidatePolicy(pol, users, ns, WithEmptyPolicyDenies(true)),
		"tenant: no rules deny, so an accept test must fail")

	deny := []byte(`{"tests":[{"src":"alice@example.com","deny":["bob@example.com:22"]}]}`)
	require.NoError(t, ValidatePolicy(deny, users, ns, WithEmptyPolicyDenies(true)))
}

// The switch must survive node and user churn: SetNodes/SetUsers recompile.
func TestTenantEmptyPolicyDenies_NodeAndUserChanges(t *testing.T) {
	t.Parallel()

	users, nodes := tenantDenyFixture()

	pm, err := NewPolicyManager(nil, nil, views.Slice[types.NodeView]{}, WithEmptyPolicyDenies(true))
	require.NoError(t, err)

	_, _, err = pm.SetUsers(users)
	require.NoError(t, err)
	_, err = pm.SetNodes(nodes.ViewSlice())
	require.NoError(t, err)

	assertDeniesEverything(t, pm, nodes)
}
