package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
)

func newCORSTestServer(t *testing.T, origins []string) *Server {
	t.Helper()
	return New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		CORS:       &CORSConfig{AllowedOrigins: origins},
	})
}

func TestCORS_AllowedOriginEchoed(t *testing.T) {
	h := newCORSTestServer(t, []string{"https://app.example.com"}).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Origin", "https://app.example.com")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q, want echoed origin", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
}

func TestCORS_DisallowedOriginNoHeader(t *testing.T) {
	h := newCORSTestServer(t, []string{"https://app.example.com"}).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want empty for disallowed origin", got)
	}
}

func TestCORS_Preflight204(t *testing.T) {
	h := newCORSTestServer(t, []string{"*"}).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/v1/runs", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("preflight missing Access-Control-Allow-Methods")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("preflight missing Access-Control-Allow-Headers")
	}
}

func TestCORS_EmptyConfigIsPassThrough(t *testing.T) {
	s := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		// CORS nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Origin", "https://app.example.com")
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want empty with nil CORS", got)
	}
}
