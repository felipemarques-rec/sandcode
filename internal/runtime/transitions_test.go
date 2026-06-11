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

// TestLookupTransition_LintGate verifies the Linter Gate sub-cycle (E1.5b):
// agent_completed → linting on lint.started, linting → agent_completed on
// lint.passed, and linting → refining on lint.failed (sharing the
// refining → executing edge with the verify path).
func TestLookupTransition_LintGate(t *testing.T) {
	t.Parallel()

	if next, err := LookupTransition(PhaseAgentCompleted, event.LintStarted); err != nil || next != PhaseLinting {
		t.Fatalf("agent_completed+lint.started: got=%s err=%v want=linting", next, err)
	}
	if next, err := LookupTransition(PhaseLinting, event.LintPassed); err != nil || next != PhaseAgentCompleted {
		t.Fatalf("linting+lint.passed: got=%s err=%v want=agent_completed", next, err)
	}
	if next, err := LookupTransition(PhaseLinting, event.LintFailed); err != nil || next != PhaseRefining {
		t.Fatalf("linting+lint.failed: got=%s err=%v want=refining", next, err)
	}
	if next, err := LookupTransition(PhaseRefining, event.RefineTriggered); err != nil || next != PhaseExecuting {
		t.Fatalf("refining+refine.triggered: got=%s err=%v want=executing", next, err)
	}
}

// TestLookupTransition_FailFromAnyPhase asserts run.failed is permitted
// from every non-terminal phase reachable today. PhaseReporting/Learning
// remain reserved enums (their drivers are observation-only / unshipped) —
// not listed here until reachable, so the closed-set property stays honest.
func TestLookupTransition_FailFromAnyPhase(t *testing.T) {
	t.Parallel()

	for _, p := range []Phase{
		PhaseSubmitted, PhaseClassified, PhasePlanned, PhaseEnriched,
		PhaseSandboxReady, PhaseExecuting, PhaseAgentCompleted,
		PhaseVerifying, PhaseLinting, PhaseRefining,
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
		// GovernanceApprovalRequired and GovernanceApproved are no longer
		// observation-only — they now drive transitions (E2.3 approval gate).
		// Only GovernanceDenied remains purely observational.
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
		event.ClassifyRequested,
		event.ArchitectRequested,
		event.PlanRequested,
		event.StrategyRequested,
		event.EnrichRequested,
		event.ExecuteRequested,
		event.VerifyRequested,
		event.LintRequested,
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

func TestTransition_ApprovalFlow(t *testing.T) {
	next, err := LookupTransition(PhaseSubmitted, event.GovernanceApprovalRequired)
	if err != nil || next != PhaseAwaitingApproval {
		t.Fatalf("submitted+approval_required → %v,%v; want awaiting_approval", next, err)
	}
	next, err = LookupTransition(PhaseAwaitingApproval, event.GovernanceApproved)
	if err != nil || next != PhaseSubmitted {
		t.Fatalf("awaiting+approved → %v,%v; want submitted", next, err)
	}
	next, err = LookupTransition(PhaseAwaitingApproval, event.RunFailed)
	if err != nil || next != PhaseFailed {
		t.Fatalf("awaiting+failed → %v,%v; want failed", next, err)
	}
	next, err = LookupTransition(PhaseAwaitingApproval, event.RunCancelled)
	if err != nil || next != PhaseCancelled {
		t.Fatalf("awaiting+cancelled → %v,%v; want cancelled", next, err)
	}
}

func TestAwaitingApprovalNotTerminal(t *testing.T) {
	if PhaseAwaitingApproval.IsTerminal() {
		t.Fatal("awaiting_approval must not be terminal")
	}
}

func TestObservationContract_ApprovalEventsDrive(t *testing.T) {
	if IsObservationOnly(event.GovernanceApprovalRequired) {
		t.Fatal("GovernanceApprovalRequired must no longer be observation-only")
	}
	if IsObservationOnly(event.GovernanceApproved) {
		t.Fatal("GovernanceApproved must no longer be observation-only")
	}
	if !IsObservationOnly(event.GovernanceDenied) {
		t.Fatal("GovernanceDenied must remain observation-only")
	}
}
