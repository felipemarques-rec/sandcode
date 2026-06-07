package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	reg := metrics.NewRegistry()
	c := reg.NewCounter("sandcode_test_counter", "Test counter.", nil)
	c.Inc()
	return New(Options{
		Registry:   reg,
		StateCache: NewStateCache(0),
	})
}

func TestHealthzReturnsOK(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "ok\n" {
		t.Errorf("body = %q, want %q", got, "ok\n")
	}
	if ct := rr.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain prefix", ct)
	}
}

func TestMetricsRendersRegistry(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "# TYPE sandcode_test_counter counter") {
		t.Errorf("missing TYPE line:\n%s", body)
	}
	if !strings.Contains(body, "sandcode_test_counter 1") {
		t.Errorf("missing sample line:\n%s", body)
	}
}

func TestHealthzRejectsPost(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	srv.Handler().ServeHTTP(rr, req)

	// Go 1.22+ method-pattern mux returns 405 for method-mismatched routes.
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	srv := newTestServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// Wait for the server to be reachable.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}
}

func TestNewPanicsOnMissingRegistry(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Registry is nil")
		}
	}()
	_ = New(Options{StateCache: NewStateCache(0)})
}

func TestNewPanicsOnMissingStateCache(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when StateCache is nil")
		}
	}()
	_ = New(Options{Registry: metrics.NewRegistry()})
}
