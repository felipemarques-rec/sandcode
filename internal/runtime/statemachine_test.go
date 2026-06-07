package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

func mkEvent(runID string, typ event.Type, ts time.Time, payload []byte) event.Event {
	ev := event.New(typ, runID, payload)
	if !ts.IsZero() {
		ev.Timestamp = ts
	}
	return ev
}

// TestApply_HappyPath drives the no-refine cognitive happy path through
// Apply one event at a time and asserts each transition lands correctly.
func TestApply_HappyPath(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-1")
	if s.Phase != PhaseSubmitted {
		t.Fatalf("initial phase = %s, want submitted", s.Phase)
	}

	steps := []struct {
		evt  event.Type
		want Phase
	}{
		{event.RunClassified, PhaseClassified},
		{event.RunEnriched, PhaseEnriched},
		{event.SandboxCreated, PhaseSandboxReady},
		{event.AgentExecuting, PhaseExecuting},
		{event.AgentCompleted, PhaseAgentCompleted},
		{event.RunCompleted, PhaseCompleted},
	}
	for _, step := range steps {
		if err := s.Apply(mkEvent("run-1", step.evt, time.Time{}, nil)); err != nil {
			t.Fatalf("Apply(%s): %v", step.evt, err)
		}
		if s.Phase != step.want {
			t.Fatalf("after %s phase = %s, want %s", step.evt, s.Phase, step.want)
		}
	}
	if !s.Phase.IsTerminal() {
		t.Fatalf("expected terminal phase, got %s", s.Phase)
	}
}

// TestApply_ObservationOnlyDoesNotTransition verifies that events listed
// in IsObservationOnly bump EventCount but leave Phase untouched.
func TestApply_ObservationOnlyDoesNotTransition(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-1")
	// SandboxDestroyed is observation-only.
	if err := s.Apply(mkEvent("run-1", event.SandboxDestroyed, time.Time{}, nil)); err != nil {
		t.Fatalf("observation-only Apply errored: %v", err)
	}
	if s.Phase != PhaseSubmitted {
		t.Fatalf("phase moved on observation-only event: %s", s.Phase)
	}
	if s.EventCount != 1 {
		t.Fatalf("EventCount = %d, want 1", s.EventCount)
	}
}

// TestApply_RefineIncrementsAttemptAndLoops drives one refine cycle.
func TestApply_RefineIncrementsAttemptAndLoops(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-1")
	s.MaxAttempts = 3
	advance(t, s, event.RunClassified, event.RunEnriched, event.SandboxCreated,
		event.AgentExecuting, event.AgentCompleted, event.VerifyStarted, event.VerifyFailed)

	if s.Phase != PhaseRefining {
		t.Fatalf("expected refining, got %s", s.Phase)
	}
	if s.Attempt != 1 {
		t.Fatalf("Attempt = %d before refine.triggered, want 1", s.Attempt)
	}

	if err := s.Apply(mkEvent("run-1", event.RefineTriggered, time.Time{}, nil)); err != nil {
		t.Fatalf("refine.triggered: %v", err)
	}
	if s.Phase != PhaseExecuting {
		t.Fatalf("phase after refine.triggered = %s, want executing", s.Phase)
	}
	if s.Attempt != 2 {
		t.Fatalf("Attempt after refine.triggered = %d, want 2", s.Attempt)
	}
}

// TestApply_RefineCapShortCircuitsToFailed confirms the safety cap.
func TestApply_RefineCapShortCircuitsToFailed(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-1")
	s.MaxAttempts = 1
	// Drive into PhaseRefining once.
	advance(t, s, event.RunClassified, event.RunEnriched, event.SandboxCreated,
		event.AgentExecuting, event.AgentCompleted, event.VerifyStarted, event.VerifyFailed)

	// Attempt is 1 and MaxAttempts is 1 → refine.triggered must terminate.
	if err := s.Apply(mkEvent("run-1", event.RefineTriggered, time.Time{}, nil)); err != nil {
		t.Fatalf("refine.triggered with cap: %v", err)
	}
	if s.Phase != PhaseFailed {
		t.Fatalf("expected failed on cap exhaustion, got %s", s.Phase)
	}
	if s.LastError == "" {
		t.Fatalf("expected LastError populated on cap exhaustion")
	}
}

// TestApply_TerminalRejectsFurtherEvents verifies that once terminal, the
// state machine refuses additional events to protect replay invariants.
func TestApply_TerminalRejectsFurtherEvents(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-1")
	advance(t, s, event.RunClassified, event.RunEnriched, event.SandboxCreated,
		event.AgentExecuting, event.AgentCompleted, event.RunCompleted)

	err := s.Apply(mkEvent("run-1", event.RunSubmitted, time.Time{}, nil))
	if !errors.Is(err, ErrTerminal) {
		t.Fatalf("expected ErrTerminal post-completion, got %v", err)
	}
}

// TestApply_RejectsMismatchedRunID protects against cross-run event mixing.
func TestApply_RejectsMismatchedRunID(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-A")
	err := s.Apply(mkEvent("run-B", event.RunClassified, time.Time{}, nil))
	if !errors.Is(err, ErrMismatchedRunID) {
		t.Fatalf("expected ErrMismatchedRunID, got %v", err)
	}
}

// TestApply_FailureCapturesErrorMessage extracts error text from the payload.
func TestApply_FailureCapturesErrorMessage(t *testing.T) {
	t.Parallel()
	s := NewExecutionState("run-1")
	advance(t, s, event.RunClassified, event.RunEnriched)

	payload := []byte(`{"reason":"exec","error":"docker daemon unreachable"}`)
	if err := s.Apply(mkEvent("run-1", event.RunFailed, time.Time{}, payload)); err != nil {
		t.Fatalf("Apply failed event: %v", err)
	}
	if s.Phase != PhaseFailed {
		t.Fatalf("expected failed, got %s", s.Phase)
	}
	if s.LastError != "docker daemon unreachable" {
		t.Fatalf("LastError = %q", s.LastError)
	}
}

// TestApply_DurationStampOnTerminal verifies Duration is set when the
// state machine reaches a terminal phase.
func TestApply_DurationStampOnTerminal(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_700_000_000, 0)
	s := NewExecutionState("run-1")
	s.CreatedAt = start

	// First event one second later, terminal event ten seconds later.
	steps := []struct {
		typ event.Type
		dt  time.Duration
	}{
		{event.RunClassified, time.Second},
		{event.RunEnriched, 2 * time.Second},
		{event.SandboxCreated, 3 * time.Second},
		{event.AgentExecuting, 4 * time.Second},
		{event.AgentCompleted, 9 * time.Second},
		{event.RunCompleted, 10 * time.Second},
	}
	for _, step := range steps {
		ts := start.Add(step.dt)
		if err := s.Apply(mkEvent("run-1", step.typ, ts, nil)); err != nil {
			t.Fatalf("Apply(%s): %v", step.typ, err)
		}
	}
	if s.Duration != 10*time.Second {
		t.Fatalf("Duration = %s, want 10s", s.Duration)
	}
}

// advance is a test helper that applies a sequence of events and fails the
// test immediately on any error. Reduces noise in tests that just need to
// reach a particular phase before exercising the interesting transition.
func advance(t *testing.T, s *ExecutionState, types ...event.Type) {
	t.Helper()
	for _, typ := range types {
		if err := s.Apply(mkEvent(s.RunID, typ, time.Time{}, nil)); err != nil {
			t.Fatalf("advance Apply(%s) from %s: %v", typ, s.Phase, err)
		}
	}
}
