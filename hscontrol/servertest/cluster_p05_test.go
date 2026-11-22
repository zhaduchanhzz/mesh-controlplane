//go:build p05cluster

// RED suite for P04+P05 (ADR 0007). See cluster_p05.go for the build tag and
// evidence/P05-servertest-red-suite.md for the S/I mapping.
//
// Rule for every negative check here (ADR 0007 leaves status codes and error
// text open): assert the security property — refused, no node row created
// anywhere, no other-tenant data in the response or netmap — never an exact
// string.

package servertest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	hscontrol "github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/servertest"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/control/controlclient"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
	"tailscale.com/types/persist"
)

const (
	clusterWait = 20 * time.Second
	// quietWindow is how long a tenant must receive nothing after a change
	// in another tenant (I07/S07). The batcher delay is 50 ms.
	quietWindow = 3 * time.Second

	tenantA = "tenant-a"
	tenantB = "tenant-b"

	allowAll        = `{"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]}`
	allowAllWithSSH = `{
		"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
		"ssh": [{"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["root"]}]
	}`
	allowSSHPortOnly = `{"acls": [{"action": "accept", "src": ["*"], "dst": ["*:22"]}]}`
)

// --- TestClusterTwoTailnetsPeersIsolated (S02, I02) ---

// A and B on one cluster, each with an allow-all + SSH policy. Every client
// sees exactly its own tailnet's other nodes and nothing of the other
// tailnet: node keys, machine/disco keys, IPs, hostnames, MagicDNS names,
// user profiles, packet filter and SSH policy.
//
// B has a third client: with independent per-tailnet IPAM, A's and B's
// first two nodes may share addresses (S06), which would make the IP leak
// check vacuous; B's third address exists only in B.
func TestClusterTwoTailnetsPeersIsolated(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAllWithSSH)
	tc.SetPolicy(t, tenantB, allowAllWithSSH)

	a := tc.Tenant(t, tenantA)
	b := tc.Tenant(t, tenantB)
	require.Equal(t, a.URL, b.URL, "every tenant is served on the cluster's single URL")

	aClients := enroll(t, a, "tenant-a-user", "tenant-a-host-1", "tenant-a-host-2")
	bClients := enroll(t, b, "tenant-b-user", "tenant-b-host-1", "tenant-b-host-2", "tenant-b-host-3")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, true)
		}
	}

	aTokens := tenantTokens(t, a, "tenant-a-user")
	bTokens := tenantTokens(t, b, "tenant-b-user")

	bOnly := exclusive(bTokens, aTokens)
	require.NotEmpty(t, bOnly.ips, "B must hold addresses A does not, or the IP leak check on A is vacuous")

	assertTenantView(t, a, aClients, bOnly)
	assertTenantView(t, b, bClients, exclusive(aTokens, bTokens))
}

// --- TestClusterAuthKeyPicksTailnet (S10, S11d/e) ---

// The auth key alone picks the tailnet: same URL, same Noise key. A
// spoofed tenant header does not move a registration. Unknown, garbage,
// tampered and spliced keys are refused and create no node anywhere.
func TestClusterAuthKeyPicksTailnet(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	idA, idB := tc.Tailnet(t, tenantA).ID, tc.Tailnet(t, tenantB).ID
	keyA := mintKey(t, a, "alice")
	keyB := mintKey(t, b, "bob")

	ca := servertest.NewClusterClient(t, a, "picks-a", servertest.ClusterClientOptions{AuthKey: keyA})
	cb := servertest.NewClusterClient(t, b, "picks-b", servertest.ClusterClientOptions{AuthKey: keyB})

	nkA, nkB := selfKey(t, ca), selfKey(t, cb)

	assertNodeOnlyIn(t, nkA, a, b)
	assertNodeOnlyIn(t, nkB, b, a)
	assertRowTailnet(t, tc, nkA, idA.String())
	assertRowTailnet(t, tc, nkB, idB.String())

	p := tc.Platform(t)

	got, err := p.TailnetForAuthKey(keyA)
	require.NoError(t, err)
	assert.Equal(t, idA, got)

	got, err = p.TailnetForAuthKey(keyB)
	require.NoError(t, err)
	assert.Equal(t, idB, got)

	got, err = p.TailnetForNode(ca.MachineKey(), nkA)
	require.NoError(t, err)
	assert.Equal(t, idA, got)

	t.Run("spoofed_tenant_header_is_ignored", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, tc.NoiseKey(t), key.NewMachine())
		nk := key.NewNode().Public()
		hdr := http.Header{}
		hdr.Set("X-Tailnet-ID", idB.String())
		hdr.Set("X-Tailnet", tenantB)

		resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
			NodeKey:  nk,
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: keyA},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "spoofed-header"},
		}, hdr)
		require.NoError(t, err)
		require.True(t, resp.OK(), "A's key must still register in A: %d %s", resp.Status, resp.Body)
		require.True(t, rr.MachineAuthorized, "registration with a valid key is authorized")

		assertNodeOnlyIn(t, nk, a, b)
		assertRowTailnet(t, tc, nk, idA.String())
	})

	t.Run("unknown_and_garbage_keys_refused", func(t *testing.T) {
		before := tc.NodeRows(t)

		idOfA, secretOfA := splitKey(t, keyA)
		idOfB, secretOfB := splitKey(t, keyB)

		cases := map[string]string{
			"garbage":              "garbage",
			"empty_looking":        "hskey-auth-",
			"well_formed_unknown":  "hskey-auth-" + strings.Repeat("0", len(idOfA)) + "-" + strings.Repeat("0", len(secretOfA)),
			"tampered_secret_of_a": "hskey-auth-" + idOfA + "-" + flipLast(secretOfA),
			"id_of_a_secret_of_b":  "hskey-auth-" + idOfA + "-" + secretOfB,
			"id_of_b_secret_of_a":  "hskey-auth-" + idOfB + "-" + secretOfA,
		}

		for name, k := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := servertest.TryNewClusterClient(t, tc.Front(), "refused-"+name,
					servertest.ClusterClientOptions{AuthKey: k})
				require.Error(t, err, "key %q must be refused", name)
			})
		}

		after := tc.NodeRows(t)
		assert.Equal(t, before, after, "refused registrations must not create or change any node row in any tailnet")

		for _, r := range after {
			assert.NotContains(t, r.Hostname, "refused-", "refused registration left a node: %s", r)
		}

		for _, k := range []string{cases["garbage"], cases["well_formed_unknown"]} {
			_, err := p.TailnetForAuthKey(k)
			require.ErrorIs(t, err, db.ErrUnknownTenant, "an unknown credential resolves to no tenant")
		}
	})
}

