package runtime

import (
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// TestReplay_GoldenCognitiveRun feeds Replay the canonical event sequence
// that orchestrator.Run + kernel.Process emit today for a successful run.
// The order MUST match what cmd/sandcode/run.go --learn produces in
// production; if a future orchestrator change reorders emissions, this
// test breaks loudly — that's intentional, replay must stay in lockstep.
func TestReplay_GoldenCognitiveRun(t *testing.T) {
	t.Parallel()

	const runID = "run-golden-001"
	start := time.Unix(1_700_000_000, 0)
	at := func(offset time.Duration) time.Time { return start.Add(offset) }

	events := []event.Event{
		mkEvent(runID, event.RunSubmitted, at(0), []byte(`{"agent":"claude-code","sandbox":"docker","strategy":"merge-to-head"}`)),
		mkEvent(runID, event.RunClassified, at(50*time.Millisecond), []byte(`{"type":"convergent","complexity":"low"}`)),
		mkEvent(runID, event.RunEnriched, at(120*time.Millisecond), []byte(`{"original_len":42,"enriched_len":981,"lessons_used":0}`)),
		mkEvent(runID, event.SandboxCreated, at(900*time.Millisecond), []byte(`{"image":"sandcode-default:latest","workdir":"/workspace"}`)),
		mkEvent(runID, event.AgentExecuting, at(time.Second), []byte(`{"agent":"claude-code"}`)),
		mkEvent(runID, event.AgentCompleted, at(8*time.Second), []byte(`{"exit_code":0,"diff_size":312}`)),
		mkEvent(runID, event.SandboxDestroyed, at(8*time.Second+100*time.Millisecond), []byte(`{"duration_ms":7100}`)),
		mkEvent(runID, event.RunCompleted, at(8*time.Second+150*time.Millisecond), []byte(`{"exit_code":0,"duration_ms":8150,"diff_size":312}`)),
	}

	state, err := Replay(runID, events)
	if err != nil {
		t.Fatalf("Replay error: %v", err)
	}

	if state.Phase != PhaseCompleted {
		t.Fatalf("terminal phase = %s, want completed", state.Phase)
	}
	if state.RunID != runID {
		t.Fatalf("RunID = %s, want %s", state.RunID, runID)
	}
	if state.EventCount != len(events) {
		t.Fatalf("EventCount = %d, want %d", state.EventCount, len(events))
	}
	if state.Duration != 8150*time.Millisecond {
		t.Fatalf("Duration = %s, want 8.15s", state.Duration)
	}
	if state.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1 (no refine on this golden)", state.Attempt)
	}
}

// TestReplay_NoCognitionFastPath replays a run that did NOT have a kernel
// configured — no RunClassified, no RunEnriched. Must still reach completed.
func TestReplay_NoCognitionFastPath(t *testing.T) {
	t.Parallel()

	const runID = "run-no-brain"
	events := []event.Event{
		mkEvent(runID, event.RunSubmitted, time.Time{}, nil),
		mkEvent(runID, event.SandboxCreated, time.Time{}, nil),
		mkEvent(runID, event.AgentExecuting, time.Time{}, nil),
		mkEvent(runID, event.AgentCompleted, time.Time{}, nil),
		mkEvent(runID, event.RunCompleted, time.Time{}, nil),
	}

	state, err := Replay(runID, events)
	if err != nil {
		t.Fatalf("Replay error: %v", err)
	}
	if state.Phase != PhaseCompleted {
		t.Fatalf("phase = %s, want completed", state.Phase)
	}
}

// TestReplay_FailureMidExecution simulates a docker crash mid-agent-exec.
func TestReplay_FailureMidExecution(t *testing.T) {
	t.Parallel()

	const runID = "run-crash"
	events := []event.Event{
		mkEvent(runID, event.RunSubmitted, time.Time{}, nil),
		mkEvent(runID, event.RunClassified, time.Time{}, nil),
		mkEvent(runID, event.RunEnriched, time.Time{}, nil),
		mkEvent(runID, event.SandboxCreated, time.Time{}, nil),
		mkEvent(runID, event.AgentExecuting, time.Time{}, nil),
		mkEvent(runID, event.RunFailed, time.Time{}, []byte(`{"reason":"exec","error":"docker daemon died"}`)),
	}

	state, err := Replay(runID, events)
	if err != nil {
		t.Fatalf("Replay error: %v", err)
	}
	if state.Phase != PhaseFailed {
		t.Fatalf("phase = %s, want failed", state.Phase)
	}
	if state.LastError != "docker daemon died" {
		t.Fatalf("LastError = %q, want 'docker daemon died'", state.LastError)
	}
}

