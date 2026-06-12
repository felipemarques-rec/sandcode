package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/memory"
	"github.com/felipemarques-rec/sandcode/internal/redact"
	_ "modernc.org/sqlite"
)

// storeSchemaVersion is the current PRAGMA user_version expected by
// the store. Bump on every schema change that needs a one-time data
// fixup; Open() runs the right migration when an older DB is opened.
//
//	0 → 1 : runs_fts virtual table introduced. Legacy databases must
//	         rebuild the FTS5 index from existing runs rows so episodic
//	         recall sees historical prompts.
const storeSchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,
    parent_id   TEXT NOT NULL DEFAULT '',
    agent       TEXT NOT NULL,
    sandbox     TEXT NOT NULL,
    prompt      TEXT NOT NULL,
    cwd         TEXT NOT NULL,
    strategy    TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0,
    exit_code   INTEGER NOT NULL DEFAULT 0,
    diff_path   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_parent  ON runs(parent_id);
CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);

CREATE TABLE IF NOT EXISTS events (
    run_id  TEXT NOT NULL,
    seq     INTEGER NOT NULL,
    ts      INTEGER NOT NULL,
    kind    TEXT NOT NULL,
    payload TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id, seq);

CREATE TABLE IF NOT EXISTS rankings (
    run_id        TEXT PRIMARY KEY,
    judge         TEXT NOT NULL,
    winner_run_id TEXT NOT NULL,
    scores        TEXT NOT NULL,
    rationale     TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

-- External-content FTS5 index over runs.prompt. The episodic memory
-- tier queries this to surface past runs that overlap keywords with
-- the current prompt. Triggers keep it in sync with INSERT / UPDATE /
-- DELETE on the parent runs table.
CREATE VIRTUAL TABLE IF NOT EXISTS runs_fts USING fts5(
    prompt,
    content='runs',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS runs_ai AFTER INSERT ON runs BEGIN
    INSERT INTO runs_fts(rowid, prompt) VALUES (new.rowid, new.prompt);
END;
CREATE TRIGGER IF NOT EXISTS runs_ad AFTER DELETE ON runs BEGIN
    INSERT INTO runs_fts(runs_fts, rowid, prompt) VALUES ('delete', old.rowid, old.prompt);
END;
CREATE TRIGGER IF NOT EXISTS runs_au AFTER UPDATE ON runs BEGIN
    INSERT INTO runs_fts(runs_fts, rowid, prompt) VALUES ('delete', old.rowid, old.prompt);
    INSERT INTO runs_fts(rowid, prompt) VALUES (new.rowid, new.prompt);
END;
`

// SQLite is a Store backed by a local SQLite file. Built on the pure-Go
// modernc.org/sqlite driver so the resulting binary requires no CGO.
type SQLite struct {
	db   *sql.DB
	path string
}

// Open opens (and migrates) the SQLite database at path. Parent dirs are
// created as needed.
func Open(path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Version-pinned migrations (PRAGMA user_version). Future schema
	// changes append to the if-ladder; the database is at the current
	// version once Open returns.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store user_version read: %w", err)
	}
	if ver < 1 {
		// Backfill runs_fts so historical run prompts become searchable
		// by episodic recall. Cheap on fresh DBs; bounded otherwise.
		if _, err := db.Exec(`INSERT INTO runs_fts(runs_fts) VALUES('rebuild')`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store runs_fts rebuild: %w", err)
		}
	}
	if ver < storeSchemaVersion {
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, storeSchemaVersion)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store user_version write: %w", err)
		}
	}
	return &SQLite{db: db, path: path}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) CreateRun(ctx context.Context, r Run) error {
	// Redact secrets before they hit the runs table and the FTS index (which
	// is otherwise echoed back into future prompts via episodic recall).
	r.Prompt = redact.Redact(r.Prompt)
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO runs(id, parent_id, agent, sandbox, prompt, cwd, strategy, status, started_at, finished_at, exit_code, diff_path)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ParentID, r.Agent, r.Sandbox, r.Prompt, r.CWD, r.Strategy,
		string(r.Status), r.StartedAt.UnixNano(), r.FinishedAt.UnixNano(),
		r.ExitCode, r.DiffPath,
	)
	return err
}

func (s *SQLite) UpdateRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE runs SET status=?, finished_at=?, exit_code=?, diff_path=? WHERE id=?`,
		string(r.Status), r.FinishedAt.UnixNano(), r.ExitCode, r.DiffPath, r.ID,
	)
	return err
}

