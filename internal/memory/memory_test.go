package memory

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeTier is the in-test Tier implementation that lets us drive
// arbitrator behaviour without spinning up sqlite.
type fakeTier struct {
	name  string
	items []Item
	err   error
	calls int
}

func (t *fakeTier) Name() string { return t.name }
func (t *fakeTier) Recall(_ context.Context, _ string, limit int) ([]Item, error) {
	t.calls++
	if t.err != nil {
		return nil, t.err
	}
	if limit < len(t.items) {
		return t.items[:limit], nil
	}
	return t.items, nil
}

func makeItem(kind ItemKind, text string) Item {
	return Item{Kind: kind, Text: text, Source: string(kind)}
}

func TestArbitratorEvenBudgetSplit(t *testing.T) {
	a := &fakeTier{name: "a", items: []Item{
		makeItem(KindLesson, "L1"), makeItem(KindLesson, "L2"),
		makeItem(KindLesson, "L3"), makeItem(KindLesson, "L4"),
	}}
	b := &fakeTier{name: "b", items: []Item{
		makeItem(KindRun, "R1"), makeItem(KindRun, "R2"),
	}}
	arb := NewArbitrator(a, b)

	got, err := arb.Recall(context.Background(), "anything", 4)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// (4+2-1)/2 = 2 per tier
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("expected one call per tier, got a=%d b=%d", a.calls, b.calls)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 items, got %d", len(got))
	}
	wantOrder := []string{"L1", "L2", "R1", "R2"}
	for i, it := range got {
		if it.Text != wantOrder[i] {
			t.Errorf("got[%d].Text = %q, want %q", i, it.Text, wantOrder[i])
		}
	}
}

func TestArbitratorFiltersNilTiers(t *testing.T) {
	a := &fakeTier{name: "a", items: []Item{makeItem(KindLesson, "L1")}}
	arb := NewArbitrator(a, nil, nil)
	if len(arb.Tiers()) != 1 {
		t.Errorf("expected 1 live tier, got %d", len(arb.Tiers()))
	}
	got, _ := arb.Recall(context.Background(), "p", 1)
	if len(got) != 1 {
		t.Errorf("got %d items, want 1", len(got))
	}
}

func TestArbitratorPartialFailureNonFatal(t *testing.T) {
	a := &fakeTier{name: "a", items: []Item{makeItem(KindLesson, "L1")}}
	b := &fakeTier{name: "b", err: errors.New("boom")}
	arb := NewArbitrator(a, b)

	got, err := arb.Recall(context.Background(), "p", 4)
	if err != nil {
		t.Fatalf("expected nil error on partial failure, got %v", err)
	}
	if len(got) != 1 || got[0].Text != "L1" {
		t.Errorf("got %v, want [L1]", textsOf(got))
	}
}

func TestArbitratorAllTiersFailingReturnsError(t *testing.T) {
	a := &fakeTier{name: "a", err: errors.New("a-down")}
	b := &fakeTier{name: "b", err: errors.New("b-down")}
	arb := NewArbitrator(a, b)

	_, err := arb.Recall(context.Background(), "p", 4)
	if err == nil {
		t.Fatal("expected joined error when every tier fails")
	}
	if !strings.Contains(err.Error(), "a-down") || !strings.Contains(err.Error(), "b-down") {
		t.Errorf("expected both errors joined, got %v", err)
	}
}

func TestArbitratorEmptyArbitrator(t *testing.T) {
	arb := NewArbitrator()
	got, err := arb.Recall(context.Background(), "p", 4)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("expected nil items, got %v", got)
	}
}

