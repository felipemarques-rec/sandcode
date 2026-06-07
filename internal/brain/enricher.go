package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/memory"
)

// Enricher is a thin compatibility shim around memory.Enricher. It
// remains in the brain package because the cognitive Kernel (and any
// older Stage-2 caller) imports `brain.NewEnricher(b)`; the actual
// prompt-building logic lives in the memory package now.
//
// New code should construct memory.Enricher directly with a
// memory.Arbitrator. Use brain.NewEnricher only when you want the
// historical "lessons-only" enrichment shape.
type Enricher struct {
	brain *SQLiteBrain
	inner *memory.Enricher
}

// NewEnricher constructs an Enricher backed by the given Brain. If the
// argument is not a *SQLiteBrain we still produce a usable enricher
// over a no-tier arbitrator — Enrich will just include the persona +
// rules + task, without lessons or episodic memory.
func NewEnricher(b Brain) *Enricher {
	sq, _ := b.(*SQLiteBrain)
	var tiers []memory.Tier
	if sq != nil {
		tiers = append(tiers, sq.AsTier())
		if sq.episodic != nil {
			tiers = append(tiers, sq.episodic)
		}
	}
	arb := memory.NewArbitrator(tiers...)
	return &Enricher{
		brain: sq,
		inner: memory.NewEnricher(arb,
			memory.WithDocs(ScanProjectDocs),
			memory.WithClassifier(classifierAdapter{NewClassifier()}),
		),
	}
}

// Enrich produces the agent prompt. See memory.Enricher.Enrich.
func (e *Enricher) Enrich(ctx context.Context, prompt string, cwd string) (string, error) {
	return e.inner.Enrich(ctx, prompt, cwd)
}

// Learn extracts and stores lessons from a completed outcome. Kept on
// Enricher for legacy callers; new code should call brain.Brain.Learn
// directly.
func (e *Enricher) Learn(ctx context.Context, outcome Outcome) (int, error) {
	if e.brain == nil {
		return 0, nil
	}
	extractor := NewExtractor()
	lessons, err := extractor.Extract(ctx, outcome)
	if err != nil {
		return 0, fmt.Errorf("extract: %w", err)
	}
	stored := 0
	for _, l := range lessons {
		l.ValidFrom = time.Now()
		if err := e.brain.Store(ctx, l); err != nil {
			continue
		}
		stored++
	}
	return stored, nil
}
