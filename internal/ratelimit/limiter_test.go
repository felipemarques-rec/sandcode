package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock lets tests advance time deterministically.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(rate float64, burst int) (*Limiter, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := New(rate, burst, time.Minute)
	l.now = clk.now
	return l, clk
}

func TestLimiter_BurstThenDeny(t *testing.T) {
	l, _ := newTestLimiter(1, 3) // 1 req/s, burst 3
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("ip"); !ok {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	ok, retry := l.Allow("ip")
	if ok {
		t.Fatal("4th request should be denied")
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retryAfter = %v, want (0,1s]", retry)
	}
}

func TestLimiter_RefillOverTime(t *testing.T) {
	l, clk := newTestLimiter(1, 1)
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("first request allowed")
	}
	if ok, _ := l.Allow("ip"); ok {
		t.Fatal("second immediate request denied")
	}
	clk.add(time.Second) // one token refilled
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("request after 1s should be allowed")
	}
}

func TestLimiter_DistinctKeysIndependent(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a allowed")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b independent of a")
	}
}

func TestLimiter_EvictsIdleBucket(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := New(1, 1, time.Minute)
	l.now = clk.now

	l.Allow("stale")
	clk.add(2 * time.Minute) // exceed ttl → next Allow sweeps

	l.Allow("fresh") // triggers sweep; "stale" should be evicted
	l.mu.Lock()
	_, stalePresent := l.buckets["stale"]
	_, freshPresent := l.buckets["fresh"]
	l.mu.Unlock()
	if stalePresent {
		t.Fatal("stale bucket should have been evicted")
	}
	if !freshPresent {
		t.Fatal("fresh bucket should be present")
	}
}

func TestLimiter_ConcurrentAllow(t *testing.T) {
	l := New(1000, 1000, time.Minute) // generous so most allow
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l.Allow("shared")
				l.Allow(string(rune('a' + n%26)))
			}
		}(i)
	}
	wg.Wait()
}
