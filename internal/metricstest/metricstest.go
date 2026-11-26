// Package metricstest reads metrics from a Prometheus registry in tests
// (P13f). It is test support only; no production code imports it.
package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Series is one gathered series: its labels and its value (a counter or
// gauge value, or a histogram's sample count).
type Series struct {
	Labels map[string]string
	Value  float64
}

// Gather returns every series of name in g.
func Gather(tb testing.TB, g prometheus.Gatherer, name string) []Series {
	tb.Helper()

	mfs, err := g.Gather()
	if err != nil {
		tb.Fatalf("metricstest: gather: %v", err)
	}

	var out []Series

	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}

		for _, m := range mf.GetMetric() {
			s := Series{Labels: map[string]string{}}
			for _, lp := range m.GetLabel() {
				s.Labels[lp.GetName()] = lp.GetValue()
			}

			switch {
			case m.GetCounter() != nil:
				s.Value = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				s.Value = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				s.Value = float64(m.GetHistogram().GetSampleCount())
			case m.GetUntyped() != nil:
				s.Value = m.GetUntyped().GetValue()
			}

			out = append(out, s)
		}
	}

	return out
}

// Value returns the value of the first series name{labels} in the default
// registry. Labels not given are not compared. ok is false when no series
// matches.
func Value(tb testing.TB, name string, labels map[string]string) (float64, bool) {
	tb.Helper()

	for _, s := range Gather(tb, prometheus.DefaultGatherer, name) {
		if s.Match(labels) {
			return s.Value, true
		}
	}

	return 0, false
}

// Sum adds the values of every series name{labels} in the default registry.
func Sum(tb testing.TB, name string, labels map[string]string) float64 {
	tb.Helper()

	var sum float64

	for _, s := range Gather(tb, prometheus.DefaultGatherer, name) {
		if s.Match(labels) {
			sum += s.Value
		}
	}

	return sum
}

// HasLabelValue reports the name of a metric in the default registry that
// has a series with a label whose value is v (e.g. a tailnet ID that must
// not be labelled), or false.
func HasLabelValue(tb testing.TB, v string) (string, bool) {
	tb.Helper()

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		tb.Fatalf("metricstest: gather: %v", err)
	}

	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetValue() == v {
					return mf.GetName(), true
				}
			}
		}
	}

	return "", false
}

// Match reports whether s has every label of want with the same value.
func (s Series) Match(want map[string]string) bool {
	for k, v := range want {
		if s.Labels[k] != v {
			return false
		}
	}

	return true
}
