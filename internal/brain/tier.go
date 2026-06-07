package brain

import (
	"context"
	"fmt"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/memory"
)

// AsTier exposes this Brain as a memory.Tier. The tier emits one
// memory.Item per recalled lesson, with Text formatted for direct
// inclusion in an enriched prompt (preserving the Stage-2 icon-prefix
// convention).
func (b *SQLiteBrain) AsTier() memory.Tier { return &brainTier{brain: b} }

type brainTier struct{ brain *SQLiteBrain }

func (*brainTier) Name() string { return "lessons" }

func (t *brainTier) Recall(ctx context.Context, prompt string, limit int) ([]memory.Item, error) {
	lessons, err := t.brain.Recall(ctx, prompt, limit)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Item, 0, len(lessons))
	for _, l := range lessons {
		out = append(out, memory.Item{
			Kind:   memory.KindLesson,
			Score:  l.Confidence,
			Text:   renderLesson(l),
			Source: "lessons",
			Metadata: map[string]any{
				"id":         l.ID,
				"run_id":     l.RunID,
				"category":   string(l.Category),
				"confidence": l.Confidence,
				"used_count": l.UsedCount,
				"valid_from": l.ValidFrom,
			},
		})
	}
	return out, nil
}

// renderLesson formats a single lesson the way enrichment-mode prompts
// have rendered them since Stage 2: icon + category + content +
// confidence suffix.
func renderLesson(l Lesson) string {
	icon := "✅"
	switch l.Category {
	case CategoryAntiPattern:
		icon = "❌"
	case CategoryPreference:
		icon = "⚙️"
	case CategoryPrinciple:
		icon = "📐"
	}
	return fmt.Sprintf("%s %s: %s (confidence: %.2f)",
		icon, strings.ToUpper(string(l.Category)), l.Content, l.Confidence)
}
