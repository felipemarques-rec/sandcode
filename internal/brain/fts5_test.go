package brain

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestFTS5RanksMoreRelevantHigher verifies that BM25 actually promotes
// the lesson that overlaps more terms with the prompt above one that
// shares a single keyword. The old LIKE-OR query gave equal weight to
// both — this regression test locks in the new behaviour.
func TestFTS5RanksMoreRelevantHigher(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	mustStore(t, b, Lesson{
		ID:         "shallow",
		RunID:      "r1",
		Category:   CategorySkill,
		Content:    "kubernetes networking is a giant pain",
		Confidence: 0.9, // higher confidence — should still lose on relevance
	})
	mustStore(t, b, Lesson{
		ID:         "deep",
		RunID:      "r2",
		Category:   CategorySkill,
		Content:    "table driven tests in golang give better coverage than ad-hoc tests",
		Confidence: 0.5,
	})

	got, err := b.Recall(ctx, "table driven tests golang", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) == 0 || got[0].ID != "deep" {
		t.Fatalf("expected 'deep' first, got: %+v", idsOf(got))
	}
}

// TestFTS5SpecialCharsDoNotCrash exercises the path where prompt
// tokens contain FTS5-reserved characters (parentheses, asterisks,
// quotes). The recall layer must not return an error to the caller.
func TestFTS5SpecialCharsDoNotCrash(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	mustStore(t, b, Lesson{
		ID:         "safe",
		RunID:      "r1",
		Category:   CategorySkill,
		Content:    "always close the database handle when shutting down",
		Confidence: 0.7,
	})

	prompts := []string{
		`how do I close(*) the "database"?`,
		`function() with weird (chars) and *stars*`,
		`AND OR NOT NEAR — these would be reserved if uppercase`,
		``,
		`   `,
	}
	for _, p := range prompts {
		if _, err := b.Recall(ctx, p, 5); err != nil {
			t.Errorf("Recall(%q): %v", p, err)
		}
	}
}

// TestFTS5InvalidatedLessonsHidden ensures the valid_to filter survives
// the move from LIKE to MATCH.
func TestFTS5InvalidatedLessonsHidden(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	mustStore(t, b, Lesson{
		ID: "live", RunID: "r1", Category: CategorySkill,
		Content: "useful lesson about postgres indexing", Confidence: 0.6,
	})
	mustStore(t, b, Lesson{
		ID: "dead", RunID: "r2", Category: CategorySkill,
		Content: "obsolete postgres lesson", Confidence: 0.9,
	})
	if err := b.Invalidate(ctx, "dead"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	got, err := b.Recall(ctx, "postgres indexing", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, l := range got {
		if l.ID == "dead" {
			t.Errorf("invalidated lesson surfaced: %+v", l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 lesson, got %d (%v)", len(got), idsOf(got))
	}
}

// TestFTS5LegacyDatabaseBackfill simulates upgrading a DB that has
// `lessons` rows but no FTS5 index (because it was created before the
// schema change). OpenBrain must detect the gap and rebuild.
func TestFTS5LegacyDatabaseBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Hand-craft a legacy DB: lessons table without any FTS5 sidecar.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE lessons (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL, category TEXT NOT NULL,
			tags TEXT NOT NULL DEFAULT '[]', content TEXT NOT NULL,
			evidence TEXT NOT NULL DEFAULT '', confidence REAL NOT NULL DEFAULT 0.5,
			used_count INTEGER NOT NULL DEFAULT 0, last_used INTEGER,
			valid_from INTEGER NOT NULL, valid_to INTEGER,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		INSERT INTO lessons(id, run_id, category, content, valid_from, created_at, updated_at)
		VALUES ('legacy-1', 'r1', 'skill', 'legacy lesson about caching strategies',
		        ?, ?, ?);`,
		time.Now().UnixNano(), time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// OpenBrain must now (a) create lessons_fts, (b) notice it's empty
	// while lessons has rows, (c) rebuild the FTS index.
	b, err := OpenBrain(path)
	if err != nil {
		t.Fatalf("OpenBrain: %v", err)
	}
	defer b.Close()

	got, err := b.Recall(context.Background(), "caching strategies", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 1 || got[0].ID != "legacy-1" {
		t.Errorf("legacy row not recalled via FTS5: %v", idsOf(got))
	}
}

// TestFTS5EmptyPromptFallsBack confirms the no-keyword path still
// returns lessons via ListLessons (preserved Stage-2 behaviour).
func TestFTS5EmptyPromptFallsBack(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	mustStore(t, b, Lesson{
		ID: "x", RunID: "r1", Category: CategorySkill,
		Content: "anything", Confidence: 0.5,
	})

	got, err := b.Recall(ctx, "the and for", 10) // all stopwords
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected fallback to return all lessons, got %d", len(got))
	}
}

// TestFTS5UpdateKeepsIndexInSync ensures the AFTER UPDATE trigger
// re-indexes a row after content changes (so Recall finds the new
// keywords and no longer finds the old ones).
func TestFTS5UpdateKeepsIndexInSync(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	mustStore(t, b, Lesson{
		ID: "shifty", RunID: "r1", Category: CategorySkill,
		Content: "first version mentions raspberry pi", Confidence: 0.5,
	})

	// Overwrite via Store(ON CONFLICT) — content swaps.
	mustStore(t, b, Lesson{
		ID: "shifty", RunID: "r1", Category: CategorySkill,
		Content: "second version mentions arduino instead", Confidence: 0.5,
	})

	old, err := b.Recall(ctx, "raspberry", 10)
	if err != nil {
		t.Fatalf("Recall old: %v", err)
	}
	if len(old) != 0 {
		t.Errorf("expected zero hits for 'raspberry' after content swap, got %v", idsOf(old))
	}
	cur, err := b.Recall(ctx, "arduino", 10)
	if err != nil {
		t.Fatalf("Recall new: %v", err)
	}
	if len(cur) != 1 || cur[0].ID != "shifty" {
		t.Errorf("expected 'shifty' for 'arduino', got %v", idsOf(cur))
	}
}

func mustStore(t *testing.T, b *SQLiteBrain, l Lesson) {
	t.Helper()
	if err := b.Store(context.Background(), l); err != nil {
		t.Fatalf("Store(%s): %v", l.ID, err)
	}
}

func idsOf(ls []Lesson) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}