// TestReplay_Determinism asserts that replaying the same event stream twice
// yields byte-for-byte identical state (modulo zero-valued timestamps, which
// we anchor manually). This is the core replay invariant.
func TestReplay_Determinism(t *testing.T) {
	t.Parallel()

	const runID = "run-deterministic"
	start := time.Unix(1_700_000_000, 0)
	events := []event.Event{
		mkEvent(runID, event.RunSubmitted, start, nil),
		mkEvent(runID, event.RunClassified, start.Add(10*time.Millisecond), nil),
		mkEvent(runID, event.RunEnriched, start.Add(20*time.Millisecond), nil),
		mkEvent(runID, event.SandboxCreated, start.Add(800*time.Millisecond), nil),
		mkEvent(runID, event.AgentExecuting, start.Add(900*time.Millisecond), nil),
		mkEvent(runID, event.AgentCompleted, start.Add(5*time.Second), nil),
		mkEvent(runID, event.RunCompleted, start.Add(5*time.Second+50*time.Millisecond), nil),
	}

	a, err := Replay(runID, events)
	if err != nil {
		t.Fatalf("first replay: %v", err)
	}
	b, err := Replay(runID, events)
	if err != nil {
		t.Fatalf("second replay: %v", err)
	}
	if a.Phase != b.Phase || a.EventCount != b.EventCount || a.Attempt != b.Attempt {
		t.Fatalf("replays diverged:\n  a=%+v\n  b=%+v", a, b)
	}
	if a.Duration != b.Duration {
		t.Fatalf("duration drift: %s vs %s", a.Duration, b.Duration)
	}
	if a.LastEventID != b.LastEventID || a.LastEventID == "" {
		t.Fatalf("LastEventID divergence or empty: %q vs %q", a.LastEventID, b.LastEventID)
	}
}

// TestReplay_AbortsOnInvalidTransition confirms strict mode: a malformed
// event stream returns the partial state plus the error rather than
// silently skipping bad events.
func TestReplay_AbortsOnInvalidTransition(t *testing.T) {
	t.Parallel()

	const runID = "run-bad"
	events := []event.Event{
		mkEvent(runID, event.RunSubmitted, time.Time{}, nil),
		// Skip classify/enrich/sandbox.created — illegal jump.
		mkEvent(runID, event.AgentCompleted, time.Time{}, nil),
	}

	state, err := Replay(runID, events)
	if err == nil {
		t.Fatalf("expected error replaying invalid stream")
	}
	// Partial state should still be PhaseSubmitted (first event was observation-only).
	if state.Phase != PhaseSubmitted {
		t.Fatalf("partial state phase = %s, want submitted", state.Phase)
	}
}

// TestReplay_EmptyStreamReturnsInitialState verifies a zero-event replay
// produces a clean PhaseSubmitted state — useful for crash recovery
// before any events have been persisted.
func TestReplay_EmptyStreamReturnsInitialState(t *testing.T) {
	t.Parallel()

	state, err := Replay("run-empty", nil)
	if err != nil {
		t.Fatalf("empty Replay error: %v", err)
	}
	if state.Phase != PhaseSubmitted {
		t.Fatalf("empty Replay phase = %s, want submitted", state.Phase)
	}
	if state.EventCount != 0 {
		t.Fatalf("empty Replay EventCount = %d, want 0", state.EventCount)
	}
}

// TestReplay_RefineLoopGolden walks a verify-fail → refine → re-execute →
// verify-pass → completed sequence and asserts Attempt is correctly
// incremented mid-replay.
//
// Pipeline (Stage 1.8 simplification — no lint/report/learn events yet):
//
//	submitted → classified → enriched → sandbox_ready → executing → agent_completed
//	→ verifying → refining → executing (Attempt++) → agent_completed
//	→ verifying → agent_completed (verify.passed)
//	→ completed (run.completed)
func TestReplay_RefineLoopGolden(t *testing.T) {
	t.Parallel()

	const runID = "run-refine"
	events := []event.Event{
		mkEvent(runID, event.RunSubmitted, time.Time{}, nil),
		mkEvent(runID, event.RunClassified, time.Time{}, nil),
		mkEvent(runID, event.RunEnriched, time.Time{}, nil),
		mkEvent(runID, event.SandboxCreated, time.Time{}, nil),
		mkEvent(runID, event.AgentExecuting, time.Time{}, nil),
		mkEvent(runID, event.AgentCompleted, time.Time{}, nil),
		mkEvent(runID, event.VerifyStarted, time.Time{}, nil),
		mkEvent(runID, event.VerifyFailed, time.Time{}, nil),
		mkEvent(runID, event.RefineTriggered, time.Time{}, nil), // Attempt: 1 → 2
		mkEvent(runID, event.AgentCompleted, time.Time{}, nil),
		mkEvent(runID, event.VerifyStarted, time.Time{}, nil),
		mkEvent(runID, event.VerifyPassed, time.Time{}, nil),
		mkEvent(runID, event.RunCompleted, time.Time{}, nil),
	}

	state, err := Replay(runID, events)
	if err != nil {
		t.Fatalf("refine-loop Replay: %v", err)
	}
	if state.Phase != PhaseCompleted {
		t.Fatalf("phase = %s, want completed", state.Phase)
	}
	if state.Attempt != 2 {
		t.Fatalf("Attempt = %d, want 2 (one successful refine)", state.Attempt)
	}
}
