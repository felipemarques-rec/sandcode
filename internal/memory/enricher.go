package memory

import (
	"context"
	"fmt"
	"strings"
)

// Enricher builds the Staff/Principal Engineer enriched prompt by
// blending a base persona, project documentation, and recall results
// from a Memory (typically an Arbitrator with semantic + episodic
// tiers).
//
// Enricher is intentionally I/O-free beyond the Memory call and the
// DocsScanner callback — that keeps it trivially testable.
type Enricher struct {
	memory  Memory
	docs    DocsScanner
	classer Classifier
	// recallLimit caps how many items we request from Memory across
	// all tiers. The Arbitrator splits this between tiers, so the
	// effective per-tier cap is roughly recallLimit / N.
	recallLimit int
}

// DocsScanner returns a project-documentation context string for the
// given working directory. The brain package historically provided
// `ScanProjectDocs(cwd)` via the Enricher; we inject it now so memory
// doesn't have to import brain. A nil DocsScanner is treated as
// "produce empty docs".
type DocsScanner func(cwd string) string

// Classifier returns a short classification ("convergent" /
// "divergent" + a complexity label) of the prompt. Same injection
// reasoning as DocsScanner: the brain package owns its concrete
// classifier; memory codes against the abstraction.
type Classifier interface {
	Classify(ctx context.Context, prompt string) Classification
}

// Classification mirrors the brain.Classification fields we care about
// at the enricher level. We define it here so memory doesn't import
// brain (which would create a cycle once brain imports memory for the
// Tier adapter).
type Classification struct {
	Type       string // "convergent" | "divergent" | …
	Complexity string // "low" | "medium" | "high"
}

// EnricherOption configures an Enricher.
type EnricherOption func(*Enricher)

// WithDocs injects a project-documentation scanner.
func WithDocs(fn DocsScanner) EnricherOption {
	return func(e *Enricher) { e.docs = fn }
}

// WithClassifier injects a prompt classifier.
func WithClassifier(c Classifier) EnricherOption {
	return func(e *Enricher) { e.classer = c }
}

// WithRecallLimit overrides the default recall budget (10).
func WithRecallLimit(n int) EnricherOption {
	return func(e *Enricher) {
		if n > 0 {
			e.recallLimit = n
		}
	}
}

// NewEnricher constructs an Enricher bound to the given Memory.
// Behaviour without options is well-defined: empty docs, no
// classification line, default recall budget.
func NewEnricher(m Memory, opts ...EnricherOption) *Enricher {
	e := &Enricher{memory: m, recallLimit: 10}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Enrich produces the final agent prompt. The structure intentionally
// mirrors the Stage-2 Enricher output so downstream agent expectations
// don't shift: identity → classification → domain context → recalled
// lessons (if any) → recalled past runs (if any) → rules → user task.
func (e *Enricher) Enrich(ctx context.Context, prompt string, cwd string) (string, error) {
	var classification Classification
	if e.classer != nil {
		classification = e.classer.Classify(ctx, prompt)
	}

	var docs string
	if e.docs != nil {
		docs = e.docs(cwd)
	}

	var items []Item
	if e.memory != nil {
		items, _ = e.memory.Recall(ctx, prompt, e.recallLimit)
	}
	lessons := filterByKind(items, KindLesson)
	runs := filterByKind(items, KindRun)

	var b strings.Builder
	b.WriteString("[SANDCODE BRAIN — Staff/Principal Engineer Mode]\n\n")

	b.WriteString("## Identity\n")
	b.WriteString("You are a Staff/Principal Software Engineer. Follow these principles:\n")
	b.WriteString("- Clean Architecture, SOLID, 12-Factor App\n")
	b.WriteString("- Resilience over speed, low coupling over convenience\n")
	b.WriteString("- Evidence-based conclusions only\n")
	b.WriteString("- Never assume unconfirmed information\n")
	b.WriteString("- Never invent files, services, indices, or contracts that don't exist\n")
	b.WriteString("- If information is missing, explicitly declare the gap\n\n")

	if classification.Type != "" {
		fmt.Fprintf(&b, "## Problem Classification: %s (complexity: %s)\n\n",
			classification.Type, classification.Complexity)
	}

	if docs != "" {
		b.WriteString("## Domain Context (from project documentation)\n")
		b.WriteString(docs)
		b.WriteString("\n\n")
	}

	if len(lessons) > 0 {
		fmt.Fprintf(&b, "## Learned Lessons (%d relevant)\n", len(lessons))
		for _, l := range lessons {
			b.WriteString(l.Text)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Split episodic runs by status so the LLM gets clearly-labelled
	// "replicate this" vs "avoid this" signal. Items without a status
	// in Metadata fall into the success bucket (treated as a neutral
	// example) — this conservatively avoids tagging a run as a failure
	// pattern when we don't actually know it failed.
	successes, failures := partitionRunsByStatus(runs)
	if len(successes) > 0 {
		fmt.Fprintf(&b, "## Similar Past Successful Runs (%d relevant)\n", len(successes))
		for _, r := range successes {
			b.WriteString(r.Text)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(failures) > 0 {
		fmt.Fprintf(&b, "## Similar Past Failed Runs (%d relevant) — Patterns to Avoid\n", len(failures))
		for _, r := range failures {
			b.WriteString(r.Text)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Rules\n")
	b.WriteString("- Validate all changes against the domain context above\n")
	b.WriteString("- Follow existing Architecture Decision Records (ADRs)\n")
	b.WriteString("- Never hallucinate structures not found in the codebase\n")
	b.WriteString("- Every conclusion must reference evidence (file:line)\n")
	b.WriteString("- Consider failure scenarios and edge cases\n")
	b.WriteString("- Prioritize minimal, focused changes\n\n")

	b.WriteString("## Task\n")
	b.WriteString(prompt)

	return b.String(), nil
}

func filterByKind(items []Item, kind ItemKind) []Item {
	out := items[:0:0]
	for _, it := range items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// partitionRunsByStatus splits KindRun items into successes and
// failures based on Metadata["status"]. Items with no status (or any
// non-failure status) land in the success bucket — see Enrich for the
// reasoning. The well-known failure tokens come from
// store.StatusFailure / StatusCancelled; we match by string so memory
// stays decoupled from the store package.
func partitionRunsByStatus(items []Item) (successes, failures []Item) {
	for _, it := range items {
		status, _ := it.Metadata["status"].(string)
		switch status {
		case "failure", "cancelled":
			failures = append(failures, it)
		default:
			successes = append(successes, it)
		}
	}
	return successes, failures
}
