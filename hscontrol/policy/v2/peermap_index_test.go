package v2

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"pgregory.net/rapid"
	"tailscale.com/tailcfg"
	"tailscale.com/types/views"
)

// pairwisePeerMap is the reference: the pair scan BuildPeerMap ran before
// the indexed build.
func pairwisePeerMap(pm *PolicyManager, nodes views.Slice[types.NodeView]) map[types.NodeID][]types.NodeID {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.buildPeerMapPairwiseLocked(nodes)
}

// drawPeerMapNodes draws user-owned and tagged nodes on IPv4, IPv6 or
// both, with random IDs, some advertising subnet or exit routes approved
// only in part.
func drawPeerMapNodes(rt *rapid.T) types.Nodes {
	n := rapid.IntRange(1, 40).Draw(rt, "nodes")
	ids := rapid.SliceOfNDistinct(rapid.IntRange(1, 10_000), n, n, rapid.ID).Draw(rt, "ids")
	nodes := make(types.Nodes, 0, n)

	for i := range n {
		nd := &types.Node{ID: types.NodeID(ids[i]), Hostname: fmt.Sprintf("n%d", i)} //nolint:gosec

		v4 := netip.AddrFrom4([4]byte{100, 64, 0, byte(i + 1)})                               //nolint:gosec
		v6 := netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0, 15: byte(i + 1)}) //nolint:gosec

		switch rapid.IntRange(0, 6).Draw(rt, "family") {
		case 0:
			nd.IPv4 = &v4
		case 1:
			nd.IPv6 = &v6
		default:
			nd.IPv4, nd.IPv6 = &v4, &v6
		}

		if rapid.IntRange(0, 9).Draw(rt, "owner") < 6 {
			u := rapid.SampledFrom(scanUsers).Draw(rt, "user")
			nd.User, nd.UserID = &u, &u.ID
		} else {
			nd.Tags = rapid.SliceOfNDistinct(rapid.SampledFrom(scanTags), 1, 2, rapid.ID).Draw(rt, "tags")
		}

		pool := make([]netip.Prefix, 0, len(scanRoutes)+2)
		for _, r := range scanRoutes {
			pool = append(pool, netip.MustParsePrefix(r))
		}

		pool = append(pool, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"))

		announced := rapid.SliceOfNDistinct(rapid.SampledFrom(pool), 0, 4, func(p netip.Prefix) string {
			return p.String()
		}).Draw(rt, "announced")
		if rapid.IntRange(0, 5).Draw(rt, "exit") == 0 {
			announced = append(announced, pool[len(pool)-2:]...)
		}

		if len(announced) > 0 {
			nd.Hostinfo = &tailcfg.Hostinfo{RoutableIPs: announced}

			for _, p := range announced {
				if rapid.IntRange(0, 9).Draw(rt, "approve") < 7 {
					nd.ApprovedRoutes = append(nd.ApprovedRoutes, p)
				}
			}
		}

		nodes = append(nodes, nd)
	}

	return nodes
}

// drawPeerMapPolicy draws ACLs and grants over the [scanAliases] pool, with
// autogroup:self, via, capability-only and internet destinations.
func drawPeerMapPolicy(rt *rapid.T) []byte {
	pol := map[string]any{
		"groups":    map[string][]string{"group:a": {"u1@", "u2@"}, "group:b": {"u3@"}},
		"tagOwners": map[string][]string{"tag:srv": {"u1@"}, "tag:db": {"u1@"}, "tag:router": {"u1@"}, "tag:exit": {"u1@"}},
		"hosts": map[string]string{
			"lan": "10.1.0.0/16", "host1": "100.64.0.5/32", "v6net": "fd7a:115c:a1e0::/122",
			"all4": "0.0.0.0/0",
		},
	}

	switch rapid.IntRange(0, 19).Draw(rt, "shape") {
	case 0:
		return must(json.Marshal(pol)) // no rules section
	case 1:
		pol["grants"] = []any{}

		return must(json.Marshal(pol))
	}

	aliases := rapid.SliceOfN(rapid.SampledFrom(scanAliases), 1, 3)

	var acls, grants []any

	for range rapid.IntRange(1, 6).Draw(rt, "rules") {
		src := aliases.Draw(rt, "src")
		dst := aliases.Draw(rt, "dst")

		switch rapid.IntRange(0, 5).Draw(rt, "dstExtra") {
		case 0:
			dst = append(dst, rapid.SampledFrom(scanDstOnly).Draw(rt, "dstOnly"))
		case 1:
			dst = []string{"autogroup:self"}
		}

		if rapid.IntRange(0, 3).Draw(rt, "acl") == 0 {
			var aclDst []string

			for _, d := range dst {
				if netip.MustParsePrefix("::/0").Contains(parseAddrOrZero(d)) {
					continue
				}

				aclDst = append(aclDst, d+":"+rapid.SampledFrom([]string{"*", "22", "80,443"}).Draw(rt, "ports"))
			}

			if len(aclDst) > 0 {
				acls = append(acls, map[string]any{"action": "accept", "src": src, "dst": aclDst})
			}

			continue
		}

		g := map[string]any{
			"src": src, "dst": dst,
			"ip": rapid.SampledFrom([][]string{{"*"}, {"tcp:443"}, {"icmp:*"}, {"udp:53", "tcp:22"}}).Draw(rt, "ip"),
		}

		switch rapid.IntRange(0, 6).Draw(rt, "grantKind") {
		case 0:
			g["via"] = []string{rapid.SampledFrom([]string{"tag:router", "tag:exit"}).Draw(rt, "via")}
		case 1:
			g["via"] = []string{rapid.SampledFrom([]string{"tag:router", "tag:exit"}).Draw(rt, "via")}

			delete(g, "ip")
			g["app"] = map[string]any{"example.com/cap/test": []map[string]any{{}}}
		case 2:
			delete(g, "ip")
			g["app"] = map[string]any{"example.com/cap/test": []map[string]any{{}}}
		case 3:
			// A capability to the internet: exit nodes list its sources.
			delete(g, "ip")
			// Policies reject app grants to autogroup:internet and /0, not
			// to hosts and broad prefixes covering it.
			g["dst"] = []string{rapid.SampledFrom([]string{"all4", "8.0.0.0/5"}).Draw(rt, "capDst")}
			g["app"] = map[string]any{"example.com/cap/test": []map[string]any{{}}}
		}

		grants = append(grants, g)
	}

	if acls != nil {
		pol["acls"] = acls
	}

	if grants != nil {
		pol["grants"] = grants
	}

	return must(json.Marshal(pol))
}

