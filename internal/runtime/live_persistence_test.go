package runtime_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/orchestrator"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// TestLivePersistence_EndToEnd is the full Stage-2 happy path:
//
//	orchestrator.Run → emits events on LocalBus
//	PersistTo → appends each event to SQLite event_log
//	(simulated crash: close the store) → reopen
//	LoadRun → recovers the persisted event stream
//	runtime.Replay → reconstructs an ExecutionState in PhaseCompleted
//
// If any link in that chain breaks (event missing, ordering wrong,
// schema migration not idempotent, replay drift), this test catches it.
func TestLivePersistence_EndToEnd(t *testing.T) {
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Wire bus → persisted event store.
	storePath := filepath.Join(t.TempDir(), "events.db")
	store, err := event.OpenStore(storePath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := event.PersistTo(bus, store)
	t.Cleanup(sub.Cancel)

	// Drive a real orchestrator.Run with nosandbox + the local fakeAgent.
	script := `echo "hello"; echo "world" > integ_persist.txt; echo "done"`
	events, await, err := orchestrator.Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		orchestrator.RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "live-persist", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            bus,
		},
	)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	for range events { // drain
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("run status=%s err=%v", res.Status, res.Err)
	}

	// Simulate crash: close the store, then reopen at the same path.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := event.OpenStore(storePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	loaded, err := reopened.LoadRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("LoadRun after reopen: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatalf("no events recovered for run %s", res.RunID)
	}

	// Replay the persisted stream through the state machine.
	state, err := runtime.Replay(res.RunID, loaded)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.Phase != runtime.PhaseCompleted {
		t.Fatalf("recovered phase = %s, want completed (events=%d)", state.Phase, len(loaded))
	}

	// The store must contain at least the canonical run lifecycle events.
	requireContains(t, loaded, event.RunSubmitted, event.SandboxCreated,
		event.AgentExecuting, event.AgentCompleted, event.RunCompleted)
}

// requireContains fails the test if any of the wanted event types is
// missing from the loaded slice.
func requireContains(t *testing.T, loaded []event.Event, want ...event.Type) {
	t.Helper()
	seen := make(map[event.Type]bool, len(loaded))
	for _, ev := range loaded {
		seen[ev.Type] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("event log missing %s; saw: %v", w, eventTypes(loaded))
		}
	}
}

func eventTypes(evs []event.Event) []event.Type {
	out := make([]event.Type, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}
