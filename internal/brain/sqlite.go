package brain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/memory"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const brainSchema = `
CREATE TABLE IF NOT EXISTS lessons (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    category    TEXT NOT NULL,
    tags        TEXT NOT NULL DEFAULT '[]',
    content     TEXT NOT NULL,
    evidence    TEXT NOT NULL DEFAULT '',
    confidence  REAL NOT NULL DEFAULT 0.5,
    used_count  INTEGER NOT NULL DEFAULT 0,
    last_used   INTEGER,
    valid_from  INTEGER NOT NULL,
    valid_to    INTEGER,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_lessons_category   ON lessons(category);
CREATE INDEX IF NOT EXISTS idx_lessons_confidence  ON lessons(confidence DESC);
CREATE INDEX IF NOT EXISTS idx_lessons_valid       ON lessons(valid_from, valid_to);
CREATE INDEX IF NOT EXISTS idx_lessons_run         ON lessons(run_id);

-- External-content FTS5 index over lessons.content and lessons.tags.
-- Triggers below keep it in sync with INSERT / UPDATE / DELETE on the
-- parent table; legacy databases without the index are repaired on
-- Open() via a self-healing rebuild (see OpenBrain).
CREATE VIRTUAL TABLE IF NOT EXISTS lessons_fts USING fts5(
    content,
    tags,
    content='lessons',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS lessons_ai AFTER INSERT ON lessons BEGIN
    INSERT INTO lessons_fts(rowid, content, tags)
    VALUES (new.rowid, new.content, new.tags);
END;
CREATE TRIGGER IF NOT EXISTS lessons_ad AFTER DELETE ON lessons BEGIN
    INSERT INTO lessons_fts(lessons_fts, rowid, content, tags)
    VALUES ('delete', old.rowid, old.content, old.tags);
END;
CREATE TRIGGER IF NOT EXISTS lessons_au AFTER UPDATE ON lessons BEGIN
    INSERT INTO lessons_fts(lessons_fts, rowid, content, tags)
    VALUES ('delete', old.rowid, old.content, old.tags);
    INSERT INTO lessons_fts(rowid, content, tags)
    VALUES (new.rowid, new.content, new.tags);
END;
`

// brainSchemaVersion is the current value of PRAGMA user_version on a
// fully-migrated brain database. Bump it whenever the schema changes
// in a way that needs a one-time data migration; OpenBrain runs the
// appropriate fixups when an older DB is opened.
//
//	0 → 1 : lessons_fts virtual table introduced. Rebuild index for
//	         databases created before this version, since their
//	         existing `lessons` rows were never seen by the AFTER
//	         INSERT trigger and BM25 has nothing to rank against.
const brainSchemaVersion = 1

// SQLiteBrain is the Brain implementation backed by a local SQLite database.
// It uses the same pure-Go sqlite driver as the store for zero-CGO portability.
type SQLiteBrain struct {
	db       *sql.DB
	path     string
	episodic memory.Tier // optional; when set, Enrich blends episodic recall
}

// OpenBrain opens (or creates) a brain database at the given path.
func OpenBrain(path string) (*SQLiteBrain, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(brainSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("brain schema: %w", err)
	}
	// Version-pinned migrations. PRAGMA user_version is the SQLite-
	// idiomatic schema tracker — survives across opens, requires no
	// extra table. We compare it against brainSchemaVersion and run
	// the gap fixups in order. After OpenBrain returns the DB is
	// guaranteed to be at the current version.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("brain user_version read: %w", err)
	}
	if ver < 1 {
		// Backfill the FTS5 index — needed for any lessons inserted
		// before the table+triggers existed. Cheap on a fresh DB.
		if _, err := db.Exec(`INSERT INTO lessons_fts(lessons_fts) VALUES('rebuild')`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("brain fts5 rebuild: %w", err)
		}
	}
	if ver < brainSchemaVersion {
		// PRAGMA user_version doesn't accept bound parameters; the
		// value is a hard-coded constant so injection isn't a concern.
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, brainSchemaVersion)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("brain user_version write: %w", err)
		}
	}
	return &SQLiteBrain{db: db, path: path}, nil
}

func (b *SQLiteBrain) Close() error { return b.db.Close() }

// Learn extracts lessons from a completed run outcome and stores them.
func (b *SQLiteBrain) Learn(ctx context.Context, outcome Outcome) (int, error) {
	extractor := NewExtractor()
	lessons, err := extractor.Extract(ctx, outcome)
	if err != nil {
		return 0, fmt.Errorf("extract: %w", err)
	}
	stored := 0
	for _, l := range lessons {
		l.ValidFrom = time.Now()
		if err := b.Store(ctx, l); err != nil {
			continue // best-effort per lesson
		}
		stored++
	}
	return stored, nil
}

// Enrich builds an enriched prompt using the memory.Enricher,
// composing this Brain's lesson tier with any episodic tier wired via
// WithEpisodic. The output structure is unchanged from Stage 2 when
// the episodic tier is absent.
func (b *SQLiteBrain) Enrich(ctx context.Context, prompt string, cwd string) (string, error) {
	arb := memory.NewArbitrator(b.AsTier(), b.episodic)
	enricher := memory.NewEnricher(arb,
		memory.WithDocs(ScanProjectDocs),
		memory.WithClassifier(classifierAdapter{NewClassifier()}),
	)
	return enricher.Enrich(ctx, prompt, cwd)
}

// WithEpisodic wires a Tier to source episodic-memory items from
// (typically `runStore.AsTier()`). Optional — when not set, Enrich
// only surfaces lessons. Returns b for chaining at construction.
func (b *SQLiteBrain) WithEpisodic(t memory.Tier) *SQLiteBrain {
	b.episodic = t
	return b
}

// classifierAdapter bridges brain.Classifier (which owns a richer
// internal struct) to memory.Classifier (the narrow interface the
// memory-package Enricher consumes).
type classifierAdapter struct{ inner *Classifier }

func (a classifierAdapter) Classify(ctx context.Context, prompt string) memory.Classification {
	c := a.inner.Classify(ctx, prompt)
	return memory.Classification{
		Type:       string(c.Type),
		Complexity: string(c.Complexity),
	}
}

func (b *SQLiteBrain) Store(ctx context.Context, l Lesson) error {
	if l.ID == "" {
		l.ID = uuid.New().String()[:12]
	}
	tagsJSON, err := json.Marshal(l.Tags)
	if err != nil {
		return err
	}
	now := time.Now()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = now
	}
	if l.ValidFrom.IsZero() {
		l.ValidFrom = now
	}
	l.UpdatedAt = now

	var lastUsed *int64
	if !l.LastUsed.IsZero() {
		v := l.LastUsed.UnixNano()
		lastUsed = &v
	}
	var validTo *int64
	if l.ValidTo != nil {
		v := l.ValidTo.UnixNano()
		validTo = &v
	}

	_, err = b.db.ExecContext(ctx, `
		INSERT INTO lessons(id, run_id, category, tags, content, evidence, confidence,
		                    used_count, last_used, valid_from, valid_to, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			content=excluded.content,
			evidence=excluded.evidence,
			confidence=excluded.confidence,
			used_count=excluded.used_count,
			last_used=excluded.last_used,
			valid_to=excluded.valid_to,
			updated_at=excluded.updated_at`,
		l.ID, l.RunID, string(l.Category), string(tagsJSON), l.Content, l.Evidence,
		l.Confidence, l.UsedCount, lastUsed, l.ValidFrom.UnixNano(), validTo,
		l.CreatedAt.UnixNano(), l.UpdatedAt.UnixNano(),
	)
	return err
}

func (b *SQLiteBrain) Recall(ctx context.Context, prompt string, limit int) ([]Lesson, error) {
	if limit <= 0 {
		limit = 10
	}
	keywords := memory.ExtractKeywords(prompt)
	match := memory.BuildFTS5Match(keywords)
	if match == "" {
		return b.ListLessons(ctx, "", limit)
	}

	// FTS5 quirk: the MATCH operator's left-hand side must be the
	// virtual table's *name*, never an alias. The JOIN below alias-
	// references `lessons_fts` as `f` for column reads only.
	const q = `
		SELECT l.id, l.run_id, l.category, l.tags, l.content, l.evidence, l.confidence,
		       l.used_count, l.last_used, l.valid_from, l.valid_to, l.created_at, l.updated_at
		FROM lessons_fts
		JOIN lessons l ON l.rowid = lessons_fts.rowid
		WHERE l.valid_to IS NULL AND lessons_fts MATCH ?
		ORDER BY bm25(lessons_fts), l.confidence DESC, l.used_count DESC
		LIMIT ?`

	rows, err := b.db.QueryContext(ctx, q, match, limit)
	if err != nil {
		// FTS5 MATCH-parse failures (extremely unlikely with quoted
		// tokens, but defensive) shouldn't black-hole recall — fall
		// back to confidence-ordered ListLessons so the caller still
		// gets *something* useful.
		return b.ListLessons(ctx, "", limit)
	}
	defer rows.Close()

	lessons, err := scanLessons(rows)
	if err != nil {
		return nil, err
	}

	// Bump used_count for recalled lessons (best-effort). The UPDATE
	// trigger keeps the FTS5 index in sync but doesn't reindex content
	// — used_count isn't an indexed column.
	for _, l := range lessons {
		_, _ = b.db.ExecContext(ctx, `
			UPDATE lessons SET used_count = used_count + 1, last_used = ? WHERE id = ?`,
			time.Now().UnixNano(), l.ID)
	}
	return lessons, nil
}

func (b *SQLiteBrain) ListLessons(ctx context.Context, category Category, limit int) ([]Lesson, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, run_id, category, tags, content, evidence, confidence,
	             used_count, last_used, valid_from, valid_to, created_at, updated_at
	      FROM lessons WHERE valid_to IS NULL`
	var args []interface{}
	if category != "" {
		q += " AND category = ?"
		args = append(args, string(category))
	}
	q += " ORDER BY confidence DESC, created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLessons(rows)
}

func (b *SQLiteBrain) Invalidate(ctx context.Context, lessonID string) error {
	_, err := b.db.ExecContext(ctx,
		`UPDATE lessons SET valid_to = ?, updated_at = ? WHERE id = ?`,
		time.Now().UnixNano(), time.Now().UnixNano(), lessonID)
	return err
}

func (b *SQLiteBrain) Prune(ctx context.Context, maxAge time.Duration, minConfidence float64) (int, error) {
	cutoff := time.Now().Add(-maxAge).UnixNano()
	res, err := b.db.ExecContext(ctx,
		`DELETE FROM lessons WHERE created_at < ? AND confidence < ?`,
		cutoff, minConfidence)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (b *SQLiteBrain) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	row := b.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN category='skill' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN category='antipattern' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN category='preference' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN category='principle' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(confidence), 0),
			COALESCE(MIN(created_at), 0),
			COALESCE(MAX(created_at), 0)
		FROM lessons WHERE valid_to IS NULL`)

	var oldest, newest int64
	if err := row.Scan(&s.TotalLessons, &s.Skills, &s.AntiPatterns, &s.Preferences,
		&s.Principles, &s.AvgConfidence, &oldest, &newest); err != nil {
		return Stats{}, err
	}
	if oldest > 0 {
		s.OldestLesson = time.Unix(0, oldest)
	}
	if newest > 0 {
		s.NewestLesson = time.Unix(0, newest)
	}
	return s, nil
}

func scanLessons(rows *sql.Rows) ([]Lesson, error) {
	var out []Lesson
	for rows.Next() {
		var l Lesson
		var tagsJSON string
		var catStr string
		var lastUsed, validFrom, validTo, createdAt, updatedAt *int64

		if err := rows.Scan(&l.ID, &l.RunID, &catStr, &tagsJSON,
			&l.Content, &l.Evidence, &l.Confidence,
			&l.UsedCount, &lastUsed, &validFrom, &validTo,
			&createdAt, &updatedAt); err != nil {
			return nil, err
		}
		l.Category = Category(catStr)
		_ = json.Unmarshal([]byte(tagsJSON), &l.Tags)
		if lastUsed != nil {
			l.LastUsed = time.Unix(0, *lastUsed)
		}
		if validFrom != nil {
			l.ValidFrom = time.Unix(0, *validFrom)
		}
		if validTo != nil {
			t := time.Unix(0, *validTo)
			l.ValidTo = &t
		}
		if createdAt != nil {
			l.CreatedAt = time.Unix(0, *createdAt)
		}
		if updatedAt != nil {
			l.UpdatedAt = time.Unix(0, *updatedAt)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
