package state

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	policyv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/policy/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// BenchmarkP14bNodeStoreEndpointBatch (P14b candidate #7): what one NodeStore
// write batch costs when its only change is a node's endpoints (endpoint
// churn, P14a steady phase). The batch goes through the real writer
// (applyBatch: copy of every node, snapshot rebuild with the previous
// adjacency reused) with the production peersFunc of the policy shape;
// batch size 1, so every UpdateNode is one batch.
//
//	go test ./hscontrol/state -run '^$' -bench BenchmarkP14bNodeStoreEndpointBatch -benchmem -count 6
//
// HEADSCALE_CAPACITY_NODES overrides the sizes (default 1000,5000,10000).
func BenchmarkP14bNodeStoreEndpointBatch(b *testing.B) {
	for _, pol := range capacityPolicies {
		if pol.taggedService {
			continue
		}

		for _, n := range capacityNodeCounts(b) {
			b.Run(fmt.Sprintf("%s/n=%d", pol.name, n), func(b *testing.B) {
				users, nodes := capacityNodes(n, false)
				pm, err := policyv2.NewPolicyManager([]byte(pol.policy), users, nodes.ViewSlice())
				require.NoError(b, err)

				store := NewNodeStore(nodes, policyPeersFunc(pm), 1, time.Second)
				store.Start()

				defer store.Stop()

				id := nodes[n/2].ID

				b.ReportAllocs()

				for i := 0; b.Loop(); i++ {
					ep := netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)}), 41641) //nolint:gosec // wraps

					store.UpdateNode(id, func(node *types.Node) {
						node.Endpoints = []netip.AddrPort{ep}
					})
				}
			})
		}
	}
}
