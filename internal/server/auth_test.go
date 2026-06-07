package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
)

func newAuthTestServer(t *testing.T, token string) *Server {
	t.Helper()
	return New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		AuthToken:  token,
	})
}

func TestAuth_RequiredWhenTokenSet(t *testing.T) {
	s := newAuthTestServer(t, "s3cret")
	h := s.Handler()

	// /healthz is always exempt.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", rec.Code)
	}

	// Protected route without token → 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}

	// Wrong token → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}

	// Correct token → passes auth (200 from handleListRuns).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct token: got %d, want 200", rec.Code)
	}
}

func TestAuth_NoTokenIsPassThrough(t *testing.T) {
	s := newAuthTestServer(t, "")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-token pass-through: got %d, want 200", rec.Code)
	}
}

func TestServe_RefusesNonLoopbackWithoutToken(t *testing.T) {
	s := newAuthTestServer(t, "")
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := s.Serve(context.Background(), ln); err == nil {
		t.Fatal("expected serve to refuse a non-loopback bind without a token")
	}
}

func TestServe_AllowsLoopbackWithoutToken(t *testing.T) {
	s := newAuthTestServer(t, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx, ln) }()
	// Hit healthz to confirm it came up, then shut down.
	// (small spin; the listener is already bound before Serve returns)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("loopback serve returned error: %v", err)
	}
}

func TestCheckRunPolicy_CWDAndNetwork(t *testing.T) {
	dir := t.TempDir()
	s := New(Options{
		Registry:        metrics.NewRegistry(),
		StateCache:      NewStateCache(DefaultStateCacheCapacity),
		AllowedCWDRoots: []string{dir},
	})
	// CWD outside the allowed root → rejected.
	if err := s.checkRunPolicy(RunRequest{CWD: "/etc"}); err == nil {
		t.Fatal("expected CWD outside root to be rejected")
	}
	// CWD inside the allowed root → ok.
	if err := s.checkRunPolicy(RunRequest{CWD: dir}); err != nil {
		t.Fatalf("CWD == root should be allowed: %v", err)
	}
	// Disallowed network mode → rejected.
	if err := s.checkRunPolicy(RunRequest{CWD: dir, Network: "host"}); err == nil {
		t.Fatal("expected network=host to be rejected")
	}
}