// --- TestClusterSameMachineKeyTwoTailnets (S05, I03) ---

// One device (one machine key) with a profile in A and one in B on the same
// cluster: two independent nodes, each map stream only its own tailnet,
// profile switch A→B→A keeps both identities, and deleting the A node
// leaves the B node and its stream untouched.
func TestClusterSameMachineKeyTwoTailnets(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	idA, idB := tc.Tailnet(t, tenantA).ID, tc.Tailnet(t, tenantB).ID
	keyA := mintKey(t, a, "alice")
	keyB := mintKey(t, b, "alice")

	peerA := servertest.NewClusterClient(t, a, "peer-a", servertest.ClusterClientOptions{AuthKey: keyA})
	peerB := servertest.NewClusterClient(t, b, "peer-b", servertest.ClusterClientOptions{AuthKey: keyB})
	pkA, pkB := selfKey(t, peerA), selfKey(t, peerB)

	device := key.NewMachine()
	hi := &tailcfg.Hostinfo{BackendLogID: "servertest-device", Hostname: "device"}

	// Profile A.
	x := servertest.NewClusterClient(t, a, "device", servertest.ClusterClientOptions{MachineKey: device, AuthKey: keyA})
	waitPeersExactly(t, x, pkA)

	persistA := x.Persist()
	nkA := persistA.PrivateNodeKey.Public()
	nodeA, ok := a.State().GetNodeByNodeKey(nkA)
	require.True(t, ok)

	// Switch to a new profile in B on the same machine.
	require.NoError(t, x.Login(ctx(t), persist.Persist{}, keyB, controlclient.LoginDefault, hi, nil))
	waitPeersExactly(t, x, pkB)

	persistB := x.Persist()
	nkB := persistB.PrivateNodeKey.Public()
	require.NotEqual(t, nkA, nkB, "each profile has its own node key")

	nodeB, ok := b.State().GetNodeByNodeKey(nkB)
	require.True(t, ok, "the B profile is a node of B")
	assert.Equal(t, device.Public(), nodeA.MachineKey())
	assert.Equal(t, device.Public(), nodeB.MachineKey())
	assert.NotEqual(t, nodeA.ID(), nodeB.ID(), "two independent node identities")
	assertNodeOnlyIn(t, nkA, a, b)
	assertNodeOnlyIn(t, nkB, b, a)
	assertRowTailnet(t, tc, nkA, idA.String())
	assertRowTailnet(t, tc, nkB, idB.String())

	p := tc.Platform(t)

	got, err := p.TailnetForNode(device.Public(), nkA)
	require.NoError(t, err)
	assert.Equal(t, idA, got)

	got, err = p.TailnetForNode(device.Public(), nkB)
	require.NoError(t, err)
	assert.Equal(t, idB, got)

	// Switch back to A with no credential: (machine key, node key) picks A,
	// and A's node is resumed, not duplicated.
	rowsBefore := len(tc.NodeRows(t))

	require.NoError(t, x.Login(ctx(t), persistA, "", controlclient.LoginDefault, hi, nil))
	x.WaitForCondition(t, "back on the A profile with only A's peer", clusterWait, func(nm *netmap.NetworkMap) bool {
		return nm.NodeKey == nkA && peerKeysEqual(nm, pkA)
	})

	resumed, ok := a.State().GetNodeByNodeKey(nkA)
	require.True(t, ok)
	assert.Equal(t, nodeA.ID(), resumed.ID(), "A's node identity survives the A→B→A switch")
	assert.Len(t, tc.NodeRows(t), rowsBefore, "switching back creates no node")

	// Both profiles streaming at once from one machine key: each stream
	// carries only its own tailnet.
	y := servertest.NewClusterClient(t, b, "device-profile-b",
		servertest.ClusterClientOptions{MachineKey: device, Persist: persistB, Hostname: "device"})
	y.WaitForCondition(t, "B profile resumed with only B's peer", clusterWait, func(nm *netmap.NetworkMap) bool {
		return nm.NodeKey == nkB && peerKeysEqual(nm, pkB)
	})
	waitPeersExactly(t, x, pkA)
	waitPeersExactly(t, peerA, nkA)
	waitPeersExactly(t, peerB, nkB)

	assertNoTokens(t, "A profile netmap", x.Netmap(), []string{nkB.String(), pkB.String(), "peer-b"})
	assertNoTokens(t, "B profile netmap", y.Netmap(), []string{nkA.String(), pkA.String(), "peer-a"})
	assertNoTokens(t, "peer-a netmap", peerA.Netmap(), []string{nkB.String(), pkB.String(), "peer-b"})
	assertNoTokens(t, "peer-b netmap", peerB.Netmap(), []string{nkA.String(), pkA.String(), "peer-a"})

	// Delete the A node: the B node, its row and its stream stay.
	framesY := y.Frames()

	changes, err := a.State().DeleteNode(resumed)
	require.NoError(t, err)
	tc.Runtime(t, tenantA).Change(changes...)

	peerA.WaitForCondition(t, "A's device node removed", clusterWait, func(nm *netmap.NetworkMap) bool {
		return len(nm.Peers) == 0
	})

	_, ok = a.State().GetNodeByNodeKey(nkA)
	assert.False(t, ok, "A node deleted")

	_, ok = b.State().GetNodeByNodeKey(nkB)
	assert.True(t, ok, "deleting the A node must not delete the B node of the same machine key")
	assertRowTailnet(t, tc, nkB, idB.String())

	assert.Never(t, func() bool { return y.Frames() != framesY || !peerKeysEqual(y.Netmap(), pkB) },
		quietWindow, 100*time.Millisecond, "deleting A's node must not touch the B profile's stream")
	assert.True(t, peerKeysEqual(peerB.Netmap(), nkB), "peer-b still sees the B profile")

	_, err = p.TailnetForNode(device.Public(), nkA)
	require.ErrorIs(t, err, db.ErrUnknownTenant, "a deleted node resolves to no tenant")

	got, err = p.TailnetForNode(device.Public(), nkB)
	require.NoError(t, err)
	assert.Equal(t, idB, got)
}