// TestBuildPeerMapIndexedMatchesPairwise is the equivalence gate for the
// indexed peer-map build: over random tailnets (users, tags, subnet and
// exit routes) and policies (ACLs, grants, autogroup:self, via,
// capability-only, internet), on the full node set and on shuffled
// subsets, it must return exactly the pair scan's map, peer order
// included. A peer the pair scan would not list is reported first as an
// isolation failure.
func TestBuildPeerMapIndexedMatchesPairwise(t *testing.T) {
	if f := flag.Lookup("rapid.checks"); f != nil && f.Value.String() == "100" {
		require.NoError(t, f.Value.Set("2000"))

		defer func() { _ = f.Value.Set("100") }()
	}

	var cov struct {
		runs, perNode, self, via, partial, full, empty, exitCap, routed, tagged int
	}

	rapid.Check(t, func(rt *rapid.T) {
		nodes := drawPeerMapNodes(rt)
		polB := drawPeerMapPolicy(rt)

		pm, err := NewPolicyManager(polB, scanUsers, nodes.ViewSlice())
		if err != nil {
			rt.Skipf("policy rejected: %v", err)
		}

		in := rapid.Permutation(nodes).Draw(rt, "order")
		in = in[:rapid.IntRange(1, len(in)).Draw(rt, "subset")]

		for _, set := range []types.Nodes{nodes, in} {
			view := set.ViewSlice()
			want := pairwisePeerMap(pm, view)
			got := pm.BuildPeerMap(view)

			for id, peers := range got {
				for _, p := range peers {
					if !slices.Contains(want[id], p) {
						rt.Fatalf("ISOLATION: indexed build lists %d as a peer of %d; pair scan does not\npolicy %s", p, id, polB)
					}
				}
			}

			require.Equalf(rt, want, got, "policy %s", polB)
		}

		// Coverage of the branches the build handles separately.
		want := pairwisePeerMap(pm, nodes.ViewSlice())
		total := 0

		for id, peers := range want {
			total += len(peers)

			for _, p := range peers {
				if !slices.Contains(want[p], id) {
					cov.exitCap++ // one-way: an exit node listing a capability source
				}
			}
		}

		cov.runs++

		switch total {
		case 0:
			cov.empty++
		case len(nodes) * (len(nodes) - 1):
			cov.full++
		default:
			cov.partial++
		}

		if pm.needsPerNodeFilter {
			cov.perNode++

			if strings.Contains(string(polB), "autogroup:self") {
				cov.self++
			}

			if strings.Contains(string(polB), `"via"`) {
				cov.via++
			}
		}

		for _, nd := range nodes {
			if len(nd.SubnetRoutes()) > 0 && len(want[nd.ID]) > 0 {
				cov.routed++
			}

			if nd.IsTagged() && len(want[nd.ID]) > 0 {
				cov.tagged++
			}
		}
	})

	t.Logf("coverage: %+v", cov)

	if cov.runs < 200 {
		return // a short -rapid.checks run
	}

	require.Positive(t, cov.perNode, "per-node policies")
	require.Positive(t, cov.self, "autogroup:self")
	require.Positive(t, cov.via, "via grants")
	require.Positive(t, cov.partial, "partial maps")
	require.Positive(t, cov.full, "full maps")
	require.Positive(t, cov.empty, "empty maps")
	require.Positive(t, cov.exitCap, "one-way exit capability entries")
	require.Positive(t, cov.routed, "subnet routers with peers")
	require.Positive(t, cov.tagged, "tagged nodes with peers")
}

// TestBuildPeerMapIndexedFallback pins the inputs the indexed build hands
// to the pair scan: duplicate IDs.
func TestBuildPeerMapIndexedFallback(t *testing.T) {
	nodes := scanManyRulesNodes(6)
	nodes[3].ID = nodes[1].ID

	pm, err := NewPolicyManager([]byte(`{"grants": [{"src": ["*"], "dst": ["*"], "ip": ["*"]}]}`),
		scanUsers, nodes.ViewSlice())
	require.NoError(t, err)
	require.Equal(t, pairwisePeerMap(pm, nodes.ViewSlice()), pm.BuildPeerMap(nodes.ViewSlice()))
}
