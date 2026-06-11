package main

import (
	"testing"
	"time"
)

func TestBuildRateLimitConfig(t *testing.T) {
	if cfg := buildRateLimitConfig(0, 0); cfg != nil {
		t.Fatalf("rate 0 ⇒ nil, got %+v", cfg)
	}
	cfg := buildRateLimitConfig(5, 0) // burst unset ⇒ default ceil(rate)
	if cfg == nil || cfg.RequestsPerSecond != 5 || cfg.Burst != 5 {
		t.Fatalf("got %+v, want rps=5 burst=5", cfg)
	}
	cfg = buildRateLimitConfig(2.5, 10)
	if cfg == nil || cfg.Burst != 10 {
		t.Fatalf("got %+v, want burst=10", cfg)
	}
}

func TestBuildCORSConfig(t *testing.T) {
	if cfg := buildCORSConfig(nil); cfg != nil {
		t.Fatalf("empty ⇒ nil, got %+v", cfg)
	}
	cfg := buildCORSConfig([]string{"https://a.example.com", "*"})
	if cfg == nil || len(cfg.AllowedOrigins) != 2 {
		t.Fatalf("got %+v, want 2 origins", cfg)
	}
}

func TestApprovalTimeoutDefault(t *testing.T) {
	if got := approvalTimeoutOrDefault(0); got != 5*time.Minute {
		t.Fatalf("default = %v, want 5m", got)
	}
	if got := approvalTimeoutOrDefault(30 * time.Second); got != 30*time.Second {
		t.Fatalf("explicit = %v, want 30s", got)
	}
}
