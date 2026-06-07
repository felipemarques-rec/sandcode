// Package store persists runs, events, and rankings.
//
// The Store interface is intentionally narrow so callers can pass a nil
// store when persistence is not desired (the orchestrator treats nil as
// "no-op").
package store

import (
	"context"
	"time"
)

// RunStatus enumerates the lifecycle states of a Run row.
type RunStatus string

const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusSuccess   RunStatus = "success"
	StatusFailure   RunStatus = "failure"
	StatusCancelled RunStatus = "cancelled"
)

// Run represents one orchestrated agent invocation.
type Run struct {
	ID         string
	ParentID   string // for sub-runs of a parallel parent
	Agent      string
	Sandbox    string
	Prompt     string
	CWD        string
	Strategy   string
	Status     RunStatus
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	DiffPath   string // path on disk where the diff was saved (or "")
}

// Event is one structured stream event from the agent.
type Event struct {
	Seq       int64
	Timestamp time.Time
	Kind      string // text|tool_call|warning|session|raw
	Payload   string // JSON-encoded payload
}

// ListFilter narrows ListRuns results.
type ListFilter struct {
	Limit    int
	Status   RunStatus
	Agent    string
	ParentID string // empty -> top-level only; "*" -> include all
}

// Ranking is the judge's verdict for a parallel parent run.
type Ranking struct {
	ParentRunID string
	Judge       string
	WinnerRunID string
	Scores      map[string]float64
	Rationale   string
	CreatedAt   time.Time
}

// Store is the persistence contract. Implementations are safe for
// concurrent use.
type Store interface {
	CreateRun(ctx context.Context, r Run) error
	UpdateRun(ctx context.Context, r Run) error
	AppendEvent(ctx context.Context, runID string, e Event) error
	GetRun(ctx context.Context, runID string) (Run, error)
	ListRuns(ctx context.Context, f ListFilter) ([]Run, error)
	ListEvents(ctx context.Context, runID string) ([]Event, error)

	SaveRanking(ctx context.Context, r Ranking) error
	GetRanking(ctx context.Context, parentRunID string) (Ranking, error)

	// RecallSimilar returns up to limit past Runs whose prompts share
	// keywords with the given prompt, ranked by BM25 (most relevant
	// first). Used by episodic-memory recall — see internal/memory.
	// Returning an empty slice (no nil error) is normal: a fresh
	// project has no relevant history.
	RecallSimilar(ctx context.Context, prompt string, limit int) ([]Run, error)

	Close() error
}