// --- TestClusterSessionCannotSwitchTenant (I01, S11a/b) ---

// One Noise session is bound to the tenant of its first resolved request.
// A later request resolving to another tenant is refused (contract:
// hscontrol.ErrSessionTenantSwitch) and the session is closed, and the
// refused request has no effect in the other tenant.
func TestClusterSessionCannotSwitchTenant(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)
	idB := tc.Tailnet(t, tenantB).ID
	keyA := mintKey(t, a, "alice")
	keyB := mintKey(t, b, "bob")
	noiseKey := tc.NoiseKey(t)

	// One device with a profile in each tenant, plus an unrelated B node.
	device := key.NewMachine()
	x := servertest.NewClusterClient(t, a, "device-a", servertest.ClusterClientOptions{MachineKey: device, AuthKey: keyA})
	y := servertest.NewClusterClient(t, b, "device-b", servertest.ClusterClientOptions{MachineKey: device, AuthKey: keyB})
	z := servertest.NewClusterClient(t, b, "other-b", servertest.ClusterClientOptions{AuthKey: keyB})

	nkA, nkB, nkZ := selfKey(t, x), selfKey(t, y), selfKey(t, z)
	dkA, dkB := x.Netmap().SelfNode.DiscoKey(), y.Netmap().SelfNode.DiscoKey()

	// The raw sessions below are the only traffic for these nodes.
	x.Disconnect(t)
	y.Disconnect(t)

	bLeaks := []string{idB.String(), tenantB, nkB.String(), nkZ.String(), "device-b", "other-b"}

	refusedSwitch := func(t *testing.T, sess *servertest.RawNoiseSession, resp *servertest.RawResponse, err error) {
		t.Helper()

		if err == nil {
			require.False(t, resp.OK(), "a request for another tenant on a bound session must be refused, got %d %s",
				resp.Status, resp.Body)
			assertNoTokens(t, "refusal body", string(resp.Body), bLeaks)
			t.Logf("refusal: %d, body names ErrSessionTenantSwitch: %v", resp.Status,
				strings.Contains(string(resp.Body), hscontrol.ErrSessionTenantSwitch.Error()))
		}

		require.Eventually(t, func() bool { return sess.PeerClosed(0) }, 5*time.Second, 50*time.Millisecond,
			"the server must close the Noise session after a tenant switch attempt")
		require.Equal(t, 1, sess.Dials(), "both requests must have used one Noise session")
	}

	endpointsOf := func(s *servertest.TestServer, nk key.NodePublic) []string {
		nv, ok := s.State().GetNodeByNodeKey(nk)
		require.True(t, ok)

		var out []string
		for _, ep := range nv.Endpoints().All() {
			out = append(out, ep.String())
		}

		return out
	}

	t.Run("map_A_then_map_B", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, device)

		resp, err := sess.LiteMap(ctx(t), nkA, dkA, "192.0.2.10:41641")
		require.NoError(t, err)
		require.True(t, resp.OK(), "positive control: A's own node maps on a fresh session: %d %s", resp.Status, resp.Body)
		require.Contains(t, endpointsOf(a, nkA), "192.0.2.10:41641", "positive control: the lite map was applied in A")

		resp, err = sess.LiteMap(ctx(t), nkB, dkB, "192.0.2.11:41641")
		refusedSwitch(t, sess, resp, err)
		assert.NotContains(t, endpointsOf(b, nkB), "192.0.2.11:41641", "the refused request must not update B's node")
	})

	t.Run("map_B_then_map_A", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, device)

		resp, err := sess.LiteMap(ctx(t), nkB, dkB, "192.0.2.20:41641")
		require.NoError(t, err)
		require.True(t, resp.OK(), "positive control: B's own node maps on a fresh session: %d %s", resp.Status, resp.Body)

		resp, err = sess.LiteMap(ctx(t), nkA, dkA, "192.0.2.21:41641")
		if err == nil {
			require.False(t, resp.OK(), "map for A on a session bound to B must be refused")
		}

		require.Eventually(t, func() bool { return sess.PeerClosed(0) }, 5*time.Second, 50*time.Millisecond,
			"the server must close the Noise session")
		assert.NotContains(t, endpointsOf(a, nkA), "192.0.2.21:41641", "the refused request must not update A's node")
	})

	t.Run("map_A_then_register_with_B_key", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, device)
		before := tc.NodeRows(t)

		resp, err := sess.LiteMap(ctx(t), nkA, dkA)
		require.NoError(t, err)
		require.True(t, resp.OK())

		nk := key.NewNode().Public()
		resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
			NodeKey:  nk,
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: keyB},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "switch-register"},
		}, nil)
		if err == nil {
			assert.False(t, resp.OK() && rr.MachineAuthorized,
				"credential of B on a session bound to A must not register: %d %s", resp.Status, resp.Body)
		}

		require.Eventually(t, func() bool { return sess.PeerClosed(0) }, 5*time.Second, 50*time.Millisecond,
			"the server must close the Noise session")
		assert.Equal(t, before, tc.NodeRows(t), "no node created or re-keyed in any tailnet")

		_, ok := b.State().GetNodeByNodeKey(nkB)
		assert.True(t, ok, "B's node of this machine keeps its node key")
	})

	t.Run("unknown_node_key_refused", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, device)

		resp, err := sess.LiteMap(ctx(t), key.NewNode().Public(), key.NewDisco().Public())
		if err == nil {
			assert.False(t, resp.OK(), "an unknown (machine key, node key) must be refused")
			assertNoTokens(t, "refusal body", string(resp.Body), bLeaks)
		}

		// Another machine's B node, presented under this machine key.
		other := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, device)

		resp, err = other.LiteMap(ctx(t), nkZ, key.NewDisco().Public())
		if err == nil {
			assert.False(t, resp.OK(), "a node key of another machine must not resolve")
			assertNoTokens(t, "refusal body", string(resp.Body), bLeaks)
		}
	})

	// Last: a registration in A with this machine may re-key A's node.
	t.Run("register_A_then_map_B", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, device)

		resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
			NodeKey:  key.NewNode().Public(),
			Auth:     &tailcfg.RegisterResponseAuth{AuthKey: keyA},
			Hostinfo: &tailcfg.Hostinfo{Hostname: "device-a"},
		}, nil)
		require.NoError(t, err)
		require.True(t, resp.OK() && rr.MachineAuthorized, "positive control: A's key registers: %d %s", resp.Status, resp.Body)

		resp, err = sess.LiteMap(ctx(t), nkB, dkB, "192.0.2.30:41641")
		refusedSwitch(t, sess, resp, err)
		assert.NotContains(t, endpointsOf(b, nkB), "192.0.2.30:41641", "the refused request must not update B's node")
	})
}

