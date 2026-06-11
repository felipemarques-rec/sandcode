package server

import (
	"net"
	"net/http"
	"strconv"
	"time"
)

// RateLimitConfig configures per-client-IP request rate limiting.
type RateLimitConfig struct {
	RequestsPerSecond float64
	Burst             int
	TTL               time.Duration // idle-bucket eviction; defaults to 10m when zero
}

// withRateLimit wraps h with per-IP token-bucket rate limiting. Returns h unchanged
// when the limiter is nil (byte-identical). GET /healthz and GET /metrics are exempt.
func (s *Server) withRateLimit(h http.Handler) http.Handler {
	if s.limiter == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			h.ServeHTTP(w, r)
			return
		}
		ok, retryAfter := s.limiter.Allow(clientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// clientIP returns the host portion of r.RemoteAddr, or the raw value when unparseable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
