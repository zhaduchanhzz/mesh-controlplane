package tenantlabel

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestLabelCap(t *testing.T) {
	reset()
	t.Cleanup(reset)
	SetLimit(3)

	for i := range 3 {
		id := fmt.Sprintf("t%d", i)
		if got := Acquire(id); got != id {
			t.Fatalf("Acquire(%s) = %q, want own label", id, got)
		}
	}

	before := value(t, overflow)
	if got := Acquire("t3"); got != Other {
		t.Fatalf("Acquire past cap = %q, want %q", got, Other)
	}

	if got := value(t, overflow) - before; got != 1 {
		t.Fatalf("overflow += %v, want 1", got)
	}

	// A labelled tailnet keeps its label while held, even after the cap is
	// reached; another runtime of it shares the label.
	if got := Acquire("t1"); got != "t1" {
		t.Fatalf("Acquire(t1) = %q after cap", got)
	}

	if got := Label("t1"); got != "t1" {
		t.Fatalf("Label(t1) = %q", got)
	}

	if got := Label(""); got != Other {
		t.Fatalf("Label(\"\") = %q", got)
	}

	if got := value(t, labelled); got != 3 {
		t.Fatalf("labelled = %v, want 3", got)
	}
}

// P13f L4: looking a label up never takes one: a tailnet this process does
// not serve (another cluster's, retired) gets "other" and costs nothing.
func TestLabelNeverAdmits(t *testing.T) {
	reset()
	t.Cleanup(reset)
	SetLimit(2)

	for i := range 100 {
		if got := Label(fmt.Sprintf("unserved-%d", i)); got != Other {
			t.Fatalf("Label of an unserved tailnet = %q, want %q", got, Other)
		}
	}

	if got := value(t, labelled); got != 0 {
		t.Fatalf("labelled = %v after lookups, want 0", got)
	}

	if got := Acquire("served"); got != "served" {
		t.Fatalf("Acquire after lookups = %q: the cap was used up", got)
	}
}

// P13f L4: the last release frees the label and deletes the tailnet's
// series; a runtime still holding it (reloaded while the old one drains)
// keeps it.
func TestReleaseRefCountedAndDeletesSeries(t *testing.T) {
	reset()
	t.Cleanup(reset)
	SetLimit(1)

	vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tenantlabel_test_total", Help: "test"},
		[]string{"tailnet", "type"})
	Register(vec)

	t.Cleanup(func() {
		mu.Lock()
		vecs = vecs[:len(vecs)-1]
		mu.Unlock()
	})

	for range 2 {
		if Acquire("a") != "a" {
			t.Fatal("two runtimes of a do not share its label")
		}
	}

	vec.WithLabelValues("a", "x").Inc()
	vec.WithLabelValues(Other, "x").Inc()

	Release("a")

	if Label("a") != "a" {
		t.Fatal("the label was freed while a runtime still holds it")
	}

	Release("a")

	if Label("a") != Other {
		t.Fatal("the label is still held after the last release")
	}

	if got := value(t, labelled); got != 0 {
		t.Fatalf("labelled = %v, want 0", got)
	}

	if n := countSeries(vec); n != 1 {
		t.Fatalf("%d series left, want only the %q one", n, Other)
	}

	if got := Acquire("b"); got != "b" {
		t.Fatalf("Acquire(b) after a was released = %q, want its own label", got)
	}

	Release("never-held") // no-op
}

func TestSetLimitDefault(t *testing.T) {
	reset()
	t.Cleanup(reset)
	SetLimit(0)

	mu.Lock()
	defer mu.Unlock()

	if limit != DefaultLimit {
		t.Fatalf("limit = %d, want %d", limit, DefaultLimit)
	}
}

func countSeries(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)

	n := 0
	for range ch {
		n++
	}

	return n
}

func value(t *testing.T, m prometheus.Metric) float64 {
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
