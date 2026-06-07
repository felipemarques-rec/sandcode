// Package governance implements the deterministic policy engine that
// gates actions inside the sandcode runtime.
//
// Design invariants:
//
//   - Decisions are pure functions of Action input. No I/O, no LLM calls,
//     no time-based reasoning. The same Action always yields the same
//     Result — required for replay safety and auditability.
//   - Policies compose via the Engine: Deny short-circuits, Review
//     accumulates reasons, Allow is the implicit default when no policy
//     objects.
//   - An immutable audit_log (see audit.go) records every decision —
//     append-only, no UPDATE/DELETE. Append is best-effort and never
//     blocks the orchestrator on storage failure.
package governance

import (
	"context"
	"fmt"
)

// Result is the verdict a Policy returns for a given Action.
//
//   - Allow: the action proceeds with no further gating.
//   - Deny: the action is refused. The Engine returns Deny on the first
//     policy that denies; subsequent policies are not evaluated.
//   - Review: the action requires human (or external API) approval before
//     proceeding. Multiple Review verdicts accumulate so the operator
//     sees every concern at once.
type Result string

const (
	Allow  Result = "allow"
	Deny   Result = "deny"
	Review Result = "review"
)

// ActionType classifies what the orchestrator is asking the engine to
// approve. Each call site in the orchestrator builds a typed Action so
// policies can apply only to the action types they care about.
type ActionType string

const (
	// ActionExecute is the initial gate: "may this run start?". Evaluated
	// before any agent invocation. DiffSize/Attempt are zero.
	ActionExecute ActionType = "execute"

	// ActionRefine gates each refine iteration. Attempt is the upcoming
	// attempt number (2, 3, …). DiffSize reflects accumulated agent changes.
	ActionRefine ActionType = "refine"

	// ActionMerge gates merge-to-head decisions after a successful run.
	// Useful when DiffSize policies want to require review on large diffs.
	ActionMerge ActionType = "merge"
)

// Action is the input to every Policy.Evaluate call. All fields are
// optional except Type and RunID — Policies must handle zero-values
// gracefully (treat "missing data" as "policy doesn't apply").
type Action struct {
	Type     ActionType
	RunID    string
	Agent    string
	Strategy string
	Prompt   string

	// DiffSize is the size of the worktree diff at the moment of
	// evaluation. Zero before agent execution begins.
	DiffSize int

	// FilesChanged is the set of paths the agent has touched. Empty
	// when the orchestrator has not yet computed the diff.
	FilesChanged []string

	// TokensUsed and CostUSD are accumulated by the Budget guard. Zero
	// when budget tracking is not enabled.
	TokensUsed int64
	CostUSD    float64

	// Attempt is the current/upcoming attempt number (1-indexed). For
	// ActionRefine, this is the attempt about to start.
	Attempt int
}

// Policy is the interface every concrete rule implements. Evaluate
// returns its verdict, a human-readable reason (used for audit + UI),
// and an error reserved for genuine infrastructure failures — a policy
// returning Deny is NOT an error.
type Policy interface {
	// Name is the stable identifier used in audit rows and tests.
	// Must be unique within an Engine.
	Name() string

	// Evaluate returns Allow/Deny/Review for the given action.
	Evaluate(ctx context.Context, a Action) (Result, string, error)
}

// Decision is the Engine's aggregate verdict. It preserves the per-policy
// breakdown so the orchestrator can persist a fine-grained audit trail.
type Decision struct {
	Result    Result
	Reasons   []string
	PerPolicy []PolicyVerdict
}

// PolicyVerdict captures one policy's contribution to a Decision.
// Used by the audit log to keep an immutable record of who said what.
type PolicyVerdict struct {
	Policy string
	Result Result
	Reason string
}

// String renders a Decision for logs.
func (d Decision) String() string {
	return fmt.Sprintf("%s (%d policies, %d reasons)", d.Result, len(d.PerPolicy), len(d.Reasons))
}
