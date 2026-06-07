package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
)

// newAuditTestServer wires a server with an on-disk SQLiteAuditLog so
// tests can append rows directly and then read them back through the
// HTTP handler.
func newAuditTestServer(t *testing.T) (*Server, *governance.SQLiteAuditLog) {
	t.Helper()
	al, err := governance.OpenAuditLog(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	t.Cleanup(func() { _ = al.Close() })

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	reg := metrics.NewRegistry()
	cache := NewStateCache(0)
	t.Cleanup(cache.Attach(bus).Cancel)

	srv := New(Options{
		Registry:   reg,
		StateCache: cache,
		Bus:        bus,
		Audit:      al,
	})
	return srv, al
}

func TestListRunAudit_EmptyReturnsEmptyArray(t *testing.T) {
	srv, _ := newAuditTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/aud00001/audit", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp ListAuditResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Errorf("Rows = %d, want 0", len(resp.Rows))
	}
	if resp.Rows == nil {
		t.Errorf("Rows is nil, want []")
	}
}

func TestListRunAudit_ReturnsRowsChronologically(t *testing.T) {
	srv, al := newAuditTestServer(t)
	const runID = "aud00002"

	rows := []governance.AuditRow{
		{
			RunID:      runID,
			ActionType: governance.ActionExecute,
			Result:     governance.Allow,
			Reasons:    []string{"no policies configured"},
		},
		{
			RunID:      runID,
			ActionType: governance.ActionRefine,
			Result:     governance.Deny,
			Reasons:    []string{"retry cap exceeded: attempt=4 max=3"},
			PolicyName: "retry_limit",
		},
		{
			RunID:      runID,
			ActionType: governance.ActionRefine,
			Result:     governance.Review,
			Reasons:    []string{"manual approval required"},
			PolicyName: "approval_required",
		},
	}
	for _, r := range rows {
		if err := al.Append(context.Background(), r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Insert a row for a different run to verify filtering.
	if err := al.Append(context.Background(), governance.AuditRow{
		RunID:      "different",
		ActionType: governance.ActionExecute,
		Result:     governance.Allow,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ListAuditResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	if len(resp.Rows) != 3 {
		t.Fatalf("Rows = %d, want 3 (different run must be filtered out)", len(resp.Rows))
	}
	// Chronological order: ActionType sequence allow, deny, review.
	wantResults := []string{"allow", "deny", "review"}
	for i, row := range resp.Rows {
		if row.Result != wantResults[i] {
			t.Errorf("Rows[%d].Result = %q, want %q", i, row.Result, wantResults[i])
		}
		if row.RunID != runID {
			t.Errorf("Rows[%d].RunID = %q, want %q", i, row.RunID, runID)
		}
	}
	// Spot-check the per-policy detail row preserves its policy name
	// and reason — that's the operator UX payoff of the whole feature.
	if resp.Rows[1].PolicyName != "retry_limit" {
		t.Errorf("Rows[1].PolicyName = %q, want retry_limit", resp.Rows[1].PolicyName)
	}
	if len(resp.Rows[1].Reasons) != 1 || resp.Rows[1].Reasons[0] == "" {
		t.Errorf("Rows[1].Reasons = %+v, want non-empty", resp.Rows[1].Reasons)
	}
}

func TestListRunAudit_NoAuditConfiguredReturns503(t *testing.T) {
	// Build a server without Options.Audit so the endpoint must 503.
	bus := event.NewLocalBus()
	defer bus.Close()
	reg := metrics.NewRegistry()
	cache := NewStateCache(0)
	t.Cleanup(cache.Attach(bus).Cancel)

	srv := New(Options{
		Registry:   reg,
		StateCache: cache,
		Bus:        bus,
		// Audit deliberately omitted.
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/whatever/audit", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestListRunAudit_LogDecisionEndToEnd(t *testing.T) {
	// Drive the same path the orchestrator takes: governance.LogDecision
	// produces an aggregate + per-policy rows; the endpoint must surface
	// them as a coherent timeline.
	srv, al := newAuditTestServer(t)
	const runID = "aud00003"

	d := governance.Decision{
		Result:  governance.Deny,
		Reasons: []string{"budget: tokens used 1000000 > limit 50000"},
		PerPolicy: []governance.PolicyVerdict{
			{Policy: "budget", Result: governance.Deny, Reason: "tokens used 1000000 > limit 50000"},
			{Policy: "retry_limit", Result: governance.Allow, Reason: ""},
		},
	}
	if err := governance.LogDecision(context.Background(), al, runID,
		governance.Action{Type: governance.ActionExecute, RunID: runID}, d); err != nil {
		t.Fatalf("LogDecision: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ListAuditResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	// LogDecision writes: 1 aggregate row + 1 per-policy row for the
	// budget deny (allow-with-empty-reason is filtered out).
	if len(resp.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2: %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Rows[0].PolicyName != "" {
		t.Errorf("Rows[0].PolicyName = %q, want empty (aggregate row)", resp.Rows[0].PolicyName)
	}
	if resp.Rows[1].PolicyName != "budget" {
		t.Errorf("Rows[1].PolicyName = %q, want budget", resp.Rows[1].PolicyName)
	}
}

func TestListRunAudit_FilterByResultDeny(t *testing.T) {
	srv, al := newAuditTestServer(t)
	const runID = "aud-filter-deny"

	rows := []governance.AuditRow{
		{RunID: runID, ActionType: governance.ActionExecute, Result: governance.Allow},
		{RunID: runID, ActionType: governance.ActionRefine, Result: governance.Deny, PolicyName: "retry_limit"},
		{RunID: runID, ActionType: governance.ActionRefine, Result: governance.Review, PolicyName: "approval_required"},
		{RunID: runID, ActionType: governance.ActionExecute, Result: governance.Deny, PolicyName: "budget"},
	}
	for _, r := range rows {
		if err := al.Append(context.Background(), r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit?result=deny", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ListAuditResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2 (deny-only)", len(resp.Rows))
	}
	for i, row := range resp.Rows {
		if row.Result != "deny" {
			t.Errorf("Rows[%d].Result = %q, want deny", i, row.Result)
		}
	}
}

func TestListRunAudit_FilterByResultAllow(t *testing.T) {
	srv, al := newAuditTestServer(t)
	const runID = "aud-filter-allow"

	for _, r := range []governance.AuditRow{
		{RunID: runID, ActionType: governance.ActionExecute, Result: governance.Allow},
		{RunID: runID, ActionType: governance.ActionRefine, Result: governance.Deny},
		{RunID: runID, ActionType: governance.ActionExecute, Result: governance.Allow},
	} {
		if err := al.Append(context.Background(), r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit?result=allow", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp ListAuditResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Errorf("Rows = %d, want 2 (allow-only)", len(resp.Rows))
	}
}

func TestListRunAudit_FilterByResultReview(t *testing.T) {
	srv, al := newAuditTestServer(t)
	const runID = "aud-filter-review"

	for _, r := range []governance.AuditRow{
		{RunID: runID, ActionType: governance.ActionExecute, Result: governance.Allow},
		{RunID: runID, ActionType: governance.ActionRefine, Result: governance.Review, PolicyName: "approval_required"},
	} {
		if err := al.Append(context.Background(), r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit?result=review", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp ListAuditResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(resp.Rows))
	}
	if resp.Rows[0].Result != "review" {
		t.Errorf("Rows[0].Result = %q, want review", resp.Rows[0].Result)
	}
}

func TestListRunAudit_FilterInvalidResult400(t *testing.T) {
	srv, _ := newAuditTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/any/audit?result=BOGUS", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid filter); body=%s", rr.Code, rr.Body.String())
	}
}

func TestListRunAudit_EmptyFilterReturnsAll(t *testing.T) {
	// ?result= (empty) must be treated as "no filter" — same behavior as
	// omitting the query param entirely.
	srv, al := newAuditTestServer(t)
	const runID = "aud-empty-filter"

	for _, r := range []governance.AuditRow{
		{RunID: runID, ActionType: governance.ActionExecute, Result: governance.Allow},
		{RunID: runID, ActionType: governance.ActionRefine, Result: governance.Deny},
	} {
		if err := al.Append(context.Background(), r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit?result=", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ListAuditResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Errorf("Rows = %d, want 2 (empty filter = no filter)", len(resp.Rows))
	}
}
