package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
)

// recordingLauncher captures the most recent Launch invocation and
// optionally publishes a canned event sequence onto a bus so handlers
// downstream of the launch (state cache, SSE) see realistic traffic.
type recordingLauncher struct {
	mu       sync.Mutex
	gotRunID string
	gotReq   RunRequest
	calls    int

	bus    event.Bus // if non-nil, publish a synthetic lifecycle when called
	events []event.Type
	done   chan struct{}
}

func (l *recordingLauncher) Launch(ctx context.Context, runID string, req RunRequest) error {
	l.mu.Lock()
	l.gotRunID = runID
	l.gotReq = req
	l.calls++
	l.mu.Unlock()

	if l.bus != nil {
		for _, t := range l.events {
			_ = l.bus.Publish(ctx, event.New(t, runID, nil))
		}
	}
	if l.done != nil {
		close(l.done)
	}
	return nil
}

func (l *recordingLauncher) snapshot() (string, RunRequest, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gotRunID, l.gotReq, l.calls
}

func newRunsTestServer(t *testing.T, launcher Launcher, bus event.Bus) *Server {
	t.Helper()
	reg := metrics.NewRegistry()
	cache := NewStateCache(0)
	if bus != nil {
		// Pre-attach the cache so GET /v1/runs/{id} can find state.
		t.Cleanup(cache.Attach(bus).Cancel)
	}
	return New(Options{
		Registry:   reg,
		StateCache: cache,
		Bus:        bus,
		Launcher:   launcher,
	})
}

func TestCreateRun_HappyPath(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	launcher := &recordingLauncher{
		bus:    bus,
		events: []event.Type{event.RunSubmitted},
		done:   make(chan struct{}),
	}
	srv := newRunsTestServer(t, launcher, bus)

	body, _ := json.Marshal(RunRequest{
		Prompt:       "write hello.txt",
		CWD:          "/tmp/repo",
		SandboxImage: "alpine",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rr.Code, rr.Body.String())
	}

	var resp CreateRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.RunID == "" {
		t.Error("response missing run_id")
	}
	if got := rr.Header().Get("Location"); got != "/v1/runs/"+resp.RunID {
		t.Errorf("Location = %q, want %q", got, "/v1/runs/"+resp.RunID)
	}

	// Wait for launcher goroutine.
	select {
	case <-launcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("launcher was not invoked within 2s")
	}
	gotID, gotReq, calls := launcher.snapshot()
	if calls != 1 {
		t.Errorf("launcher calls = %d, want 1", calls)
	}
	if gotID != resp.RunID {
		t.Errorf("launcher got runID = %q, want %q", gotID, resp.RunID)
	}
	if gotReq.Prompt != "write hello.txt" {
		t.Errorf("launcher got prompt = %q", gotReq.Prompt)
	}
}

func TestCreateRun_ValidationErrors(t *testing.T) {
	srv := newRunsTestServer(t, LauncherFunc(func(context.Context, string, RunRequest) error {
		return nil
	}), nil)

	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty prompt", `{"cwd":"/r","sandbox_image":"a"}`, "prompt: required"},
		{"empty cwd", `{"prompt":"p","sandbox_image":"a"}`, "cwd: required"},
		{"empty image", `{"prompt":"p","cwd":"/r"}`, "sandbox_image: required"},
		{"negative timeout", `{"prompt":"p","cwd":"/r","sandbox_image":"a","timeout_seconds":-1}`, "timeout_seconds: must be >= 0"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/runs",
				strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("body = %q, want substring %q", rr.Body.String(), tc.want)
			}
		})
	}
}

