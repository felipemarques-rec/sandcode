package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/approval"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

func newApprovalTestServer(t *testing.T, reg *approval.Registry) *Server {
	t.Helper()
	return New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		Approvals:  reg,
	})
}

func TestApprovals_ResolveWaiter(t *testing.T) {
	reg := approval.NewRegistry()
	h := newApprovalTestServer(t, reg).Handler()

	got := make(chan approval.Decision, 1)
	go func() {
		d, _ := reg.RequestApproval(t.Context(), approval.Request{RunID: "run1"})
		got <- d
	}()
	for len(reg.Pending()) == 0 {
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/approvals/run1", strings.NewReader(`{"decision":"approve","approver":"bob"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if d := <-got; !d.Approved || d.Approver != "bob" {
		t.Fatalf("decision = %+v, want approved by bob", d)
	}
}

func TestApprovals_NoWaiter404(t *testing.T) {
	h := newApprovalTestServer(t, approval.NewRegistry()).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/approvals/ghost", strings.NewReader(`{"decision":"approve"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestApprovals_BadDecision400(t *testing.T) {
	h := newApprovalTestServer(t, approval.NewRegistry()).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/approvals/x", strings.NewReader(`{"decision":"maybe"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

// TestApprovals_ApproverFromPrincipal confirms that when a keyring is
// configured, the resolved Decision.Approver comes from the authenticated
// principal (set by withAuth), NOT from the client-supplied body.approver — so
// a caller cannot spoof the approver identity.
func TestApprovals_ApproverFromPrincipal(t *testing.T) {
	roles := rbac.RoleSet{
		"approver": {Capabilities: []string{rbac.CapApprove}},
	}
	entries := []rbac.KeyEntry{
		{Token: "approver-tok", Principal: rbac.Principal{ID: "alice", Roles: []string{"approver"}}},
	}
	keyring := rbac.NewKeyring(roles, entries)

	reg := approval.NewRegistry()
	s := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		Approvals:  reg,
		Keyring:    keyring,
	})
	h := s.Handler()

	got := make(chan approval.Decision, 1)
	go func() {
		d, _ := reg.RequestApproval(t.Context(), approval.Request{RunID: "run1"})
		got <- d
	}()
	for len(reg.Pending()) == 0 {
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/approvals/run1", strings.NewReader(`{"decision":"approve","approver":"attacker"}`))
	req.Header.Set("Authorization", "Bearer approver-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if d := <-got; !d.Approved || d.Approver != "alice" {
		t.Fatalf("decision = %+v, want approved by alice (principal), not the spoofed body.approver", d)
	}
}

func TestApprovals_NilRegistry503(t *testing.T) {
	s := New(Options{Registry: metrics.NewRegistry(), StateCache: NewStateCache(DefaultStateCacheCapacity)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/approvals/x", strings.NewReader(`{"decision":"approve"}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}
