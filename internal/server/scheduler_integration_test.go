package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/scheduler"
)

func newTestServerWithScheduler(t *testing.T, cfg scheduler.Config, launch func(runID string) error) *Server {
	t.Helper()
	srv := New(Options{
		Registry:        metrics.NewRegistry(),
		StateCache:      NewStateCache(0),
		SchedulerConfig: &cfg,
		Launcher: LauncherFunc(func(ctx context.Context, runID string, req RunRequest) error {
			return launch(runID)
		}),
		LaunchDrainTimeout: 2 * time.Second,
	})
	return srv
}

func postRun(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const validBody = `{"prompt":"hi","cwd":"/tmp","sandbox_image":"img"}`

func TestServer_SchedulerNil_UnboundedPathUnchanged(t *testing.T) {
	var ran sync.WaitGroup
	ran.Add(1)
	srv := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(0),
		Launcher: LauncherFunc(func(ctx context.Context, runID string, req RunRequest) error {
			ran.Done()
			return nil
		}),
	})
	w := postRun(t, srv.Handler(), validBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202 (unbounded path must be unchanged)", w.Code)
	}
	ran.Wait()
}

func TestServer_Scheduler_202ThenQueueFull429(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1) // signals the worker dequeued POST#1
	srv := newTestServerWithScheduler(t, scheduler.Config{PoolSize: 1, QueueCap: 1},
		func(runID string) error { started <- struct{}{}; <-release; return nil })
	srv.startScheduler() // test hook: builds + starts the scheduler outside serve()
	defer func() { close(release); _ = srv.stopScheduler(context.Background()) }()
	h := srv.Handler()

	if w := postRun(t, h, validBody); w.Code != http.StatusAccepted {
		t.Fatalf("first POST status=%d want 202", w.Code)
	}
	// Deterministic barrier: block until the single worker has actually
	// dequeued POST#1 (queue.len back to 0) before issuing POST#2.
	// Without this, POST#2 races the worker pop: if the worker hasn't
	// popped yet, queue.len()==1 >= QueueCap==1 and POST#2 wrongly 429s.
	<-started

	if w := postRun(t, h, validBody); w.Code != http.StatusAccepted {
		t.Fatalf("second POST (fills queue) status=%d want 202", w.Code)
	}
	w := postRun(t, h, validBody)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third POST status=%d want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

func TestServer_Scheduler_BadPriority400(t *testing.T) {
	srv := newTestServerWithScheduler(t, scheduler.Config{PoolSize: 1, QueueCap: 4},
		func(runID string) error { return nil })
	srv.startScheduler()
	defer func() { _ = srv.stopScheduler(context.Background()) }()
	body := `{"prompt":"hi","cwd":"/tmp","sandbox_image":"img","priority":"urgent"}`
	if w := postRun(t, srv.Handler(), body); w.Code != http.StatusBadRequest {
		t.Fatalf("bad priority status=%d want 400", w.Code)
	}
}

func TestServer_Scheduler_DeleteCancels(t *testing.T) {
	release := make(chan struct{})
	srv := newTestServerWithScheduler(t, scheduler.Config{PoolSize: 1, QueueCap: 4},
		func(runID string) error { <-release; return nil })
	srv.startScheduler()
	defer func() { close(release); _ = srv.stopScheduler(context.Background()) }()
	h := srv.Handler()

	postRun(t, h, validBody)       // occupies the pool slot
	w2 := postRun(t, h, validBody) // queued
	var cr CreateRunResponse
	json.Unmarshal(w2.Body.Bytes(), &cr)

	req := httptest.NewRequest(http.MethodDelete, "/v1/runs/"+cr.RunID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE queued run status=%d want 204", rec.Code)
	}
	// Deleting an unknown/running id => 409.
	req2 := httptest.NewRequest(http.MethodDelete, "/v1/runs/does-not-exist", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("DELETE unknown status=%d want 409", rec2.Code)
	}
}

