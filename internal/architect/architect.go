// Package architect implements the Architect role: given a task prompt and its
// classification, it designs the solution structure (approach, likely files,
// risks). The kernel injects this as guidance into the prompt that feeds the
// planner, enricher, and implementer. Opt-in; failures degrade gracefully.
package architect

import "context"

// ArchPlan is the Architect's design output.
type ArchPlan struct {
	Approach  string   // recommended solution structure / approach
	Files     []string // files likely to be created or modified
	Risks     []string // key risks / pitfalls to watch
	Architect string   // producer id, e.g. "llm:claude-haiku-4-5-20251001"
}

// DesignRequest is the flattened input. Classification is passed as primitives
// so this package does not import internal/brain (keeps the dependency one-way).
type DesignRequest struct {
	Prompt      string
	ProblemType string // "divergent" / "convergent"
	Complexity  string // "low" / "medium" / "high"
}

// Architect designs solution structure for a task.
type Architect interface {
	Design(ctx context.Context, req DesignRequest) (ArchPlan, error)
}
