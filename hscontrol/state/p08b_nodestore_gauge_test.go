package state

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

func p08bNodes(ids ...types.NodeID) types.Nodes {
	out := make(types.Nodes, 0, len(ids))
	for _, id := range ids {
		n := createTestNode(id, 1, "user1", "node")
		n.GivenName = ""
		out = append(out, &n)
	}

	return out
}

// P08b: in cluster mode every tailnet runtime has its own NodeStore. The
// process-wide gauge headscale_nodestore_nodes must be the sum over the
// running stores, not the count of the store that wrote last.
func TestP08bNodeStoreGaugeIsNotLastWriter(t *testing.T) {
	base := p08bValue(t, nodeStoreNodesCount)

	a := NewNodeStore(p08bNodes(1, 2), allowAllPeersFunc, TestBatchSize, TestBatchTimeout)
	a.SetMetricsTailnet("11111111-1111-4111-8111-111111111111")

	a.Start()
	defer a.Stop()

	b := NewNodeStore(p08bNodes(3, 4, 5), allowAllPeersFunc, TestBatchSize, TestBatchTimeout)
	b.SetMetricsTailnet("22222222-2222-4222-8222-222222222222")
	b.Start()

	if got := p08bValue(t, nodeStoreNodesCount) - base; got != 5 {
		t.Fatalf("two stores (2+3 nodes): gauge = base%+g, want base+5", got)
	}

	n := createTestNode(10, 1, "user1", "node10")
	a.PutNode(n)

	if got := p08bValue(t, nodeStoreNodesCount) - base; got != 6 {
		t.Fatalf("after a write in the first store: gauge = base%+g, want base+6", got)
	}

	tA := tailnetNodes.WithLabelValues("11111111-1111-4111-8111-111111111111")
	tB := tailnetNodes.WithLabelValues("22222222-2222-4222-8222-222222222222")

	if a, b := p08bValue(t, tA), p08bValue(t, tB); a != 3 || b != 3 {
		t.Fatalf("tailnet_nodes: A=%g B=%g, want 3 and 3", a, b)
	}

	b.Stop()

	if got := p08bValue(t, nodeStoreNodesCount) - base; got != 3 {
		t.Fatalf("after the second store stopped: gauge = base%+g, want base+3", got)
	}

	if got := p08bValue(t, tB); got != 0 {
		t.Fatalf("tailnet_nodes of the stopped store = %g, want 0", got)
	}

	// A write racing Stop adds nothing back.
	b.setCount(7)

	if got := p08bValue(t, nodeStoreNodesCount) - base; got != 3 {
		t.Fatalf("count after Stop moved the gauge: base%+g", got)
	}
}

func p08bValue(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()

	var d dto.Metric
	err := m.Write(&d)
	if err != nil {
		t.Fatal(err)
	}

	if g := d.GetGauge(); g != nil {
		return g.GetValue()
	}

	return d.GetCounter().GetValue()
}
