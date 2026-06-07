package orchestrator

import (
	"fmt"
)

// validateDAG checks the plan + DAGOptions are executable. Returns a
// sentinel error (ErrEmptyPlan / planner.ErrCycle / ErrDiamondNotSupported /
// ErrJudgeRequiredForMultiRoot) when shape is invalid. Returns nil when
// the plan is ready to execute. Runs before any worktree provisioning —
// no side effects on failure.
func validateDAG(opts DAGOptions) error {
	if len(opts.Plan.Nodes) == 0 {
		return ErrEmptyPlan
	}

	// Reuse planner-level validation: cycles, duplicates, dangling deps,
	// empty IDs/prompts. Returns sentinel errors from the planner package.
	if err := opts.Plan.Validate(); err != nil {
		return err
	}

	// Diamond check: any node with >1 dependency. Slice 4 does not
	// support fan-in — explicit rejection beats silent partial execution.
	for _, n := range opts.Plan.Nodes {
		if len(n.DependsOn) > 1 {
			return fmt.Errorf("%w: node %q depends on %v", ErrDiamondNotSupported, n.ID, n.DependsOn)
		}
	}

	// Judge required when there are multiple root chains to rank.
	if len(opts.Plan.Roots()) > 1 && opts.Judge == nil {
		return ErrJudgeRequiredForMultiRoot
	}

	return nil
}