func (s *SQLite) AppendEvent(ctx context.Context, runID string, e Event) error {
	// Defense-in-depth: redact secrets from every persisted event payload
	// (text/tool_call/raw streams may carry keys the agent printed).
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO events(run_id, seq, ts, kind, payload) VALUES (?, ?, ?, ?, ?)`,
		runID, e.Seq, e.Timestamp.UnixNano(), e.Kind, redact.Redact(e.Payload),
	)
	return err
}

func (s *SQLite) GetRun(ctx context.Context, runID string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, parent_id, agent, sandbox, prompt, cwd, strategy, status, started_at, finished_at, exit_code, diff_path
        FROM runs WHERE id=?`, runID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("run %q: %w", runID, ErrRunNotFound)
	}
	return r, err
}

func (s *SQLite) ListRuns(ctx context.Context, f ListFilter) ([]Run, error) {
	q := `SELECT id, parent_id, agent, sandbox, prompt, cwd, strategy, status, started_at, finished_at, exit_code, diff_path FROM runs`
	var args []interface{}
	var clauses []string
	if f.ParentID != "*" {
		clauses = append(clauses, "parent_id = ?")
		args = append(args, f.ParentID)
	}
	if f.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.Agent != "" {
		clauses = append(clauses, "agent = ?")
		args = append(args, f.Agent)
	}
	if len(clauses) > 0 {
		q += " WHERE "
		for i, c := range clauses {
			if i > 0 {
				q += " AND "
			}
			q += c
		}
	}
	q += " ORDER BY started_at DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLite) ListEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT seq, ts, kind, payload FROM events WHERE run_id=? ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var ts int64
		if err := rows.Scan(&ev.Seq, &ts, &ev.Kind, &ev.Payload); err != nil {
			return nil, err
		}
		ev.Timestamp = time.Unix(0, ts)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *SQLite) SaveRanking(ctx context.Context, r Ranking) error {
	scoresJSON, err := json.Marshal(r.Scores)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO rankings(run_id, judge, winner_run_id, scores, rationale, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(run_id) DO UPDATE SET
            judge=excluded.judge,
            winner_run_id=excluded.winner_run_id,
            scores=excluded.scores,
            rationale=excluded.rationale,
            created_at=excluded.created_at`,
		r.ParentRunID, r.Judge, r.WinnerRunID, string(scoresJSON), r.Rationale,
		r.CreatedAt.UnixNano(),
	)
	return err
}

func (s *SQLite) GetRanking(ctx context.Context, parentRunID string) (Ranking, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT run_id, judge, winner_run_id, scores, rationale, created_at
        FROM rankings WHERE run_id=?`, parentRunID)
	var r Ranking
	var scoresJSON string
	var createdAt int64
	if err := row.Scan(&r.ParentRunID, &r.Judge, &r.WinnerRunID, &scoresJSON, &r.Rationale, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Ranking{}, fmt.Errorf("ranking for %q not found", parentRunID)
		}
		return Ranking{}, err
	}
	if err := json.Unmarshal([]byte(scoresJSON), &r.Scores); err != nil {
		return Ranking{}, err
	}
	r.CreatedAt = time.Unix(0, createdAt)
	return r, nil
}

// RecallSimilar returns past Runs whose prompts share keywords with
// the query, ranked by BM25 (most relevant first). Empty results are
// returned as an empty slice with nil error: a fresh project naturally
// has no episodic history.
//
// FTS5 quirk: MATCH's LHS must be the *table name*, not an alias.
func (s *SQLite) RecallSimilar(ctx context.Context, prompt string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 5
	}
	keywords := memory.ExtractKeywords(prompt)
	match := memory.BuildFTS5Match(keywords)
	if match == "" {
		return nil, nil
	}

	const q = `
		SELECT r.id, r.parent_id, r.agent, r.sandbox, r.prompt, r.cwd,
		       r.strategy, r.status, r.started_at, r.finished_at, r.exit_code, r.diff_path
		FROM runs_fts
		JOIN runs r ON r.rowid = runs_fts.rowid
		WHERE runs_fts MATCH ?
		ORDER BY bm25(runs_fts), r.started_at DESC
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, q, match, limit)
	if err != nil {
		return nil, nil // best-effort: empty results, no error path for the caller
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scannable abstracts *sql.Row and *sql.Rows for the shared scan helper.
type scannable interface {
	Scan(dest ...interface{}) error
}

func scanRun(row scannable) (Run, error) {
	var r Run
	var status string
	var started, finished int64
	err := row.Scan(&r.ID, &r.ParentID, &r.Agent, &r.Sandbox, &r.Prompt, &r.CWD,
		&r.Strategy, &status, &started, &finished, &r.ExitCode, &r.DiffPath)
	if err != nil {
		return Run{}, err
	}
	r.Status = RunStatus(status)
	r.StartedAt = time.Unix(0, started)
	if finished > 0 {
		r.FinishedAt = time.Unix(0, finished)
	}
	return r, nil
}
