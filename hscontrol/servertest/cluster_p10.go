//go:build p05cluster

package servertest

import (
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P10 helpers: change tailnet ownership under a running cluster, the way an
// operator move (placement CAS) or another cluster would.

// SetTailnetOwner sets owner_cluster_id and owner_epoch of slug directly in
// the shared database.
func (tc *TestCluster) SetTailnetOwner(tb testing.TB, slug, owner string, epoch uint64) {
	tb.Helper()

	err := tc.raw.Model(&types.Tailnet{}).Where("id = ?", tc.Tailnet(tb, slug).ID.String()).
		Updates(map[string]any{"owner_cluster_id": owner, "owner_epoch": epoch, "updated_at": time.Now().UTC()}).Error
	if err != nil {
		tb.Fatalf("servertest: set owner of %q: %v", slug, err)
	}
}

// SetTailnetStatus sets the status of slug directly in the shared database.
func (tc *TestCluster) SetTailnetStatus(tb testing.TB, slug string, status types.TailnetStatus) {
	tb.Helper()

	err := tc.raw.Model(&types.Tailnet{}).Where("id = ?", tc.Tailnet(tb, slug).ID.String()).
		Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()}).Error
	if err != nil {
		tb.Fatalf("servertest: set status of %q: %v", slug, err)
	}
}

// TailnetRow reads the tailnets row of slug.
func (tc *TestCluster) TailnetRow(tb testing.TB, slug string) types.Tailnet {
	tb.Helper()

	var tn types.Tailnet

	err := tc.raw.Where("id = ?", tc.Tailnet(tb, slug).ID.String()).First(&tn).Error
	if err != nil {
		tb.Fatalf("servertest: read tailnet %q: %v", slug, err)
	}

	return tn
}

// CreateTailnetLive creates slug through the platform (PENDING with a
// deny-all policy) while the cluster runs, then makes it ACTIVE. The cluster
// must pick it up without a restart.
func (tc *TestCluster) CreateTailnetLive(tb testing.TB, slug string) *SeededTailnet {
	tb.Helper()

	p := tc.Platform(tb)

	tn, err := p.CreateTailnet(slug)
	if err != nil {
		tb.Fatalf("servertest: create tailnet %q: %v", slug, err)
	}

	id := tailnet.MustParseID(tn.ID)

	err = p.SetTailnetStatus(id, types.TailnetActive)
	if err != nil {
		tb.Fatalf("servertest: activate tailnet %q: %v", slug, err)
	}

	st := &SeededTailnet{ID: id, Slug: slug}
	tc.tailnets[slug] = st

	return st
}

// WaitRuntime waits until the cluster serves (want=true) or stops serving
// (want=false) slug.
func (tc *TestCluster) WaitRuntime(tb testing.TB, slug string, want bool, within time.Duration) {
	tb.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, ok := tc.Cluster.Runtime(tc.Tailnet(tb, slug).ID); ok == want {
			return
		}

		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // polling the cluster's reconcile loop
	}

	tb.Fatalf("servertest: runtime of %q present=%v not reached within %s", slug, want, within)
}
