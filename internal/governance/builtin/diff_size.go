package builtin

import (
	"context"
	"fmt"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

// DiffSize triggers Review (not Deny) when the agent's diff exceeds a
// configured byte threshold. Large diffs are not inherently bad — they
// just warrant human inspection before merge.
//
// The policy applies to ActionRefine (between iterations) and
// ActionMerge (final gate). It is a no-op for ActionExecute where the
// diff doesn't exist yet.
type DiffSize struct {
	// ReviewAboveBytes is the threshold in bytes for the worktree diff.
	// Zero disables the policy.
	ReviewAboveBytes int
}

// Name returns the stable identifier used in audit rows.
func (d DiffSize) Name() string { return "diff_size" }

func (d DiffSize) Evaluate(_ context.Context, a governance.Action) (governance.Result, string, error) {
	if d.ReviewAboveBytes <= 0 {
		return governance.Allow, "", nil
	}
	if a.Type != governance.ActionMerge && a.Type != governance.ActionRefine {
		return governance.Allow, "", nil
	}
	if a.DiffSize > d.ReviewAboveBytes {
		return governance.Review,
			fmt.Sprintf("diff size %d bytes exceeds review threshold %d",
				a.DiffSize, d.ReviewAboveBytes),
			nil
	}
	return governance.Allow, "", nil
}
