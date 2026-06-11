package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

// testKeyring builds a keyring with a viewer (run:read only) and an operator
// (run:read + run:create) principal, each behind its own bearer token.
func testKeyring() *rbac.Keyring {
	roles := rbac.RoleSet{
		"viewer":   {Capabilities: []string{rbac.CapRunRead}},
		"operator": {Capabilities: []string{rbac.CapRunRead, rbac.CapRunCreate}},
	}
	entries := []rbac.KeyEntry{
		{Token: "viewer-tok", Principal: rbac.Principal{ID: "v", Roles: []string{"viewer"}}},
		{Token: "operator-tok", Principal: rbac.Principal{ID: "o", Roles: []string{"operator"}}},
	}
	return rbac.NewKeyring(roles, entries)
}

func newKeyringTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		Keyring:    testKeyring(),
	})
}

// TestRBAC_KeyringAuth confirms the keyring branch of withAuth: healthz exempt,
// unknown token 401, valid token threads a principal through the request ctx.
func TestRBAC_KeyringAuth(t *testing.T) {
	s := newKeyringTestServer(t)
	h := s.Handler()

	// /healthz exempt.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", rec.Code)
	}

	// No token → 401 with WWW-Authenticate: Bearer.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/runs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("no token: WWW-Authenticate = %q, want Bearer", got)
	}

	// Unknown token → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: got %d, want 401", rec.Code)
	}

	// Valid viewer token → run:read passes (200 from handleListRuns).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer viewer-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET /v1/runs: got %d, want 200", rec.Code)
	}
}

// TestRBAC_PrincipalThreadedToCtx verifies withAuth injects the looked-up
// principal so downstream handlers can read it.
func TestRBAC_PrincipalThreadedToCtx(t *testing.T) {
	s := newKeyringTestServer(t)
	var seen rbac.Principal
	var ok bool
	probe := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = principalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer operator-tok")
	probe.ServeHTTP(rec, req)
	if !ok {
		t.Fatal("expected a principal in ctx")
	}
	if seen.ID != "o" {
		t.Fatalf("principal ID = %q, want o", seen.ID)
	}
}

// TestRBAC_LegacyTokenInjectsAdmin confirms the single-token path still
// injects an admin principal (so capability checks short-circuit allow) while
// staying byte-identical for legacy handlers.
func TestRBAC_LegacyTokenInjectsAdmin(t *testing.T) {
	s := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
		AuthToken:  "tok",
	})
	var seen rbac.Principal
	var ok bool
	probe := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = principalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer tok")
	probe.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy correct token: got %d, want 200", rec.Code)
	}
	if !ok {
		t.Fatal("expected an admin principal in ctx on legacy success")
	}
	// Keyring is nil on this path; the admin principal must resolve to
	// all-access even against an empty RoleSet.
	if !(rbac.RoleSet{}).Resolve(seen).AllowsCapability(rbac.CapRunCreate) {
		t.Fatal("legacy principal should resolve to all-access (admin)")
	}
}

// TestRBAC_RequireCapability exercises requireCapability directly.
func TestRBAC_RequireCapability(t *testing.T) {
	s := newKeyringTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel: next ran
	})
	wrapped := s.requireCapability(rbac.CapRunCreate, next)

	// No principal in ctx → 401.
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/runs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: got %d, want 401", rec.Code)
	}

	// Viewer (run:read only) → 403.
	viewer := rbac.Principal{ID: "v", Roles: []string{"viewer"}}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/runs", nil)
	req = req.WithContext(withPrincipal(req.Context(), viewer))
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer run:create: got %d, want 403", rec.Code)
	}

	// Operator (run:create) → next runs.
	operator := rbac.Principal{ID: "o", Roles: []string{"operator"}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/runs", nil)
	req = req.WithContext(withPrincipal(req.Context(), operator))
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("operator run:create: got %d, want 418 (next ran)", rec.Code)
	}
}

// TestRBAC_RequireCapabilityLegacyShortCircuit confirms the keyring-nil path
// passes straight through (byte-identical legacy behavior).
func TestRBAC_RequireCapabilityLegacyShortCircuit(t *testing.T) {
	s := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(DefaultStateCacheCapacity),
	})
	ran := false
	wrapped := s.requireCapability(rbac.CapRunCreate, func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/runs", nil))
	if !ran {
		t.Fatal("keyring-nil: next should run unconditionally")
	}
}

// TestRBAC_RouteCapabilityEnforced wires the full handler chain: a viewer
// cannot POST /v1/runs (run:create) but can GET /v1/runs (run:read).
func TestRBAC_RouteCapabilityEnforced(t *testing.T) {
	s := newKeyringTestServer(t)
	h := s.Handler()

	// Viewer POST /v1/runs → 403 (capability gate fires before launcher 503).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer viewer-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer POST /v1/runs: got %d, want 403", rec.Code)
	}

	// Operator POST /v1/runs → passes capability; launcher nil ⇒ 503 (not 403).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer operator-tok")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("operator POST /v1/runs: got 403, expected to pass capability gate")
	}
}
