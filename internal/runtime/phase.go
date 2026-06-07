// Package runtime implements the deterministic execution state machine
// for a single sandcode run.
//
// Design invariants:
//
//   - State is derived ONLY from events. ExecutionState.Apply is the sole
//     mutation entry point — there is no public setter.
//   - Phase transitions are pure functions of (currentPhase, event.Type).
//     Same input → same output. Always.
//   - The transition table is CLOSED: any (from, eventType) pair not listed
//     is rejected with ErrInvalidTransition. This guarantees replay safety.
//   - The state machine is event-source-agnostic: it does not know whether
//     events arrived from the in-process bus, the SQLite event store, or a
//     future NATS replay. It just applies them in order.
package runtime

// Phase is the high-level execution phase of a run. The set is closed —
// new phases require an explicit enum addition and a transition-table update.
type Phase string

const (
	// PhaseSubmitted is the initial state immediately after Run() accepts
	// the request and emits run.submitted.
	PhaseSubmitted Phase = "submitted"

	// PhaseClassified means the cognitive kernel has classified the prompt
	// (convergent/divergent, complexity).
	PhaseClassified Phase = "classified"

	// PhasePlanned means the planner has decomposed the prompt into a
	// subtask DAG. Optional — convergent low-complexity runs skip this.
	PhasePlanned Phase = "planned"

	// PhaseEnriched means recall + Grill-with-Docs + system prompt build
	// produced the final enriched prompt that goes to the agent.
	PhaseEnriched Phase = "enriched"

	// PhaseSandboxReady means the container is up and the agent process
	// is about to be invoked (sandbox.created emitted).
	PhaseSandboxReady Phase = "sandbox_ready"

	// PhaseExecuting means the agent is running in the sandbox
	// (agent.executing emitted).
	PhaseExecuting Phase = "executing"

	// PhaseAgentCompleted means the agent process has finished (exit code
	// captured) but post-run steps (verify/lint/report) have not started.
	PhaseAgentCompleted Phase = "agent_completed"

	// PhaseVerifying means the verifier is running tests/lints (Stage 2).
	PhaseVerifying Phase = "verifying"

	// PhaseRefining means a verify failure triggered a refine iteration
	// (Stage 2). On entry the Attempt counter increments.
	PhaseRefining Phase = "refining"

	// PhaseLinting means the linter gate is running (Stage 2).
	PhaseLinting Phase = "linting"

	// PhaseReporting means REPORT.md generation is in progress (Stage 2).
	PhaseReporting Phase = "reporting"

	// PhaseLearning means lesson extraction is running (Stage 2).
	PhaseLearning Phase = "learning"

	// PhaseCompleted is the terminal success phase.
	PhaseCompleted Phase = "completed"

	// PhaseFailed is the terminal failure phase.
	PhaseFailed Phase = "failed"

	// PhaseCancelled is the terminal user-cancelled phase.
	PhaseCancelled Phase = "cancelled"
)

// IsTerminal reports whether the phase is a final state — no further
// transitions are valid.
func (p Phase) IsTerminal() bool {
	switch p {
	case PhaseCompleted, PhaseFailed, PhaseCancelled:
		return true
	default:
		return false
	}
}

// String returns the phase name for logging and serialization.
func (p Phase) String() string { return string(p) }
