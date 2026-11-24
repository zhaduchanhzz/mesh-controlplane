package db

import (
	"net/netip"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/types/key"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
)

// addNodeIPs inserts a node with the given overlay addresses (nil = NULL)
// through the tenant handle, so tailnet_id is set by tenantscope.
func addNodeIPs(t *testing.T, hsdb *HSDatabase, v4, v6 *netip.Addr) error {
	t.Helper()

	u, err := hsdb.CreateUser(types.User{Name: "u-" + uuid.NewV4().String()[:8]})
	require.NoError(t, err)

	return hsdb.DB.Create(&types.Node{
		MachineKey: key.NewMachine().Public(), NodeKey: key.NewNode().Public(),
		Hostname: "laptop", UserID: &u.ID, RegisterMethod: util.RegisterMethodAuthKey,
		IPv4: v4, IPv6: v6,
	}).Error
}

func requireUniqueViolation(t *testing.T, err error, msg string) {
	t.Helper()

	require.Error(t, err, msg)
	assert.Contains(t, strings.ToLower(err.Error()), "unique", msg)
}

// TestNodeIPUniquePerTailnet (P06, owner decision 2026-11-24): two nodes of
// one tailnet never hold the same overlay address, enforced by the database
// and not only by the in-memory allocator (AGENTS 7). Two tailnets are two
// overlays and may reuse addresses; NULL (no address of that family) may
// repeat.
func TestNodeIPUniquePerTailnet(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			a := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))
			bdb := tenantDB(t, p, platformTailnet(t, p, "local", types.TailnetActive, false))

			v4, v6 := nap("100.64.0.1"), nap("fd7a:115c:a1e0::1")
			require.NoError(t, addNodeIPs(t, a, v4, v6))

			// Same tailnet: refused per family.
			requireUniqueViolation(t, addNodeIPs(t, a, v4, nap("fd7a:115c:a1e0::2")), "same IPv4 in one tailnet")
			requireUniqueViolation(t, addNodeIPs(t, a, nap("100.64.0.2"), v6), "same IPv6 in one tailnet")
			requireUniqueViolation(t, addNodeIPs(t, a, v4, nil), "same IPv4, no IPv6")
			requireUniqueViolation(t, addNodeIPs(t, a, nil, v6), "same IPv6, no IPv4")

			// Another tailnet is another overlay: the same addresses pass.
			require.NoError(t, addNodeIPs(t, bdb, v4, v6), "same addresses in another tailnet")

			// NULL repeats in one tailnet.
			require.NoError(t, addNodeIPs(t, a, nil, nil))
			require.NoError(t, addNodeIPs(t, a, nil, nil))
			require.NoError(t, addNodeIPs(t, a, nap("100.64.0.3"), nil))
			require.NoError(t, addNodeIPs(t, a, nap("100.64.0.4"), nil))
			require.NoError(t, addNodeIPs(t, a, nil, nap("fd7a:115c:a1e0::3")))
			require.NoError(t, addNodeIPs(t, a, nil, nap("fd7a:115c:a1e0::4")))

			// An update cannot move a node onto an address in use either.
			var n types.Node
			require.NoError(t, a.DB.Where("ipv4 = ?", "100.64.0.3").Take(&n).Error)
			requireUniqueViolation(t, a.DB.Model(&types.Node{}).Where("id = ?", n.ID).
				Update("ipv4", "100.64.0.1").Error, "update onto an IPv4 in use")
			requireUniqueViolation(t, a.DB.Model(&types.Node{}).Where("id = ?", n.ID).
				Update("ipv6", "fd7a:115c:a1e0::1").Error, "update onto an IPv6 in use")

			var cnt int64
			require.NoError(t, a.DB.Model(&types.Node{}).Count(&cnt).Error)
			assert.EqualValues(t, 7, cnt, "refused inserts left no rows")
		})
	}
}
