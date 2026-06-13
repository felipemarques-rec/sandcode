// Package costopt implements deterministic model routing: it maps a task
// classification to the model the agent should run, so simple work uses cheap/fast
// models and hard/novel work uses stronger ones. Pure, deterministic; mirrors
// internal/strategy.
package costopt

import "github.com/felipemarques-rec/sandcode/internal/brain"

// Model identifiers per tier (Opus 4.8 / Sonnet 4.6 / Haiku 4.5).
const (
	ModelStrong   = "claude-opus-4-8"           // high complexity or divergent
	ModelBalanced = "claude-sonnet-4-6"         // medium complexity
	ModelFast     = "claude-haiku-4-5-20251001" // low complexity, convergent
)

// Router picks the agent model from a task classification.
type Router interface {
	Route(c brain.Classification) (model, reason string)
}

// RuleRouter is the deterministic default router.
type RuleRouter struct{}

// New returns the default RuleRouter.
func New() RuleRouter { return RuleRouter{} }

// Route maps classification → model. Deterministic and total.
func (RuleRouter) Route(c brain.Classification) (string, string) {
	if c.Complexity == brain.ComplexityHigh || c.Type == brain.Divergent {
		return ModelStrong, "high-complexity or divergent → strongest model"
	}
	if c.Complexity == brain.ComplexityMedium {
		return ModelBalanced, "medium complexity → balanced model"
	}
	return ModelFast, "low-complexity convergent → fast/cheap model"
}