// --- TestClusterOverlapAndConfigIndependence (S06, S07, I07) ---

// Same user names and hostnames in A and B; per-tailnet IPAM (the same
// addresses in both); MagicDNS names not de-duplicated across tenants. A
// policy change or a rename (DNS name change) in one tenant sends nothing
// to the other tenant's streams.
func TestClusterOverlapAndConfigIndependence(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	tc.SetPolicy(t, tenantA, allowAll)
	tc.SetPolicy(t, tenantB, allowAll)

	a, b := tc.Tenant(t, tenantA), tc.Tenant(t, tenantB)

	aClients := enroll(t, a, "alice", "laptop", "phone")
	bClients := enroll(t, b, "alice", "laptop", "phone")

	for _, group := range [][]*servertest.TestClient{aClients, bClients} {
		for _, c := range group {
			waitTenantMesh(t, c, group, false)
		}
	}

	aLaptop, aPhone := aClients[0], aClients[1]
	bLaptop, bPhone := bClients[0], bClients[1]

	t.Run("names_and_ipam_independent", func(t *testing.T) {
		for _, c := range slices.Concat(aClients, bClients) {
			nm := c.Netmap()
			want := c.Name + "." + servertest.ClusterBaseDomain + "."
			assert.Equal(t, want, nm.SelfNode.Name(),
				"%s: the same hostname in another tailnet must not force a suffixed name", c.Name)
		}

		assert.Equal(t, aLaptop.Netmap().SelfNode.Addresses().AsSlice(), bLaptop.Netmap().SelfNode.Addresses().AsSlice(),
			"per-tailnet IPAM: the first node of each tailnet gets the same first addresses")
		assert.Equal(t, aPhone.Netmap().SelfNode.Addresses().AsSlice(), bPhone.Netmap().SelfNode.Addresses().AsSlice(),
			"per-tailnet IPAM: allocation in B is not shifted by A")

		// Same IP and name, different node: each lookup stays in its tenant.
		aPeer, ok := aLaptop.PeerByName("phone")
		require.True(t, ok)
		bPeer, ok := bLaptop.PeerByName("phone")
		require.True(t, ok)
		assert.Equal(t, selfKey(t, aPhone), aPeer.Key(), "A's laptop resolves 'phone' to A's phone")
		assert.Equal(t, selfKey(t, bPhone), bPeer.Key(), "B's laptop resolves 'phone' to B's phone")
		assert.NotEqual(t, aPeer.Key(), bPeer.Key())

		_, ok = a.State().GetNodeByNodeKey(selfKey(t, bPhone))
		assert.False(t, ok, "A's state does not know B's phone")
	})

	t.Run("policy_change_in_A_is_silent_in_B", func(t *testing.T) {
		before := settledFrames(t, bClients)

		tc.SetPolicy(t, tenantA, allowSSHPortOnly)

		for _, c := range aClients {
			c.WaitForCondition(t, "A's filter narrowed to port 22", clusterWait, onlyPort22)
		}

		assertQuiet(t, bClients, before, "a policy change in A")

		for _, c := range bClients {
			assert.False(t, onlyPort22(c.Netmap()), "%s: B's packet filter must not follow A's policy", c.Name)
		}
	})

	t.Run("rename_in_A_is_silent_in_B", func(t *testing.T) {
		before := settledFrames(t, bClients)

		nv, ok := a.State().GetNodeByNodeKey(selfKey(t, aPhone))
		require.True(t, ok)

		_, c, err := a.State().RenameNode(nv.ID(), "phone-renamed")
		require.NoError(t, err)
		tc.Runtime(t, tenantA).Change(c)

		wantA := "phone-renamed." + servertest.ClusterBaseDomain + "."
		aLaptop.WaitForCondition(t, "A's laptop sees the renamed phone", clusterWait, func(nm *netmap.NetworkMap) bool {
			return slices.ContainsFunc(nm.Peers, func(p tailcfg.NodeView) bool { return p.Name() == wantA })
		})

		assertQuiet(t, bClients, before, "a rename (MagicDNS change) in A")

		bPeer, ok := bLaptop.PeerByName("phone")
		require.True(t, ok)
		assert.Equal(t, "phone."+servertest.ClusterBaseDomain+".", bPeer.Name(), "B's phone keeps its name")
	})

	t.Run("policy_change_in_B_is_silent_in_A", func(t *testing.T) {
		before := settledFrames(t, aClients)

		tc.SetPolicy(t, tenantB, allowSSHPortOnly)

		for _, c := range bClients {
			c.WaitForCondition(t, "B's filter narrowed to port 22", clusterWait, onlyPort22)
		}

		assertQuiet(t, aClients, before, "a policy change in B")
	})
}

