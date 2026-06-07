package runtime_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
)

// TestCrashRecovery_ReplayReproducesTerminalState is the core durability
// guarantee of the runtime + event-store stack.
//
// Scenario:
//  1. Open a fresh event store at a temp path.
//  2. Write the full canonical event sequence for a successful run.
//  3. Close the store (simulates a process crash mid-bookkeeping).
//  4. Re-open the store at the SAME path.
//  5. LoadRun yields the persisted events.
//  6. Replay reconstructs an ExecutionState in PhaseCompleted.
//
// If any step breaks — schema migration not idempotent, events missing,
// ordering scrambled, replay drifting — this test fires loudly.
func TestCrashRecovery_ReplayReproducesTerminalState(t *testing.T) {
	t.Parallel()

	const runID = "run-crash-A"
	path := filepath.Join(t.TempDir(), "events.db")
	ctx := context.Background()

	// --- Phase 1: write events to a first store, then close (simulated crash). ---
	s1, err := event.OpenStore(path)
	if err != nil {
		t.Fatalf("first OpenStore: %v", err)
	}
	start := time.Unix(1_700_000_000, 0)
	events := []event.Event{
		mkPersisted(runID, event.RunSubmitted, start, []byte(`{"agent":"claude-code"}`)),
		mkPersisted(runID, event.RunClassified, start.Add(50*time.Millisecond), []byte(`{"type":"convergent"}`)),
		mkPersisted(runID, event.RunEnriched, start.Add(120*time.Millisecond), nil),
		mkPersisted(runID, event.SandboxCreated, start.Add(700*time.Millisecond), nil),
		mkPersisted(runID, event.AgentExecuting, start.Add(time.Second), nil),
		mkPersisted(runID, event.AgentCompleted, start.Add(5*time.Second), nil),
		mkPersisted(runID, event.SandboxDestroyed, start.Add(5*time.Second+50*time.Millisecond), nil),
		mkPersisted(runID, event.RunCompleted, start.Add(5*time.Second+100*time.Millisecond), nil),
	}
	for _, ev := range events {
		if err := s1.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%s): %v", ev.Type, err)
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}

	// --- Phase 2: re-open at the same path (simulates restart). ---
	s2, err := event.OpenStore(path)
	if err != nil {
		t.Fatalf("second OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	loaded, err := s2.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after reopen: %v", err)
	}
	if len(loaded) != len(events) {
		t.Fatalf("recovered %d events, want %d", len(loaded), len(events))
	}

	// --- Phase 3: replay through the state machine. ---
	state, err := runtime.Replay(runID, loaded)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.Phase != runtime.PhaseCompleted {
		t.Fatalf("recovered phase = %s, want completed", state.Phase)
	}
	if state.EventCount != len(events) {
		t.Fatalf("EventCount = %d, want %d", state.EventCount, len(events))
	}
	if state.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1 (no refine in golden)", state.Attempt)
	}
}

