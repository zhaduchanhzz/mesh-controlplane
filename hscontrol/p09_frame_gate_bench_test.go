package hscontrol

import (
	"fmt"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// P09, S17 hot path: writeMap now checks servable() a second time right
// before the socket write and records the frame's gate/write times under a
// process-wide mutex. These benchmarks put that per-frame cost next to the
// cost of encoding the frame it guards, sequentially and with every CPU
// writing frames at once (one map session per goroutine, as a broadcast to
// many clients does).

func benchHeadscale() *Headscale {
	var held atomic.Bool

	held.Store(true)

	return &Headscale{leaseGate: held.Load}
}

// s17Additions is exactly what the S17 fix added per frame.
func s17Additions(h *Headscale) {
	if !h.servable() {
		panic("not servable")
	}

	gate := time.Now()
	recordFrameWritten(gate, time.Now())
}

func BenchmarkS17FrameGate(b *testing.B) {
	h := benchHeadscale()

	b.Run("sequential", func(b *testing.B) {
		for b.Loop() {
			s17Additions(h)
		}
	})

	b.Run("parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				s17Additions(h)
			}
		})
	})
}

// benchMapResponse is an update frame with n peers, the size a client gets
// when its tailnet changes.
func benchMapResponse(n int) *tailcfg.MapResponse {
	peers := make([]*tailcfg.Node, n)
	for i := range peers {
		peers[i] = &tailcfg.Node{
			ID:        tailcfg.NodeID(i + 1),
			StableID:  tailcfg.StableNodeID(fmt.Sprintf("n%d", i+1)),
			Name:      fmt.Sprintf("node-%d.a.tailnet.internal.", i+1),
			Key:       key.NewNode().Public(),
			DiscoKey:  key.NewDisco().Public(),
			Addresses: []netip.Prefix{netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, byte(i >> 8), byte(i)}), 32)},
			AllowedIPs: []netip.Prefix{
				netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, byte(i >> 8), byte(i)}), 32),
			},
			Endpoints: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:41641")},
			HomeDERP:  901,
		}
	}

	return &tailcfg.MapResponse{Peers: peers}
}

func BenchmarkS17EncodeFrame(b *testing.B) {
	for _, n := range []int{10, 100} {
		msg := benchMapResponse(n)

		b.Run(fmt.Sprintf("peers=%d", n), func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_, err := encodeMapResponse(util.ZstdCompression, msg)
					if err != nil {
						panic(err)
					}
				}
			})
		})
	}
}