// --- TestClusterRefusesForeignOrInactiveTailnet (I01 owner fence, S11e) ---

// Tailnets owned by another cluster or not ACTIVE get no runtime; their
// credentials and existing nodes are refused, without naming the tailnet.
func TestClusterRefusesForeignOrInactiveTailnet(t *testing.T) {
	refused := []servertest.TailnetSeed{
		{Slug: "foreign", Owner: "some-other-cluster", MintKey: true, SeedNode: true},
		{Slug: "suspended", Status: types.TailnetSuspended, MintKey: true, SeedNode: true},
		{Slug: "pending", Status: types.TailnetPending, MintKey: true, SeedNode: true},
		{Slug: "deleting", Status: types.TailnetDeleting, MintKey: true, SeedNode: true},
	}

	tc := servertest.NewTestCluster(t, append([]servertest.TailnetSeed{{Slug: tenantA}}, refused...)...)
	tc.SetPolicy(t, tenantA, allowAll)

	require.True(t, tc.HasRuntime(t, tenantA), "positive control: the owned ACTIVE tailnet has a runtime")

	owned, err := tc.Platform(t).OwnedTailnets(tc.ClusterID)
	require.NoError(t, err)

	for _, tn := range owned {
		assert.NotEqual(t, tc.Tailnet(t, "foreign").ID.String(), tn.ID, "a foreign tailnet is not owned by this cluster")
	}

	noiseKey := tc.NoiseKey(t)

	for _, s := range refused {
		t.Run(s.Slug, func(t *testing.T) {
			st := tc.Tailnet(t, s.Slug)
			leaks := []string{st.ID.String(), s.Slug, "seed-" + s.Slug}

			assert.False(t, tc.HasRuntime(t, s.Slug), "no runtime for a %s tailnet", s.Slug)

			before := tc.NodeRows(t)

			_, err := servertest.TryNewClusterClient(t, tc.Front(), "refused-"+s.Slug,
				servertest.ClusterClientOptions{AuthKey: st.AuthKey})
			require.Error(t, err, "a key of a %s tailnet must be refused", s.Slug)

			sess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, key.NewMachine())

			resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
				NodeKey:  key.NewNode().Public(),
				Auth:     &tailcfg.RegisterResponseAuth{AuthKey: st.AuthKey},
				Hostinfo: &tailcfg.Hostinfo{Hostname: "refused-raw-" + s.Slug},
			}, nil)
			if err == nil {
				assert.False(t, resp.OK() && rr.MachineAuthorized, "raw registration must be refused")
				assert.Empty(t, rr.AuthURL, "no pending login for a refused tailnet")
				assertNoTokens(t, "register refusal body", string(resp.Body), leaks)
			}

			// A node that already exists in that tailnet cannot map either.
			nodeSess := servertest.NewRawNoiseSession(t, tc.URL, noiseKey, st.NodeMachine)

			resp, err = nodeSess.LiteMap(ctx(t), st.NodeKey, st.NodeDisco)
			if err == nil {
				assert.False(t, resp.OK(), "a node of a %s tailnet must not map", s.Slug)
				assertNoTokens(t, "map refusal body", string(resp.Body), leaks)
			}

			assert.Equal(t, before, tc.NodeRows(t), "no node created or changed in any tailnet")
		})
	}
}