// TestCrashRecovery_PartialWritesReplayToLatestPhase mirrors a more
// realistic crash: the process died mid-execution, only the first few
// events made it to disk. The replay must reconstruct the partial state
// without erroring or jumping to a terminal phase.
func TestCrashRecovery_PartialWritesReplayToLatestPhase(t *testing.T) {
	t.Parallel()

	const runID = "run-partial"
	path := filepath.Join(t.TempDir(), "events.db")
	ctx := context.Background()

	s1, err := event.OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	start := time.Unix(1_700_000_000, 0)
	partial := []event.Event{
		mkPersisted(runID, event.RunSubmitted, start, nil),
		mkPersisted(runID, event.RunClassified, start.Add(50*time.Millisecond), nil),
		mkPersisted(runID, event.RunEnriched, start.Add(120*time.Millisecond), nil),
		mkPersisted(runID, event.SandboxCreated, start.Add(700*time.Millisecond), nil),
		mkPersisted(runID, event.AgentExecuting, start.Add(time.Second), nil),
		// crash — no AgentCompleted, no RunCompleted
	}
	for _, ev := range partial {
		if err := s1.Append(ctx, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	_ = s1.Close()

	s2, err := event.OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	loaded, err := s2.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	state, err := runtime.Replay(runID, loaded)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.Phase != runtime.PhaseExecuting {
		t.Fatalf("partial replay phase = %s, want executing", state.Phase)
	}
	if state.Phase.IsTerminal() {
		t.Fatalf("partial replay must NOT land in a terminal phase")
	}
}

// TestCrashRecovery_FailurePathReplaysToFailed simulates an orchestrator
// that emitted run.failed and crashed before any cleanup events.
// The state machine must still recover to PhaseFailed with the original
// error message intact.
func TestCrashRecovery_FailurePathReplaysToFailed(t *testing.T) {
	t.Parallel()

	const runID = "run-failpath"
	path := filepath.Join(t.TempDir(), "events.db")
	ctx := context.Background()

	s1, err := event.OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	start := time.Unix(1_700_000_000, 0)
	persisted := []event.Event{
		mkPersisted(runID, event.RunSubmitted, start, nil),
		mkPersisted(runID, event.RunClassified, start.Add(50*time.Millisecond), nil),
		mkPersisted(runID, event.RunEnriched, start.Add(120*time.Millisecond), nil),
		mkPersisted(runID, event.RunFailed, start.Add(500*time.Millisecond),
			[]byte(`{"reason":"sandbox","error":"image pull failed"}`)),
	}
	for _, ev := range persisted {
		if err := s1.Append(ctx, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	_ = s1.Close()

	s2, _ := event.OpenStore(path)
	t.Cleanup(func() { _ = s2.Close() })

	loaded, _ := s2.LoadRun(ctx, runID)
	state, err := runtime.Replay(runID, loaded)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if state.Phase != runtime.PhaseFailed {
		t.Fatalf("phase = %s, want failed", state.Phase)
	}
	if state.LastError != "image pull failed" {
		t.Fatalf("LastError = %q, want 'image pull failed'", state.LastError)
	}
}

// TestCrashRecovery_DoubleReplayIsIdempotent asserts that loading +
// replaying the same store twice yields equivalent ExecutionState.
// Guards against any time-of-day non-determinism in Apply or scan.
func TestCrashRecovery_DoubleReplayIsIdempotent(t *testing.T) {
	t.Parallel()

	const runID = "run-double"
	path := filepath.Join(t.TempDir(), "events.db")
	ctx := context.Background()

	s, err := event.OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	start := time.Unix(1_700_000_000, 0)
	persisted := []event.Event{
		mkPersisted(runID, event.RunSubmitted, start, nil),
		mkPersisted(runID, event.RunClassified, start.Add(time.Millisecond), nil),
		mkPersisted(runID, event.RunEnriched, start.Add(2*time.Millisecond), nil),
		mkPersisted(runID, event.SandboxCreated, start.Add(3*time.Millisecond), nil),
		mkPersisted(runID, event.AgentExecuting, start.Add(4*time.Millisecond), nil),
		mkPersisted(runID, event.AgentCompleted, start.Add(5*time.Millisecond), nil),
		mkPersisted(runID, event.RunCompleted, start.Add(6*time.Millisecond), nil),
	}
	for _, ev := range persisted {
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	loaded1, _ := s.LoadRun(ctx, runID)
	loaded2, _ := s.LoadRun(ctx, runID)
	a, _ := runtime.Replay(runID, loaded1)
	b, _ := runtime.Replay(runID, loaded2)

	if a.Phase != b.Phase ||
		a.EventCount != b.EventCount ||
		a.Attempt != b.Attempt ||
		a.Duration != b.Duration ||
		a.LastEventID != b.LastEventID {
		t.Fatalf("non-idempotent replay:\n  a=%+v\n  b=%+v", a, b)
	}
}

// mkPersisted is a test helper that produces an Event suitable for the
// event store: it carries a fresh UUID (per event.New), a deterministic
// timestamp, and an optional payload.
func mkPersisted(runID string, typ event.Type, ts time.Time, payload []byte) event.Event {
	ev := event.New(typ, runID, payload)
	ev.Timestamp = ts
	ev.CorrelationID = runID
	return ev
}
