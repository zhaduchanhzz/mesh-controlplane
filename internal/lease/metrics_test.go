package lease

import (
	"context"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/metricstest"
)

// P13f: the lease of a cluster is observable on the operator metrics
// listener: which process holds it (held, epoch), how long until it would
// self-fence (deadline), how long renewals take, and what happened to it
// (acquired, lost, self-fence). The values come from the default registry,
// which the cluster's metrics listener serves.
func TestLeaseMetrics(t *testing.T) {
	s := openStore(t)
	cluster := "metrics-c1"

	m, err := newManager(s, cluster, "proc-a", fast)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	acquired := make(chan Lease, 1)
	lost := make(chan string, 1)
	done := make(chan struct{})

	go func() {
		m.Run(ctx, func(_ context.Context, l Lease) error { acquired <- l; return nil },
			func(_ Lease, reason string) { lost <- reason })
		close(done)
	}()

	var l Lease
	select {
	case l = <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("never acquired")
	}

	me := map[string]string{"cluster": cluster, "holder": "proc-a"}

	held, ok := metricstest.Value(t, "headscale_cluster_lease_held", me)
	if !ok || held != 1 {
		t.Fatalf("headscale_cluster_lease_held%v = %v (present %t), want 1", me, held, ok)
	}

	epoch, _ := metricstest.Value(t, "headscale_cluster_lease_epoch", me)
	if epoch != float64(l.Epoch) {
		t.Errorf("headscale_cluster_lease_epoch = %v, want %d", epoch, l.Epoch)
	}

	left, ok := metricstest.Value(t, "headscale_cluster_lease_deadline_seconds", me)
	if !ok || left <= 0 || left > (fast.TTL-fast.SafetyMargin).Seconds() {
		t.Errorf("headscale_cluster_lease_deadline_seconds = %v (present %t), want in (0, %s]",
			left, ok, fast.TTL-fast.SafetyMargin)
	}

	c := map[string]string{"cluster": cluster}

	if n, _ := metricstest.Value(t, "headscale_cluster_lease_events_total",
		map[string]string{"cluster": cluster, "event": "acquired"}); n < 1 {
		t.Errorf("acquired events = %v, want >= 1", n)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, _ := metricstest.Value(t, "headscale_cluster_lease_renew_duration_seconds", c)
		if n >= 1 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("no renewal recorded in headscale_cluster_lease_renew_duration_seconds")
		}

		time.Sleep(50 * time.Millisecond)
	}

	// Another holder takes the row over: the next renewal finds the lease
	// lost.
	err = s.db.Exec(`UPDATE cluster_leases SET holder = 'proc-b', epoch = epoch + 1 WHERE cluster_id = ?`, cluster).Error
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("lease loss not noticed")
	}

	if held, _ := metricstest.Value(t, "headscale_cluster_lease_held", me); held != 0 {
		t.Errorf("held after the loss = %v, want 0", held)
	}

	if left, _ := metricstest.Value(t, "headscale_cluster_lease_deadline_seconds", me); left != 0 {
		t.Errorf("deadline after the loss = %v, want 0", left)
	}

	if n, _ := metricstest.Value(t, "headscale_cluster_lease_events_total",
		map[string]string{"cluster": cluster, "event": "lost"}); n < 1 {
		t.Errorf("lost events = %v, want >= 1", n)
	}

	cancel()
	<-done

	// A stopped manager is no longer reported.
	if _, ok := metricstest.Value(t, "headscale_cluster_lease_held", me); ok {
		t.Error("a stopped manager still has a headscale_cluster_lease_held series")
	}
}

// P13f: a self-fence (renewals hang, F03) is its own event, so an alert can
// tell a database outage from a takeover.
func TestLeaseMetricsSelfFence(t *testing.T) {
	s := openStore(t)
	cluster := "metrics-c2"

	m, err := newManager(stuckStore{s}, cluster, "proc-a", fast)
	if err != nil {
		t.Fatal(err)
	}

	lost := make(chan string, 1)

	go m.Run(t.Context(), func(context.Context, Lease) error { return nil },
		func(_ Lease, reason string) { lost <- reason })

	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("never self-fenced")
	}

	n, _ := metricstest.Value(t, "headscale_cluster_lease_events_total",
		map[string]string{"cluster": cluster, "event": "self_fence"})
	if n < 1 {
		t.Errorf("self_fence events = %v, want >= 1", n)
	}
}