// --- TestClusterWebAuthDisabledInMultiTenant (P04b scope, ADR 0007 §4) ---

// Without an auth key there is no tenant to bind a pending web/OIDC login
// to. A multi-tenant cluster must refuse clearly instead of handing out an
// auth URL that could later be approved into the wrong tailnet.
func TestClusterWebAuthDisabledInMultiTenant(t *testing.T) {
	tc := servertest.NewTestCluster(t, servertest.TailnetSeed{Slug: tenantA}, servertest.TailnetSeed{Slug: tenantB})
	before := tc.NodeRows(t)

	for _, flags := range []controlclient.LoginFlags{controlclient.LoginDefault, controlclient.LoginInteractive} {
		t.Run(fmt.Sprintf("controlclient_flags_%d", flags), func(t *testing.T) {
			c, err := servertest.TryNewClusterClient(t, tc.Front(), "web-login", servertest.ClusterClientOptions{SkipLogin: true})
			require.NoError(t, err)

			url, err := c.Direct().TryLogin(ctx(t), flags)
			assert.Empty(t, url, "a multi-tenant cluster must not hand out an auth URL")
			require.Error(t, err, "registration without a credential must be refused clearly")
		})
	}

	t.Run("raw_register_without_auth", func(t *testing.T) {
		sess := servertest.NewRawNoiseSession(t, tc.URL, tc.NoiseKey(t), key.NewMachine())

		resp, rr, err := sess.Register(ctx(t), &tailcfg.RegisterRequest{
			NodeKey:  key.NewNode().Public(),
			Hostinfo: &tailcfg.Hostinfo{Hostname: "web-raw"},
		}, nil)
		if err == nil {
			assert.False(t, resp.OK() && rr.MachineAuthorized)
			assert.Empty(t, rr.AuthURL, "no pending auth URL")

			if resp.OK() {
				assert.NotEmpty(t, rr.Error, "a 200 answer must carry the refusal in Error")
			}

			assertNoTokens(t, "refusal body", string(resp.Body),
				[]string{tc.Tailnet(t, tenantA).ID.String(), tc.Tailnet(t, tenantB).ID.String(), tenantA, tenantB})
		}
	})

	assert.Equal(t, before, tc.NodeRows(t), "no node created in any tailnet")
}

