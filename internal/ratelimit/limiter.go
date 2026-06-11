// Package ratelimit provides a per-key token-bucket rate limiter with no HTTP
// dependency, so it can be unit-tested in isolation and reused by any caller.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter is a per-key token-bucket rate limiter, safe for concurrent use.
// Construct with New; the zero value is not usable.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64 // tokens (requests) per second
	burst     float64 // bucket capacity
	ttl       time.Duration
	now       func() time.Time // injectable clock (tests)
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter allowing `rate` requests/second per key with capacity `burst`
// (clamped to >= 1). Buckets idle longer than `ttl` are evicted opportunistically.
func New(rate float64, burst int, ttl time.Duration) *Limiter {
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	return &Limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   b,
		ttl:     ttl,
		now:     time.Now,
	}
}

// Allow refills key's bucket by elapsed*rate (capped at burst), then takes one token.
// When denied, retryAfter is the rounded-up time until one token is available.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.rate <= 0 { // defensive: a non-positive rate never limits
		return true, 0
	}

	now := l.now()
	l.sweep(now)

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	secs := (1 - b.tokens) / l.rate
	return false, time.Duration(math.Ceil(secs)) * time.Second
}

// sweep evicts idle buckets. Called under l.mu, at most once per ttl window.
func (l *Limiter) sweep(now time.Time) {
	if l.ttl <= 0 || now.Sub(l.lastSweep) < l.ttl {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.last) >= l.ttl {
			delete(l.buckets, k)
		}
	}
}
