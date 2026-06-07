package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/memory"
)

// AsTier exposes this Store as a memory.Tier sourced from past Runs.
// Each emitted Item summarises one historical run in a single line of
// text suitable for direct inclusion in an enriched prompt: prompt
// excerpt, agent, status, exit code, and elapsed wall time.
func (s *SQLite) AsTier() memory.Tier { return &storeTier{store: s} }

type storeTier struct{ store *SQLite }

func (*storeTier) Name() string { return "episodic" }

func (t *storeTier) Recall(ctx context.Context, prompt string, limit int) ([]memory.Item, error) {
	runs, err := t.store.RecallSimilar(ctx, prompt, limit)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Item, 0, len(runs))
	for _, r := range runs {
		out = append(out, memory.Item{
			Kind:   memory.KindRun,
			Score:  0, // BM25 is internal to the SQL ORDER BY
			Text:   renderRun(r),
			Source: "episodic",
			Metadata: map[string]any{
				"run_id":    r.ID,
				"agent":     r.Agent,
				"sandbox":   r.Sandbox,
				"status":    string(r.Status),
				"exit_code": r.ExitCode,
				"started":   r.StartedAt,
			},
		})
	}
	return out, nil
}

// renderRun produces the per-run line the enricher embeds. Statuses
// get a leading marker so the consuming LLM can quickly spot whether a
// historical prompt resembled a success or a failure.
func renderRun(r Run) string {
	icon := "•"
	switch r.Status {
	case StatusSuccess:
		icon = "✅"
	case StatusFailure:
		icon = "❌"
	case StatusCancelled:
		icon = "⏸"
	}
	dur := r.FinishedAt.Sub(r.StartedAt)
	excerpt := r.Prompt
	if len(excerpt) > 120 {
		excerpt = excerpt[:117] + "…"
	}
	return fmt.Sprintf("%s [%s] %s (exit=%d, %s) — %s",
		icon, r.Agent, strings.ToUpper(string(r.Status)), r.ExitCode, dur.Round(1e9), excerpt)
}
