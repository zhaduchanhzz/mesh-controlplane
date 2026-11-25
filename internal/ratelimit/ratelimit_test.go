package ratelimit

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	t0 := time.Unix(1000, 0)
	b := NewBucket(2, 3, t0)

	for i := range 3 {
		if !b.Allow(t0) {
			t.Fatalf("token %d of the burst refused", i)
		}
	}

	if b.Allow(t0) {
		t.Fatal("past the burst allowed")
	}

	if !b.Allow(t0.Add(500 * time.Millisecond)) {
		t.Fatal("refill at 2/s: one token after 500ms")
	}

	if b.Allow(t0.Add(500 * time.Millisecond)) {
		t.Fatal("only one token refilled")
	}

	// A clock going back does not mint tokens.
	if b.Allow(t0) {
		t.Fatal("clock going back allowed")
	}

	if !b.Allow(t0.Add(time.Hour)) || !b.Allow(t0.Add(time.Hour)) || !b.Allow(t0.Add(time.Hour)) || b.Allow(t0.Add(time.Hour)) {
		t.Fatal("refill is capped at the burst")
	}
}

func TestBucketsIndependentAndBounded(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s := NewBuckets(1, 1)

	if !s.Allow("a", t0) || s.Allow("a", t0) {
		t.Fatal("a: one token")
	}

	if !s.Allow("b", t0) {
		t.Fatal("b is not limited by a")
	}

	for i := range MaxKeys + 10 {
		s.Allow(string(rune(i))+"x", t0)
	}

	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()

	if n > MaxKeys {
		t.Fatalf("%d buckets, bound %d", n, MaxKeys)
	}
}
