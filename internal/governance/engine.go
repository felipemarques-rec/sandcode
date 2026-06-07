package governance

import (
	"context"
	"fmt"
)

// Engine runs an ordered list of Policies and aggregates their verdicts
// into a single Decision.
//
// Aggregation rules:
//
//   - The first Deny short-circuits — remaining policies are not asked.
//     Rationale: a denied action is denied; surfacing more reasons after
//     that is noise. The verdict still records which policy denied.
//   - Multiple Review verdicts accumulate. The operator sees every
//     concern, since reviewing one is no cheaper than reviewing them all.
//   - Allow is the implicit default. An Engine with zero policies always
//     Allows.
//   - A policy returning a non-nil error is treated as a Deny with the
//     error message — defensive default for genuine infra failures.
type Engine struct {
	policies []Policy
}

// NewEngine creates an Engine over the given policies. Order matters
// only for the short-circuit semantics — putting cheaper / more-likely-
// to-deny policies first reduces unnecessary work.
func NewEngine(policies ...Policy) *Engine {
	return &Engine{policies: policies}
}

// AddPolicy appends a Policy to the engine. Returns an error if the
// name collides with an existing policy — duplicate names would break
// audit traceability.
func (e *Engine) AddPolicy(p Policy) error {
	for _, existing := range e.policies {
		if existing.Name() == p.Name() {
			return fmt.Errorf("governance: duplicate policy name %q", p.Name())
		}
	}
	e.policies = append(e.policies, p)
	return nil
}

// Policies returns a copy of the engine's policy list — useful for
// audit-tab UIs and tests. Returning a copy ensures callers can't
// mutate the engine through the slice.
func (e *Engine) Policies() []Policy {
	out := make([]Policy, len(e.policies))
	copy(out, e.policies)
	return out
}

// Evaluate runs the policies and returns the aggregate Decision.
// On any policy returning an error, the Engine yields Deny and records
// the error in the per-policy breakdown — this protects against
// silently-skipped policies, which would create gaps in the audit trail.
func (e *Engine) Evaluate(ctx context.Context, a Action) Decision {
	d := Decision{Result: Allow}
	for _, p := range e.policies {
		res, reason, err := p.Evaluate(ctx, a)
		if err != nil {
			d.Result = Deny
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s: %v", p.Name(), err))
			d.PerPolicy = append(d.PerPolicy, PolicyVerdict{
				Policy: p.Name(), Result: Deny, Reason: err.Error(),
			})
			return d
		}
		d.PerPolicy = append(d.PerPolicy, PolicyVerdict{
			Policy: p.Name(), Result: res, Reason: reason,
		})
		switch res {
		case Deny:
			d.Result = Deny
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s: %s", p.Name(), reason))
			return d // short-circuit
		case Review:
			d.Result = Review
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s: %s", p.Name(), reason))
			// continue — accumulate other reviews
		case Allow:
			// no-op
		}
	}
	return d
}
