package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/compliance"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/rbac"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

func seedComplianceStores(t *testing.T) (store.Store, governance.AuditLog) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	al, err := governance.OpenAuditLog(dir + "/audit.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })

	ctx := context.Background()
	if err := st.CreateRun(ctx, store.Run{
		ID: "run-1", Agent: "claude", Prompt: "do work", Status: store.StatusSuccess,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := al.Append(ctx, governance.AuditRow{
		RunID: "run-1", ActionType: "agent.apply_patch", Result: governance.Review,
		PolicyName: "diff_size", Reasons: []string{"too large"},
	}); err != nil {
		t.Fatal(err)
	}
	return st, al
}

func TestCompliance_JSON(t *testing.T) {
	st, al := seedComplianceStores(t)
	s := New(Options{
		Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity),
		Audit: al, RunStore: st,
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs/run-1/compliance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var rep compliance.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Run.ID != "run-1" || rep.Summary.Total != 1 || rep.Integrity.Digest == "" {
		t.Fatalf("unexpected report: %+v", rep)
	}
}

func TestCompliance_Markdown(t *testing.T) {
	st, al := seedComplianceStores(t)
	s := New(Options{
		Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity),
		Audit: al, RunStore: st,
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs/run-1/compliance?format=md", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestCompliance_BadFormat(t *testing.T) {
	st, al := seedComplianceStores(t)
	s := New(Options{
		Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity),
		Audit: al, RunStore: st,
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs/run-1/compliance?format=pdf", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestCompliance_NoAudit503(t *testing.T) {
	s := New(Options{Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity)})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs/run-1/compliance", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

func TestCompliance_UnknownRun404(t *testing.T) {
	_, al := seedComplianceStores(t)
	s := New(Options{
		Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity),
		Audit: al,
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs/ghost/compliance", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestCompliance_RBACForbidden(t *testing.T) {
	st, al := seedComplianceStores(t)
	roles := rbac.RoleSet{
		"viewer":  {Capabilities: []string{rbac.CapRunRead}},
		"auditor": {Capabilities: []string{rbac.CapRunRead, rbac.CapAuditRead}},
	}
	kr := rbac.NewKeyring(roles, []rbac.KeyEntry{
		{Token: "viewer-tok", Principal: rbac.Principal{ID: "v", Roles: []string{"viewer"}}},
		{Token: "auditor-tok", Principal: rbac.Principal{ID: "a", Roles: []string{"auditor"}}},
	})
	s := New(Options{
		Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity),
		Audit: al, RunStore: st, Keyring: kr,
	})
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs/run-1/compliance", nil)
	req.Header.Set("Authorization", "Bearer viewer-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: got %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/runs/run-1/compliance", nil)
	req.Header.Set("Authorization", "Bearer auditor-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auditor: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
