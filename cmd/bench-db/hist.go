//go:build benchdb

package main

import (
	"math"
	"time"
)

// hist is a log-bucketed latency histogram (bucket width ~2%), mergeable
// across processes so the coordinator reports percentiles over all clusters.
type hist struct {
	Buckets []uint64 `json:"b"`
	Count   uint64   `json:"n"`
	Errors  uint64   `json:"e"`
	MaxNs   int64    `json:"max"`
	SumNs   int64    `json:"sum"`
	// FirstErr is the first error message seen (redacted by construction:
	// database errors here carry no secrets).
	FirstErr string `json:"first_err,omitempty"`
}

const (
	histBase    = 1.02
	histMinNs   = 1000.0 // 1µs
	histBuckets = 1000   // covers 1µs .. ~400s
)

func newHist() *hist { return &hist{Buckets: make([]uint64, histBuckets)} }

func bucketOf(d time.Duration) int {
	ns := float64(d.Nanoseconds())
	if ns <= histMinNs {
		return 0
	}

	b := int(math.Log(ns/histMinNs) / math.Log(histBase))
	if b >= histBuckets {
		b = histBuckets - 1
	}

	return b
}

func (h *hist) add(d time.Duration, err error) {
	if err != nil {
		h.Errors++
		if h.FirstErr == "" {
			h.FirstErr = err.Error()
		}

		return
	}

	h.Buckets[bucketOf(d)]++
	h.Count++
	h.SumNs += d.Nanoseconds()

	if d.Nanoseconds() > h.MaxNs {
		h.MaxNs = d.Nanoseconds()
	}
}

func (h *hist) merge(o *hist) {
	for i, v := range o.Buckets {
		h.Buckets[i] += v
	}

	h.Count += o.Count
	h.Errors += o.Errors
	h.SumNs += o.SumNs

	if o.MaxNs > h.MaxNs {
		h.MaxNs = o.MaxNs
	}

	if h.FirstErr == "" {
		h.FirstErr = o.FirstErr
	}
}

// quantile returns the upper edge of the bucket holding quantile q, in ms.
func (h *hist) quantile(q float64) float64 {
	if h.Count == 0 {
		return 0
	}

	target := uint64(math.Ceil(q * float64(h.Count)))

	var seen uint64

	for i, v := range h.Buckets {
		seen += v
		if seen >= target {
			return histMinNs * math.Pow(histBase, float64(i+1)) / 1e6
		}
	}

	return float64(h.MaxNs) / 1e6
}

type histSummary struct {
	Count    uint64  `json:"count"`
	Errors   uint64  `json:"errors"`
	MeanMs   float64 `json:"mean_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MaxMs    float64 `json:"max_ms"`
	FirstErr string  `json:"first_err,omitempty"`
}

func (h *hist) summary() histSummary {
	s := histSummary{
		Count: h.Count, Errors: h.Errors, FirstErr: h.FirstErr,
		P50Ms: h.quantile(0.50), P95Ms: h.quantile(0.95), P99Ms: h.quantile(0.99),
		MaxMs: float64(h.MaxNs) / 1e6,
	}
	if h.Count > 0 {
		s.MeanMs = float64(h.SumNs) / float64(h.Count) / 1e6
	}

	return s
}
