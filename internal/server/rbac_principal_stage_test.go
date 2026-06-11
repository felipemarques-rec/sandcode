package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

// principalRecordingLauncher captures the req.Principal it is handed so the
// test can assert the authenticated identity was staged server-side (NEVER
// decoded from the body).
type principalRecordingLauncher struct {
	mu     sync.Mutex
	gotReq RunRequest
	done   chan struct{}
}

func (l *principalRecordingLauncher) Launch(ctx context.Context, runID string, req RunRequest) error {
	l.mu.Lock()
	l.gotReq = req
	l.mu.Unlock()
	if l.done != nil {
		close(l.done)
	}
	return nil
}

func (l *principalRecordingLauncher) snapshot() RunRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gotReq
}

// TestCreateRun_StagesAuthenticatedPrincipal drives POST /v1/runs with a valid
// operator token through the full handler chain and asserts the launcher
// received the authenticated principal staged on RunRequest — with ID and
// roles taken from the keyring, not the body.
func TestCreateRun_StagesAuthenticatedPrincipal(t *testing.T) {
	launcher := &principalRecordingLauncher{done: make(chan struct{})}
	srv := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(0),
		Launcher:   launcher,
		Keyring:    testKeyring(),
	})

	// Body attempts to smuggle a forged principal — it must be ignored
	// because Principal carries json:"-".
	body := []byte(`{"prompt":"p","cwd":"/r","sandbox_image":"a"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer operator-tok")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rr.Code, rr.Body.String())
	}

	select {
	case <-launcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("launcher not invoked within 2s")
	}

	got := launcher.snapshot()
	if got.Principal.ID != "o" {
		t.Errorf("staged Principal.ID = %q, want %q", got.Principal.ID, "o")
	}
	if len(got.Principal.Roles) != 1 || got.Principal.Roles[0] != "operator" {
		t.Errorf("staged Principal.Roles = %v, want [operator]", got.Principal.Roles)
	}
}

// TestCreateRun_PrincipalFromBodyIgnored confirms a client cannot inject its
// own roles: a forged "principal" in the JSON body is rejected as an unknown
// field (DisallowUnknownFields), proving Principal is not body-decodable.
func TestCreateRun_PrincipalFromBodyIgnored(t *testing.T) {
	launcher := &principalRecordingLauncher{done: make(chan struct{})}
	srv := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(0),
		Launcher:   launcher,
		Keyring:    testKeyring(),
	})

	body := []byte(`{"prompt":"p","cwd":"/r","sandbox_image":"a","principal":{"id":"admin","roles":["operator"]}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer operator-tok")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("forged principal in body: status = %d, want 400 (unknown field): %s", rr.Code, rr.Body.String())
	}
}

// TestCreateRun_LegacyNoKeyring_ZeroPrincipal confirms that with no keyring
// (legacy path) the staged Principal is the zero value — byte-identical, empty
// roles.
func TestCreateRun_LegacyNoKeyring_ZeroPrincipal(t *testing.T) {
	launcher := &principalRecordingLauncher{done: make(chan struct{})}
	srv := New(Options{
		Registry:   metrics.NewRegistry(),
		StateCache: NewStateCache(0),
		Launcher:   launcher,
		// no Keyring, no AuthToken => no-auth legacy path
	})

	body, _ := json.Marshal(RunRequest{Prompt: "p", CWD: "/r", SandboxImage: "a"})
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rr.Code, rr.Body.String())
	}

	select {
	case <-launcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("launcher not invoked within 2s")
	}

	got := launcher.snapshot()
	if !reflectZeroPrincipal(got.Principal) {
		t.Errorf("legacy staged Principal = %+v, want zero", got.Principal)
	}
}

func reflectZeroPrincipal(p rbac.Principal) bool {
	return p.ID == "" && len(p.Roles) == 0
}
