package runtime

import (
	"errors"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// TestLookupTransition_HappyPath walks the full cognitive happy path and
// asserts each step is permitted by the closed table.
func TestLookupTransition_HappyPath(t *testing.T) {
	t.Parallel()

	steps := []struct {
		from Phase
		evt  event.Type
		want Phase
	}{
		{PhaseSubmitted, event.RunClassified, PhaseClassified},
		{PhaseClassified, event.RunEnriched, PhaseEnriched},
		{PhaseEnriched, event.SandboxCreated, PhaseSandboxReady},
		{PhaseSandboxReady, event.AgentExecuting, PhaseExecuting},
		{PhaseExecuting, event.AgentCompleted, PhaseAgentCompleted},
		{PhaseAgentCompleted, event.RunCompleted, PhaseCompleted},
	}

	for _, s := range steps {
		got, err := LookupTransition(s.from, s.evt)
		if err != nil {
			t.Fatalf("LookupTransition(%s, %s) error: %v", s.from, s.evt, err)
		}
		if got != s.want {
			t.Fatalf("LookupTransition(%s, %s) = %s, want %s", s.from, s.evt, got, s.want)
		}
	}
}

// TestLookupTransition_NoCognitionFastPath asserts runs without a kernel
// (no classify/enrich events) can still progress submitted → sandbox_ready.
func TestLookupTransition_NoCognitionFastPath(t *testing.T) {
	t.Parallel()

	got, err := LookupTransition(PhaseSubmitted, event.SandboxCreated)
	if err != nil {
		t.Fatalf("submitted+sandbox.created not permitted: %v", err)
	}
	if got != PhaseSandboxReady {
		t.Fatalf("got %s want sandbox_ready", got)
	}
}

// TestLookupTransition_RefineLoop verifies verify_failed → refining and
// the re-execute loop transition, plus the verify_passed shortcut back
// to agent_completed.
func TestLookupTransition_RefineLoop(t *testing.T) {
	t.Parallel()

	if next, err := LookupTransition(PhaseVerifying, event.VerifyFailed); err != nil || next != PhaseRefining {
		t.Fatalf("verifying+verify.failed: got=%s err=%v want=refining", next, err)
	}
	if next, err := LookupTransition(PhaseRefining, event.RefineTriggered); err != nil || next != PhaseExecuting {
		t.Fatalf("refining+refine.triggered: got=%s err=%v want=executing", next, err)
	}
	if next, err := LookupTransition(PhaseVerifying, event.VerifyPassed); err != nil || next != PhaseAgentCompleted {
		t.Fatalf("verifying+verify.passed: got=%s err=%v want=agent_completed", next, err)
	}
}

// TestLookupTransition_FailFromAnyPhase asserts run.failed is permitted
// from every non-terminal phase reachable today. PhaseLinting/Reporting/
// Learning are reserved enums (Stage-2 refine ships their drivers) — not
// listed here until they are reachable, so the closed-set property stays
// honest.
func TestLookupTransition_FailFromAnyPhase(t *testing.T) {
	t.Parallel()

	for _, p := range []Phase{
		PhaseSubmitted, PhaseClassified, PhasePlanned, PhaseEnriched,
		PhaseSandboxReady, PhaseExecuting, PhaseAgentCompleted,
		PhaseVerifying, PhaseRefining,
	} {
		next, err := LookupTransition(p, event.RunFailed)
		if err != nil {
			t.Fatalf("run.failed must be valid from %s: %v", p, err)
		}
		if next != PhaseFailed {
			t.Fatalf("from %s + run.failed → %s, want failed", p, next)
		}
	}
}

// TestLookupTransition_RejectsInvalidMoves asserts the table is closed —
// random nonsense pairs return ErrInvalidTransition, not a default phase.
func TestLookupTransition_RejectsInvalidMoves(t *testing.T) {
	t.Parallel()

	bad := []struct {
		from Phase
		evt  event.Type
	}{
		{PhaseSubmitted, event.AgentCompleted},    // cannot complete before executing
		{PhaseExecuting, event.RunClassified},     // cannot reclassify mid-exec
		{PhaseCompleted, event.RunSubmitted},      // terminal phases reject everything (also observation-only ignored anyway)
		{PhaseClassified, event.SandboxCreated},   // must enrich first
		{PhaseSandboxReady, event.AgentCompleted}, // must enter executing first
	}

	for _, b := range bad {
		_, err := LookupTransition(b.from, b.evt)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("expected ErrInvalidTransition for (%s, %s), got: %v", b.from, b.evt, err)
		}
	}
}

// TestIsObservationOnly_StableContract ensures the observation-only set is
// what the state machine expects. If this changes, downstream Apply tests
// must be updated together.
func TestIsObservationOnly_StableContract(t *testing.T) {
	t.Parallel()

	mustBe := []event.Type{
		event.RunSubmitted,
		event.SandboxDestroyed,
		event.AgentToolCalled,
		event.BrainLessonRecalled,
		event.BrainLessonExtracted,
		event.GovernanceApprovalRequired,
		event.GovernanceApproved,
		event.GovernanceDenied,
		event.BudgetThresholdReached,
		event.BudgetExceeded,
		event.RunScheduled,
		event.RunDequeued,
		event.RunStrategySelected,
		event.DAGStarted,
		event.DAGChainStarted,
		event.DAGNodeStarted,
		event.DAGNodeCompleted,
		event.DAGChainCompleted,
		event.DAGSynthesisStarted,
		event.DAGSynthesisCompleted,
		event.DAGCompleted,
		event.ReportGenerated,
		event.ReviewGenerated,
		event.RunArchitected,
		event.SecurityReviewed,
		event.PerformanceReviewed,
		event.RefactoringReviewed,
	}
	for _, e := range mustBe {
		if !IsObservationOnly(e) {
			t.Fatalf("%s must be observation-only", e)
		}
	}

	mustNotBe := []event.Type{
		event.RunClassified, event.RunEnriched, event.SandboxCreated,
		event.AgentExecuting, event.AgentCompleted, event.RunCompleted,
		event.RunFailed, event.RunCancelled,
	}
	for _, e := range mustNotBe {
		if IsObservationOnly(e) {
			t.Fatalf("%s must NOT be observation-only — it must transition", e)
		}
	}
}

// TestPhase_IsTerminal sanity-checks the terminal-phase set.
func TestPhase_IsTerminal(t *testing.T) {
	t.Parallel()
	for _, p := range []Phase{PhaseCompleted, PhaseFailed, PhaseCancelled} {
		if !p.IsTerminal() {
			t.Fatalf("%s should be terminal", p)
		}
	}
	for _, p := range []Phase{
		PhaseSubmitted, PhaseClassified, PhaseEnriched, PhaseExecuting, PhaseRefining,
	} {
		if p.IsTerminal() {
			t.Fatalf("%s should NOT be terminal", p)
		}
	}
}
