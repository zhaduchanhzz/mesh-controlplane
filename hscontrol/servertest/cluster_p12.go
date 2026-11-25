//go:build p05cluster

package servertest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P12: two cluster IDs on one shared database, each with its own process,
// listener, server_url, Noise key and lease, as c1 and c2 in the lab.

// AddCluster starts another cluster on tc's shared database: its own
// cluster ID, listener, server_url, Noise key and admin socket, registered
// in the placement registry as an ACTIVE shared cluster. It waits until the
// cluster holds its lease (/ready). Both share tc's tailnet names, so
// Tailnet/WaitRuntime/HasRuntime work on either.
func (tc *TestCluster) AddCluster(tb testing.TB) *TestCluster {
	tb.Helper()

	addr := freeLoopbackAddr(tb)

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		tb.Fatalf("servertest: %v", err)
	}

	sockDir, err := os.MkdirTemp("", "p12")
	if err != nil {
		tb.Fatalf("servertest: socket dir: %v", err)
	}

	tb.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	cfg := *tc.cfg
	cfg.ClusterID = "p12-cluster-" + uuid.NewV4().String()[:8]
	cfg.ServerURL = "http://localhost:" + port
	cfg.Addr = cfgListenAddr(addr)
	cfg.UnixSocket = filepath.Join(sockDir, "hs.sock")
	cfg.Noise.PrivateKeyPath = filepath.Join(tb.TempDir(), "noise_private.key")

	o := &TestCluster{
		URL:       cfg.ServerURL,
		ClusterID: cfg.ClusterID,
		cfg:       &cfg,
		tailnets:  tc.tailnets,
		raw:       tc.raw,
		host:      "localhost",
	}

	o.registerCluster(tb)

	p := o.startProcess(tb, addr)
	o.Cluster = p.Cluster
	o.waitHealthy(tb, p.done)

	return o
}

// Registry is the placement registry on the shared database, as
// mesh-placement uses it.
func (tc *TestCluster) Registry(tb testing.TB) *placement.Registry {
	tb.Helper()

	r, err := placement.New(tc.Platform(tb).RegistryDB(), placement.Config{})
	if err != nil {
		tb.Fatalf("servertest: registry: %v", err)
	}

	return r
}

// TestOperator is the operator of registry calls made by tests.
var TestOperator = placement.Op{Actor: "spiffe://mesh/operator/servertest"}

// PlaceTailnet creates slug through placement (shared class: the registry
// chooses the owner among the clusters with a live lease) and activates
// it. It returns the owner cluster ID; the owner loads it live.
func (tc *TestCluster) PlaceTailnet(tb testing.TB, slug string) string {
	tb.Helper()

	ctx := context.Background()
	r := tc.Registry(tb)

	tn, err := r.CreateTailnet(ctx, TestOperator, slug, placement.ClassShared)
	if err != nil {
		tb.Fatalf("servertest: place tailnet %q: %v", slug, err)
	}

	_, err = r.ActivateTailnet(ctx, TestOperator, tn.ID, tn.OwnerEpoch)
	if err != nil {
		tb.Fatalf("servertest: activate tailnet %q: %v", slug, err)
	}

	tc.tailnets[slug] = &SeededTailnet{ID: tailnet.MustParseID(tn.ID), Slug: slug}

	return tn.OwnerClusterID
}

// MoveOwner requests the move of slug from cluster from to cluster to
// through the registry, at the current epoch (ADR 0010: the owner hands it
// over once it stopped serving; owner_epoch + 1 then). It returns the epoch
// the move was requested at.
func (tc *TestCluster) MoveOwner(tb testing.TB, slug, from, to string) uint64 {
	tb.Helper()

	row := tc.TailnetRow(tb, slug)

	tn, err := tc.Registry(tb).MoveOwner(context.Background(), TestOperator, row.ID, from, row.OwnerEpoch, to)
	if err != nil {
		tb.Fatalf("servertest: move %q %s → %s: %v", slug, from, to, err)
	}

	return tn.OwnerEpoch
}
