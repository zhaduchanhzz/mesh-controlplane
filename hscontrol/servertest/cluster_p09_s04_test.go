//go:build p05cluster

package servertest_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"tailscale.com/types/netmap"
)

// P09, S04 (06-tests-benchmarks.md:12): a tailnet created with grants=[]
// while its first clients enrol and its first policy is written, several
// times, on a running cluster. Checked on EVERY netmap the clients received:
// no peer without a packet filter allowing it, never an allow-all rule, no
// node of another tailnet. Before activation nothing can enrol: the tenant
// handle (which minting a key needs) is refused and the cluster does not
// load the tailnet.

const s04Policy = `{"acls":[{"action":"accept","src":["s04u@"],"dst":["s04u@:*"]}]}`

// allowAllRule reports a filter rule open to any source.
func allowAllRule(nm *netmap.NetworkMap) string {
	for _, r := range nm.PacketFilterRules.All() {
		for _, s := range r.SrcIPs {
			if s == "*" || s == "0.0.0.0/0" || s == "::/0" {
				return s
			}
		}
	}

	return ""
}

func checkS04History(t *testing.T, c *servertest.TestClient, prefix string) {
	t.Helper()

	h := c.History()
	require.NotEmpty(t, h, "%s received netmaps", c.Name)

	for i, nm := range h {
		if nm.PacketFilterRules.Len() == 0 {
			assert.Empty(t, nm.Peers, "%s netmap %d: a peer without any packet filter (before the policy allows it)", c.Name, i)
		}

		assert.Empty(t, allowAllRule(nm), "%s netmap %d: allow-all rule", c.Name, i)

		for _, p := range nm.Peers {
			hn := p.Hostinfo().Hostname()
			assert.True(t, strings.HasPrefix(hn, prefix), "%s netmap %d: peer %q of another tailnet", c.Name, i, hn)
		}
	}
}

func TestS04CreateEnrolAndFirstPolicyRace(t *testing.T) {
	c := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA})
	c.SetPolicy(t, tenantA, allowAll)

	// A neighbour tailnet that allows everything inside itself.
	neighbours := enroll(t, c.Tenant(t, tenantA), "alice", "s04-n1", "s04-n2")

	const iterations = 6

	for i := range iterations {
		slug := fmt.Sprintf("s04-%d", i)
		prefix := slug + "-"

		c.CreateTailnetPending(t, slug)

		// PENDING: no credential can be minted, nothing is loaded.
		require.Error(t, c.TenantHandleErr(t, slug), "%s: tenant handle of a PENDING tailnet", slug)

		if i == 0 {
			assert.Never(t, func() bool { return c.HasRuntime(t, slug) }, 2500*time.Millisecond, 50*time.Millisecond,
				"a PENDING tailnet is never loaded")
		}

		c.ActivateTailnet(t, slug)
		key := c.MintTenantKey(t, slug, "s04u")

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			clients []*servertest.TestClient
		)

		for _, n := range []string{"a", "b"} {
			name := prefix + n

			wg.Go(func() {
				deadline := time.Now().Add(clusterWait)
				for time.Now().Before(deadline) {
					cl, err := servertest.TryNewClusterClient(t, c.Front(), name, servertest.ClusterClientOptions{AuthKey: key})
					if err == nil {
						mu.Lock()
						clients = append(clients, cl)
						mu.Unlock()

						return
					}

					// The runtime is loaded by the next reconcile (<= 2 s).
					time.Sleep(50 * time.Millisecond) //nolint:forbidigo // retrying enrolment
				}
			})
		}

		// The first policy. Even iterations: only once both clients hold a
		// netmap, so they must have run under the deny-all policy first.
		// Odd ones: at a random moment of the enrolment.
		deterministic := i%2 == 0
		delay := time.Duration(rand.IntN(2500)) * time.Millisecond //nolint:gosec // random race point

		wg.Go(func() {
			if !deterministic {
				time.Sleep(delay) //nolint:forbidigo // random race point
				c.SetPolicyViaDB(t, slug, s04Policy)

				return
			}

			deadline := time.Now().Add(clusterWait)
			for time.Now().Before(deadline) {
				mu.Lock()
				ready := len(clients) == 2 && len(clients[0].History()) > 0 && len(clients[1].History()) > 0
				mu.Unlock()

				if ready {
					break
				}

				time.Sleep(50 * time.Millisecond) //nolint:forbidigo // waiting for both first netmaps
			}

			c.SetPolicyViaDB(t, slug, s04Policy)
		})

		wg.Wait()
		require.Len(t, clients, 2, "%s: both clients enrolled", slug)

		waitPeersExactly(t, clients[0], selfKey(t, clients[1]))
		waitPeersExactly(t, clients[1], selfKey(t, clients[0]))

		denied, total := 0, 0

		for _, cl := range clients {
			checkS04History(t, cl, prefix)

			for _, nm := range cl.History() {
				total++

				if nm.PacketFilterRules.Len() == 0 {
					denied++
				}
			}
		}

		mode := "random " + delay.String()
		if deterministic {
			mode = "after both first netmaps"
			// Each client ran under grants=[] first: no peer, no rule.
			assert.GreaterOrEqual(t, denied, 2, "%s: the clients saw the deny-all policy before the first policy", slug)
		}

		t.Logf("S04 %s: policy written %s; %d netmaps, %d of them deny-all (no rule)", slug, mode, total, denied)

	}

	for _, n := range neighbours {
		for j, nm := range n.History() {
			for _, p := range nm.Peers {
				assert.False(t, strings.HasPrefix(p.Hostinfo().Hostname(), "s04-") &&
					!strings.HasPrefix(p.Hostinfo().Hostname(), "s04-n"),
					"%s netmap %d: a node of a new tailnet", n.Name, j)
			}
		}
	}
}
