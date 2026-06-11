package governance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// AuditLog is the durable, append-only record of every governance
// decision. Implementations MUST guarantee:
//
//   - Append never replaces or modifies prior rows.
//   - Records are returned in chronological order for a given run_id.
//   - List supports filtering by run_id only; broader queries are
//     deliberately out of scope (compliance/forensics is its own UI).
type AuditLog interface {
	// Append persists one audit row. Returns an error if the row's ID
	// collides with an existing entry — IDs are PRIMARY KEY.
	Append(ctx context.Context, r AuditRow) error

	// ListByRun returns all audit rows for a run_id, chronologically.
	ListByRun(ctx context.Context, runID string) ([]AuditRow, error)

	// Close releases the backing storage handle.
	Close() error
}

// AuditRow captures one Engine.Evaluate decision. The Reasons field
// preserves the per-policy breakdown so an operator can see exactly
// who said what.
type AuditRow struct {
	ID         string
	RunID      string
	ActionType ActionType
	Result     Result
	Reasons    []string // serialized to TEXT via newline-join
	PolicyName string   // empty when aggregate; set when storing a per-policy row
	Approver   string   // empty for auto; set to user/system ID when reviewed
	CreatedAt  time.Time
}

// schemaAudit is the immutable schema for governance decisions.
// No UPDATE/DELETE methods exist on SQLiteAuditLog — the append-only
// invariant is enforced by the type surface, not just by convention.
const schemaAudit = `
CREATE TABLE IF NOT EXISTS audit_log (
    id           TEXT PRIMARY KEY,
    run_id       TEXT NOT NULL,
    action_type  TEXT NOT NULL,
    result       TEXT NOT NULL,
    reasons      TEXT NOT NULL DEFAULT '',
    policy_name  TEXT NOT NULL DEFAULT '',
    approver     TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_run     ON audit_log(run_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_result  ON audit_log(result, created_at);
`

// SQLiteAuditLog is the production AuditLog backed by SQLite (pure-Go).
type SQLiteAuditLog struct {
	db   *sql.DB
	path string
}

// OpenAuditLog opens (and migrates) an audit log database at path.
// Parent directories are created as needed.
func OpenAuditLog(path string) (*SQLiteAuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("audit: open: %w", err)
	}
	if _, err := db.Exec(schemaAudit); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("audit: schema: %w", err)
	}
	return &SQLiteAuditLog{db: db, path: path}, nil
}

// Close releases the database handle.
func (a *SQLiteAuditLog) Close() error { return a.db.Close() }

// Append persists one audit row. Auto-assigns an ID and CreatedAt when
// the caller leaves them zero-valued so quick-fire call sites don't
// need to wire those concerns.
func (a *SQLiteAuditLog) Append(ctx context.Context, r AuditRow) error {
	if r.RunID == "" {
		return fmt.Errorf("audit: Append requires a non-empty RunID")
	}
	if r.ActionType == "" {
		return fmt.Errorf("audit: Append requires a non-empty ActionType")
	}
	if r.Result == "" {
		return fmt.Errorf("audit: Append requires a non-empty Result")
	}
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	reasonsBlob := strings.Join(r.Reasons, "\n")
	_, err := a.db.ExecContext(ctx, `
        INSERT INTO audit_log (id, run_id, action_type, result, reasons, policy_name, approver, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RunID, string(r.ActionType), string(r.Result),
		reasonsBlob, r.PolicyName, r.Approver, r.CreatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// ListByRun returns rows for run_id sorted ASC by created_at (ties
// broken by rowid).
func (a *SQLiteAuditLog) ListByRun(ctx context.Context, runID string) ([]AuditRow, error) {
	rows, err := a.db.QueryContext(ctx, `
        SELECT id, run_id, action_type, result, reasons, policy_name, approver, created_at
        FROM audit_log
        WHERE run_id = ?
        ORDER BY created_at ASC, rowid ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: query: %w", err)
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var actionType, result, reasonsBlob string
		var createdAt int64
		if err := rows.Scan(&r.ID, &r.RunID, &actionType, &result, &reasonsBlob,
			&r.PolicyName, &r.Approver, &createdAt); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		r.ActionType = ActionType(actionType)
		r.Result = Result(result)
		if reasonsBlob != "" {
			r.Reasons = strings.Split(reasonsBlob, "\n")
		}
		r.CreatedAt = time.Unix(0, createdAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LogApproval records that a Review verdict was granted. It appends a single
// audit row with Result=Approved and the approver id. ID and CreatedAt are
// filled by Append. Returns nil when log is nil.
func LogApproval(ctx context.Context, log AuditLog, runID string, action ActionType, approver string) error {
	if log == nil {
		return nil
	}
	return log.Append(ctx, AuditRow{
		RunID:      runID,
		ActionType: action,
		Result:     Approved,
		Approver:   approver,
	})
}

// LogDecision is a convenience helper that converts a Decision into an
// aggregate audit row + per-policy detail rows, and appends all of them.
// Idempotent failure handling: if the first append fails, the rest are
// skipped and the error is returned. Callers may also use Append
// directly for full control.
func LogDecision(ctx context.Context, log AuditLog, runID string, action Action, d Decision) error {
	if log == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	// Aggregate row first.
	if err := log.Append(ctx, AuditRow{
		RunID:      runID,
		ActionType: action.Type,
		Result:     d.Result,
		Reasons:    d.Reasons,
	}); err != nil {
		return err
	}
	for _, pv := range d.PerPolicy {
		if pv.Result == Allow && pv.Reason == "" {
			continue // skip noise — explicit allows with no reason are uninteresting
		}
		if err := log.Append(ctx, AuditRow{
			RunID:      runID,
			ActionType: action.Type,
			Result:     pv.Result,
			Reasons:    []string{pv.Reason},
			PolicyName: pv.Policy,
		}); err != nil {
			return err
		}
	}
	return nil
}
