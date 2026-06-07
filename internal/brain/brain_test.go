package brain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestBrain(t *testing.T) *SQLiteBrain {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain_test.db")
	b, err := OpenBrain(path)
	if err != nil {
		t.Fatalf("OpenBrain: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestStoreLessonAndRecall(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// Store a skill lesson
	lesson := Lesson{
		ID:         "test-001",
		RunID:      "run-abc",
		Category:   CategorySkill,
		Tags:       []string{"golang", "testing"},
		Content:    "Using table-driven tests in golang improves coverage",
		Evidence:   "agent produced 95% coverage with table tests",
		Confidence: 0.85,
	}
	if err := b.Store(ctx, lesson); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Recall by keyword
	lessons, err := b.Recall(ctx, "table driven tests golang", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(lessons) == 0 {
		t.Fatal("expected at least 1 recalled lesson")
	}
	if lessons[0].ID != "test-001" {
		t.Errorf("expected lesson test-001, got %s", lessons[0].ID)
	}

	// used_count bump is applied after scan, verify via fresh query
	updated, err := b.ListLessons(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(updated) == 0 || updated[0].UsedCount != 1 {
		t.Errorf("expected used_count=1 after recall, got %d", updated[0].UsedCount)
	}
}

func TestStoreAndListByCategory(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	for _, l := range []Lesson{
		{ID: "s1", RunID: "r1", Category: CategorySkill, Content: "skill one", Confidence: 0.9},
		{ID: "s2", RunID: "r2", Category: CategorySkill, Content: "skill two", Confidence: 0.7},
		{ID: "a1", RunID: "r3", Category: CategoryAntiPattern, Content: "bad pattern", Confidence: 0.6},
	} {
		if err := b.Store(ctx, l); err != nil {
			t.Fatalf("Store %s: %v", l.ID, err)
		}
	}

	skills, err := b.ListLessons(ctx, CategorySkill, 10)
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(skills))
	}

	all, err := b.ListLessons(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListLessons all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 total, got %d", len(all))
	}
}

func TestInvalidateLesson(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	_ = b.Store(ctx, Lesson{ID: "inv1", RunID: "r1", Category: CategorySkill, Content: "soon invalid", Confidence: 0.5})

	if err := b.Invalidate(ctx, "inv1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	// Invalidated lessons should not appear in list
	lessons, err := b.ListLessons(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(lessons) != 0 {
		t.Errorf("expected 0 valid lessons after invalidation, got %d", len(lessons))
	}
}

func TestPruneLowConfidence(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	old := time.Now().Add(-48 * time.Hour)
	_ = b.Store(ctx, Lesson{
		ID: "old1", RunID: "r1", Category: CategorySkill,
		Content: "old low conf", Confidence: 0.2, CreatedAt: old,
	})
	_ = b.Store(ctx, Lesson{
		ID: "new1", RunID: "r2", Category: CategorySkill,
		Content: "new high conf", Confidence: 0.9,
	})

	n, err := b.Prune(ctx, 24*time.Hour, 0.3)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 pruned, got %d", n)
	}

	remaining, _ := b.ListLessons(ctx, "", 10)
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(remaining))
	}
}

func TestStats(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	_ = b.Store(ctx, Lesson{ID: "s1", RunID: "r1", Category: CategorySkill, Content: "a", Confidence: 0.8})
	_ = b.Store(ctx, Lesson{ID: "a1", RunID: "r2", Category: CategoryAntiPattern, Content: "b", Confidence: 0.6})
	_ = b.Store(ctx, Lesson{ID: "p1", RunID: "r3", Category: CategoryPreference, Content: "c", Confidence: 0.7})

	s, err := b.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.TotalLessons != 3 {
		t.Errorf("expected 3 total, got %d", s.TotalLessons)
	}
	if s.Skills != 1 {
		t.Errorf("expected 1 skill, got %d", s.Skills)
	}
	if s.AntiPatterns != 1 {
		t.Errorf("expected 1 antipattern, got %d", s.AntiPatterns)
	}
}

func TestLearnFromOutcome(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	outcome := Outcome{
		RunID:    "run-learn",
		Agent:    "claude-code",
		Prompt:   "fix authentication bug",
		Diff:     "+func auth() {}\n-func oldAuth() {}",
		Status:   "success",
		ExitCode: 0,
		Duration: 15 * time.Second,
		Score:    0.92,
	}

	n, err := b.Learn(ctx, outcome)
	if err != nil {
		t.Fatalf("Learn: %v", err)
	}
	if n == 0 {
		t.Error("expected at least 1 lesson from successful outcome")
	}

	lessons, _ := b.ListLessons(ctx, "", 20)
	if len(lessons) == 0 {
		t.Fatal("no lessons found after Learn")
	}
	t.Logf("Extracted %d lessons from outcome", len(lessons))
	for _, l := range lessons {
		t.Logf("  [%s] %s (conf=%.2f)", l.Category, l.Content, l.Confidence)
	}
}

func TestClassifier(t *testing.T) {
	c := NewClassifier()
	ctx := context.Background()

	tests := []struct {
		prompt   string
		wantType ProblemType
	}{
		{"fix the login bug", Convergent},
		{"add a logout button", Convergent},
		{"design the authentication architecture comparing OAuth vs SAML", Divergent},
		{"explore alternative approaches for caching strategy", Divergent},
	}

	for _, tt := range tests {
		cl := c.Classify(ctx, tt.prompt)
		if cl.Type != tt.wantType {
			t.Errorf("Classify(%q) = %s, want %s", tt.prompt, cl.Type, tt.wantType)
		}
	}
}

func TestEnricher(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// Pre-store a lesson
	_ = b.Store(ctx, Lesson{
		ID: "e1", RunID: "r1", Category: CategorySkill,
		Tags: []string{"authentication"}, Content: "Use JWT for auth", Confidence: 0.9,
	})

	// Create temp dir with CONTEXT.md
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "CONTEXT.md"), []byte("# Auth Service\nUses JWT tokens"), 0o644)

	enriched, err := b.Enrich(ctx, "fix the authentication middleware", dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	// Verify enriched prompt contains key components
	checks := []string{
		"Staff/Principal Engineer",
		"CONTEXT.md",
		"Auth Service",
		"authentication",
	}
	for _, check := range checks {
		if !containsStr(enriched, check) {
			t.Errorf("enriched prompt missing %q", check)
		}
	}
	t.Logf("Enriched prompt length: %d chars", len(enriched))
}

func containsStr(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (len(s) >= len(substr)) && (s == substr || len(s) > len(substr) && contains(s, substr))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
