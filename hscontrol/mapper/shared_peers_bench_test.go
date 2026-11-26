package mapper

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
)

// BenchmarkSharedPeersFullMaps measures a cold full-map storm: every node of
// the tailnet gets its peer list once (as after a restart or failover, when
// every client reconnects), with the delta-first hashes of each response.
// "off" renders per viewer (before F4); "on" shares entries (F4). The cache
// is emptied at the start of each iteration, so "on" includes filling it.
//
// Sizes: P14F_F4_BENCH_SIZES (comma separated node counts, default 1000).
func BenchmarkSharedPeersFullMaps(b *testing.B) {
	sizes := []int{1000}

	if env := os.Getenv("P14F_F4_BENCH_SIZES"); env != "" {
		sizes = nil

		for f := range strings.SplitSeq(env, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 10 {
				b.Fatalf("bad P14F_F4_BENCH_SIZES entry %q", f)
			}

			sizes = append(sizes, n)
		}
	}

	pols := map[string]string{
		"allow_all": allowAllPolicyJSON,
		"isolated":  `{"acls":[{"action":"accept","src":["autogroup:member"],"dst":["autogroup:self:*"]}]}`,
	}

	for _, n := range sizes {
		for _, name := range []string{"allow_all", "isolated"} {
			for _, mode := range []string{"off", "on"} {
				b.Run(fmt.Sprintf("%s/%d/%s", name, n, mode), func(b *testing.B) {
					td, cleanup := setupBatcherWithTestData(b, NewBatcherAndMapper, n/10, 10, 1)
					defer cleanup()

					_, err := td.State.SetPolicy([]byte(pols[name]))
					if err != nil {
						b.Fatal(err)
					}

					td.Config.Tuning.IncrementalNetmap.Enabled = mode == "on"
					m := td.Batcher.mapper

					peerEntries := 0

					b.ReportAllocs()
					b.ResetTimer()

					for range b.N {
						dropSharedPeers(m)

						peerEntries = 0

						for i := range td.Nodes {
							id := td.Nodes[i].n.ID
							peers := m.state.ListPeers(id)

							resp, err := m.NewMapResponseBuilder(id).
								WithCapabilityVersion(tailcfg.CapabilityVersion(100)).
								WithUserProfiles(peers).
								WithPeers(peers).
								Build()
							if err != nil {
								b.Fatal(err)
							}

							_ = hashResponse(resp, m.sharedPeerCache())
							peerEntries += len(resp.Peers)
						}
					}

					b.StopTimer()
					b.ReportMetric(float64(peerEntries), "peer-entries/op")
				})
			}
		}
	}
}

// dropSharedPeers empties m's shared cache; its memoised hashes go with it.
func dropSharedPeers(m *mapper) {
	m.shared.Store(nil)
}
