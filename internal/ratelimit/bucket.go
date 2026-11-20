// Package ratelimit implements a small, dependency-free token bucket.
package ratelimit

import (
	"sync"
	"time"
)

// Bucket is a token bucket safe for concurrent use.
type Bucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	perSec   float64
	last     time.Time
	now      func() time.Time
}

// New creates a bucket allowing perMinute sustained requests with the given
// burst capacity.
func New(perMinute, burst int) *Bucket {
	return newWithClock(perMinute, burst, time.Now)
}

func newWithClock(perMinute, burst int, now func() time.Time) *Bucket {
	if perMinute < 1 {
		perMinute = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Bucket{
		capacity: float64(burst),
		tokens:   float64(burst),
		perSec:   float64(perMinute) / 60.0,
		last:     now(),
		now:      now,
	}
}

// Allow consumes one token. The returned duration is the suggested wait when
// the request is refused.
func (b *Bucket) Allow() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.perSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	deficit := 1 - b.tokens
	wait := time.Duration(deficit / b.perSec * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}
