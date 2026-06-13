// Package stepback implements the Step-Back Reasoner: given a task prompt and its
// classification, it distills the high-level principles/abstractions that reframe
// the problem. The kernel injects these as guidance into the prompt feeding the
// planner and enricher. Opt-in; failures degrade gracefully. Mirrors internal/architect.
package stepback

import "context"

// Result is the Step-Back output: the high-level principles that reframe the task.
type Result struct {
	Principles []string // 2-4 high-level principles/abstractions
	Reasoner   string   // producer id, e.g. "llm:claude-haiku-4-5-20251001"
}

// ReasonRequest is the flattened input (primitives — no internal/brain import).
type ReasonRequest struct {
	Prompt      string
	ProblemType string // "divergent" / "convergent"
	Complexity  string // "low" / "medium" / "high"
}

// StepBack distills reframing principles for a task.
type StepBack interface {
	Reason(ctx context.Context, req ReasonRequest) (Result, error)
}
