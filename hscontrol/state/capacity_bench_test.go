package state

// Capacity benchmarks for P00b: how the peer-map build and the NodeStore
// snapshot rebuild scale with tailnet size under the TestScale policy
// shapes (hscontrol/servertest/scale_test.go). Nodes are grouped ten per
// user, as in TestScale, so autogroup:self and same-user visibility match.
//
// Run, for example:
//
//	go test ./hscontrol/state -run '^$' -bench 'BenchmarkCapacity_' \
//	  -benchmem -count=3 -timeout=60m
//
// Besides ns/op, B/op and allocs/op, each benchmark reports:
//   - peer-entries: directed entries in the resulting adjacency map;
//   - retained-MB: live heap held by one result after a forced GC (the
//     peer map alone, or the whole snapshot including the node views).
//
// Set HEADSCALE_CAPACITY_NODES to a comma-separated list to override the
// default 1000,5000,10000.

import (
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	policyv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/policy/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

const capacityDevicesPerUser = 10

// capacityPolicies mirror TestScale's cases.
var capacityPolicies = []struct {
	name          string
	policy        string
	taggedService bool
}{
	{name: "allow_all", policy: `{"grants": [{"src": ["*"], "dst": ["*"], "ip": ["tcp:443"]}]}`},
	{name: "isolated", policy: `{"acls": []}`},
	{name: "personal_devices", policy: `{"grants": [{
		"src": ["autogroup:member"], "dst": ["autogroup:self"], "ip": ["tcp:443"]}]}`},
	{name: "shared_service", policy: `{
		"tagOwners": {"tag:service": ["cap-user-0@"]},
		"grants": [{"src": ["autogroup:member"], "dst": ["tag:service"], "ip": ["tcp:443"]}]}`, taggedService: true},
}

func capacityNodeCounts(b *testing.B) []int {
	b.Helper()

	value, ok := os.LookupEnv("HEADSCALE_CAPACITY_NODES")
	if !ok {
		return []int{1000, 5000, 10000}
	}

	var counts []int

	for field := range strings.SplitSeq(value, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		require.NoError(b, err, "HEADSCALE_CAPACITY_NODES")

		counts = append(counts, n)
	}

	return counts
}

// capacityNodes builds n user-owned nodes, ten per user, each with a unique
// IPv4 and IPv6 address. With taggedService node 1 is tag:service instead.
func capacityNodes(n int, taggedService bool) ([]types.User, types.Nodes) {
	users := make([]types.User, (n+capacityDevicesPerUser-1)/capacityDevicesPerUser)
	for i := range users {
		users[i] = types.User{ID: uint(i + 1), Name: fmt.Sprintf("cap-user-%d", i)} //nolint:gosec
	}

	nodes := make(types.Nodes, 0, n)

	for i := range n {
		node := createTestNode(types.NodeID(i+1), users[i/capacityDevicesPerUser].ID, //nolint:gosec
			users[i/capacityDevicesPerUser].Name, fmt.Sprintf("cap-node-%05d", i))
		u := users[i/capacityDevicesPerUser]
		node.User = &u

		ip4 := netip.AddrFrom4([4]byte{100, 64 + byte(i>>16), byte(i >> 8), byte(i)}) //nolint:gosec
		ip6 := netip.AddrFrom16([16]byte{
			0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0, 13: byte(i >> 16), 14: byte(i >> 8), 15: byte(i), //nolint:gosec
		})
		node.IPv4, node.IPv6 = &ip4, &ip6

		if taggedService && i == 0 {
			node.Tags = []string{"tag:service"}
			node.UserID, node.User = nil, nil
		}

		nodes = append(nodes, &node)
	}

	return users, nodes
}

func capacityLiveHeap() uint64 {
	var m runtime.MemStats

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)

	return m.HeapAlloc
}

func capacityPeerEntries(peers map[types.NodeID][]types.NodeID) int {
	total := 0
	for _, ids := range peers {
		total += len(ids)
	}

	return total
}

// BenchmarkCapacity_BuildPeerMap measures PolicyManager.BuildPeerMap alone.
func BenchmarkCapacity_BuildPeerMap(b *testing.B) {
	for _, pol := range capacityPolicies {
		for _, n := range capacityNodeCounts(b) {
			b.Run(fmt.Sprintf("%s/n=%d", pol.name, n), func(b *testing.B) {
				users, nodes := capacityNodes(n, pol.taggedService)
				pm, err := policyv2.NewPolicyManager([]byte(pol.policy), users, nodes.ViewSlice())
				require.NoError(b, err)

				views := nodes.ViewSlice()

				before := capacityLiveHeap()
				held := pm.BuildPeerMap(views)
				retained := capacityLiveHeap() - before
				entries := capacityPeerEntries(held)

				runtime.KeepAlive(held)

				held = nil

				b.ReportAllocs()

				for b.Loop() {
					pm.BuildPeerMap(views)
				}

				b.ReportMetric(float64(entries), "peer-entries")
				b.ReportMetric(float64(retained)/(1<<20), "retained-MB")
			})
		}
	}
}

// BenchmarkCapacity_SnapshotRebuild measures snapshotFromNodes with a
// topology change (peer adjacency and routes recomputed), wired to the
// production policyPeersFunc, i.e. the cost NodeStore pays per
// topology-changing batch.
func BenchmarkCapacity_SnapshotRebuild(b *testing.B) {
	for _, pol := range capacityPolicies {
		for _, n := range capacityNodeCounts(b) {
			b.Run(fmt.Sprintf("%s/n=%d", pol.name, n), func(b *testing.B) {
				users, nodes := capacityNodes(n, pol.taggedService)
				pm, err := policyv2.NewPolicyManager([]byte(pol.policy), users, nodes.ViewSlice())
				require.NoError(b, err)

				byID := make(map[types.NodeID]types.Node, n)
				for _, node := range nodes {
					byID[node.ID] = *node
				}

				peersFunc := policyPeersFunc(pm)

				before := capacityLiveHeap()
				held := snapshotFromNodes(byID, peersFunc, nil, false, false)
				retained := capacityLiveHeap() - before
				entries := capacityPeerEntries(held.peersByNode)

				runtime.KeepAlive(&held)

				held = Snapshot{}

				b.ReportAllocs()

				for b.Loop() {
					snapshotFromNodes(byID, peersFunc, nil, false, false)
				}

				b.ReportMetric(float64(entries), "peer-entries")
				b.ReportMetric(float64(retained)/(1<<20), "retained-MB")
			})
		}
	}
}
