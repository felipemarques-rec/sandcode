package brain

import (
	"context"
	"fmt"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/memory"
	"github.com/google/uuid"
)

// Extractor analyzes run outcomes and extracts lessons.
// Phase 1 uses rule-based extraction; Phase 3 adds LLM-assisted extraction.
type Extractor struct{}

// NewExtractor creates a rule-based extractor.
func NewExtractor() *Extractor {
	return &Extractor{}
}

// Extract analyzes an Outcome and returns lessons to store.
func (e *Extractor) Extract(_ context.Context, outcome Outcome) ([]Lesson, error) {
	var lessons []Lesson

	switch outcome.Status {
	case "success":
		lessons = append(lessons, e.extractFromSuccess(outcome)...)
	case "failure":
		lessons = append(lessons, e.extractFromFailure(outcome)...)
	}

	// Always extract duration-based lessons
	lessons = append(lessons, e.extractPerformance(outcome)...)

	return lessons, nil
}

func (e *Extractor) extractFromSuccess(o Outcome) []Lesson {
	var lessons []Lesson

	// Success with high judge score → skill
	if o.Score >= 0.8 {
		lessons = append(lessons, Lesson{
			ID:         uuid.New().String()[:12],
			RunID:      o.RunID,
			Category:   CategorySkill,
			Tags:       extractTags(o.Prompt),
			Content:    fmt.Sprintf("Agent %s successfully completed task with score %.2f", o.Agent, o.Score),
			Evidence:   truncateEvidence(o.Diff, 500),
			Confidence: scaleConfidence(o.Score),
		})
	}

	// Large diff on success → might indicate over-engineering
	diffLines := strings.Count(o.Diff, "\n")
	if diffLines > 200 {
		lessons = append(lessons, Lesson{
			ID:       uuid.New().String()[:12],
			RunID:    o.RunID,
			Category: CategoryPreference,
			Tags:     []string{"diff-size", "minimality"},
			Content: fmt.Sprintf(
				"Agent %s produced a large diff (%d lines) for this task type. Consider requesting minimal changes.",
				o.Agent, diffLines),
			Evidence:   fmt.Sprintf("diff_lines=%d", diffLines),
			Confidence: 0.5,
		})
	}

	return lessons
}

func (e *Extractor) extractFromFailure(o Outcome) []Lesson {
	var lessons []Lesson

	// General failure anti-pattern
	lessons = append(lessons, Lesson{
		ID:       uuid.New().String()[:12],
		RunID:    o.RunID,
		Category: CategoryAntiPattern,
		Tags:     append(extractTags(o.Prompt), "failure", o.Agent),
		Content: fmt.Sprintf(
			"Agent %s failed (exit=%d) on this task type. %s",
			o.Agent, o.ExitCode, o.Rationale),
		Evidence:   truncateEvidence(o.Diff, 300),
		Confidence: 0.6,
	})

	// Timeout detection
	if o.ExitCode == -1 || o.Duration.Minutes() > 10 {
		lessons = append(lessons, Lesson{
			ID:       uuid.New().String()[:12],
			RunID:    o.RunID,
			Category: CategoryAntiPattern,
			Tags:     []string{"timeout", o.Agent},
			Content: fmt.Sprintf(
				"Agent %s timed out after %s. Consider breaking the task into smaller subtasks or increasing timeout.",
				o.Agent, o.Duration.Round(1e9)),
			Evidence:   fmt.Sprintf("duration=%s, exit_code=%d", o.Duration, o.ExitCode),
			Confidence: 0.8,
		})
	}

	return lessons
}

func (e *Extractor) extractPerformance(o Outcome) []Lesson {
	var lessons []Lesson

	// Fast success → high-confidence skill
	if o.Status == "success" && o.Duration.Seconds() < 30 {
		lessons = append(lessons, Lesson{
			ID:       uuid.New().String()[:12],
			RunID:    o.RunID,
			Category: CategorySkill,
			Tags:     []string{"fast", o.Agent, "performance"},
			Content: fmt.Sprintf(
				"Agent %s completed task in %s — fast execution pattern.",
				o.Agent, o.Duration.Round(1e9)),
			Evidence:   fmt.Sprintf("duration=%s", o.Duration),
			Confidence: 0.7,
		})
	}

	return lessons
}

// extractTags pulls meaningful keywords from the prompt for lesson tagging.
func extractTags(prompt string) []string {
	keywords := memory.ExtractKeywords(prompt)
	if len(keywords) > 5 {
		keywords = keywords[:5]
	}
	return keywords
}

func truncateEvidence(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func scaleConfidence(score float64) float64 {
	// Map judge score [0,1] to confidence [0.5, 0.95]
	return 0.5 + score*0.45
}
