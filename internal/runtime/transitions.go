package runtime

import (
	"errors"
	"fmt"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// ErrInvalidTransition is returned when Apply is asked to perform a
// (from-phase, event-type) move that is not in the transition table.
// This is the safety net that makes replay deterministic: a corrupted
// or out-of-order event stream cannot silently desync the state machine.
var ErrInvalidTransition = errors.New("runtime: invalid transition")

// transition is a (from-phase, event-type) tuple keyed in the table.
type transition struct {
	From  Phase
	Event event.Type
}

// transitionTable is the closed set of legal phase moves.
//
// Reading guide:
//
//	{ from, event } → next phase
//
// The same event can legally appear from multiple from-phases (e.g.
// run.failed can fire from almost anywhere — runs can crash at any point).
// What MUST be unique is the (from, event) pair → at most one destination.
var transitionTable = map[transition]Phase{
	// Happy path: submitted → classified → (planned?) → enriched → sandbox_ready → executing → agent_completed → … → completed
	{From: PhaseSubmitted, Event: event.RunClassified}: PhaseClassified,

	{From: PhaseClassified, Event: event.RunPlanned}:  PhasePlanned,
	{From: PhaseClassified, Event: event.RunEnriched}: PhaseEnriched, // skip-planning fast path
	{From: PhasePlanned, Event: event.RunEnriched}:    PhaseEnriched,

	{From: PhaseEnriched, Event: event.SandboxCreated}:     PhaseSandboxReady,
	{From: PhaseSandboxReady, Event: event.AgentExecuting}: PhaseExecuting,

	// Non-cognitive shortcut: when no kernel is wired, runs go straight from
	// submitted to sandbox_ready (no classify/enrich events fire).
	{From: PhaseSubmitted, Event: event.SandboxCreated}: PhaseSandboxReady,

	{From: PhaseExecuting, Event: event.AgentCompleted}: PhaseAgentCompleted,

	// Stage-2 verify/refine sub-cycle. PhaseAgentCompleted is the canonical
	// "ready for finalization" state. Verify is a parenthetical detour that
	// either blesses the run (verify.passed → back to agent_completed) or
	// triggers a refine attempt (verify.failed → refining → executing).
	//
	// PhaseReporting / PhaseLearning remain reserved for when their driving
	// events (report.generated is observation-only today; learn.completed)
	// ship — intentionally absent to keep the closed-set property honest.
	{From: PhaseAgentCompleted, Event: event.VerifyStarted}: PhaseVerifying,
	{From: PhaseVerifying, Event: event.VerifyPassed}:       PhaseAgentCompleted,
	{From: PhaseVerifying, Event: event.VerifyFailed}:       PhaseRefining,
	{From: PhaseRefining, Event: event.RefineTriggered}:     PhaseExecuting, // Apply bumps Attempt

	// Linter Gate sub-cycle (E1.5b). The lint gate runs after a passing
	// verify (from agent_completed). A passing lint returns to agent_completed
	// for finalization; a failing lint triggers a refine attempt, sharing the
	// refining → executing edge with the verify path.
	{From: PhaseAgentCompleted, Event: event.LintStarted}: PhaseLinting,
	{From: PhaseLinting, Event: event.LintPassed}:         PhaseAgentCompleted,
	{From: PhaseLinting, Event: event.LintFailed}:         PhaseRefining,

	// Terminal finalization: run.completed from agent_completed closes the run.
	{From: PhaseAgentCompleted, Event: event.RunCompleted}: PhaseCompleted,

	// Approval gate (E2.3): a Review verdict at the pre-run gate parks the run
	// in awaiting_approval. Approval resumes the normal flow (back to
	// submitted); reject/timeout surface as run.failed; cancel as run.cancelled.
	{From: PhaseSubmitted, Event: event.GovernanceApprovalRequired}: PhaseAwaitingApproval,
	{From: PhaseAwaitingApproval, Event: event.GovernanceApproved}:  PhaseSubmitted,
	{From: PhaseAwaitingApproval, Event: event.RunFailed}:           PhaseFailed,
	{From: PhaseAwaitingApproval, Event: event.RunCancelled}:        PhaseCancelled,

	// Cancellation is valid from any non-terminal phase. Listed explicitly
	// rather than wildcarded so the table stays exhaustive and greppable.
	{From: PhaseSubmitted, Event: event.RunCancelled}:      PhaseCancelled,
	{From: PhaseClassified, Event: event.RunCancelled}:     PhaseCancelled,
	{From: PhasePlanned, Event: event.RunCancelled}:        PhaseCancelled,
	{From: PhaseEnriched, Event: event.RunCancelled}:       PhaseCancelled,
	{From: PhaseSandboxReady, Event: event.RunCancelled}:   PhaseCancelled,
	{From: PhaseExecuting, Event: event.RunCancelled}:      PhaseCancelled,
	{From: PhaseAgentCompleted, Event: event.RunCancelled}: PhaseCancelled,
	{From: PhaseVerifying, Event: event.RunCancelled}:      PhaseCancelled,
	{From: PhaseLinting, Event: event.RunCancelled}:        PhaseCancelled,
	{From: PhaseRefining, Event: event.RunCancelled}:       PhaseCancelled,

	// Failure can fire from any non-terminal phase. Same rationale as above —
	// list rather than wildcard.
	{From: PhaseSubmitted, Event: event.RunFailed}:      PhaseFailed,
	{From: PhaseClassified, Event: event.RunFailed}:     PhaseFailed,
	{From: PhasePlanned, Event: event.RunFailed}:        PhaseFailed,
	{From: PhaseEnriched, Event: event.RunFailed}:       PhaseFailed,
	{From: PhaseSandboxReady, Event: event.RunFailed}:   PhaseFailed,
	{From: PhaseExecuting, Event: event.RunFailed}:      PhaseFailed,
	{From: PhaseAgentCompleted, Event: event.RunFailed}: PhaseFailed,
	{From: PhaseVerifying, Event: event.RunFailed}:      PhaseFailed,
	{From: PhaseLinting, Event: event.RunFailed}:        PhaseFailed,
	{From: PhaseRefining, Event: event.RunFailed}:       PhaseFailed,
}

// LookupTransition reports the next phase for a (from, event) pair, or
// (zero, ErrInvalidTransition) if the move is not in the closed table.
func LookupTransition(from Phase, typ event.Type) (Phase, error) {
	next, ok := transitionTable[transition{From: from, Event: typ}]
	if !ok {
		return "", fmt.Errorf("%w: from=%s event=%s", ErrInvalidTransition, from, typ)
	}
	return next, nil
}

// IsObservationOnly reports whether an event type is purely observational —
// it MUST NOT trigger a phase transition even when it fires mid-run. These
// events are still appended to the event log (for replay/audit) but the
// state machine ignores them when computing the next phase.
//
// Example observation-only events: lint.passed, report.generated, review.generated,
// run.architected, security.reviewed, perf.reviewed, refactor.reviewed,
// learn.completed, dag.started, etc.
//
// Without this set, the transition table would have to enumerate every
// "stay in phase X" no-op for run.submitted, sandbox.destroyed, etc., which
// bloats the table and makes the closed-set property harder to reason about.
func IsObservationOnly(typ event.Type) bool {
	switch typ {
	case event.RunSubmitted,
		event.SandboxDestroyed,
		event.AgentToolCalled,
		event.BrainLessonRecalled,
		event.BrainLessonExtracted, // fires post-completion via Kernel.Learn
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
		event.LintRequested:
		return true
	default:
		return false
	}
}
