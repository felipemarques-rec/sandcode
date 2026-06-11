package main

import (
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
)

// TestBuildRoleRegistry_EmptySlice verifies that an empty spec slice returns
// (nil, nil) — preserving the legacy orchestrator path.
func TestBuildRoleRegistry_EmptySlice(t *testing.T) {
	reg, err := buildRoleRegistry(nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if reg != nil {
		t.Fatal("expected nil registry for empty specs")
	}
}

// TestBuildRoleRegistry_EmptySliceExplicit covers an explicitly empty (not nil) slice.
func TestBuildRoleRegistry_EmptySliceExplicit(t *testing.T) {
	reg, err := buildRoleRegistry([]string{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if reg != nil {
		t.Fatal("expected nil registry for empty specs")
	}
}

// TestBuildRoleRegistry_ValidSingle verifies that a single valid spec
// (implementer=codex) results in a registry with the codex provider registered.
func TestBuildRoleRegistry_ValidSingle(t *testing.T) {
	reg, err := buildRoleRegistry([]string{"implementer=codex"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
	p, err := reg.Resolve(t.Context(), agent.RoleImplementer)
	if err != nil {
		t.Fatalf("resolve implementer: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.Name() != "codex" {
		t.Fatalf("expected codex provider, got %q", p.Name())
	}
}

// TestBuildRoleRegistry_ValidMultiple verifies multiple roles can be registered
// in a single call.
func TestBuildRoleRegistry_ValidMultiple(t *testing.T) {
	specs := []string{
		"implementer=codex",
		"reviewer=claude",
		"planner=cursor",
	}
	reg, err := buildRoleRegistry(specs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}

	ctx := t.Context()

	p, err := reg.Resolve(ctx, agent.RoleImplementer)
	if err != nil || p == nil {
		t.Fatalf("resolve implementer: err=%v provider=%v", err, p)
	}
	if p.Name() != "codex" {
		t.Errorf("implementer: expected codex, got %q", p.Name())
	}

	p, err = reg.Resolve(ctx, agent.RoleReviewer)
	if err != nil || p == nil {
		t.Fatalf("resolve reviewer: err=%v provider=%v", err, p)
	}
	// claude-code resolves as "claude-code" (Name())
	if !strings.Contains(p.Name(), "claude") {
		t.Errorf("reviewer: expected claude provider, got %q", p.Name())
	}

	p, err = reg.Resolve(ctx, agent.RolePlanner)
	if err != nil || p == nil {
		t.Fatalf("resolve planner: err=%v provider=%v", err, p)
	}
	if p.Name() != "cursor" {
		t.Errorf("planner: expected cursor, got %q", p.Name())
	}
}

// TestBuildRoleRegistry_NonImplementerRole verifies that non-implementer valid
// roles (e.g. reviewer=claude) are registered without error — they are valid
// vocabulary even when SP1 only acts on implementer at runtime.
func TestBuildRoleRegistry_NonImplementerRole(t *testing.T) {
	reg, err := buildRoleRegistry([]string{"reviewer=claude"})
	if err != nil {
		t.Fatalf("unexpected error for valid non-implementer role: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}

// TestBuildRoleRegistry_UnknownRole verifies that an unknown role token
// returns an error containing the role name and valid roles list.
func TestBuildRoleRegistry_UnknownRole(t *testing.T) {
	_, err := buildRoleRegistry([]string{"implementr=codex"}) // typo: "implementr"
	if err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
	if !strings.Contains(err.Error(), "implementr") {
		t.Errorf("error should mention the unknown role: %v", err)
	}
	if !strings.Contains(err.Error(), "planner") {
		t.Errorf("error should list valid roles: %v", err)
	}
}

// TestBuildRoleRegistry_UnknownAgent verifies that an unknown agent token
// propagates resolveAgent's error.
func TestBuildRoleRegistry_UnknownAgent(t *testing.T) {
	_, err := buildRoleRegistry([]string{"implementer=noagent"})
	if err == nil {
		t.Fatal("expected error for unknown agent, got nil")
	}
	if !strings.Contains(err.Error(), "noagent") {
		t.Errorf("error should mention the unknown agent: %v", err)
	}
}

// TestBuildRoleRegistry_MalformedNoEquals verifies that a spec without '='
// returns an invalid-format error.
func TestBuildRoleRegistry_MalformedNoEquals(t *testing.T) {
	_, err := buildRoleRegistry([]string{"implementercodex"})
	if err == nil {
		t.Fatal("expected error for missing '=', got nil")
	}
	if !strings.Contains(err.Error(), "want role=agent") {
		t.Errorf("error message should mention format: %v", err)
	}
}

// TestBuildRoleRegistry_MalformedEmptyRole verifies that "=codex" (empty role) errors.
func TestBuildRoleRegistry_MalformedEmptyRole(t *testing.T) {
	_, err := buildRoleRegistry([]string{"=codex"})
	if err == nil {
		t.Fatal("expected error for empty role, got nil")
	}
	if !strings.Contains(err.Error(), "want role=agent") {
		t.Errorf("error message should mention format: %v", err)
	}
}

// TestBuildRoleRegistry_MalformedEmptyAgent verifies that "implementer=" (empty agent) errors.
func TestBuildRoleRegistry_MalformedEmptyAgent(t *testing.T) {
	_, err := buildRoleRegistry([]string{"implementer="})
	if err == nil {
		t.Fatal("expected error for empty agent, got nil")
	}
	if !strings.Contains(err.Error(), "want role=agent") {
		t.Errorf("error message should mention format: %v", err)
	}
}

// TestBuildRoleRegistry_AllValidRoles verifies all 9 known roles are accepted,
// including the diagram's Performance Reviewer / Refactoring Specialist (E1.4b).
func TestBuildRoleRegistry_AllValidRoles(t *testing.T) {
	allRoles := []string{
		"planner=claude",
		"architect=claude",
		"implementer=codex",
		"verifier=cursor",
		"reviewer=claude",
		"security_reviewer=claude",
		"performance_reviewer=claude",
		"refactoring_specialist=claude",
		"reporter=claude",
	}
	reg, err := buildRoleRegistry(allRoles)
	if err != nil {
		t.Fatalf("all 9 valid roles should register without error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}
