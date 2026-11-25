// Package ratelimit is a token bucket and a bounded set of buckets by key,
// the model of internal/bootstrap (P11), for the public /api/v2 of a cluster
// (P08b).
package ratelimit

import (
	"sync"
	"time"
)

// Bucket is a token bucket: Rate tokens per second up to Burst.
type Bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// NewBucket returns a full bucket.
func NewBucket(rate float64, burst int, now time.Time) *Bucket {
	return &Bucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now}
}

// Allow takes a token if there is one.
func (b *Bucket) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}

		b.last = now
	}

	if b.tokens < 1 {
		return false
	}

	b.tokens--

	return true
}

// MaxKeys bounds a [Buckets]: past it the set starts over, losing at most
// one burst of history, so a flood of keys cannot grow memory.
const MaxKeys = 100_000

// Buckets is a set of buckets by key, created full on first use.
type Buckets struct {
	mu    sync.Mutex
	rate  float64
	burst int
	m     map[string]*Bucket
}

// NewBuckets returns an empty set.
func NewBuckets(rate float64, burst int) *Buckets {
	return &Buckets{rate: rate, burst: burst, m: make(map[string]*Bucket)}
}

// Allow takes a token from key's bucket.
func (s *Buckets) Allow(key string, now time.Time) bool {
	s.mu.Lock()

	b, ok := s.m[key]
	if !ok {
		if len(s.m) >= MaxKeys {
			s.m = make(map[string]*Bucket)
		}

		b = NewBucket(s.rate, s.burst, now)
		s.m[key] = b
	}

	s.mu.Unlock()

	return b.Allow(now)
}