// --- helpers ---

func ctx(t *testing.T) context.Context {
	t.Helper()

	c, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)

	return c
}

func mintKey(t *testing.T, s *servertest.TestServer, user string) string {
	t.Helper()

	u := s.CreateUser(t, user)

	return s.CreatePreAuthKey(t, types.UserID(u.ID))
}

// enroll registers one client per hostname in s's tailnet, all owned by user.
func enroll(t *testing.T, s *servertest.TestServer, user string, hostnames ...string) []*servertest.TestClient {
	t.Helper()

	k := mintKey(t, s, user)
	out := make([]*servertest.TestClient, 0, len(hostnames))

	for _, h := range hostnames {
		out = append(out, servertest.NewClusterClient(t, s, h, servertest.ClusterClientOptions{AuthKey: k}))
	}

	return out
}

func selfKey(t *testing.T, c *servertest.TestClient) key.NodePublic {
	t.Helper()

	nm := c.Netmap()
	require.NotNil(t, nm, "%s has a netmap", c.Name)

	return nm.NodeKey
}

func peerKeysEqual(nm *netmap.NetworkMap, want ...key.NodePublic) bool {
	if nm == nil || len(nm.Peers) != len(want) {
		return false
	}

	for _, p := range nm.Peers {
		if !slices.Contains(want, p.Key()) {
			return false
		}
	}

	return true
}

func waitPeersExactly(t *testing.T, c *servertest.TestClient, want ...key.NodePublic) {
	t.Helper()

	c.WaitForCondition(t, fmt.Sprintf("exactly %d expected peers", len(want)), clusterWait,
		func(nm *netmap.NetworkMap) bool { return peerKeysEqual(nm, want...) })
}

// waitTenantMesh waits until c sees exactly the other members of group, with
// a packet filter (and SSH rules when ssh is set).
func waitTenantMesh(t *testing.T, c *servertest.TestClient, group []*servertest.TestClient, ssh bool) {
	t.Helper()

	var want []key.NodePublic

	for _, o := range group {
		if o != c {
			want = append(want, selfKey(t, o))
		}
	}

	c.WaitForCondition(t, "tenant mesh complete", clusterWait, func(nm *netmap.NetworkMap) bool {
		return peerKeysEqual(nm, want...) && nm.PacketFilterRules.Len() > 0 &&
			(!ssh || (nm.SSHPolicy != nil && len(nm.SSHPolicy.Rules) > 0))
	})
}

// tokens are identifiers of one tenant that must never appear in another
// tenant's view; ips are matched on address boundaries.
type tokens struct {
	strs []string
	ips  []string
}

// tenantTokens lists every identifier of s's nodes and of user.
func tenantTokens(t *testing.T, s *servertest.TestServer, user string) tokens {
	t.Helper()

	out := tokens{strs: []string{user}}

	for _, nv := range s.State().ListNodes().All() {
		out.strs = append(out.strs,
			nv.NodeKey().String(), nv.MachineKey().String(), nv.DiscoKey().String(),
			nv.Hostname(), nv.GivenName()+"."+servertest.ClusterBaseDomain)
		out.ips = append(out.ips, nv.IPsAsString()...)
	}

	return out
}

// exclusive drops from theirs the IPs that ours also uses (overlap is
// allowed, S06); every other token stays forbidden.
func exclusive(theirs, ours tokens) tokens {
	out := tokens{strs: theirs.strs}

	for _, ip := range theirs.ips {
		if !slices.Contains(ours.ips, ip) {
			out.ips = append(out.ips, ip)
		}
	}

	return out
}

// assertTenantView checks each client's netmap against the server-side
// truth of its own tenant, and scans it for the other tenant's tokens.
func assertTenantView(t *testing.T, own *servertest.TestServer, clients []*servertest.TestClient, foreign tokens) {
	t.Helper()

	ownNodes := map[key.NodePublic]types.NodeView{}
	for _, nv := range own.State().ListNodes().All() {
		ownNodes[nv.NodeKey()] = nv
	}

	require.Len(t, ownNodes, len(clients), "the runtime holds exactly its own tenant's nodes")
	t.Logf("checking %d clients against %d foreign tokens and %d foreign-only addresses",
		len(clients), len(foreign.strs), len(foreign.ips))

	for _, c := range clients {
		nm := c.Netmap()
		self, ok := ownNodes[nm.NodeKey]
		require.True(t, ok, "%s is a node of its own tenant", c.Name)

		for _, p := range nm.Peers {
			nv, ok := ownNodes[p.Key()]
			require.True(t, ok, "%s: peer %s is not a node of this tenant", c.Name, p.Key().ShortString())

			var got []string
			for _, pfx := range p.Addresses().All() {
				got = append(got, pfx.Addr().String())
			}

			assert.ElementsMatch(t, nv.IPsAsString(), got, "%s: peer addresses come from this tenant's IPAM", c.Name)
			assert.Equal(t, nv.Hostname(), p.Hostinfo().Hostname())
		}

		for _, pid := range own.State().ListPeers(self.ID()).All() {
			_, ok := ownNodes[pid.NodeKey()]
			assert.True(t, ok, "%s: server-side peer list stays in the tenant", c.Name)
		}

		assertNoTokens(t, c.Name+" netmap", nm, foreign.strs)
		assertNoIPs(t, c.Name+" netmap", nm, foreign.ips)
	}
}

