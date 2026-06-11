package main

import (
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

// TestPermittedFor_NilRoleSet confirms the no-RBAC seam: a nil RoleSet yields a
// nil filter so callers (mcpExtraArgs) allow every tool — byte-identical.
func TestPermittedFor_NilRoleSet(t *testing.T) {
	if got := permittedFor(nil, rbac.Principal{ID: "x", Roles: []string{"whatever"}}); got != nil {
		t.Fatalf("permittedFor(nil, _) = non-nil, want nil")
	}
	if got := permittedFor(nil, rbac.AdminPrincipal()); got != nil {
		t.Fatalf("permittedFor(nil, admin) = non-nil, want nil")
	}
}

// TestPermittedFor_OperatorRole confirms a configured RoleSet produces a filter
// that permits exactly the operator's tools and rejects others.
func TestPermittedFor_OperatorRole(t *testing.T) {
	rs := rbac.RoleSet{
		"operator": {Tools: []string{"mcp__context7__query-docs", "Read"}},
		"viewer":   {Tools: []string{"Read"}},
	}
	filter := permittedFor(rs, rbac.Principal{ID: "o", Roles: []string{"operator"}})
	if filter == nil {
		t.Fatal("permittedFor(roleSet, operator) = nil, want non-nil")
	}
	if !filter("mcp__context7__query-docs") {
		t.Error("operator filter should permit mcp__context7__query-docs")
	}
	if !filter("Read") {
		t.Error("operator filter should permit Read")
	}
	if filter("Bash") {
		t.Error("operator filter should reject Bash")
	}
}

// TestPermittedFor_Admin confirms an admin principal resolves to all-access even
// against an empty/sparse RoleSet — the filter permits everything.
func TestPermittedFor_Admin(t *testing.T) {
	rs := rbac.RoleSet{"viewer": {Tools: []string{"Read"}}}
	filter := permittedFor(rs, rbac.AdminPrincipal())
	if filter == nil {
		t.Fatal("permittedFor(roleSet, admin) = nil, want non-nil")
	}
	for _, tool := range []string{"Read", "Bash", "mcp__anything__x", "WhateverTool"} {
		if !filter(tool) {
			t.Errorf("admin filter should permit %q", tool)
		}
	}
}