func TestExtractKeywords(t *testing.T) {
	got := ExtractKeywords("the BIG ANT and; quick! tests for the (rabbits)?")
	want := []string{"quick", "tests", "rabbits"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildFTS5MatchEscapesQuotes(t *testing.T) {
	got := BuildFTS5Match([]string{`foo"bar`, "baz"})
	want := `"foobar" OR "baz"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildFTS5MatchEmpty(t *testing.T) {
	if got := BuildFTS5Match(nil); got != "" {
		t.Errorf("BuildFTS5Match(nil) = %q, want empty", got)
	}
}

// stubClassifier returns a fixed Classification for tests.
type stubClassifier struct{ c Classification }

func (s stubClassifier) Classify(context.Context, string) Classification { return s.c }

func TestEnricherIncludesAllSections(t *testing.T) {
	lessonTier := &fakeTier{
		name: "lessons",
		items: []Item{
			{Kind: KindLesson, Text: "✅ SKILL: use table tests (confidence: 0.85)"},
		},
	}
	runTier := &fakeTier{
		name: "episodic",
		items: []Item{
			{
				Kind:     KindRun,
				Text:     "✅ [claude] SUCCESS (exit=0, 2s) — write hello.txt",
				Metadata: map[string]any{"status": "success"},
			},
		},
	}
	arb := NewArbitrator(lessonTier, runTier)

	e := NewEnricher(arb,
		WithClassifier(stubClassifier{Classification{Type: "convergent", Complexity: "low"}}),
		WithDocs(func(string) string { return "README excerpt here." }),
		WithRecallLimit(4),
	)

	out, err := e.Enrich(context.Background(), "do the thing", "/cwd")
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	for _, want := range []string{
		"## Identity",
		"## Problem Classification: convergent (complexity: low)",
		"## Domain Context",
		"README excerpt here.",
		"## Learned Lessons (1 relevant)",
		"✅ SKILL: use table tests (confidence: 0.85)",
		"## Similar Past Successful Runs (1 relevant)",
		"✅ [claude] SUCCESS (exit=0, 2s) — write hello.txt",
		"## Rules",
		"## Task\ndo the thing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Patterns to Avoid") {
		t.Errorf("failure section appeared with no failed runs:\n%s", out)
	}
}

func TestEnricherSplitsRunsByStatus(t *testing.T) {
	runTier := &fakeTier{
		name: "episodic",
		items: []Item{
			{Kind: KindRun, Text: "✅ ok-1", Metadata: map[string]any{"status": "success"}},
			{Kind: KindRun, Text: "❌ fail-1", Metadata: map[string]any{"status": "failure"}},
			{Kind: KindRun, Text: "⏸ cancel-1", Metadata: map[string]any{"status": "cancelled"}},
			{Kind: KindRun, Text: "✅ ok-2", Metadata: map[string]any{"status": "success"}},
		},
	}
	arb := NewArbitrator(runTier)
	e := NewEnricher(arb)

	out, err := e.Enrich(context.Background(), "do thing", "/cwd")
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !strings.Contains(out, "## Similar Past Successful Runs (2 relevant)") {
		t.Errorf("missing success header:\n%s", out)
	}
	if !strings.Contains(out, "## Similar Past Failed Runs (2 relevant) — Patterns to Avoid") {
		t.Errorf("missing failure header (cancelled should land here):\n%s", out)
	}
	// Order within each section should follow tier order (success items
	// appear before failures because partitionRunsByStatus preserves
	// per-status insertion order).
	successHeader := strings.Index(out, "Successful Runs")
	failureHeader := strings.Index(out, "Failed Runs")
	if successHeader >= failureHeader {
		t.Errorf("success section should come before failure section")
	}
}

func TestEnricherFailureOnlyFixture(t *testing.T) {
	runTier := &fakeTier{
		name: "episodic",
		items: []Item{
			{Kind: KindRun, Text: "❌ only-fail", Metadata: map[string]any{"status": "failure"}},
		},
	}
	e := NewEnricher(NewArbitrator(runTier))
	out, _ := e.Enrich(context.Background(), "do thing", "/cwd")
	if strings.Contains(out, "Successful Runs") {
		t.Errorf("success section should be absent:\n%s", out)
	}
	if !strings.Contains(out, "Failed Runs (1 relevant) — Patterns to Avoid") {
		t.Errorf("failure section missing:\n%s", out)
	}
}

func TestEnricherUnstatusedRunsTreatedAsSuccess(t *testing.T) {
	runTier := &fakeTier{
		name: "episodic",
		items: []Item{
			{Kind: KindRun, Text: "neutral"}, // no metadata at all
		},
	}
	e := NewEnricher(NewArbitrator(runTier))
	out, _ := e.Enrich(context.Background(), "do thing", "/cwd")
	if !strings.Contains(out, "Successful Runs (1 relevant)") {
		t.Errorf("unstatused run should land in success bucket:\n%s", out)
	}
}

func TestEnricherSkipsEmptySections(t *testing.T) {
	arb := NewArbitrator() // no tiers
	e := NewEnricher(arb)

	out, err := e.Enrich(context.Background(), "do the thing", "/cwd")
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	for _, banned := range []string{
		"## Problem Classification",
		"## Domain Context",
		"## Learned Lessons",
		"## Similar Past Runs",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("output should not include %q (no provider injected)\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "## Identity") || !strings.Contains(out, "## Task\ndo the thing") {
		t.Errorf("output missing always-on sections:\n%s", out)
	}
}

func textsOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}
