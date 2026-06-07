package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/scheduler"
)

// promptLauncher signals a buffered started chan on every Launch then
// returns promptly — we want a clean scheduler drain on shutdown, not
// a forced LaunchDrainTimeout cut.
type promptLauncher struct {
	started chan struct{}
}

func (l *promptLauncher) Launch(_ context.Context, _ string, _ RunRequest) error {
	l.started <- struct{}{}
	return nil
}

// TestServe_SchedulerDrainsAndReServes drives the real Serve() path
// with SchedulerConfig set: a POST is admitted+dequeued+launched, ctx
// cancel drains the scheduler through awaitDrain→stopScheduler without
// hanging, s.sched is cleared, and a second Serve() rebuilds a fresh
// running scheduler (the re-serve defect #1 guards). Deterministic:
// readiness is the sibling test's POST-retry pattern, sync is channels.
func TestServe_SchedulerDrainsAndReServes(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()
	launcher := &promptLauncher{started: make(chan struct{}, 8)}
	srv := New(Options{
		Registry:           metrics.NewRegistry(),
		StateCache:         NewStateCache(0),
		Bus:                bus,
		Launcher:           launcher,
		SchedulerConfig:    &scheduler.Config{PoolSize: 1, QueueCap: 4},
		LaunchDrainTimeout: 2 * time.Second,
	})

	// postRun POSTs /v1/runs to addr, retrying until the listener is
	// ready (mirrors TestServeDrainsInFlightLaunches' readiness loop),
	// and returns the status code.
	postRunHTTP := func(addr string) int {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			body, _ := json.Marshal(RunRequest{
				Prompt:       "p",
				CWD:          "/r",
				SandboxImage: "img",
			})
			r, err := http.Post("http://"+addr+"/v1/runs", "application/json",
				bytes.NewReader(body))
			if err == nil {
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				return r.StatusCode
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("could not POST /v1/runs within 2s")
		return 0
	}

	// --- First serve: admit, drain, prove s.sched cleared. ---
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	if code := postRunHTTP(addr); code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", code)
	}
	select {
	case <-launcher.started:
	case <-time.After(2 * time.Second):
		t.Fatal("launcher never started (run not admitted/dequeued)")
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("first Serve returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Serve did not return within 5s after cancel (drain hung)")
	}
	if srv.sched != nil {
		t.Fatal("srv.sched != nil after shutdown: stopScheduler did not clear it")
	}

	// --- Re-serve: a fresh Serve() must rebuild a running scheduler. ---
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 2: %v", err)
	}
	addr2 := ln2.Addr().String()

	ctx2, cancel2 := context.WithCancel(context.Background())
	served2 := make(chan error, 1)
	go func() { served2 <- srv.Serve(ctx2, ln2) }()

	if code := postRunHTTP(addr2); code != http.StatusAccepted {
		t.Fatalf("re-serve POST status = %d, want 202 (startScheduler did not rebuild)", code)
	}
	select {
	case <-launcher.started:
	case <-time.After(2 * time.Second):
		t.Fatal("re-serve launcher never started (rebuilt scheduler not dispatching)")
	}

	cancel2()
	select {
	case err := <-served2:
		if err != nil {
			t.Errorf("re-serve Serve returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("re-serve Serve did not return within 5s after cancel")
	}
}
