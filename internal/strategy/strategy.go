// Package strategy chooses an execution strategy from a deterministic
// rule set. Same inputs → same output → auditable + cache-friendly +
// testable. There is no LLM in this hot path: strategy selection is a
// transparent rule that operators can read, override, and reason about.
//
// Inputs:
//
//   - brain.Classification — what kind of task is this (Convergent vs
//     Divergent, Low/Medium/High complexity).
//   - planner.TaskDAG — the (possibly empty) decomposition.
//
// Output: a Strategy plus a short human-readable Reason explaining
// which rule fired. The reason is part of the contract — it shows up
// in audit rows and the run.strategy_selected event payload, so
// operators can answer "why did the kernel choose parallel here?"
// without reading code.
//
// See master plan §9.2.
package strategy

import (
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/planner"
)

// Strategy is the canonical set of execution shapes the orchestrator
// understands. Strings are stable wire values used in events and audit.
type Strategy string

const (
	// StrategySingle: one agent, no verification loop. The cheapest
	// path; default when nothing else fires.
	StrategySingle Strategy = "single"

	// StrategyRefine: one agent inside a verify+refine loop. Used for
	// high-complexity tasks where a single attempt is unlikely to pass
	// a verifier first try.
	StrategyRefine Strategy = "refine"

	// StrategyParallel: multiple agents fan out concurrently; a judge
	// picks the winner. Used when the plan exposes multiple independent
	// root nodes (i.e. the work is genuinely parallelisable).
	StrategyParallel Strategy = "parallel"
)

// Selector chooses a Strategy. The Reason is meant for humans — keep
// it under ~80 chars; "rule that fired" rather than a sentence.
type Selector interface {
	Select(c brain.Classification, plan planner.TaskDAG) (Strategy, string)
}

// RuleSelector implements Selector with a small ordered rule list.
// The first matching rule wins; ties are impossible because the
// conditions are mutually exclusive on the planning DAG shape, but
// when in doubt the higher-leverage strategies (parallel, refine)
// rank above the default.
type RuleSelector struct{}

// New returns the default RuleSelector. Stateless — the zero value is
// also fully usable; New exists for symmetry with other packages.
func New() RuleSelector { return RuleSelector{} }

// Select applies the rule set:
//
//  1. If the plan has more than one root → StrategyParallel.
//     Multi-root means independent work items; fan out.
//  2. Else if complexity == High → StrategyRefine.
//     A single hard task is unlikely to pass verify on first try.
//  3. Otherwise → StrategySingle.
//
// Force-flags on the request (ForceParallel, ForceRefine) are
// intentionally NOT honored in this slice — no caller sets them yet
// and over-eager API surface ages badly. When a caller needs them,
// add a Request type and prepend two rules above (1).
func (RuleSelector) Select(c brain.Classification, plan planner.TaskDAG) (Strategy, string) {
	if len(plan.Roots()) > 1 {
		return StrategyParallel, "plan has multiple roots — parallelisable work"
	}
	if c.Complexity == brain.ComplexityHigh {
		return StrategyRefine, "high complexity — verify+refine loop"
	}
	return StrategySingle, "default — no rule fired"
}
