// Package builtin holds the off-the-shelf policies sandcode ships with.
// Each policy is a small focused rule; combine them via governance.NewEngine.
package builtin

import (
	"context"
	"fmt"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

// RetryLimit denies refine attempts beyond MaxAttempts. This is defense
// in depth: the orchestrator's RefineOptions.MaxAttempts is the primary
// cap, but policies travel with the run config and can be enforced
// uniformly across CLI, HTTP API, and webhook entry points.
type RetryLimit struct {
	MaxAttempts int
}

// Name returns the stable identifier used in audit rows.
func (r RetryLimit) Name() string { return "retry_limit" }

// Evaluate denies ActionRefine when the upcoming Attempt would exceed
// MaxAttempts. Other action types are always allowed by this policy.
//
// A zero or negative MaxAttempts disables the policy (allow-all) — keeps
// the zero-value useful so callers can opt-in incrementally.
func (r RetryLimit) Evaluate(_ context.Context, a governance.Action) (governance.Result, string, error) {
	if r.MaxAttempts <= 0 {
		return governance.Allow, "", nil
	}
	if a.Type != governance.ActionRefine {
		return governance.Allow, "", nil
	}
	if a.Attempt > r.MaxAttempts {
		return governance.Deny,
			fmt.Sprintf("retry cap exceeded: attempt=%d max=%d", a.Attempt, r.MaxAttempts),
			nil
	}
	return governance.Allow, "", nil
}
