package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P12c A6: `cluster tailnet policy` validates before it stores. A policy
// that does not parse would make the tailnet fail to load at its next
// failover or move; an empty one revokes every access and is stored only
// when asked for explicitly (--empty).
func TestCLITailnetPolicyValidates(t *testing.T) {
	ctx := context.Background()
	p := p12Platform(t, "sqlite", "c1")
	p12Register(t, p, "c1", placement.ClassShared, placement.ClusterActive)
	p12Live(t, p, "c1")

	tn, err := createTailnetCLI(ctx, p, "c1", "a")
	require.NoError(t, err)

	id := tailnet.MustParseID(tn.ID)
	require.NoError(t, p.SetTailnetStatus(id, types.TailnetActive))

	stored := func() (string, uint64) {
		t.Helper()

		hs, err := p.TenantDB(id)
		require.NoError(t, err)

		pol, err := hs.GetPolicy()
		require.NoError(t, err)

		row, err := p.TailnetBySlug("a")
		require.NoError(t, err)

		return pol.Data, row.PolicyRevision
	}

	before, rev := stored()

	for name, data := range map[string]string{
		"unparseable":     `{"acls": [`,
		"unknown field":   `{"aclz": []}`,
		"empty file":      "",
		"whitespace only": " \n\t",
	} {
		err := setTailnetPolicyCLI(p, "a", []byte(data), false)
		require.Error(t, err, "%s: refused", name)

		got, gotRev := stored()
		assert.Equal(t, before, got, "%s: stored policy unchanged", name)
		assert.Equal(t, rev, gotRev, "%s: policy revision unchanged", name)
	}

	allow := `{"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]}`
	require.NoError(t, setTailnetPolicyCLI(p, "a", []byte(allow), false))

	got, gotRev := stored()
	assert.Equal(t, allow, got)
	assert.Equal(t, rev+1, gotRev)

	// An explicit empty policy (deny all) is stored as such.
	require.NoError(t, setTailnetPolicyCLI(p, "a", nil, true))

	got, gotRev = stored()
	assert.Empty(t, got)
	assert.Equal(t, rev+2, gotRev)

	// --empty with content is a contradiction.
	require.Error(t, setTailnetPolicyCLI(p, "a", []byte(allow), true))
}
