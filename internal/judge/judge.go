// Package judge ranks the outputs of parallel agent runs.
package judge

import (
	"context"
	"time"
)

// Candidate is one parallel sub-run made available to the judge.
type Candidate struct {
	RunID    string
	Agent    string
	ExitCode int
	Status   string
	Duration time.Duration
	Diff     string // unified diff produced by the agent
	Stdout   string // tail of agent's textual output (last ~2KB)
}

// Ranking is the structured judgement returned by Rank.
type Ranking struct {
	Winner    string             // RunID of the chosen candidate
	Scores    map[string]float64 // RunID -> 0..1 score
	Rationale string             // free-form explanation
	Judge     string             // judge name (e.g. "llm:claude-haiku-4-5")
}

// Judge ranks a slice of candidates against the original prompt.
type Judge interface {
	Name() string
	Rank(ctx context.Context, prompt string, cands []Candidate) (Ranking, error)
}
