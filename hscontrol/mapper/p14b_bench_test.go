package mapper

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
	"tailscale.com/util/zstdframe"
)

// P14b baseline microbenchmarks (tickets/P14.md, candidates #1 and #6): the
// per-recipient cost the server pays when a change reaches a node of a
// large allow-all tailnet (P00b: encoding was ~31 % of server CPU there).
// Real State on SQLite (setupBatcherWithTestData), ten nodes per user.
//
//   - listpeers: State.ListPeers for one node, what filterVisiblePeerPatches
//     pays per recipient of a patch (#6);
//   - full/build, full/build+json, full/build+json+zstd: one recipient's full
//     map, as built (fullMapResponse) and encoded (encodeMapResponse in
//     hscontrol/poll.go: json.Marshal + zstd FastestCompression) (#1);
//   - policychange/build+json+zstd: policyChangeResponse with every current
//     peer in PeersChanged (#2, #3).
//
// A broadcast to every node of the tailnet costs n times the per-recipient
// number; nothing is shared between recipients today.
//
//	go test ./hscontrol/mapper -run '^$' -bench BenchmarkP14b -benchmem -count 6
//
// HEADSCALE_P14B_NODES overrides the default sizes (1000,2500,5000).

func p14bNodeCounts(b *testing.B) []int {
	b.Helper()

	v, ok := os.LookupEnv("HEADSCALE_P14B_NODES")
	if !ok {
		return []int{1000, 2500, 5000}
	}

	var out []int

	for f := range strings.SplitSeq(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			b.Fatalf("HEADSCALE_P14B_NODES: %v", err)
		}

		out = append(out, n)
	}

	return out
}

func BenchmarkP14bPerRecipientMap(b *testing.B) {
	if testing.Short() {
		b.Skip("builds tailnets of thousands of nodes")
	}

	for _, n := range p14bNodeCounts(b) {
		td, cleanup := setupBatcherWithTestData(b, NewBatcherAndMapper, (n+9)/10, 10, 1)
		m := newMapper(td.Config, td.State)
		id := td.Nodes[len(td.Nodes)/2].n.ID
		capVer := tailcfg.CurrentCapabilityVersion

		b.Run(fmt.Sprintf("listpeers/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = td.State.ListPeers(id)
			}
		})

		b.Run(fmt.Sprintf("full/build/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_, err := m.fullMapResponse(id, capVer)
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		encode := func(b *testing.B, build func() (*tailcfg.MapResponse, error), compress bool) {
			b.Helper()
			b.ReportAllocs()

			var jsonBytes, wireBytes int

			for b.Loop() {
				mr, err := build()
				if err != nil {
					b.Fatal(err)
				}

				body, err := json.Marshal(mr)
				if err != nil {
					b.Fatal(err)
				}

				jsonBytes = len(body)

				if compress {
					body = zstdframe.AppendEncode(nil, body, zstdframe.FastestCompression)
				}

				wireBytes = len(body)
			}

			b.ReportMetric(float64(jsonBytes), "json-B")
			b.ReportMetric(float64(wireBytes), "wire-B")
		}

		full := func() (*tailcfg.MapResponse, error) { return m.fullMapResponse(id, capVer) }
		policy := func() (*tailcfg.MapResponse, error) {
			return m.policyChangeResponse(id, capVer, td.State.ListPeers(id))
		}

		b.Run(fmt.Sprintf("full/build+json/n=%d", n), func(b *testing.B) { encode(b, full, false) })
		b.Run(fmt.Sprintf("full/build+json+zstd/n=%d", n), func(b *testing.B) { encode(b, full, true) })
		b.Run(fmt.Sprintf("policychange/build+json+zstd/n=%d", n), func(b *testing.B) { encode(b, policy, true) })

		cleanup()
	}
}
