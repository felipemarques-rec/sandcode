package planner

import "context"

// Planner decomposes a prompt into a TaskDAG of subtasks. The returned
// DAG MUST satisfy TaskDAG.Validate() — implementations are expected to
// run Validate before returning so callers can rely on a well-formed
// graph downstream.
//
// Implementations are free to fall back to a single-node DAG when the
// prompt is simple enough that decomposition adds no value; that keeps
// the API uniform so callers don't need a branch for "no plan needed".
type Planner interface {
	Decompose(ctx context.Context, prompt string) (TaskDAG, error)
}

// NoOpPlanner wraps any prompt into a single-node DAG. It exists for
// three reasons:
//
//   - It's the trivial Planner implementation tests can use without
//     spinning up an LLM mock.
//   - It's the conservative fallback when the LLM is unavailable or
//     when classification says "low complexity, no decomposition needed".
//   - It documents the minimal contract: every prompt becomes at least
//     one node, never zero.
//
// The constant ID below ("root") is deliberate — it gives downstream
// consumers a stable handle for the single-node case so they don't have
// to special-case "is this a NoOp output?".
type NoOpPlanner struct{}

// NoOpNodeID is the ID NoOpPlanner uses for its single node. Exposed
// so executors that need to recognise the no-op shape (e.g. for "skip
// DAG scheduling, run directly") can do so without string fragility.
const NoOpNodeID = "root"

func (NoOpPlanner) Decompose(_ context.Context, prompt string) (TaskDAG, error) {
	d := TaskDAG{Nodes: []Node{
		{ID: NoOpNodeID, Prompt: prompt},
	}}
	// Validate so this implementation enforces the same contract the
	// LLM impl does — keeps callers' assumptions uniform.
	if err := d.Validate(); err != nil {
		return TaskDAG{}, err
	}
	return d, nil
}
