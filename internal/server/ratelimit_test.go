package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
)

func newRLTestServer(t *testing.T, rps float64, burst int) *Server {
	t.Helper()
	return New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		RateLimit:  &RateLimitConfig{RequestsPerSecond: rps, Burst: burst, TTL: time.Minute},
	})
}

func TestRateLimit_429AfterBurst(t *testing.T) {
	h := newRLTestServer(t, 1, 2).Handler() // burst 2
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/runs", nil)
		req.RemoteAddr = "203.0.113.7:5555"
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly limited", i)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request code = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After header")
	}
}

func TestRateLimit_HealthzAndMetricsExempt(t *testing.T) {
	h := newRLTestServer(t, 1, 1).Handler()
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.RemoteAddr = "203.0.113.8:1"
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatal("/healthz must be exempt from rate limiting")
		}
	}
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = "203.0.113.8:1"
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatal("/metrics must be exempt from rate limiting")
		}
	}
}

func TestRateLimit_NilConfigPassThrough(t *testing.T) {
	s := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		// RateLimit nil
	})
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/runs", nil)
		req.RemoteAddr = "203.0.113.9:1"
		s.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatal("nil RateLimit must never limit (byte-identical)")
		}
	}
}

func TestPerimeter_CORSHeaderOn429(t *testing.T) {
	// CORS outermost + rate-limit inside: a 429 must still carry the CORS header
	// so a browser can read the throttled response.
	s := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		RateLimit:  &RateLimitConfig{RequestsPerSecond: 1, Burst: 1, TTL: time.Minute},
		CORS:       &CORSConfig{AllowedOrigins: []string{"https://app.example.com"}},
	})
	h := s.Handler()

	send := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/runs", nil)
		req.RemoteAddr = "203.0.113.10:9"
		req.Header.Set("Origin", "https://app.example.com")
		h.ServeHTTP(rec, req)
		return rec
	}

	send()        // consume the single burst token
	rec := send() // now limited
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request code = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO on 429 = %q, want echoed origin (CORS must be outermost)", got)
	}
}
