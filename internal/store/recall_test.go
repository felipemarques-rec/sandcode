package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store_test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRecallSimilarRanksByBM25(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateRun(t, s, Run{ID: "shallow", Agent: "a", Sandbox: "ns",
		Prompt: "kubernetes networking is a giant pain",
		Status: StatusSuccess, StartedAt: time.Now(),
	})
	mustCreateRun(t, s, Run{ID: "deep", Agent: "a", Sandbox: "ns",
		Prompt: "table driven tests in golang give better coverage than ad-hoc tests",
		Status: StatusSuccess, StartedAt: time.Now(),
	})

	got, err := s.RecallSimilar(ctx, "table driven tests golang", 5)
	if err != nil {
		t.Fatalf("RecallSimilar: %v", err)
	}
	if len(got) == 0 || got[0].ID != "deep" {
		t.Fatalf("expected 'deep' first, got: %v", runIDs(got))
	}
}

func TestRecallSimilarEmptyPromptReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	mustCreateRun(t, s, Run{ID: "r1", Agent: "a", Sandbox: "ns",
		Prompt: "anything", Status: StatusSuccess, StartedAt: time.Now(),
	})
	got, err := s.RecallSimilar(context.Background(), "the and for", 5)
	if err != nil {
		t.Fatalf("RecallSimilar: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no recall for stopword-only prompt, got %v", runIDs(got))
	}
}

func TestRecallSimilarLegacyDatabaseBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_store.db")

	// Pre-create the runs table WITHOUT the FTS5 sidecar so the
	// Open()-time migration has work to do.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY, parent_id TEXT NOT NULL DEFAULT '',
			agent TEXT NOT NULL, sandbox TEXT NOT NULL, prompt TEXT NOT NULL,
			cwd TEXT NOT NULL, strategy TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL, started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL DEFAULT 0,
			exit_code INTEGER NOT NULL DEFAULT 0,
			diff_path TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO runs(id, agent, sandbox, prompt, cwd, status, started_at)
		VALUES ('legacy-1', 'a', 'ns', 'legacy run about postgres tuning', '/r', 'success', ?);`,
		time.Now().UnixNano()); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	got, err := s.RecallSimilar(context.Background(), "postgres tuning", 5)
	if err != nil {
		t.Fatalf("RecallSimilar: %v", err)
	}
	if len(got) != 1 || got[0].ID != "legacy-1" {
		t.Errorf("legacy run not recalled via FTS5: %v", runIDs(got))
	}
}

func mustCreateRun(t *testing.T, s *SQLite, r Run) {
	t.Helper()
	if err := s.CreateRun(context.Background(), r); err != nil {
		t.Fatalf("CreateRun(%s): %v", r.ID, err)
	}
}

func runIDs(rs []Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
