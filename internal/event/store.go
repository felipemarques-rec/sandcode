package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the append-only event log abstraction. Implementations MUST
// guarantee that:
//
//   - Append never replaces or modifies prior rows (no UPSERT semantics).
//   - LoadRun returns events in the order they were appended for a run_id
//     (occurred_at ASC, ties broken by insertion order via rowid).
//   - LoadSince returns global events occurring at-or-after a wall-clock
//     instant — used by catch-up subscribers.
//
// The interface is small and surface-stable so swapping SQLite for NATS
// JetStream or another backend in Stage 4 does not break callers.
type Store interface {
	Append(ctx context.Context, ev Event) error
	LoadRun(ctx context.Context, runID string) ([]Event, error)
	LoadSince(ctx context.Context, t time.Time, limit int) ([]Event, error)
	Close() error
}

// ErrAppendOnly is returned by helpers that detect non-append mutations.
// The schema itself enforces this via the lack of an UPDATE method, but
// we surface the constraint at the type level for callers that want to
// audit at compile time.
var ErrAppendOnly = errors.New("event: store is append-only")

// SQLiteStore is the production Store backed by a local SQLite database.
// Built on modernc.org/sqlite (pure-Go, no CGO).
type SQLiteStore struct {
	db   *sql.DB
	path string
}

// schemaEventLog defines the event_log table. UPDATE/DELETE are not used
// anywhere in this package — the append-only invariant is enforced by the
// fact that no such methods exist on SQLiteStore. Compaction (future) is
// an external batch process, not a runtime operation.
const schemaEventLog = `
CREATE TABLE IF NOT EXISTS event_log (
    id              TEXT PRIMARY KEY,
    run_id          TEXT NOT NULL,
    parent_run_id   TEXT NOT NULL DEFAULT '',
    type            TEXT NOT NULL,
    payload         BLOB NOT NULL,
    occurred_at     INTEGER NOT NULL,
    correlation_id  TEXT NOT NULL DEFAULT '',
    causation_id    TEXT NOT NULL DEFAULT '',
    schema_version  INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_event_run  ON event_log(run_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_event_type ON event_log(type, occurred_at);
CREATE INDEX IF NOT EXISTS idx_event_corr ON event_log(correlation_id);
CREATE INDEX IF NOT EXISTS idx_event_time ON event_log(occurred_at);
`

// OpenStore opens (and migrates) an event log database at path.
// Parent dirs are created as needed. Uses WAL mode + 5s busy timeout to
// match the patterns set by internal/store.
func OpenStore(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("event store: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("event store: open: %w", err)
	}
	if _, err := db.Exec(schemaEventLog); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("event store: schema: %w", err)
	}
	return &SQLiteStore{db: db, path: path}, nil
}

// Close releases the database handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Append persists one event. Returns an error if the event's ID is
// already present — IDs are PRIMARY KEY, so silently overwriting an event
// would be a correctness bug. Callers must generate unique IDs (event.New
// uses UUIDv4 which is sufficient).
//
// Payload is stored as BLOB; if the caller passes nil, we store an empty
// byte slice so LoadRun returns deterministic empty payloads, not nil.
func (s *SQLiteStore) Append(ctx context.Context, ev Event) error {
	if ev.ID == "" {
		return fmt.Errorf("event store: Append requires a non-empty event ID")
	}
	if ev.RunID == "" {
		return fmt.Errorf("event store: Append requires a non-empty RunID")
	}
	if ev.Type == "" {
		return fmt.Errorf("event store: Append requires a non-empty Type")
	}
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	payload := ev.Payload
	if payload == nil {
		payload = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO event_log (id, run_id, parent_run_id, type, payload, occurred_at, correlation_id, causation_id, schema_version)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.RunID, ev.ParentRunID, string(ev.Type), payload,
		ts.UnixNano(), ev.CorrelationID, ev.CausationID, 1,
	)
	if err != nil {
		return fmt.Errorf("event store: append %s: %w", ev.Type, err)
	}
	return nil
}

// LoadRun returns every event for a run, ordered by occurred_at ASC
// (ties broken by row insertion order — events appended in the same
// nanosecond preserve their append order).
func (s *SQLiteStore) LoadRun(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, run_id, parent_run_id, type, payload, occurred_at, correlation_id, causation_id
        FROM event_log
        WHERE run_id = ?
        ORDER BY occurred_at ASC, rowid ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("event store: load run %s: %w", runID, err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// LoadSince returns events whose occurred_at is at-or-after t, ordered
// ASC. If limit > 0, results are capped. Used by catch-up subscribers
// resuming after a downtime window.
func (s *SQLiteStore) LoadSince(ctx context.Context, t time.Time, limit int) ([]Event, error) {
	q := `
        SELECT id, run_id, parent_run_id, type, payload, occurred_at, correlation_id, causation_id
        FROM event_log
        WHERE occurred_at >= ?
        ORDER BY occurred_at ASC, rowid ASC`
	args := []any{t.UnixNano()}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("event store: load since %s: %w", t, err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Count returns the total row count — useful for tests and metrics.
func (s *SQLiteStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log`).Scan(&n)
	return n, err
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var ev Event
		var typ string
		var occurredAt int64
		var payload []byte
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.ParentRunID, &typ, &payload,
			&occurredAt, &ev.CorrelationID, &ev.CausationID); err != nil {
			return nil, fmt.Errorf("event store: scan: %w", err)
		}
		ev.Type = Type(typ)
		// modernc.org/sqlite returns nil for zero-length BLOBs. Normalize to
		// an empty slice so callers can treat Payload uniformly without
		// nil-checking — the input contract guarantees []byte{} round-trips
		// faithfully.
		if payload == nil {
			payload = []byte{}
		}
		ev.Payload = payload
		ev.Timestamp = time.Unix(0, occurredAt)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarshalJSONPayload is a small helper that marshals any struct into a
// JSON payload suitable for Event.Payload. Returns an empty slice on
// error — callers that need strict semantics should marshal themselves.
func MarshalJSONPayload(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
