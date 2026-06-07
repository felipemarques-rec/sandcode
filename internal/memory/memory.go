// Package memory composes typed recall results from multiple "tiers"
// (semantic / episodic / future others) into a single ordered stream
// that prompt enrichers and agents can consume.
//
// Design tenets:
//
//   - Tiers stay independent. Each Tier is responsible for its own
//     storage, ranking, and BM25 (or whatever it uses) — the package
//     does NOT try to normalise scores across tiers because scores
//     from different corpora aren't comparable.
//   - The Arbitrator distributes the caller's limit budget evenly
//     across tiers and concatenates the per-tier orderings. Consumers
//     that care about per-tier counts can group on Item.Kind.
//   - No dependencies on brain/store. The brain and store packages
//     ship their own Tier adapters that return memory.Items.
package memory

import (
	"context"
	"errors"
)

// ItemKind classifies a recalled item by its source modality. The set
// is open in spirit (callers may invent new tiers) but the well-known
// values are listed here so consumers can switch on them.
type ItemKind string

const (
	// KindLesson is a learned cognitive lesson (skill / antipattern /
	// preference / principle). Source: brain.
	KindLesson ItemKind = "lesson"

	// KindRun is a past run with a similar prompt. Source: store.
	KindRun ItemKind = "run"
)

// Item is the unified recall result. Kind tells the consumer which
// payload type Source/Metadata carry; Score is the source-tier's local
// relevance (only meaningful within a tier — see package doc).
type Item struct {
	Kind     ItemKind       `json:"kind"`
	Score    float64        `json:"score"`
	Text     string         `json:"text"`   // rendered text the consumer may quote
	Source   string         `json:"source"` // tier name, for attribution in prompts
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Tier is the minimum a memory backend must implement.
type Tier interface {
	// Name returns a short stable identifier ("lessons", "episodic",
	// …) used for logs, metrics, and the `source` field of returned
	// Items.
	Name() string

	// Recall returns up to limit items ordered most-relevant-first by
	// the tier's own metric. Returning an empty slice is normal when
	// the tier has nothing to say about the prompt.
	Recall(ctx context.Context, prompt string, limit int) ([]Item, error)
}

// Memory is the consumer-facing recall surface. Arbitrator is the
// only implementation today, but enrichers and agents code to this
// interface so a future single-tier or mock implementation slots in.
type Memory interface {
	Recall(ctx context.Context, prompt string, limit int) ([]Item, error)
}

// Arbitrator fans Recall out to its registered tiers and concatenates
// the results. Tier order at construction is preserved in the output:
// earlier tiers appear before later ones for the same logical "rank".
type Arbitrator struct {
	tiers []Tier
}

// NewArbitrator constructs an Arbitrator over the given tiers. nil
// tiers are filtered out so callers can write
// `NewArbitrator(brainTier, episodicTier)` even when one is optionally
// nil.
func NewArbitrator(tiers ...Tier) *Arbitrator {
	live := tiers[:0:0]
	for _, t := range tiers {
		if t != nil {
			live = append(live, t)
		}
	}
	return &Arbitrator{tiers: live}
}

// Tiers returns the registered tiers in order. Test helper; production
// consumers should use Recall.
func (a *Arbitrator) Tiers() []Tier { return a.tiers }

// Recall asks each tier for its share of the budget and returns the
// concatenated result. Per-tier failures are NOT fatal — they're
// recorded into the multi-error and other tiers continue; this matches
// the "graceful degradation" rule the Brain has always honoured. If
// EVERY tier fails, the joined error is returned.
//
// Budget split: limit / len(tiers), rounded up. A 5-limit / 2-tier
// call asks each tier for 3 items (total ≤ 6). The slight overshoot is
// deliberate — it gives tiers room to express strong matches without
// being clipped by an aggressive cap.
func (a *Arbitrator) Recall(ctx context.Context, prompt string, limit int) ([]Item, error) {
	if len(a.tiers) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	perTier := (limit + len(a.tiers) - 1) / len(a.tiers)

	var out []Item
	var errs []error
	for _, t := range a.tiers {
		items, err := t.Recall(ctx, prompt, perTier)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, items...)
	}
	// All tiers failed — surface a unified error.
	if len(errs) == len(a.tiers) && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	// At least one tier returned — the partial failure is non-fatal.
	// We intentionally don't surface partial errors to the caller; the
	// recall path is best-effort and the Memory contract says "may
	// return empty" for any tier that has nothing useful to add.
	return out, nil
}