// TestServer_Scheduler_PendingClearedOnStop is the spec-mandated
// "pending map cleared on stop" regression: queued runs that Stop
// cancels must not leak their staged RunRequest in s.pending. Linear
// (no defer): the only in-process wait is stopScheduler AFTER
// close(release), so an early Fatalf cannot hang (it just leaks one
// blocked worker goroutine, process-bounded — standard in Go tests).
func TestServer_Scheduler_PendingClearedOnStop(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv := newTestServerWithScheduler(t, scheduler.Config{PoolSize: 1, QueueCap: 8},
		func(runID string) error { started <- struct{}{}; <-release; return nil })
	srv.startScheduler()
	h := srv.Handler()

	if w := postRun(t, h, validBody); w.Code != http.StatusAccepted {
		t.Fatalf("POST#1 status=%d want 202", w.Code)
	}
	<-started // run#1 dispatched; launchFunc already takePending'd it

	// Worker is busy with run#1 (PoolSize 1) → these two get queued.
	if w := postRun(t, h, validBody); w.Code != http.StatusAccepted {
		t.Fatalf("POST#2 status=%d want 202", w.Code)
	}
	if w := postRun(t, h, validBody); w.Code != http.StatusAccepted {
		t.Fatalf("POST#3 status=%d want 202", w.Code)
	}
	srv.pendingMu.Lock()
	staged := len(srv.pending)
	srv.pendingMu.Unlock()
	if staged != 2 {
		t.Fatalf("pending=%d want 2 (the two queued runs are staged)", staged)
	}

	// Drain the in-flight run, then stop: Stop cancels the 2 queued
	// runs; stopScheduler must purge their pending entries.
	close(release)
	if err := srv.stopScheduler(context.Background()); err != nil {
		t.Fatalf("stopScheduler: %v", err)
	}
	srv.pendingMu.Lock()
	leaked := len(srv.pending)
	srv.pendingMu.Unlock()
	if leaked != 0 {
		t.Fatalf("pending map leaked %d entries after Stop; want 0", leaked)
	}
}

func TestRunRequest_PriorityFieldRoundTrips(t *testing.T) {
	body := `{"prompt":"p","cwd":"/tmp","sandbox_image":"i","priority":"high"}`
	var rr RunRequest
	dec := json.NewDecoder(bytes.NewBufferString(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rr); err != nil {
		t.Fatalf("decode with priority: %v", err)
	}
	if rr.Priority != "high" {
		t.Fatalf("Priority=%q want high", rr.Priority)
	}
	// Omitted priority must still decode (back-compat) and Validate.
	var rr2 RunRequest
	if err := json.Unmarshal([]byte(`{"prompt":"p","cwd":"/tmp","sandbox_image":"i"}`), &rr2); err != nil {
		t.Fatalf("decode without priority: %v", err)
	}
	if rr2.Priority != "" {
		t.Fatalf("missing priority should be empty, got %q", rr2.Priority)
	}
	if err := rr2.Validate(); err != nil {
		t.Fatalf("Validate without priority: %v", err)
	}
}

func TestServer_Scheduler_StoppedReturns503(t *testing.T) {
	srv := newTestServerWithScheduler(t, scheduler.Config{PoolSize: 1, QueueCap: 4},
		func(runID string) error { return nil })
	srv.startScheduler()
	// Stop the scheduler, THEN submit: Submit → ErrStopped → 503.
	// (stopScheduler nils s.sched, so re-route would hit the legacy
	// path — re-point sched at a stopped instance to exercise the
	// ErrStopped HTTP mapping deterministically.)
	stopped := scheduler.New(scheduler.Config{PoolSize: 1, QueueCap: 4},
		func(_ context.Context, _ string) error { return nil }, nil)
	stopped.Start()
	_ = stopped.Stop(context.Background())
	srv.sched = stopped
	srv.pending = make(map[string]RunRequest)

	w := postRun(t, srv.Handler(), validBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST while scheduler stopped: status=%d want 503", w.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &er)
	if er.Error != "server draining" {
		t.Fatalf("503 body error=%q want \"server draining\"", er.Error)
	}
}