// view is the part of a netmap a tenant boundary must hold for.
func view(v any) string {
	nm, ok := v.(*netmap.NetworkMap)
	if !ok {
		if s, ok := v.(string); ok {
			return s
		}

		b, _ := json.Marshal(v)

		return string(b)
	}

	if nm == nil {
		return ""
	}

	b, err := json.Marshal(struct {
		Self         tailcfg.NodeView
		Peers        []tailcfg.NodeView
		Filter       any
		SSH          *tailcfg.SSHPolicy
		DNS          tailcfg.DNSConfig
		UserProfiles any
	}{nm.SelfNode, nm.Peers, nm.PacketFilterRules, nm.SSHPolicy, nm.DNS, nm.UserProfiles})
	if err != nil {
		panic(err)
	}

	return string(b)
}

func assertNoTokens(t *testing.T, what string, v any, forbidden []string) {
	t.Helper()

	s := view(v)
	for _, f := range forbidden {
		if f == "" {
			continue
		}

		assert.NotContains(t, s, f, "%s leaks another tenant's %q", what, f)
	}
}

func assertNoIPs(t *testing.T, what string, v any, ips []string) {
	t.Helper()

	s := view(v)
	for _, ip := range ips {
		re := regexp.MustCompile(`(^|[^0-9A-Fa-f:.])` + regexp.QuoteMeta(ip) + `([^0-9A-Fa-f:.]|$)`)
		assert.False(t, re.MatchString(s), "%s leaks another tenant's address %s", what, ip)
	}
}

func assertNodeOnlyIn(t *testing.T, nk key.NodePublic, in, notIn *servertest.TestServer) {
	t.Helper()

	_, ok := in.State().GetNodeByNodeKey(nk)
	assert.True(t, ok, "node %s is in its tenant", nk.ShortString())

	_, ok = notIn.State().GetNodeByNodeKey(nk)
	assert.False(t, ok, "node %s must not exist in the other tenant", nk.ShortString())
}

func assertRowTailnet(t *testing.T, tc *servertest.TestCluster, nk key.NodePublic, wantTailnet string) {
	t.Helper()

	var found []servertest.NodeRow

	for _, r := range tc.NodeRows(t) {
		if strings.Contains(r.NodeKey, strings.TrimPrefix(nk.String(), "nodekey:")) {
			found = append(found, r)
		}
	}

	require.Len(t, found, 1, "exactly one row for node key %s", nk.ShortString())
	assert.Equal(t, wantTailnet, found[0].TailnetID)
}

func splitKey(t *testing.T, k string) (string, string) {
	t.Helper()

	rest, ok := strings.CutPrefix(k, "hskey-auth-")
	require.True(t, ok, "auth key has the hskey-auth- prefix")

	// Fixed-length identifier (db.keyIdentifierLength = 12), then '-'.
	require.Greater(t, len(rest), 13)
	require.Equal(t, byte('-'), rest[12])

	return rest[:12], rest[13:]
}

func flipLast(s string) string {
	last := s[len(s)-1]

	repl := byte('0')
	if last == '0' {
		repl = '1'
	}

	return s[:len(s)-1] + string(repl)
}

func frames(cs []*servertest.TestClient) []int {
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.Frames()
	}

	return out
}

// settledFrames waits until cs received no map response for a full second
// (their own tenant's setup can still be settling) and returns the counts.
// A quiet check that starts earlier would blame the other tenant for them.
func settledFrames(t *testing.T, cs []*servertest.TestClient) []int {
	t.Helper()

	last := frames(cs)
	stableSince := time.Now()
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // sampling counters

		cur := frames(cs)
		if !slices.Equal(cur, last) {
			last, stableSince = cur, time.Now()

			continue
		}

		if time.Since(stableSince) >= time.Second {
			return cur
		}
	}

	t.Fatalf("clients did not settle within 15s: frames %v", last)

	return nil
}

func assertQuiet(t *testing.T, cs []*servertest.TestClient, before []int, cause string) {
	t.Helper()

	assert.Never(t, func() bool {
		return !slices.Equal(frames(cs), before)
	}, quietWindow, 100*time.Millisecond, "%s must send no map response to the other tenant's clients", cause)
}

func onlyPort22(nm *netmap.NetworkMap) bool {
	if nm == nil || nm.PacketFilterRules.Len() == 0 {
		return false
	}

	for _, r := range nm.PacketFilterRules.All() {
		for _, d := range r.DstPorts {
			if d.Ports.First != 22 || d.Ports.Last != 22 {
				return false
			}
		}
	}

	return true
}
