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
		if got := Label(id); got != id {
			t.Fatalf("Label(%s) = %q, want own label", id, got)
		}
	}

	before := value(t, overflow)
	if got := Label("t3"); got != Other {
		t.Fatalf("Label past cap = %q, want %q", got, Other)
	}

	if got := value(t, overflow) - before; got != 1 {
		t.Fatalf("overflow += %v, want 1", got)
	}

	// Once labelled, always labelled, even after the cap is reached.
	if got := Label("t1"); got != "t1" {
		t.Fatalf("Label(t1) = %q after cap", got)
	}

	if got := Label(""); got != Other {
		t.Fatalf("Label(\"\") = %q", got)
	}

	if got := value(t, labelled); got != 3 {
		t.Fatalf("labelled = %v, want 3", got)
	}
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
