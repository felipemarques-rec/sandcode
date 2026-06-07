package governance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestAudit(t *testing.T) *SQLiteAuditLog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	a, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestAudit_AppendListRoundtrip(t *testing.T) {
	t.Parallel()
	a := openTestAudit(t)
	ctx := context.Background()

	rows := []AuditRow{
		{RunID: "r1", ActionType: ActionExecute, Result: Allow, Reasons: []string{}},
		{RunID: "r1", ActionType: ActionRefine, Result: Review, Reasons: []string{"diff over threshold"}, PolicyName: "diff_size"},
		{RunID: "r1", ActionType: ActionRefine, Result: Deny, Reasons: []string{"budget over"}, PolicyName: "budget"},
	}
	for _, r := range rows {
		if err := a.Append(ctx, r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := a.ListByRun(ctx, "r1")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	if got[0].Result != Allow || got[1].Result != Review || got[2].Result != Deny {
		t.Fatalf("results out of order: %+v", got)
	}
	if got[1].PolicyName != "diff_size" {
		t.Fatalf("PolicyName roundtrip broken: %s", got[1].PolicyName)
	}
}

func TestAudit_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()
	a := openTestAudit(t)
	ctx := context.Background()
	cases := []AuditRow{
		{ActionType: ActionExecute, Result: Allow},
		{RunID: "x", Result: Allow},
		{RunID: "x", ActionType: ActionExecute},
	}
	for i, r := range cases {
		if err := a.Append(ctx, r); err == nil {
			t.Fatalf("case %d: expected error for %+v", i, r)
		}
	}
}

func TestAudit_AutoAssignsIDAndTime(t *testing.T) {
	t.Parallel()
	a := openTestAudit(t)
	ctx := context.Background()
	before := time.Now()
	if err := a.Append(ctx, AuditRow{
		RunID: "r-auto", ActionType: ActionExecute, Result: Allow,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := a.ListByRun(ctx, "r-auto")
	if got[0].ID == "" {
		t.Fatalf("ID was not auto-assigned")
	}
	if got[0].CreatedAt.Before(before) {
		t.Fatalf("CreatedAt %s before %s", got[0].CreatedAt, before)
	}
}

func TestAudit_IsolatesByRunID(t *testing.T) {
	t.Parallel()
	a := openTestAudit(t)
	ctx := context.Background()
	_ = a.Append(ctx, AuditRow{RunID: "rA", ActionType: ActionExecute, Result: Allow})
	_ = a.Append(ctx, AuditRow{RunID: "rB", ActionType: ActionExecute, Result: Deny})
	a1, _ := a.ListByRun(ctx, "rA")
	b1, _ := a.ListByRun(ctx, "rB")
	if len(a1) != 1 || a1[0].Result != Allow {
		t.Fatalf("rA isolation broken: %+v", a1)
	}
	if len(b1) != 1 || b1[0].Result != Deny {
		t.Fatalf("rB isolation broken: %+v", b1)
	}
}

func TestAudit_SchemaIsAppendOnly(t *testing.T) {
	t.Parallel()
	if strings.Contains(schemaAudit, "ON CONFLICT") {
		t.Fatalf("schemaAudit contains ON CONFLICT — append-only invariant broken")
	}
	if strings.Contains(schemaAudit, "REPLACE") {
		t.Fatalf("schemaAudit contains REPLACE — append-only invariant broken")
	}
}

func TestLogDecision_WritesAggregateAndPerPolicyRows(t *testing.T) {
	t.Parallel()
	a := openTestAudit(t)
	ctx := context.Background()

	action := Action{Type: ActionRefine, RunID: "rD", Attempt: 4}
	d := Decision{
		Result:  Deny,
		Reasons: []string{"retry_limit: attempt=4 max=3"},
		PerPolicy: []PolicyVerdict{
			{Policy: "diff_size", Result: Allow, Reason: ""},
			{Policy: "retry_limit", Result: Deny, Reason: "attempt=4 max=3"},
		},
	}

	if err := LogDecision(ctx, a, "rD", action, d); err != nil {
		t.Fatalf("LogDecision: %v", err)
	}

	rows, _ := a.ListByRun(ctx, "rD")
	// Expect 2 rows: aggregate + the non-empty PerPolicy verdict for
	// retry_limit. The Allow-with-no-reason from diff_size is skipped.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (aggregate + retry_limit)", len(rows))
	}
	// Aggregate row has empty policy_name.
	if rows[0].PolicyName != "" || rows[0].Result != Deny {
		t.Fatalf("aggregate row wrong: %+v", rows[0])
	}
	// Per-policy row carries the policy name.
	if rows[1].PolicyName != "retry_limit" || rows[1].Result != Deny {
		t.Fatalf("per-policy row wrong: %+v", rows[1])
	}
}

func TestLogDecision_NilLogIsNoOp(t *testing.T) {
	t.Parallel()
	if err := LogDecision(context.Background(), nil, "r", Action{Type: ActionExecute}, Decision{Result: Allow}); err != nil {
		t.Fatalf("LogDecision(nil) returned error: %v", err)
	}
}