func TestCreateRun_RejectsUnknownFields(t *testing.T) {
	srv := newRunsTestServer(t, LauncherFunc(func(context.Context, string, RunRequest) error {
		return nil
	}), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"prompt":"p","cwd":"/r","sandbox_image":"a","extra_field":true}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateRun_NoLauncher503(t *testing.T) {
	srv := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(0),
		// no Launcher
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"prompt":"p","cwd":"/r","sandbox_image":"a"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestGetRun_ReturnsCachedState(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	srv := newRunsTestServer(t, nil, bus)

	// Drive the cache by publishing.
	if err := bus.Publish(context.Background(), event.New(event.RunSubmitted, "abc12345", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/abc12345", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var st runtime.ExecutionState
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.RunID != "abc12345" {
		t.Errorf("RunID = %q", st.RunID)
	}
	if st.Phase != runtime.PhaseSubmitted {
		t.Errorf("phase = %s, want %s", st.Phase, runtime.PhaseSubmitted)
	}
}

func TestGetRun_404(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/missing01", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunEventsSSE_StreamsAndClosesOnTerminal(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	srv := newRunsTestServer(t, nil, bus)

	// Disable keepalive jitter to keep test output deterministic.
	srv.opts.SSEKeepalive = -1

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/runs/sse00001/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	// Give the handler a moment to set up the bus subscription.
	time.Sleep(50 * time.Millisecond)

	for _, typ := range []event.Type{event.RunSubmitted, event.SandboxCreated, event.RunCompleted} {
		if err := bus.Publish(context.Background(), event.New(typ, "sse00001", nil)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"event: run.submitted",
		"event: sandbox.created",
		"event: run.completed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}
}

func TestRunEventsSSE_FiltersByRunID(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	srv := newRunsTestServer(t, nil, bus)
	srv.opts.SSEKeepalive = -1

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/runs/wantme00/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	time.Sleep(50 * time.Millisecond)
	// Publish events for two runs interleaved.
	for i := 0; i < 3; i++ {
		_ = bus.Publish(context.Background(), event.New(event.SandboxCreated, "ignored0", nil))
	}
	_ = bus.Publish(context.Background(), event.New(event.RunCompleted, "wantme00", nil))

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)
	if strings.Contains(got, "ignored0") {
		t.Errorf("body should not include other run's events:\n%s", got)
	}
	if !strings.Contains(got, "wantme00") {
		t.Errorf("body missing target run's event:\n%s", got)
	}
}

func TestCreateRunThenGet_PopulatesStateCache(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	launcher := &recordingLauncher{
		bus:    bus,
		events: []event.Type{event.RunSubmitted, event.SandboxCreated},
		done:   make(chan struct{}),
	}
	srv := newRunsTestServer(t, launcher, bus)

	// POST a run.
	body, _ := json.Marshal(RunRequest{
		Prompt:       "p",
		CWD:          "/r",
		SandboxImage: "img",
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202: %s", rr.Code, rr.Body.String())
	}
	var resp CreateRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Wait for the launcher (which publishes onto the bus → state cache).
	select {
	case <-launcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("launcher never invoked")
	}

	// GET the same run — state cache must have observed the published events.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/runs/"+resp.RunID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var st runtime.ExecutionState
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.RunID != resp.RunID {
		t.Errorf("state.RunID = %q, want %q", st.RunID, resp.RunID)
	}
	// SandboxCreated transitions PhaseSubmitted → PhaseSandboxReady.
	if st.Phase != runtime.PhaseSandboxReady {
		t.Errorf("phase = %s, want %s", st.Phase, runtime.PhaseSandboxReady)
	}
}

// slowLauncher signals when it has started, then blocks on its ctx.
// It exists to verify drain-on-shutdown semantics.
type slowLauncher struct {
	started chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (l *slowLauncher) Launch(ctx context.Context, runID string, req RunRequest) error {
	l.once.Do(func() { close(l.started) })
	<-ctx.Done()
	close(l.exited)
	return ctx.Err()
}

func TestServeDrainsInFlightLaunches(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	launcher := &slowLauncher{
		started: make(chan struct{}),
		exited:  make(chan struct{}),
	}
	srv := New(Options{
		Registry:           metrics.NewRegistry(),
		StateCache:         NewStateCache(0),
		Bus:                bus,
		Launcher:           launcher,
		LaunchDrainTimeout: 2 * time.Second,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serveCtx, cancelServe := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx, ln) }()

	// Wait for the listener to be ready, then POST a run.
	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		body, _ := json.Marshal(RunRequest{
			Prompt:       "p",
			CWD:          "/r",
			SandboxImage: "img",
		})
		r, err := http.Post("http://"+addr+"/v1/runs", "application/json",
			bytes.NewReader(body))
		if err == nil {
			resp = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("could not POST /v1/runs within 2s")
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", resp.StatusCode)
	}

	// Confirm the launcher is mid-flight.
	select {
	case <-launcher.started:
	case <-time.After(2 * time.Second):
		t.Fatal("launcher never started")
	}

	// Now cancel the server context. Drain must wait for the launcher
	// (which honors ctx) and then Serve must return cleanly.
	cancelServe()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return after cancel within 4s")
	}
	select {
	case <-launcher.exited:
	case <-time.After(time.Second):
		t.Fatal("launcher did not exit")
	}
}

func TestRunEventsSSE_503WhenBusUnconfigured(t *testing.T) {
	srv := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(0),
		// no Bus
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/x/events", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}
