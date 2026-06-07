// Package brain implements the cognitive learning loop for sandcode.
//
// The Brain is responsible for:
//   - Learning from run outcomes (extracting skills and anti-patterns)
//   - Recalling relevant lessons for future runs
//   - Enriching prompts with system context and learned knowledge
//   - Classifying problems for strategy selection
//   - Pruning stale or low-confidence lessons
//
// All Brain operations are opt-in and non-blocking. A nil Brain or
// Brain errors never prevent normal run execution (graceful degradation).
package brain

import (
	"context"
	"time"
)

// Category classifies a lesson by its cognitive function.
type Category string

const (
	// CategorySkill is a positive pattern extracted from successful runs.
	CategorySkill Category = "skill"

	// CategoryAntiPattern is a negative pattern extracted from failures.
	CategoryAntiPattern Category = "antipattern"

	// CategoryPreference is a user/project-specific convention.
	CategoryPreference Category = "preference"

	// CategoryPrinciple is a general engineering principle validated by evidence.
	CategoryPrinciple Category = "principle"
)

// Lesson is a unit of learned knowledge extracted from a run outcome.
type Lesson struct {
	ID         string
	RunID      string
	Category   Category
	Tags       []string
	Content    string  // human-readable description
	Evidence   string  // code snippet, diff excerpt, or error message
	Confidence float64 // 0.0-1.0; decays over time if unused
	UsedCount  int
	LastUsed   time.Time
	ValidFrom  time.Time  // bi-temporal: when this knowledge became valid
	ValidTo    *time.Time // bi-temporal: nil = still valid
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Outcome captures the full result of a run for learning purposes.
type Outcome struct {
	RunID     string
	Agent     string
	Prompt    string
	Diff      string
	Status    string // "success" | "failure" | "cancelled"
	ExitCode  int
	Duration  time.Duration
	Score     float64 // judge score if available, 0 otherwise
	Rationale string  // judge rationale if available
}

// Stats summarizes the current state of the brain's knowledge.
type Stats struct {
	TotalLessons  int
	Skills        int
	AntiPatterns  int
	Preferences   int
	Principles    int
	AvgConfidence float64
	OldestLesson  time.Time
	NewestLesson  time.Time
}

// Brain is the cognitive learning interface. Implementations must be
// safe for concurrent use.
type Brain interface {
	// Learn extracts lessons from a completed run outcome.
	// Returns the number of lessons extracted.
	Learn(ctx context.Context, outcome Outcome) (int, error)

	// Recall returns the most relevant lessons for a given prompt,
	// ranked by relevance × confidence. limit caps the result count.
	Recall(ctx context.Context, prompt string, limit int) ([]Lesson, error)

	// Enrich builds an enriched prompt by combining the raw prompt
	// with system context, recalled lessons, and project documentation.
	Enrich(ctx context.Context, prompt string, cwd string) (string, error)

	// Store persists a single lesson. Used by extractors.
	Store(ctx context.Context, lesson Lesson) error

	// Invalidate marks a lesson as no longer valid (sets ValidTo).
	Invalidate(ctx context.Context, lessonID string) error

	// Prune removes lessons older than maxAge with confidence below
	// minConfidence. Returns the count of pruned lessons.
	Prune(ctx context.Context, maxAge time.Duration, minConfidence float64) (int, error)

	// Stats returns aggregate statistics about stored lessons.
	Stats(ctx context.Context) (Stats, error)

	// ListLessons returns lessons filtered by category. Empty category = all.
	ListLessons(ctx context.Context, category Category, limit int) ([]Lesson, error)

	// Close releases resources.
	Close() error
}
